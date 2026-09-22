package fetch

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

func client(t *testing.T, change func(*Config)) *Client {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Delay, cfg.Retries = 0, 0
	if change != nil {
		change(&cfg)
	}
	if cfg.AttemptTimeout > cfg.Timeout {
		cfg.AttemptTimeout = cfg.Timeout
	}
	c, err := newContractClient(cfg, func(message string) { t.Log(message) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestRobotsPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		allowed    bool
	}{
		{"allow", "User-agent: *\nAllow: /\n", 200, true},
		{"missing", "", 404, true},
		{"denied", "", 403, false},
		{"unauthorized", "", 401, false},
		{"unavailable", "", 503, false},
		{"unexpected_status", "", 204, false},
		{"html_error", "<html>Access denied</html>", 200, false},
		{"invalid_rules", "not robots at all", 200, false},
		{"disallow", "User-agent: *\nDisallow: /private\n", 200, false},
		{"specific_agent", "User-agent: *\nAllow: /\nUser-agent: aos-cx-docs-dldr\nDisallow: /private\n", 200, false},
		{"allow_override", "User-agent: *\nDisallow: /\nAllow: /private\n", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
				} else {
					hits.Add(1)
					io.WriteString(w, "content")
				}
			}))
			defer server.Close()
			r, err := client(t, nil).Do(context.Background(), model.Request{URL: server.URL + "/private"})
			if r.Body != nil {
				r.Body.Close()
			}
			if (err == nil) != tc.allowed || (hits.Load() == 1) != tc.allowed {
				t.Fatalf("allowed=%v hits=%d error=%v", tc.allowed, hits.Load(), err)
			}
		})
	}
}

func TestRedirectChecksTargetRobotsForGETAndHEAD(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			var targetHits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					io.WriteString(w, "User-agent: *\nDisallow: /\n")
					return
				}
				targetHits.Add(1)
			}))
			defer target.Close()
			start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(404)
					return
				}
				http.Redirect(w, r, target.URL+"/private", 302)
			}))
			defer start.Close()
			_, err := client(t, nil).Do(context.Background(), model.Request{URL: start.URL + "/start", Method: method})
			if err == nil || !strings.Contains(err.Error(), "disallows") || targetHits.Load() != 0 {
				t.Fatalf("unexpected redirect policy: hits=%d error=%v", targetHits.Load(), err)
			}
		})
	}
}

func TestHeadRedirectIsBodyFreeAndValidatorsAreScoped(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		methods = append(methods, r.Method)
		if r.URL.Path == "/start" {
			if r.Header.Get("If-None-Match") != "" {
				t.Error("validator leaked to redirecting endpoint")
			}
			http.Redirect(w, r, "/final", 307)
			return
		}
		if r.Header.Get("If-None-Match") != `"v1"` {
			t.Error("missing final endpoint validator")
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", "999999999")
		w.Header().Set("X-Final", "yes")
	}))
	defer server.Close()
	r, err := client(t, nil).Do(context.Background(), model.Request{URL: server.URL + "/start", Method: http.MethodHead,
		ValidatorURL: server.URL + "/final", ETag: `"v1"`})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) != 0 || r.URL != server.URL+"/final" || r.Headers.Get("X-Final") != "yes" ||
		strings.Join(methods, ",") != "HEAD,HEAD" {
		t.Fatalf("response=%+v methods=%v body=%q error=%v", r, methods, body, err)
	}
}

func TestSizeLimitIncludesDecodedGzipAndChunked(t *testing.T) {
	for _, mode := range []string{"length", "chunked", "gzip"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(404)
					return
				}
				if mode == "gzip" {
					w.Header().Set("Content-Encoding", "gzip")
					gz := gzip.NewWriter(w)
					io.WriteString(gz, strings.Repeat("a", 2048))
					gz.Close()
				} else {
					if mode == "chunked" {
						w.(http.Flusher).Flush()
					}
					io.WriteString(w, strings.Repeat("a", 2048))
				}
			}))
			defer server.Close()
			r, err := client(t, func(c *Config) { c.MaxBytes = 1024 }).Do(context.Background(), model.Request{URL: server.URL + "/large"})
			if err == nil {
				_, err = io.ReadAll(r.Body)
				r.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "byte limit") {
				t.Fatalf("expected bounded body failure: %v", err)
			}
		})
	}
}

func TestCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"headers", "body", "throttle", "retry"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(404)
					return
				}
				if mode == "retry" {
					w.Header().Set("Retry-After", "10")
					w.WriteHeader(503)
					return
				}
				if mode == "body" {
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			c := client(t, func(c *Config) {
				c.Timeout = 100 * time.Millisecond
				c.Retries = 2
				if mode == "throttle" {
					c.Delay = time.Second
				}
			})
			start := time.Now()
			r, err := c.Do(context.Background(), model.Request{URL: server.URL + "/slow"})
			if err == nil {
				_, err = io.ReadAll(r.Body)
				r.Body.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
				t.Fatalf("deadline not propagated promptly: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client(t, nil).Do(ctx, model.Request{URL: "https://example.invalid"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation hidden: %v", err)
	}
}

func TestSharedPerOriginStartGap(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		if r.URL.Path == "/robots.txt" {
			io.WriteString(w, "User-agent: *\nCrawl-delay: 0.03\nAllow: /\n")
		} else {
			io.WriteString(w, "OK")
		}
	}))
	defer server.Close()
	c := client(t, func(c *Config) { c.Delay = 20 * time.Millisecond })
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			r, err := c.Do(context.Background(), model.Request{URL: server.URL + "/topic"})
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		})
	}
	wg.Wait()
	if len(starts) != 5 {
		t.Fatalf("expected one robots and four topic requests: %v", starts)
	}
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 25*time.Millisecond {
			t.Fatalf("shared gap violated: %v", starts)
		}
	}
}

func TestRetriesAreBoundedAndRespectRetryAfter(t *testing.T) {
	for _, header := range []string{"0", "600"} {
		t.Run(header, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(404)
					return
				}
				hits.Add(1)
				w.Header().Set("Retry-After", header)
				w.WriteHeader(429)
			}))
			defer server.Close()
			start := time.Now()
			_, err := client(t, func(c *Config) { c.Retries = 1 }).Do(context.Background(), model.Request{URL: server.URL + "/busy"})
			if err == nil {
				t.Fatal("429 reported success")
			}
			if header == "600" {
				if hits.Load() != 1 || !strings.Contains(err.Error(), "retry this job later") {
					t.Fatalf("long Retry-After ignored: hits=%d err=%v", hits.Load(), err)
				}
			} else if hits.Load() != 2 || time.Since(start) < time.Second {
				t.Fatalf("retry count/wait not bounded correctly: hits=%d", hits.Load())
			}
		})
	}
	now := time.Now().UTC().Truncate(time.Second)
	delay, err := retryDelay(now.Add(7*time.Second).Format(http.TimeFormat), 0, now)
	if err != nil || delay != 7*time.Second {
		t.Fatalf("HTTP-date retry: %s %v", delay, err)
	}
}

func TestURLIdentityAndValidation(t *testing.T) {
	for _, bad := range []string{"//host/book.html", "file:///etc/passwd", "https://user:pass@host/",
		"https://bad host/a", "https://host:bad/", "https://host:99999/", "https://host:/",
		"https://host/a\\b", "https://host/a\nb", "https://host/?a=%zz", "https://[not-ip]/"} {
		if _, err := NormalizeURL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	got, err := CanonicalURL("https://host/some guide%20name.html?page=one%20two&docId=x#bookmark")
	want := "https://host/some%20guide%20name.html?page=one%20two&docId=x"
	if err != nil || got != want {
		t.Fatalf("got %q want %q err=%v", got, want, err)
	}
	got, err = NormalizeURL("https://host/topic.html?name=\u00e9 &original=a+b&path=%20")
	if err != nil || got != "https://host/topic.html?name=%C3%A9%20&original=a+b&path=%20" {
		t.Fatalf("query identity changed: %q %v", got, err)
	}
}

func TestRedirectFailuresAndRobotsBootstrapRedirect(t *testing.T) {
	for _, mode := range []string{"missing", "scheme", "loop", "robots-redirect"} {
		t.Run(mode, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					if mode == "robots-redirect" {
						http.Redirect(w, r, "/rules.txt", 301)
					} else {
						w.WriteHeader(404)
					}
					return
				}
				if r.URL.Path == "/rules.txt" {
					io.WriteString(w, "User-agent: *\nDisallow: /start\n")
					return
				}
				hits.Add(1)
				switch mode {
				case "scheme":
					w.Header().Set("Location", "file:///private")
				case "loop":
					w.Header().Set("Location", "/start")
				}
				w.WriteHeader(302)
			}))
			defer server.Close()
			_, err := client(t, nil).Do(context.Background(), model.Request{URL: server.URL + "/start"})
			if err == nil {
				t.Fatal("invalid/denied redirect succeeded")
			}
			if mode == "loop" && hits.Load() != 11 {
				t.Fatalf("redirect count is unbounded: %d", hits.Load())
			}
			if mode == "robots-redirect" && hits.Load() != 0 {
				t.Fatal("redirected robots denial ignored")
			}
		})
	}
}

func TestHTTPSDowngradeIsRejected(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP downgrade target was requested")
	}))
	defer target.Close()
	start := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		http.Redirect(w, r, target.URL+"/target", 302)
	}))
	defer start.Close()
	c := client(t, nil)
	// Trust only the fixture server's test TLS configuration for this test.
	c.http.Transport = start.Client().Transport
	_, err := c.Do(context.Background(), model.Request{URL: start.URL + "/start"})
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("downgrade was not rejected: %v", err)
	}
}

func TestExactBodyLimitAndDenialStatus(t *testing.T) {
	for _, size := range []int{1023, 1024, 1025} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/robots.txt" {
				w.WriteHeader(404)
				return
			}
			w.(http.Flusher).Flush()
			io.WriteString(w, strings.Repeat("a", size))
		}))
		c := client(t, func(c *Config) { c.MaxBytes = 1024 })
		r, err := c.Do(context.Background(), model.Request{URL: server.URL + "/content"})
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		server.Close()
		if size <= 1024 && (err != nil || len(body) != size) {
			t.Fatalf("valid boundary rejected: size=%d read=%d error=%v", size, len(body), err)
		}
		if size > 1024 && (err == nil || len(body) > 1024) {
			t.Fatalf("byte limit not exact: size=%d read=%d error=%v", size, len(body), err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", "999999999")
		w.WriteHeader(403)
	}))
	defer server.Close()
	_, err := client(t, nil).Do(context.Background(), model.Request{URL: server.URL + "/page"})
	var status *StatusError
	if !errors.As(err, &status) || status.Status != 403 {
		t.Fatalf("access denial hidden by invalid error body: %v", err)
	}
}
