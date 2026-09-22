package cache

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
)

func nativeStore(t *testing.T, dir string, cfg fetch.Config) *Store {
	t.Helper()
	notify := func(message string) { t.Log(message) }
	transport, err := newContractClient(cfg, notify)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.Close)
	store, err := Open(dir, transport, cfg.MaxBytes, notify)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func assertNoParts(t *testing.T, dir string) {
	t.Helper()
	parts, err := filepath.Glob(filepath.Join(dir, "*.part"))
	if err != nil || len(parts) != 0 {
		t.Fatalf("partial attempts were not discarded: %v %v", parts, err)
	}
}

func TestTruncatedStreamingResponseRetriesCompleteBody(t *testing.T) {
	for _, mode := range []string{"identity", "gzip-header", "gzip-body"} {
		t.Run(mode, func(t *testing.T) {
			payload := func(text string) string {
				if mode == "identity" {
					return text
				}
				var data strings.Builder
				gz := gzip.NewWriter(&data)
				if _, err := io.WriteString(gz, text); err != nil {
					t.Fatal(err)
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
				return data.String()
			}
			first, complete := payload(strings.Repeat("x", 100)), payload(strings.Repeat("y", 100))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(404)
					return
				}
				if mode != "identity" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				if requests.Add(1) == 1 {
					w.Header().Set("Content-Length", strconv.Itoa(len(first)))
					end := 20
					if mode == "gzip-header" {
						end = 5
					}
					io.WriteString(w, first[:end])
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(complete)))
				io.WriteString(w, complete)
			}))
			defer server.Close()
			dir := t.TempDir()
			store := nativeStore(t, dir, fetch.Config{Retries: 1, Timeout: 5 * time.Second, MaxBytes: 1024})
			r, err := store.Get(context.Background(), server.URL+"/guide", false)
			if err != nil {
				t.Fatalf("truncated body was not retried: requests=%d err=%v", requests.Load(), err)
			}
			defer r.Body.Close()
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != strings.Repeat("y", 100) || requests.Load() != 2 {
				t.Fatalf("partial bytes mixed or budget wrong: requests=%d body=%q err=%v", requests.Load(), body, err)
			}
			assertNoParts(t, dir)
		})
	}
}

func TestStatusAndBodyFailuresShareBudgetAndPreservePreviousCache(t *testing.T) {
	var failing atomic.Bool
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		if !failing.Load() {
			w.Header().Set("ETag", `"original"`)
			io.WriteString(w, "previous complete bytes")
			return
		}
		if attempts.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, strings.Repeat("x", 20))
	}))
	defer server.Close()
	dir := t.TempDir()
	store := nativeStore(t, dir, fetch.Config{Retries: 1, Timeout: 5 * time.Second, MaxBytes: 1024})
	target := server.URL + "/guide"
	readResource(t, store, target, false)
	metadataPaths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	before, err := os.ReadFile(metadataPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	failing.Store(true)
	_, err = store.Get(context.Background(), target, true)
	if err == nil || attempts.Load() != 2 {
		t.Fatalf("retry loops multiplied or failure hidden: attempts=%d err=%v", attempts.Load(), err)
	}
	after, err := os.ReadFile(metadataPaths[0])
	if err != nil || string(before) != string(after) {
		t.Fatalf("failed refresh changed previous metadata: %v", err)
	}
	if readResource(t, store, target, false) != "previous complete bytes" || attempts.Load() != 2 {
		t.Fatal("failed update destroyed verified old bytes or triggered stale refresh success")
	}
	assertNoParts(t, dir)
}

func TestStreamingRetriesUseOriginalDeadline(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", "100")
		if attempts.Add(1) == 1 {
			io.WriteString(w, strings.Repeat("x", 20))
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	dir := t.TempDir()
	store := nativeStore(t, dir, fetch.Config{Retries: 2, Timeout: 1200 * time.Millisecond, MaxBytes: 1024})
	start := time.Now()
	_, err := store.Get(context.Background(), server.URL+"/guide", false)
	if !errors.Is(err, context.DeadlineExceeded) || attempts.Load() != 2 || time.Since(start) > 1800*time.Millisecond {
		t.Fatalf("streaming retry restarted the deadline: elapsed=%s attempts=%d err=%v", time.Since(start), attempts.Load(), err)
	}
	entries := cachePayloadEntries(t, dir)
	if len(entries) != 0 {
		t.Fatalf("deadline failure published or retained partial output: %v", entries)
	}
}

func TestPermanentStreamingFailuresAreNotRetried(t *testing.T) {
	for _, mode := range []string{"denied", "size", "encoding", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			var attempts atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(404)
					return
				}

				attempts.Add(1)
				switch mode {
				case "denied":
					w.WriteHeader(403)
				case "size":
					w.(http.Flusher).Flush()
					io.WriteString(w, strings.Repeat("a", 2048))
				case "encoding":
					w.Header().Set("Content-Encoding", "unknown")
					io.WriteString(w, "content")
				case "cancelled":
					w.Header().Set("Content-Length", "100")
					io.WriteString(w, strings.Repeat("a", 20))
					w.(http.Flusher).Flush()
					cancel()
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			store := nativeStore(t, dir, fetch.Config{Retries: 2, Timeout: 5 * time.Second, MaxBytes: 1024})
			_, err := store.Get(ctx, server.URL+"/guide", false)
			if err == nil || attempts.Load() != 1 {
				t.Fatalf("permanent failure retried or hidden: attempts=%d err=%v", attempts.Load(), err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			entries := cachePayloadEntries(t, dir)
			if len(entries) != 0 {
				t.Fatalf("failure left cache output: %v", entries)
			}
		})
	}
}

func TestDecodedStreamFailureRetriesAndPublishesOnlyFinalBytes(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		attempt := attempts.Add(1)
		w.Header().Set("Content-Encoding", "gzip")
		if attempt == 1 {
			io.WriteString(w, "malformed gzip")
			return
		}
		gz := gzip.NewWriter(w)
		io.WriteString(gz, "final decoded bytes")
		gz.Close()
	}))
	defer server.Close()
	dir := t.TempDir()
	store := nativeStore(t, dir, fetch.Config{
		Retries: 1, Timeout: 3 * time.Second, AttemptTimeout: time.Second, MaxBytes: 1024,
	})
	resource, err := store.Get(context.Background(), server.URL+"/guide", false)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resource.Body)
	closeErr := resource.Body.Close()
	if readErr != nil || closeErr != nil || string(body) != "final decoded bytes" ||
		attempts.Load() != 2 {
		t.Fatalf("decoded retry body=%q attempts=%d read=%v close=%v", body, attempts.Load(), readErr, closeErr)
	}
	assertNoParts(t, dir)
}

func TestConcurrentSameURLWaitersShareRetriedWriter(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		attempt := attempts.Add(1)
		w.Header().Set("Content-Length", "20")
		if attempt == 1 {
			io.WriteString(w, "partial")
			return
		}
		io.WriteString(w, "shared-final-content")
	}))
	defer server.Close()
	dir := t.TempDir()
	store := nativeStore(t, dir, fetch.Config{
		Retries: 1, Timeout: 3 * time.Second, AttemptTimeout: time.Second, MaxBytes: 1024,
	})
	type outcome struct {
		body string
		err  error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			resource, err := store.Get(context.Background(), server.URL+"/guide", false)
			if err != nil {
				results <- outcome{err: err}
				return
			}
			body, readErr := io.ReadAll(resource.Body)
			closeErr := resource.Body.Close()
			results <- outcome{body: string(body), err: errors.Join(readErr, closeErr)}
		}()
	}
	for range 2 {
		result := <-results
		if result.err != nil || result.body != "shared-final-content" {
			t.Fatalf("shared retry result=%+v", result)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("same-URL waiters created duplicate retry writers: attempts=%d", attempts.Load())
	}
	assertNoParts(t, dir)
}

func TestFragmentsStayOutOfHTTPAndCacheIdentity(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		hits.Add(1)
		if r.RequestURI != "/some%20guide%20name.html?page=one%20two" {
			t.Errorf("wrong retrieval identity: %q", r.RequestURI)
		}
		io.WriteString(w, "body")
	}))
	defer server.Close()
	dir := t.TempDir()
	store := nativeStore(t, dir, fetch.Config{Timeout: time.Second, MaxBytes: 1024})
	raw := server.URL + "/some guide%20name.html?page=one%20two"
	readResource(t, store, raw+"#installation", false)
	readResource(t, store, raw+"#another%20bookmark", false)
	if hits.Load() != 1 {
		t.Fatalf("bookmark changed cache identity: %d requests", hits.Load())
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(paths) != 1 {
		t.Fatalf("expected one canonical cache entry: %v", paths)
	}
	m, body, err := store.read(context.Background(), strings.TrimSuffix(filepath.Base(paths[0]), ".json"),
		server.URL+"/some%20guide%20name.html?page=one%20two")
	if err != nil {
		t.Fatal(err)
	}
	body.Close()
	if strings.Contains(m.SourceURL+m.URL, "#") {
		t.Fatalf("fragment persisted as retrieval identity: %+v", m)
	}
}
