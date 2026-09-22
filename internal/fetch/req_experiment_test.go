//go:build reqexperiment

package fetch

import (
	"context"
	"crypto/x509"
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

func TestReqFixedConfiguration(t *testing.T) {
	transport := newReqTransport()
	defer transport.CloseIdleConnections()
	if transport.TLSClientConfig.InsecureSkipVerify || transport.TLSHandshakeContext == nil ||
		!transport.DisableCompression || transport.AutoDecompression || transport.Options.EnableH2C ||
		len(transport.Headers) != 0 || len(transport.Cookies) != 0 || transport.MaxResponseHeaderBytes != 1<<20 {
		t.Fatal("experimental backend configuration differs from inspected policy")
	}
	request, err := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, wantErr := http.ProxyFromEnvironment(request)
	got, gotErr := transport.Proxy(request)
	if (wantErr != nil) != (gotErr != nil) || (want == nil) != (got == nil) ||
		(want != nil && got != nil && want.String() != got.String()) {
		t.Fatal("experimental backend changed inherited proxy selection")
	}
}

func TestReqTLSVerificationFailsClosed(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer server.Close()
	c, err := NewReqExperiment(Config{Timeout: time.Second, MaxBytes: 1024}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Do(context.Background(), model.Request{URL: server.URL + "/guide"})
	var unknown x509.UnknownAuthorityError
	if err == nil || !errors.As(err, &unknown) || hits.Load() != 0 {
		t.Fatalf("untrusted TLS was not rejected before HTTP: hits=%d error=%v", hits.Load(), err)
	}
}

func TestReqHTTP2HeadersRawBytesAndCookieIsolation(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.ProtoMajor != 2 || r.Header.Get("User-Agent") != userAgent || r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("unexpected protocol or declared headers: %s %v", r.Proto, r.Header)
		}
		for _, forbidden := range []string{"Cookie", "Authorization", "Proxy-Authorization", "Accept-Language",
			"Sec-Ch-Ua", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Upgrade-Insecure-Requests"} {
			if r.Header.Get(forbidden) != "" {
				t.Errorf("backend injected %s", forbidden)
			}
		}
		w.Header().Set("Set-Cookie", "fixture=value; Path=/; Secure")
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", 302)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=windows-1252")
		w.Write([]byte{0xe9, 0x00, 0xff})
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := newReqTransport()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	transport.TLSClientConfig.RootCAs = pool
	c, err := NewWithRoundTripper(Config{Timeout: time.Second, MaxBytes: 1024}, func(string) {}, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodGet} {
		r, err := c.Do(context.Background(), model.Request{URL: server.URL + "/start#ignored", Method: method})
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(r.Body)
		r.Body.Close()
		if readErr != nil || r.URL != server.URL+"/final" {
			t.Fatalf("redirect/body identity: %+v %v", r, readErr)
		}
		if method == http.MethodHead && len(body) != 0 {
			t.Fatal("HEAD returned body")
		}
		if method == http.MethodGet && string(body) != string([]byte{0xe9, 0x00, 0xff}) {
			t.Fatalf("backend transformed raw charset/binary bytes: %x", body)
		}
	}
	if hits.Load() != 7 {
		t.Fatalf("unexpected hidden requests: %d", hits.Load())
	}
}

func TestReqHTTP2BodyRetryAndCancellation(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path == "/slow" {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Length", "100")
		if hits.Add(1) == 1 {
			io.WriteString(w, strings.Repeat("x", 20))
			return
		}
		io.WriteString(w, strings.Repeat("y", 100))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := newReqTransport()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	transport.TLSClientConfig.RootCAs = pool
	c, err := NewWithRoundTripper(Config{Retries: 1, Timeout: 3 * time.Second, MaxBytes: 1024}, func(message string) { t.Log(message) }, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var body []byte
	_, err = c.Download(context.Background(), model.Request{URL: server.URL + "/guide"}, func(r model.Resource) error {
		var err error
		body, err = io.ReadAll(r.Body)
		return err
	})
	if err != nil || hits.Load() != 2 || string(body) != strings.Repeat("y", 100) {
		t.Fatalf("HTTP/2 complete-body retries differ: hits=%d len=%d error=%v", hits.Load(), len(body), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = c.Download(ctx, model.Request{URL: server.URL + "/slow"}, func(r model.Resource) error {
		_, err := io.Copy(io.Discard, r.Body)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HTTP/2 cancellation lost: %v", err)
	}
}
