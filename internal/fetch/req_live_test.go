//go:build reqexperiment

package fetch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
)

type requestEvidence struct {
	URL             string `json:"url"`
	Method          string `json:"method"`
	FinalURL        string `json:"final_url,omitempty"`
	Status          int    `json:"status,omitempty"`
	ContentType     string `json:"content_type,omitempty"`
	ContentEncoding string `json:"content_encoding,omitempty"`
	Protocol        string `json:"protocol,omitempty"`
	TLSALPN         string `json:"tls_alpn,omitempty"`
	TLSVerified     bool   `json:"tls_verified,omitempty"`
	ProxyConfigured bool   `json:"inherited_proxy_configured"`
	StartedAt       string `json:"started_at"`
	HeaderMillis    int64  `json:"header_millis"`
	TotalMillis     int64  `json:"total_millis"`
	HeaderWrites    int32  `json:"request_header_writes"`
	ObservedBytes   int64  `json:"observed_encoded_bytes"`
	SHA256          string `json:"observed_encoded_sha256,omitempty"`
	BodyComplete    bool   `json:"observed_body_complete"`
	Error           string `json:"error,omitempty"`
}

type evidenceTransport struct {
	backend http.RoundTripper
	mu      sync.Mutex
	records []*requestEvidence
}

func (t *evidenceTransport) CloseIdleConnections() {
	if closer, ok := t.backend.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *evidenceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.RoundTripWithDispatch(r, func() error { return nil })
}

func (t *evidenceTransport) RoundTripWithDispatch(r *http.Request, start func() error) (*http.Response, error) {
	t.mu.Lock()
	if len(t.records) >= 64 {
		t.mu.Unlock()
		return nil, errors.New("M2A experiment exceeded the 64-request budget")
	}
	record := &requestEvidence{URL: r.URL.String(), Method: r.Method, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	t.records = append(t.records, record)
	t.mu.Unlock()
	if r.Header.Get("User-Agent") != "aos-cx-docs-dldr/"+model.Version || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
		return nil, errors.New("M2A experiment refused unexpected agent/cookie/authentication state")
	}
	proxy, err := http.ProxyFromEnvironment(r)
	if err != nil {
		return nil, errors.New("invalid inherited proxy configuration")
	}
	record.ProxyConfigured = proxy != nil
	started := time.Now()
	var writes atomic.Int32
	r = r.Clone(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
		WroteHeaders: func() { writes.Add(1) },
	}))
	response, err := fetch.DispatchRoundTrip(t.backend, r, start)
	record.HeaderMillis = time.Since(started).Milliseconds()
	record.HeaderWrites = writes.Load()
	if err != nil {
		// Network errors may include proxy credentials or user-info; persist only
		// a type here, with endpoint and timing already recorded separately.
		record.Error = fmt.Sprintf("transport failure (%T)", err)
		record.TotalMillis = record.HeaderMillis
		return nil, err
	}
	record.FinalURL = r.URL.String()
	if response.Request != nil {
		record.FinalURL = response.Request.URL.String()
	}
	record.Status, record.Protocol = response.StatusCode, response.Proto
	record.ContentType = response.Header.Get("Content-Type")
	record.ContentEncoding = response.Header.Get("Content-Encoding")
	if response.TLS != nil {
		record.TLSALPN = response.TLS.NegotiatedProtocol
		record.TLSVerified = len(response.TLS.VerifiedChains) != 0
	}
	response.Body = &evidenceBody{
		ReadCloser: response.Body, record: record, started: started, hash: sha256.New(),
		denied: response.StatusCode == 401 || response.StatusCode == 403,
	}
	return response, nil
}

type evidenceBody struct {
	io.ReadCloser
	record  *requestEvidence
	started time.Time
	hash    hash.Hash
	denied  bool
	closed  bool
}

func (b *evidenceBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.hash.Write(p[:n])
	b.record.ObservedBytes += int64(n)
	if errors.Is(err, io.EOF) {
		b.record.BodyComplete = true
	} else if err != nil {
		b.record.Error = fmt.Sprintf("body read failure (%T)", err)
	}
	return n, err
}

func (b *evidenceBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	if b.denied && b.record.ObservedBytes == 0 {
		// Hash at most 4 KiB of the existing denial response, never request a
		// replacement resource or persist its potentially identifying contents.
		if _, err := io.Copy(io.Discard, io.LimitReader(b, 4096)); err != nil {
			b.record.Error = fmt.Sprintf("denial response sampling failure (%T)", err)
		}
	}
	err := b.ReadCloser.Close()
	if err != nil {
		b.record.Error = fmt.Sprintf("response close failure (%T)", err)
	}
	b.record.SHA256 = hex.EncodeToString(b.hash.Sum(nil))
	b.record.TotalMillis = time.Since(b.started).Milliseconds()
	return err
}

func TestReqLiveFeasibility(t *testing.T) {
	dir := os.Getenv("AOSCX_M2A_PROBE_DIR")
	if dir == "" {
		t.Skip("M2A live probe requires an explicit owned artifact directory without a previous result")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil || !strings.HasPrefix(filepath.Base(absolute), "aoscx-go-m2a-") {
		t.Fatal("expected an owned aoscx-go-m2a-* artifact directory")
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("probe artifact root must be an existing real directory")
	}
	resultPath := filepath.Join(absolute, "req-live-result.json")
	if _, err := os.Lstat(resultPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("probe result already exists; refusing an accidental repeat or overwrite")
	}
	evidence := &evidenceTransport{backend: fetch.ReqExperimentRoundTripper()}
	var warnings []string
	notify := func(message string) { warnings = append(warnings, message) }
	client, err := fetch.NewWithRoundTripper(fetch.Config{
		Delay: 250 * time.Millisecond, Timeout: 20 * time.Second, Retries: 0, MaxBytes: 4 << 20,
	}, notify, evidence)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := cache.Open(filepath.Join(absolute, "req-trial-cache"), client, 4<<20, notify)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close trial cache: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	started := time.Now()
	catalog, loadErr := source.LoadCatalog(ctx, store, model.PortalURL)
	result := map[string]any{
		"profile": fetch.ReqExperimentProfile, "source_url": model.PortalURL,
		"started_at": started.UTC().Format(time.RFC3339Nano), "elapsed_millis": time.Since(started).Milliseconds(),
		"limits":   map[string]any{"backend_round_trips": 64, "application_retries": 0, "per_resource_bytes": 4 << 20, "deadline_seconds": 90},
		"requests": evidence.records, "warning_count": len(warnings),
		"guide_inventory_topic_asset_access": "not attempted; catalogue gate precedes representative guide sampling",
	}
	outcome := "catalogue-complete"
	if loadErr != nil {
		outcome = "catalogue-failed"
		var status *fetch.StatusError
		if errors.As(loadErr, &status) {
			result["failed_url"], result["failed_status"] = status.URL, status.Status
			if status.Status == 401 || status.Status == 403 {
				outcome = "access-denied"
			}
		}
		result["error_type"] = fmt.Sprintf("%T", loadErr)
	} else {
		result["catalogue_final_url"], result["catalogue_fetched_at"] = catalog.SourceURL, catalog.FetchedAt
		result["platform_count"], result["version_count"], result["guide_count"] = len(catalog.Platforms), len(catalog.Versions), len(catalog.Guides)
		result["mapping_errors"] = len(catalog.Warnings)
		if len(catalog.Warnings) != 0 {
			outcome = "catalogue-incomplete"
		}
		data, err := json.MarshalIndent(catalog, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(absolute, "observed-catalogue.json"), append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result["outcome"] = outcome
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("M2A %s; %d observed requests; redacted evidence: %s", outcome, len(evidence.records), resultPath)
	if outcome != "catalogue-complete" {
		t.Skip("LIVE CATALOGUE NOT VERIFIED; refusal/failure is recorded, not a functional pass")
	}
}
