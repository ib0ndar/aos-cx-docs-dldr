package fetch

import (
	"compress/gzip"
	"context"
	"crypto/rand"
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

func TestDestinationPolicyClassification(t *testing.T) {
	for _, tc := range []struct {
		url              string
		public, fixtures bool
	}{
		{"https://arubanetworking.hpe.com/techdocs/a.html", true, true},
		{"https://support.hpe.com./hpesc/public/docDisplay?docId=a", true, true},
		{"https://8.8.8.8/a", true, true},
		{"https://[2606:4700::1111]/a", true, true},
		{"https://[::ffff:8.8.8.8]/a", true, true},
		{"https://[64:ff9b::808:808]/a", true, true},
		{"https://１２７.example/a", true, true},
		{"http://arubanetworking.hpe.com/techdocs/a.html", true, true},
		{"http://8.8.8.8/a", true, true},
		{"ftp://arubanetworking.hpe.com/a", false, false},
		{"http://127.0.0.1:8080/a", false, true},
		{"https://127.0.0.1/a", false, true},
		{"http://127.9.8.7/a", false, true},
		{"http://[::1]:8080/a", false, true},
		{"http://[::ffff:127.0.0.1]/a", false, true},
		{"http://localhost:8080/a", false, true},
		{"https://LOCALHOST./a", false, true},
		{"https://docs.localhost/a", false, false},
		{"https://[::1%25lo0]/a", false, false},
		{"https://0.0.0.0/a", false, false},
		{"https://0.1.2.3/a", false, false},
		{"https://[::]/a", false, false},
		{"https://10.1.2.3/a", false, false},
		{"https://172.16.0.1/a", false, false},
		{"https://192.168.1.1/a", false, false},
		{"https://100.64.0.1/a", false, false},
		{"https://169.254.169.254/latest/meta-data", false, false},
		{"https://224.0.0.1/a", false, false},
		{"https://255.255.255.255/a", false, false},
		{"https://192.0.2.1/a", false, false},
		{"https://198.18.0.1/a", false, false},
		{"https://198.51.100.1/a", false, false},
		{"https://203.0.113.1/a", false, false},
		{"https://[fe80::1]/a", false, false},
		{"https://[fc00::1]/a", false, false},
		{"https://[fd12:3456::1]/a", false, false},
		{"https://[fec0::1]/a", false, false},
		{"https://[ff02::1]/a", false, false},
		{"https://[2001:db8::1]/a", false, false},
		{"https://[2001:2::1]/a", false, false},
		{"https://[::ffff:10.0.0.1]/a", false, false},
		{"https://[::ffff:169.254.169.254]/a", false, false},
		{"https://[::10.0.0.1]/a", false, false},
		{"https://[64:ff9b::a00:1]/a", false, false},
		{"https://[64:ff9b::7f00:1]/a", false, false},
		{"https://[2002:c0a8:101::1]/a", false, false},
		{"https://2130706433/a", false, false},
		{"https://0x7f000001/a", false, false},
		{"https://0x7f.1/a", false, false},
		{"https://0177.0.0.1/a", false, false},
		{"https://127.1/a", false, false},
		{"https://127.0.0.1./a", false, false},
		{"https://10.0.0.1.nip.io.0/a", false, false},
		{"https://１２７.０.０.１/a", false, false},
		{"https:///a", false, false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			for _, fixtures := range []bool{false, true} {
				want := tc.public
				if fixtures {
					want = tc.fixtures
				}
				err := CheckDestination(tc.url, fixtures)
				var refused *DestinationError
				if (err == nil) != want || (err != nil && (!errors.As(err, &refused) || refused.URL != tc.url ||
					!strings.Contains(err.Error(), tc.url))) {
					t.Fatalf("fixtures=%v allowed=%v error=%v", fixtures, want, err)
				}
			}
		})
	}
	if !LoopbackFixtureURL("http://127.0.0.1:1234/portal.html") || !LoopbackFixtureURL("https://localhost/portal") ||
		LoopbackFixtureURL("http://192.168.1.10/portal") || LoopbackFixtureURL("https://arubanetworking.hpe.com/") ||
		LoopbackFixtureURL("ftp://127.0.0.1/") {
		t.Fatal("fixture portal detection is not limited to loopback http(s)")
	}
}

func destinationBackends() map[string]func(Config, func(string)) (*Client, error) {
	return map[string]func(Config, func(string)) (*Client, error){"http": New, "compatible": NewCompatible}
}

func TestBothBackendsRefuseLoopbackOutsideFixtureMode(t *testing.T) {
	for name, construct := range destinationBackends() {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
			}))
			defer server.Close()
			c, err := construct(Config{Timeout: 2 * time.Second, Retries: 2, MaxBytes: 1024}, func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				_, err = c.Download(context.Background(), model.Request{URL: server.URL + "/guide", Method: method},
					func(model.Resource) error { return nil })
				var refused *DestinationError
				var diagnostic *RetrievalError
				if !errors.As(err, &refused) || !errors.As(err, &diagnostic) || diagnostic.ApplicationAttempts != 1 ||
					!strings.Contains(err.Error(), server.URL+"/guide") || hits.Load() != 0 {
					t.Fatalf("%s loopback request was not refused before robots or dial: hits=%d err=%v", method, hits.Load(), err)
				}
			}
		})
	}
}

func TestBothBackendsRefuseNonPublicRedirectAndRobotsHops(t *testing.T) {
	targets := []string{
		"https://10.0.0.1/guide",
		"https://[::ffff:192.168.0.1]/guide",
		"https://169.254.169.254/latest/meta-data",
		"https://2130706433/guide",
		"https://localhost.localhost/guide",
		"https://[fd00::1]/guide",
		"http://192.168.1.10/guide",
	}
	for name, construct := range destinationBackends() {
		t.Run(name, func(t *testing.T) {
			for _, target := range targets {
				var starts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/robots.txt" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					starts.Add(1)
					http.Redirect(w, r, target, http.StatusFound)
				}))
				c, err := construct(Config{Timeout: 2 * time.Second, Retries: 2, MaxBytes: 1024, AllowLoopbackFixtures: true}, func(string) {})
				if err != nil {
					t.Fatal(err)
				}
				_, err = c.Download(context.Background(), model.Request{URL: server.URL + "/start"},
					func(model.Resource) error { return nil })
				c.Close()
				server.Close()
				var refused *DestinationError
				if !errors.As(err, &refused) || refused.URL != target || starts.Load() != 1 {
					t.Fatalf("redirect to %s was not refused once: starts=%d err=%v", target, starts.Load(), err)
				}
			}
			var content atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.Redirect(w, r, "https://10.1.2.3/robots.txt", http.StatusMovedPermanently)
					return
				}
				content.Add(1)
			}))
			defer server.Close()
			c, err := construct(Config{Timeout: 2 * time.Second, Retries: 2, MaxBytes: 1024, AllowLoopbackFixtures: true}, func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, err = c.Download(context.Background(), model.Request{URL: server.URL + "/guide"},
				func(model.Resource) error { return nil })
			var refused *DestinationError
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), "cannot establish publisher robots rules") ||
				content.Load() != 0 {
				t.Fatalf("robots redirect to a private address did not fail closed: content=%d err=%v", content.Load(), err)
			}
		})
	}
}

func TestByteLimitFailuresAreNeverRetried(t *testing.T) {
	payload := make([]byte, 4096)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"identity-length", "identity-chunked", "gzip-chunked"} {
		t.Run(mode, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				hits.Add(1)
				switch mode {
				case "identity-length":
					w.Write(payload)
				case "identity-chunked":
					w.(http.Flusher).Flush()
					w.Write(payload)
				default:
					// Random bytes do not compress, so the encoded stream
					// itself crosses the limit before the decoded output.
					w.Header().Set("Content-Encoding", "gzip")
					w.(http.Flusher).Flush()
					gz := gzip.NewWriter(w)
					gz.Write(payload)
					gz.Close()
				}
			}))
			defer server.Close()
			c := client(t, func(c *Config) { c.Retries, c.MaxBytes = 2, 2048 })
			c.backoff = func(context.Context, time.Duration) error { return nil }
			_, err := c.Download(context.Background(), model.Request{URL: server.URL + "/large"}, func(r model.Resource) error {
				_, err := io.Copy(io.Discard, r.Body)
				return err
			})
			var transient *transientBodyError
			if err == nil || !strings.Contains(err.Error(), "byte limit") || errors.As(err, &transient) || hits.Load() != 1 {
				t.Fatalf("oversized response was retried or misclassified: hits=%d err=%v", hits.Load(), err)
			}
		})
	}
}
