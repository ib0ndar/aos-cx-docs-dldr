package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"

	"github.com/imroc/req/v3"
)

func directProxy(*http.Request) (*url.URL, error) { return nil, nil }

func sameProxy(a, b *url.URL) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.String() == b.String()
}

func TestPublicDialMarksOnlyDirectPlainHTTPHops(t *testing.T) {
	viaProxy := func(*http.Request) (*url.URL, error) { return url.Parse("http://10.0.0.8:3128") }
	for _, tc := range []struct {
		url    string
		proxy  func(*http.Request) (*url.URL, error)
		marked bool
	}{
		{"http://arubanetworking.hpe.com/techdocs/a.html", directProxy, true},
		{"https://arubanetworking.hpe.com/techdocs/a.html", directProxy, false},
		{"http://arubanetworking.hpe.com/techdocs/a.html", viaProxy, false},
	} {
		ctx, err := withPublicDial(context.Background(), tc.url, false, tc.proxy)
		if err != nil {
			t.Fatal(err)
		}
		if _, marked := ctx.Value(publicDialKey{}).(publicDialPolicy); marked != tc.marked {
			t.Fatalf("%s marked=%v, want %v", tc.url, marked, tc.marked)
		}
	}
}

func TestPublicDialRefusesNonPublicResolvedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	dial := publicDialContext(&net.Dialer{Timeout: time.Second})
	target := "http://docs.example.com/guide"
	for _, tc := range []struct {
		name    string
		ctx     func() context.Context
		refused bool
	}{
		{"unmarked", context.Background, false},
		{"marked", func() context.Context {
			ctx, _ := withPublicDial(context.Background(), target, false, directProxy)
			return ctx
		}, true},
		{"marked fixture", func() context.Context {
			ctx, _ := withPublicDial(context.Background(), target, true, directProxy)
			return ctx
		}, false},
	} {
		conn, err := dial(tc.ctx(), "tcp", listener.Addr().String())
		var refused *DestinationError
		if tc.refused {
			if !errors.As(err, &refused) || refused.URL != target || !strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatalf("%s: loopback dial was not refused: %v", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		conn.Close()
	}
}

// dialHook replaces the dial function of either backend so the tests can see
// what reaches it and simulate a refusal at dial time.
func dialHook(t *testing.T, c *Client, dial func(context.Context, string, string) (net.Conn, error)) {
	t.Helper()
	switch backend := c.http.Transport.(scheduledBackend).backend.(type) {
	case *http.Transport:
		probe := httptest.NewRequest(http.MethodGet, "http://docs.example.com/", nil)
		got, gotErr := backend.Proxy(probe)
		want, wantErr := http.ProxyFromEnvironment(probe)
		if !sameProxy(got, want) || (gotErr == nil) != (wantErr == nil) {
			t.Fatal("standard backend no longer takes its proxy from the environment")
		}
		backend.DialContext = dial
	case *compatibilityBackend:
		probe := httptest.NewRequest(http.MethodGet, "http://docs.example.com/", nil)
		got, gotErr := backend.transport.Proxy(probe)
		want, wantErr := http.ProxyFromEnvironment(probe)
		if !sameProxy(got, want) || (gotErr == nil) != (wantErr == nil) {
			t.Fatal("compatible backend no longer takes its proxy from the environment")
		}
		var _ *req.Transport = backend.transport.SetDial(dial)
	default:
		t.Fatalf("unexpected backend %T", backend)
	}
}

func TestBothBackendsCarryPublicDialPolicyToTheDialer(t *testing.T) {
	for name, construct := range destinationBackends() {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Write([]byte("guide"))
			}))
			defer server.Close()
			c, err := construct(Config{Timeout: 2 * time.Second, Retries: 2, MaxBytes: 1024, AllowLoopbackFixtures: true}, func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			var marked, unmarked atomic.Int32
			guarded := publicDialContext(&net.Dialer{Timeout: time.Second})
			dialHook(t, c, func(ctx context.Context, network, address string) (net.Conn, error) {
				if _, ok := ctx.Value(publicDialKey{}).(publicDialPolicy); ok {
					marked.Add(1)
				} else {
					unmarked.Add(1)
				}
				return guarded(ctx, network, address)
			})
			if _, err := c.Download(context.Background(), model.Request{URL: server.URL + "/guide"},
				func(model.Resource) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if marked.Load() == 0 || unmarked.Load() != 0 {
				t.Fatalf("direct plain-HTTP dials did not carry the policy: marked=%d unmarked=%d", marked.Load(), unmarked.Load())
			}
		})
	}
}

func TestBothBackendsReturnDialRefusalWithoutRetry(t *testing.T) {
	for name, construct := range destinationBackends() {
		t.Run(name, func(t *testing.T) {
			var robots atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				robots.Add(1)
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			c, err := construct(Config{Timeout: 2 * time.Second, Retries: 2, MaxBytes: 1024, AllowLoopbackFixtures: true}, func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			target := server.URL + "/guide"
			var dials atomic.Int32
			dialHook(t, c, func(ctx context.Context, network, address string) (net.Conn, error) {
				dials.Add(1)
				// Simulate a name that resolved to a private address: the
				// refusal arrives from the dialer, wrapped as net.Dial wraps it.
				return nil, &net.OpError{Op: "dial", Net: network, Err: &DestinationError{
					URL: target, Reason: "plain HTTP host resolved to 10.0.0.5: host is a non-public IP address"}}
			})
			_, err = c.Download(context.Background(), model.Request{URL: target}, func(model.Resource) error { return nil })
			var refused *DestinationError
			var diagnostic *RetrievalError
			if !errors.As(err, &refused) || refused.URL != target || !errors.As(err, &diagnostic) ||
				diagnostic.ApplicationAttempts != 1 || !strings.Contains(err.Error(), "10.0.0.5") {
				t.Fatalf("dial refusal was not final: dials=%d err=%v", dials.Load(), err)
			}
		})
	}
}
