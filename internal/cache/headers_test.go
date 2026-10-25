package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

type headerStub struct {
	calls   []model.Request
	status  int
	headers http.Header
}

func (s *headerStub) Do(ctx context.Context, req model.Request) (model.Resource, error) {
	s.calls = append(s.calls, req)
	body := ""
	if s.status == http.StatusOK {
		body = "original"
	}
	return model.Resource{URL: req.URL, Status: s.status, Headers: s.headers.Clone(),
		Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (s *headerStub) Download(ctx context.Context, req model.Request, consume func(model.Resource) error) (model.Resource, error) {
	r, err := s.Do(ctx, req)
	if err != nil {
		return model.Resource{}, err
	}
	err = consume(r)
	r.Body.Close()
	r.Body = http.NoBody
	return r, err
}

func publisherHeaders() http.Header {
	return http.Header{
		"Etag":          {`"v1"`},
		"Last-Modified": {"Wed, 01 Jan 2025 00:00:00 GMT"},
		"Content-Type":  {"text/html; charset=utf-8"},
		"Doc-Id":        {"sd00001234en_us"},
		"Set-Cookie":    {"session=secret; Secure; HttpOnly"},
		"Set-Cookie2":   {"legacy=secret"},
	}
}

func metadataPath(dir, raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return filepath.Join(dir, hex.EncodeToString(digest[:])+".json")
}

func assertNoCredentials(t *testing.T, headers http.Header, path string) {
	t.Helper()
	if headers.Get("Set-Cookie") != "" || headers.Get("Set-Cookie2") != "" ||
		headers.Get("Etag") != `"v1"` || headers.Get("Content-Type") == "" || headers.Get("Doc-Id") == "" {
		t.Fatalf("returned headers lost validators or kept credentials: %v", headers)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(data)), "cookie") || strings.Contains(string(data), "secret") ||
		!strings.Contains(string(data), `\"v1\"`) || !strings.Contains(string(data), "Doc-Id") {
		t.Fatalf("persisted metadata kept credentials or lost validators: %s", data)
	}
}

func TestCredentialHeadersAreNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	backend := &headerStub{status: http.StatusOK, headers: publisherHeaders()}
	store, err := Open(dir, backend, 1024, func(message string) { t.Error(message) })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, err := store.Get(context.Background(), testURL, false)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	assertNoCredentials(t, r.Headers, metadataPath(dir, testURL))
	backend.status = http.StatusNotModified
	r, err = store.Get(context.Background(), testURL, true)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if last := backend.calls[len(backend.calls)-1]; last.ETag != `"v1"` || last.Modified == "" {
		t.Fatalf("validators were not retained for revalidation: %+v", last)
	}
	assertNoCredentials(t, r.Headers, metadataPath(dir, testURL))
}

func TestLegacyEntriesWithCredentialHeadersStillOpenWithoutDownload(t *testing.T) {
	dir := t.TempDir()
	backend := &headerStub{status: http.StatusOK, headers: publisherHeaders()}
	first, err := Open(dir, backend, 1024, func(message string) { t.Error(message) })
	if err != nil {
		t.Fatal(err)
	}
	readResource(t, first, testURL, false)
	first.Close()
	// Rewrite the entry as an earlier build stored it: every header verbatim.
	path := metadataPath(dir, testURL)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m metadata
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.Headers = publisherHeaders()
	if data, err = json.MarshalIndent(m, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir, backend, 1024, func(message string) { t.Error(message) })
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	r, err := second.Get(context.Background(), testURL, false)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || string(body) != "original" || len(backend.calls) != 1 || second.Stats().UniqueReused != 1 {
		t.Fatalf("legacy entry was not reused: body=%q calls=%d err=%v", body, len(backend.calls), err)
	}
	if r.Headers.Get("Set-Cookie") != "" || r.Headers.Get("Etag") != `"v1"` {
		t.Fatalf("legacy credentials were returned: %v", r.Headers)
	}
	backend.status = http.StatusNotModified
	r, err = second.Get(context.Background(), testURL, true)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	assertNoCredentials(t, r.Headers, path)
	if len(backend.calls) != 2 || backend.calls[1].ETag != `"v1"` {
		t.Fatalf("legacy entry was downloaded again instead of revalidated: %+v", backend.calls)
	}
}
