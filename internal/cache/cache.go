package cache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errUnsafeEntry = errors.New("unsafe cache entry")
var errClosed = errors.New("cache store is closed")

type metadata struct {
	Schema    int         `json:"schema"`
	SourceURL string      `json:"source_url"`
	URL       string      `json:"url"`
	Status    int         `json:"status"`
	Headers   http.Header `json:"headers"`
	SHA256    string      `json:"sha256"`
	Size      int64       `json:"size"`
	CheckedAt time.Time   `json:"checked_at"`
}

// Store coordinates transactions per canonical URL. Immutable content-named
// bodies are published before the URL metadata pointer.
type Store struct {
	root      *os.Root
	transport model.Transport
	maxBytes  int64
	notify    func(string)
	mu        sync.Mutex
	flights   map[string]*flight
	fresh     map[string]bool // Network-validated during this Store lifetime.
	closed    bool
	calls     sync.WaitGroup
	work      sync.WaitGroup
	publish   sync.Mutex
	notifyMu  sync.Mutex
	reused    map[string]bool
	reads     atomic.Uint64
	downloads atomic.Uint64
	validated atomic.Uint64
	failures  atomic.Uint64
}

// Stats describes completed cache activity. UniqueReused counts canonical URLs
// satisfied from verified existing bytes, while VerifiedBodyOpens includes
// deliberate repeated reads such as the HTML archive's two passes.
type Stats struct {
	VerifiedBodyOpens uint64 `json:"verified_body_opens"`
	Downloaded        uint64 `json:"downloaded_resources"`
	Revalidated       uint64 `json:"revalidated_resources"`
	UniqueReused      uint64 `json:"unique_resources_reused"`
	Failed            uint64 `json:"failed_retrievals"`
}

type flight struct {
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	refresh   bool
	networked bool
	finished  bool
	abandoned bool
	err       error
}

func Open(path string, transport model.Transport, maxBytes int64, notify func(string)) (*Store, error) {
	return OpenContext(context.Background(), path, transport, maxBytes, notify)
}

func OpenContext(ctx context.Context, path string, transport model.Transport, maxBytes int64, notify func(string)) (*Store, error) {
	if maxBytes < 1 || transport == nil || notify == nil {
		return nil, errors.New("cache requires a transport, positive byte limit and diagnostic callback")
	}
	root, err := openRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	return &Store{root: root, transport: transport, maxBytes: maxBytes, notify: notify,
		flights: map[string]*flight{}, fresh: map[string]bool{}, reused: map[string]bool{}}, nil
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	reused := len(s.reused)
	s.mu.Unlock()
	return Stats{
		VerifiedBodyOpens: s.reads.Load(),
		Downloaded:        s.downloads.Load(),
		Revalidated:       s.validated.Load(),
		UniqueReused:      uint64(reused),
		Failed:            s.failures.Load(),
	}
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errClosed
	}
	s.closed = true
	for _, active := range s.flights {
		active.cancel()
	}
	s.mu.Unlock()
	s.calls.Wait()
	s.work.Wait()
	return s.root.Close()
}

func (s *Store) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	s.calls.Add(1)
	return nil
}

func (s *Store) warn(message string) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.notify(message)
}

func (s *Store) acquire(ctx context.Context, source, key string, refresh bool) (*flight, bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, false, errClosed
	}
	if active := s.flights[source]; active != nil {
		if active.abandoned {
			return active, false, true, nil
		}
		active.waiters++
		return active, true, false, nil
	}
	if refresh {
		delete(s.fresh, source)
	}
	workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	active := &flight{done: make(chan struct{}), cancel: cancel, waiters: 1, refresh: refresh}
	s.flights[source] = active
	s.work.Add(1)
	go s.run(workCtx, source, key, active)
	return active, false, false, nil
}

func (s *Store) run(ctx context.Context, source, key string, active *flight) {
	defer s.work.Done()
	active.networked, active.err = s.ensure(ctx, source, key, active.refresh)
	s.mu.Lock()
	active.finished = true
	if active.err == nil && (active.refresh || active.networked) {
		s.fresh[source] = true
	} else if active.err != nil && (active.refresh || active.networked) {
		delete(s.fresh, source)
	}
	if s.flights[source] == active {
		delete(s.flights, source)
	}
	close(active.done)
	active.cancel()
	s.mu.Unlock()
}

func (s *Store) wait(ctx context.Context, active *flight) error {
	select {
	case <-active.done:
		s.mu.Lock()
		active.waiters--
		s.mu.Unlock()
		return active.err
	case <-ctx.Done():
		s.mu.Lock()
		active.waiters--
		if active.waiters == 0 && !active.finished {
			active.abandoned = true
			active.cancel()
		}
		s.mu.Unlock()
		return ctx.Err()
	}
}

func (s *Store) prepare(ctx context.Context, source, key string, refresh bool) error {
	for {
		active, joined, retiring, err := s.acquire(ctx, source, key, refresh)
		if err != nil {
			return err
		}
		if retiring {
			select {
			case <-active.done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err = s.wait(ctx, active)
		if err != nil {
			return err
		}
		if refresh && joined && !active.refresh && !active.networked {
			continue
		}
		return nil
	}
}

func (s *Store) regular(name string) error {
	if err := storage.Check(s.root, name); err != nil {
		return fmt.Errorf("%w: %v", errUnsafeEntry, err)
	}
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: must be a regular file, not a symlink or directory: %s", errUnsafeEntry, name)
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (s *Store) read(ctx context.Context, key, source string) (metadata, *os.File, error) {
	var m metadata
	f, err := s.root.Open(key + ".json")
	if err != nil {
		return m, nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return m, nil, err
	}
	if len(data) > 1<<20 {
		return m, nil, errors.New("oversized cache metadata")
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, nil, err
	}
	final, err := fetch.CanonicalURL(m.URL)
	if err != nil || final != m.URL || m.Schema != 1 || m.SourceURL != source ||
		m.Status != http.StatusOK || !digestPattern.MatchString(m.SHA256) ||
		m.Size < 0 || m.Size > s.maxBytes || m.CheckedAt.IsZero() || m.Headers == nil {
		return m, nil, errors.New("invalid cache metadata identity, status, size, hash or timestamp")
	}
	if err := s.regular(m.SHA256 + ".bin"); err != nil {
		return m, nil, err
	}
	body, err := s.root.Open(m.SHA256 + ".bin")
	if err != nil {
		return m, nil, err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(contextReader{ctx, body}, s.maxBytes+1))
	if err != nil || n != m.Size || hex.EncodeToString(hash.Sum(nil)) != m.SHA256 {
		body.Close()
		if err != nil {
			return m, nil, err
		}
		return m, nil, errors.New("cache body hash or size does not match")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		body.Close()
		return m, nil, err
	}
	return m, body, nil
}

func (s *Store) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	if err := s.begin(); err != nil {
		return model.Resource{}, err
	}
	defer s.calls.Done()
	if err := ctx.Err(); err != nil {
		return model.Resource{}, err
	}
	source, err := fetch.CanonicalURL(raw)
	if err != nil {
		return model.Resource{}, err
	}
	digest := sha256.Sum256([]byte(source))
	key := hex.EncodeToString(digest[:])
	if err := s.prepare(ctx, source, key, refresh); err != nil {
		return model.Resource{}, err
	}
	m, body, err := s.read(ctx, key, source)
	if err != nil {
		return model.Resource{}, err
	}
	s.reads.Add(1)
	return model.Resource{URL: m.URL, Status: m.Status, Headers: m.Headers, Body: body}, nil
}

// Prefetch makes one URL available in the verified disk cache without holding
// a response body open. Concurrent calls for the same canonical URL deduplicate.
func (s *Store) Prefetch(ctx context.Context, raw string, refresh bool) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.calls.Done()
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := fetch.CanonicalURL(raw)
	if err != nil {
		return err
	}
	if refresh {
		s.mu.Lock()
		fresh := s.fresh[source]
		s.mu.Unlock()
		if fresh {
			return nil
		}
	}
	digest := sha256.Sum256([]byte(source))
	return s.prepare(ctx, source, hex.EncodeToString(digest[:]), refresh)
}

func (s *Store) ensure(ctx context.Context, source, key string, refresh bool) (networked bool, err error) {
	if err := s.regular(key + ".json"); err != nil {
		return false, err
	}
	m, body, err := s.read(ctx, key, source)
	if err != nil {
		if errors.Is(err, errUnsafeEntry) {
			return false, err
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if !errors.Is(err, os.ErrNotExist) {
			s.warn(fmt.Sprintf("ignoring damaged cache for %s: %s", source, err))
		} else if _, metadataErr := s.root.Stat(key + ".json"); metadataErr == nil {
			s.warn(fmt.Sprintf("ignoring damaged cache for %s: body is missing", source))
		}
		body = nil
	}
	if body != nil && !refresh {
		closeErr := body.Close()
		if closeErr == nil {
			s.mu.Lock()
			s.reused[source] = true
			s.mu.Unlock()
		}
		return false, closeErr
	}
	if body != nil {
		defer body.Close()
	}
	s.mu.Lock()
	delete(s.fresh, source)
	s.mu.Unlock()
	req := model.Request{URL: source, Refresh: refresh}
	if body != nil {
		req.ValidatorURL, req.ETag, req.Modified = m.URL, m.Headers.Get("ETag"), m.Headers.Get("Last-Modified")
	}
	var name string
	var temp *os.File
	networked = true
	defer func() {
		if err != nil {
			s.failures.Add(1)
		}
	}()
	defer func() {
		if temp != nil {
			if err := temp.Close(); err != nil {
				s.warn(fmt.Sprintf("could not close cache temporary file %s: %s", name, err))
			}
		}
		if name != "" {
			if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.warn(fmt.Sprintf("could not remove cache temporary file %s: %s", name, err))
			}
		}
	}()
	hash := sha256.New()
	var n int64
	r, err := s.transport.Download(ctx, req, func(r model.Resource) error {
		if r.Status == http.StatusNotModified {
			return nil
		}
		if r.Status != http.StatusOK {
			return fmt.Errorf("expected a complete HTTP 200 response, received %d: %s", r.Status, r.URL)
		}
		if temp == nil {
			name = "." + rand.Text() + ".part"
			var err error
			temp, err = s.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
		} else {
			if err := temp.Truncate(0); err != nil {
				return err
			}
			if _, err := temp.Seek(0, io.SeekStart); err != nil {
				return err
			}
		}
		hash.Reset()
		var err error
		n, err = io.Copy(io.MultiWriter(temp, hash), io.LimitReader(contextReader{ctx, r.Body}, s.maxBytes+1))
		if err != nil {
			return fmt.Errorf("cache response write failed: %w", err)
		}
		if n > s.maxBytes {
			return errors.New("response exceeds configured cache byte limit")
		}
		return temp.Sync()
	})
	if err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if r.Status == http.StatusNotModified {
		if body == nil || r.URL != m.URL || (req.ETag == "" && req.Modified == "") {
			return true, errors.New("server returned 304 without a verified matching conditional cache entry")
		}
		for k, v := range r.Headers {
			m.Headers[k] = v
		}
		m.CheckedAt = time.Now().UTC()
		if err := s.writeMetadata(key, m); err != nil {
			return true, err
		}
		_, verified, err := s.read(ctx, key, source)
		if err != nil {
			return true, err
		}
		err = verified.Close()
		if err == nil {
			s.validated.Add(1)
		}
		return true, err
	}
	finalURL, err := fetch.CanonicalURL(r.URL)
	if err != nil {
		return true, err
	}
	closeErr := temp.Close()
	temp = nil
	if closeErr != nil {
		return true, fmt.Errorf("cache response close failed: %w", closeErr)
	}
	m = metadata{Schema: 1, SourceURL: source, URL: finalURL, Status: r.Status,
		Headers: r.Headers.Clone(), SHA256: hex.EncodeToString(hash.Sum(nil)), Size: n, CheckedAt: time.Now().UTC()}
	s.publish.Lock()
	if err := s.regular(m.SHA256 + ".bin"); err != nil {
		s.publish.Unlock()
		return true, err
	}
	if err := s.root.Rename(name, m.SHA256+".bin"); err != nil {
		s.publish.Unlock()
		return true, err
	}
	s.publish.Unlock()
	name = ""
	if err := s.writeMetadata(key, m); err != nil {
		return true, err
	}
	_, verified, err := s.read(ctx, key, source)
	if err != nil {
		return true, err
	}
	err = verified.Close()
	if err == nil {
		s.downloads.Add(1)
	}
	return true, err
}

func (s *Store) writeMetadata(key string, m metadata) error {
	if err := s.regular(key + ".json"); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	name := "." + rand.Text() + ".part"
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.warn(fmt.Sprintf("could not remove metadata temporary file %s: %s", name, err))
		}
	}()
	_, writeErr := f.Write(append(data, '\n'))
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return s.root.Rename(name, key+".json")
}
