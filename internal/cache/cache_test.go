package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
)

const testURL = "https://docs.example/guide?page=one"

type stub struct {
	calls    []model.Request
	response string
	status   int
	err      error
}

func (s *stub) Do(ctx context.Context, req model.Request) (model.Resource, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return model.Resource{}, s.err
	}
	return model.Resource{URL: req.URL, Status: s.status,
		Headers: http.Header{"Etag": {`"v1"`}, "Last-Modified": {"Wed, 01 Jan 2025 00:00:00 GMT"}},
		Body:    io.NopCloser(strings.NewReader(s.response))}, nil
}

func (s *stub) Download(ctx context.Context, req model.Request, consume func(model.Resource) error) (model.Resource, error) {
	r, err := s.Do(ctx, req)
	if err != nil {
		return model.Resource{}, err
	}
	err = consume(r)
	r.Body.Close()
	r.Body = http.NoBody
	return r, err
}

func openStore(t *testing.T, dir string, backend *stub, warnings *[]string) *Store {
	t.Helper()
	store, err := Open(dir, backend, 1024, func(message string) { *warnings = append(*warnings, message) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func readResource(t *testing.T, store *Store, raw string, refresh bool) string {
	t.Helper()
	r, err := store.Get(context.Background(), raw, refresh)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if _, ok := r.Body.(*os.File); !ok {
		t.Fatal("cache response body is not disk-backed")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func cachePayloadEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(entries, func(entry os.DirEntry) bool {
		return entry.Name() == RootMarkerName
	})
}

func TestRetrievalDiagnosticSurvivesCacheCoordination(t *testing.T) {
	cause := &fetch.StatusError{URL: testURL, Status: http.StatusServiceUnavailable}
	backend := &stub{err: &fetch.RetrievalError{
		RequestURL: testURL, FinalURL: testURL, Stage: fetch.StageHeaders,
		Elapsed: 25 * time.Millisecond, ApplicationRetries: 2, Cause: cause,
	}}
	var warnings []string
	store := openStore(t, t.TempDir(), backend, &warnings)
	_, err := store.Get(context.Background(), testURL, false)
	var diagnostic *fetch.RetrievalError
	var status *fetch.StatusError
	if !errors.As(err, &diagnostic) || !errors.As(err, &status) ||
		diagnostic.Stage != fetch.StageHeaders || diagnostic.ApplicationRetries != 2 ||
		status.Status != http.StatusServiceUnavailable {
		t.Fatalf("cache hid retrieval diagnostics: %T %v", err, err)
	}
}

func TestReuseAcrossRunsConditionalRefreshAndQueryIdentity(t *testing.T) {
	dir := t.TempDir()
	backend := &stub{response: "original\x00\xff", status: 200}
	var warnings []string
	first := openStore(t, dir, backend, &warnings)
	if got := readResource(t, first, testURL, false); got != backend.response {
		t.Fatal("binary source bytes changed")
	}
	second := openStore(t, dir, backend, &warnings)
	readResource(t, second, testURL+"#fragment", false)
	if len(backend.calls) != 1 {
		t.Fatal("verified cache was not reused")
	}
	backend.status, backend.response = 304, ""
	if got := readResource(t, second, testURL, true); got != "original\x00\xff" {
		t.Fatal("304 lost original body")
	}
	last := backend.calls[len(backend.calls)-1]
	if last.ETag != `"v1"` || last.Modified == "" || last.ValidatorURL != testURL || !last.Refresh {
		t.Fatalf("invalid conditional request: %+v", last)
	}
	backend.status, backend.response = 200, "other"
	readResource(t, second, "https://docs.example/guide?page=two", false)
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 2 || len(warnings) != 0 {
		t.Fatalf("query identity/warnings: %v %v", files, warnings)
	}
}

func TestCorruptBodyIsWarnedAndFetchedAgain(t *testing.T) {
	dir := t.TempDir()
	backend := &stub{response: "original", status: 200}
	var warnings []string
	store := openStore(t, dir, backend, &warnings)
	readResource(t, store, testURL, false)
	files, _ := filepath.Glob(filepath.Join(dir, "*.bin"))
	if err := os.WriteFile(files[0], []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if readResource(t, store, testURL, false) != "original" || len(warnings) == 0 || len(backend.calls) != 2 {
		t.Fatal("corrupt cache not recovered with warning")
	}
}

func TestRefreshFailureRetainsCacheButDoesNotReturnStaleSuccess(t *testing.T) {
	backend := &stub{response: "original", status: 200}
	var warnings []string
	store := openStore(t, t.TempDir(), backend, &warnings)
	readResource(t, store, testURL, false)
	backend.err = errors.New("offline")
	if _, err := store.Get(context.Background(), testURL, true); err == nil {
		t.Fatal("refresh failure became stale success")
	}
	if got := readResource(t, store, testURL, false); got != "original" {
		t.Fatal("refresh failure destroyed old cache")
	}
}

func TestStatsDistinguishBodyOpensDownloadsRevalidationReuseAndFailure(t *testing.T) {
	backend := &stub{response: "original", status: http.StatusOK}
	var warnings []string
	store := openStore(t, t.TempDir(), backend, &warnings)
	readResource(t, store, testURL, false)
	if got := store.Stats(); got != (Stats{VerifiedBodyOpens: 1, Downloaded: 1}) {
		t.Fatalf("cold-download counters are ambiguous: %+v", got)
	}
	readResource(t, store, testURL, false)
	if got := store.Stats(); got != (Stats{VerifiedBodyOpens: 2, Downloaded: 1, UniqueReused: 1}) {
		t.Fatalf("warm-read counters are ambiguous: %+v", got)
	}
	backend.status, backend.response = http.StatusNotModified, ""
	readResource(t, store, testURL, true)
	if got := store.Stats(); got != (Stats{VerifiedBodyOpens: 3, Downloaded: 1, Revalidated: 1, UniqueReused: 1}) {
		t.Fatalf("conditional-validation counters are ambiguous: %+v", got)
	}
	backend.err = errors.New("offline")
	if _, err := store.Get(context.Background(), "https://docs.example/failed", false); err == nil {
		t.Fatal("failed retrieval unexpectedly succeeded")
	}
	if got := store.Stats(); got != (Stats{
		VerifiedBodyOpens: 3, Downloaded: 1, Revalidated: 1, UniqueReused: 1, Failed: 1,
	}) {
		t.Fatalf("failed-retrieval counters are ambiguous: %+v", got)
	}
}

func TestInvalidResponsesNeverCreateMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"unconditional304", 304, ""},
		{"partial206", 206, "partial"},
		{"error403", 403, "denied"},
		{"oversized", 200, strings.Repeat("a", 2048)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var warnings []string
			store := openStore(t, dir, &stub{response: tc.body, status: tc.code}, &warnings)
			if _, err := store.Get(context.Background(), testURL, false); err == nil {
				t.Fatal("invalid response succeeded")
			}
			files := cachePayloadEntries(t, dir)
			if len(files) != 0 {
				t.Fatalf("partial cache files retained: %v", files)
			}
		})
	}
}

func TestSymlinkCacheEntriesAndRootAreRejected(t *testing.T) {
	for _, kind := range []string{"root", "metadata", "body"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			backend := &stub{response: "original", status: 200}
			var warnings []string
			if kind == "root" {
				target := filepath.Join(dir, "target")
				link := filepath.Join(dir, "link")
				os.Mkdir(target, 0o700)
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				if _, err := Open(link, backend, 1024, func(string) {}); err == nil {
					t.Fatal("symlink cache root accepted")
				}
				return
			}
			store := openStore(t, dir, backend, &warnings)
			readResource(t, store, testURL, false)
			pattern := "*.json"
			if kind == "body" {
				pattern = "*.bin"
			}
			paths, _ := filepath.Glob(filepath.Join(dir, pattern))
			path := paths[0]
			target := filepath.Join(dir, "kept")
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			backend.response = "changed"
			if _, err := store.Get(context.Background(), testURL, true); err == nil {
				t.Fatal("symlink cache entry accepted")
			}
			if len(backend.calls) != 1 {
				t.Fatal("unsafe cache was used for a network operation")
			}
		})
	}
}

func TestInvalidMetadataNeverSuppliesValidators(t *testing.T) {
	dir := t.TempDir()
	backend := &stub{response: "original", status: 200}
	var warnings []string
	store := openStore(t, dir, backend, &warnings)
	readResource(t, store, testURL, false)
	digest := sha256.Sum256([]byte(testURL))
	path := filepath.Join(dir, hex.EncodeToString(digest[:])+".json")
	data, _ := os.ReadFile(path)
	var m metadata
	json.Unmarshal(data, &m)
	m.SHA256 = "../outside"
	data, _ = json.Marshal(m)
	os.WriteFile(path, data, 0o600)
	readResource(t, store, testURL, true)
	if backend.calls[len(backend.calls)-1].ETag != "" || len(warnings) == 0 {
		t.Fatal("invalid metadata trusted or damage warning lost")
	}
}
