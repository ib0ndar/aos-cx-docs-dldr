package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

func TestRobotsErrorBodiesNeverAllowDownstreamRequests(t *testing.T) {
	for _, tc := range []struct{ name, contentType, body string }{
		{"json", "application/json", `{"error":"Access denied"}`},
		{"json_as_text", "text/plain", `{"error":"Access denied"}`},
		{"xml", "application/xml", `<error xmlns="urn:hpe:error">Access denied</error>`},
		{"xml_as_text", "text/plain", `<error xmlns="urn:hpe:error">Access denied</error>`},
		{"unknown_only", "text/plain", "Error: Access denied\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var downstream atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.Header().Set("Content-Type", tc.contentType)
					io.WriteString(w, tc.body)
					return
				}
				downstream.Add(1)
				io.WriteString(w, "must not be retrieved")
			}))
			defer server.Close()
			r, err := client(t, nil).Do(context.Background(), model.Request{URL: server.URL + "/guide"})
			if r.Body != nil {
				r.Body.Close()
			}
			if err == nil || downstream.Load() != 0 {
				t.Fatalf("unestablished robots allowed retrieval: downstream=%d err=%v", downstream.Load(), err)
			}
		})
	}
}

func TestLegitimateRobotsEmptyCommentsAndGroups(t *testing.T) {
	for _, rules := range []string{
		"", "\n# No restrictions\n", "\ufeff# UTF-8 BOM\n",
		"User-agent: *\n", "User-agent: *\nDisallow:\n",
		"User-agent: *\nAllow: /\nCustom-extension: ignored\n",
		"Sitemap: https://publisher.example/sitemap.xml\n",
		"Host: publisher.example\n",
	} {
		t.Run(rules, func(t *testing.T) {
			var downstream atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.Header().Set("Content-Type", "text/plain")
					io.WriteString(w, rules)
					return
				}
				downstream.Add(1)
				io.WriteString(w, "permitted")
			}))
			defer server.Close()
			r, err := client(t, nil).Do(context.Background(), model.Request{URL: server.URL + "/guide"})
			if r.Body != nil {
				r.Body.Close()
			}
			if err != nil || downstream.Load() != 1 {
				t.Fatalf("legitimate robots rejected: %q downstream=%d error=%v", rules, downstream.Load(), err)
			}
		})
	}
}

func TestRobotsAndRedirectsShareRequestRetryBudget(t *testing.T) {
	for _, mode := range []string{"robots", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var first, last atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					if mode == "robots" && first.Add(1) == 1 {
						w.WriteHeader(503)
					} else {
						w.WriteHeader(404)
					}
					return
				}
				if mode == "redirect" && r.URL.Path == "/start" {
					if first.Add(1) == 1 {
						w.WriteHeader(503)
					} else {
						http.Redirect(w, r, "/final", 302)
					}
					return
				}
				last.Add(1)
				w.WriteHeader(503)
			}))
			defer server.Close()
			c := client(t, func(c *Config) { c.Retries = 1 })
			_, err := c.Do(context.Background(), model.Request{URL: server.URL + "/start"})
			if err == nil || first.Load() != 2 || last.Load() != 1 {
				t.Fatalf("per-hop retries multiplied: first=%d final=%d error=%v", first.Load(), last.Load(), err)
			}
		})
	}
}

func TestRobotsStreamingFailureUsesDownloadBudget(t *testing.T) {
	var robots, guides atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			if robots.Add(1) == 1 {
				w.Header().Set("Content-Length", "100")
				io.WriteString(w, "User-agent: *\nAllow:")
			} else {
				w.WriteHeader(404)
			}
			return
		}
		guides.Add(1)
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, strings.Repeat("x", 20))
	}))
	defer server.Close()
	c := client(t, func(c *Config) { c.Retries, c.Timeout = 1, 5*time.Second })
	_, err := c.Download(context.Background(), model.Request{URL: server.URL + "/guide"}, func(r model.Resource) error {
		_, err := io.Copy(io.Discard, r.Body)
		return err
	})
	if err == nil || robots.Load() != 2 || guides.Load() != 1 {
		t.Fatalf("robots and body retry budgets diverged: robots=%d guides=%d err=%v", robots.Load(), guides.Load(), err)
	}
}

func TestDownloadSinkFailureDoesNotTriggerNetworkRetry(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		hits.Add(1)
		io.WriteString(w, "content")
	}))
	defer server.Close()
	c := client(t, func(c *Config) { c.Retries = 2 })
	_, err := c.Download(context.Background(), model.Request{URL: server.URL + "/guide"}, func(model.Resource) error {
		return io.ErrUnexpectedEOF
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) || hits.Load() != 1 {
		t.Fatalf("sink error misclassified as a network failure: hits=%d error=%v", hits.Load(), err)
	}
}
