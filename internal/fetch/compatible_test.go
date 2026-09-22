package fetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
	"github.com/imroc/req/v3"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type wireProfile struct {
	settings []http2.Setting
	flow     uint32
	priority http2.PriorityParam
	pseudo   []string
	regular  []string
	headers  map[string]string
}

func captureProfile(t *testing.T, create func() *compatibilityBackend) wireProfile {
	t.Helper()
	captured := make(chan wireProfile, 1)
	failures := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("expected HTTP2")
	}))
	server.EnableHTTP2 = true
	server.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		"h2": func(_ *http.Server, conn *tls.Conn, _ http.Handler) {
			defer conn.Close()
			fail := func(err error) { failures <- err }
			if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				fail(err)
				return
			}

			preface := make([]byte, len(http2.ClientPreface))
			if _, err := io.ReadFull(conn, preface); err != nil {
				fail(err)
				return
			}
			if string(preface) != http2.ClientPreface {
				fail(fmt.Errorf("invalid HTTP2 preface"))
				return
			}
			framer := http2.NewFramer(conn, conn)
			framer.ReadMetaHeaders = hpack.NewDecoder(1<<20, nil)
			if err := framer.WriteSettings(); err != nil {
				fail(err)
				return
			}
			profile := wireProfile{headers: map[string]string{}}
			for {
				frame, err := framer.ReadFrame()
				if err != nil {
					fail(err)
					return
				}
				switch frame := frame.(type) {
				case *http2.SettingsFrame:
					if !frame.IsAck() {
						if err := frame.ForeachSetting(func(s http2.Setting) error {
							profile.settings = append(profile.settings, s)
							return nil
						}); err != nil {
							fail(err)
							return
						}
						if err := framer.WriteSettingsAck(); err != nil {
							fail(err)
							return
						}
					}
				case *http2.WindowUpdateFrame:
					if frame.StreamID == 0 {
						profile.flow = frame.Increment
					}
				case *http2.MetaHeadersFrame:
					profile.priority = frame.Priority
					for _, field := range frame.Fields {
						profile.headers[field.Name] = field.Value
						if strings.HasPrefix(field.Name, ":") {
							profile.pseudo = append(profile.pseudo, field.Name)
						} else {
							profile.regular = append(profile.regular, field.Name)
						}
					}
					captured <- profile
					var block bytes.Buffer
					encoder := hpack.NewEncoder(&block)
					for _, h := range []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "2"}} {
						if err := encoder.WriteField(h); err != nil {
							fail(err)
							return
						}
					}
					if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: frame.StreamID, EndHeaders: true, BlockFragment: block.Bytes()}); err != nil {
						fail(err)
						return
					}
					if err := framer.WriteData(frame.StreamID, true, []byte("OK")); err != nil {
						fail(err)
					}
					return
				}
			}
		},
	}
	server.StartTLS()
	defer server.Close()
	backend := create()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	backend.transport.TLSClientConfig.RootCAs = pool
	defer backend.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/robots.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := backend.RoundTrip(request)
	if err != nil {
		select {
		case serverErr := <-failures:
			t.Fatalf("request=%v server=%v", err, serverErr)
		default:
			t.Fatal(err)
		}
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "OK" {
		t.Fatalf("wire fixture body: %q %v", body, err)
	}
	select {
	case got := <-captured:
		return got
	case err := <-failures:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return wireProfile{}
}

func scriptedHTTP2Server(t *testing.T, refuse func(int32) bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("expected raw HTTP2 handler")
	}))
	server.EnableHTTP2 = true
	server.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		"h2": func(_ *http.Server, conn *tls.Conn, _ http.Handler) {
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
				return
			}
			preface := make([]byte, len(http2.ClientPreface))
			if _, err := io.ReadFull(conn, preface); err != nil || string(preface) != http2.ClientPreface {
				return
			}
			framer := http2.NewFramer(conn, conn)
			framer.ReadMetaHeaders = hpack.NewDecoder(1<<20, nil)
			if err := framer.WriteSettings(); err != nil {
				return
			}
			for {
				frame, err := framer.ReadFrame()
				if err != nil {
					return
				}
				switch frame := frame.(type) {
				case *http2.SettingsFrame:
					if !frame.IsAck() {
						if err := framer.WriteSettingsAck(); err != nil {
							return
						}
					}
				case *http2.MetaHeadersFrame:
					if current := attempts.Add(1); refuse(current) {
						if err := framer.WriteRSTStream(frame.StreamID, http2.ErrCodeRefusedStream); err != nil {
							return
						}

						continue
					}
					var block bytes.Buffer
					encoder := hpack.NewEncoder(&block)
					if err := encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"}); err != nil {
						return
					}
					if err := encoder.WriteField(hpack.HeaderField{Name: "content-length", Value: "2"}); err != nil {
						return
					}
					if err := framer.WriteHeaders(http2.HeadersFrameParam{
						StreamID: frame.StreamID, EndHeaders: true, BlockFragment: block.Bytes(),
					}); err != nil {
						return
					}
					if err := framer.WriteData(frame.StreamID, true, []byte("OK")); err != nil {
						return
					}
				}
			}
		},
	}
	server.StartTLS()
	return server, &attempts
}

func refusedStreamServer(t *testing.T, refusals int) (*httptest.Server, *atomic.Int32) {
	return scriptedHTTP2Server(t, func(attempt int32) bool { return int(attempt) <= refusals })
}

func TestWireAttemptAllowanceAccountsRealHTTP2RefusedStreamReplay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend func(*x509.CertPool) http.RoundTripper
	}{
		{"compatible", func(roots *x509.CertPool) http.RoundTripper {
			backend := newCompatibilityBackend(true, true)
			backend.transport.TLSClientConfig.RootCAs = roots
			return backend
		}},
		{"standard", func(roots *x509.CertPool) http.RoundTripper {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.ForceAttemptHTTP2 = true
			transport.TLSClientConfig = &tls.Config{RootCAs: roots}
			return transport
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, attempts := refusedStreamServer(t, 1)
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			budget, err := NewAttemptBudget(CompatibleWireAttemptAllowance, CompatibleWireAttemptAllowance)
			if err != nil {
				t.Fatal(err)
			}
			backend := tc.backend(roots)
			client := &http.Client{Transport: scheduledBackend{backend: backend, attempts: budget}}
			request, err := http.NewRequestWithContext(
				context.WithValue(context.Background(), dispatchKey{}, func() error { return nil }),
				http.MethodGet, server.URL+"/refused", nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			stats := budget.Stats()
			if readErr != nil || closeErr != nil || string(body) != "OK" ||
				attempts.Load() != 2 || stats.AttemptedTransmissions != 2 || stats.ObservedHeaderWrites != 2 ||
				stats.AdmittedRequests != 1 || stats.ReservedAttempts != 0 || stats.AllowanceViolated {
				t.Fatalf("transparent replay accounting failed: body=%q server=%d stats=%+v errors=%v/%v",
					body, attempts.Load(), stats, readErr, closeErr)
			}
			request, _ = http.NewRequestWithContext(
				context.WithValue(context.Background(), dispatchKey{}, func() error { return nil }),
				http.MethodGet, server.URL+"/blocked", nil,
			)
			_, err = client.Do(request)
			var exhausted *AttemptBudgetError
			if !errors.As(err, &exhausted) || attempts.Load() != 2 {
				t.Fatalf("N+1 dispatch was not blocked: server=%d err=%v", attempts.Load(), err)
			}
			if closer, ok := backend.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
		})
	}
}

func TestCompatibilityGroupsAreSeparatedOnWire(t *testing.T) {
	for _, headers := range []bool{false, true} {
		for _, protocol := range []bool{false, true} {
			t.Run(fmt.Sprintf("H%t-P%t", headers, protocol), func(t *testing.T) {
				got := captureProfile(t, func() *compatibilityBackend { return newCompatibilityBackend(headers, protocol) })
				if got.headers["user-agent"] != userAgent || got.headers["accept-encoding"] != "gzip" {
					t.Fatalf("fixed application identity changed: %v", got.headers)
				}
				if headers {
					if got.headers["accept-language"] != "zh-CN,zh;q=0.9" || got.headers["sec-fetch-mode"] != "navigate" {
						t.Fatal("preset header group missing")
					}
					if got.regular[0] != "pragma" || got.regular[1] != "cache-control" {
						t.Fatalf("regular-header ordering not applied: %v", got.regular)
					}
				} else if len(got.regular) != 2 {
					t.Fatalf("header group leaked into ordinary case: %v", got.regular)
				}
				if protocol {
					want := []http2.Setting{
						{ID: http2.SettingHeaderTableSize, Val: 65536}, {ID: http2.SettingEnablePush, Val: 0},
						{ID: http2.SettingMaxConcurrentStreams, Val: 1000}, {ID: http2.SettingInitialWindowSize, Val: 6291456},
						{ID: http2.SettingMaxHeaderListSize, Val: 262144},
					}
					if !reflect.DeepEqual(got.settings, want) || got.flow != 15663105 ||
						got.priority != (http2.PriorityParam{Exclusive: true, Weight: 255}) ||
						!reflect.DeepEqual(got.pseudo, []string{":method", ":authority", ":scheme", ":path"}) {
						t.Fatalf("preset HTTP2 group wrong: %+v", got)
					}
				} else {
					want := []http2.Setting{
						{ID: http2.SettingEnablePush, Val: 0}, {ID: http2.SettingInitialWindowSize, Val: 4 << 20},
						{ID: http2.SettingMaxHeaderListSize, Val: 1 << 20},
					}
					if !reflect.DeepEqual(got.settings, want) || got.flow != 1<<30 ||
						got.priority != (http2.PriorityParam{}) ||
						!reflect.DeepEqual(got.pseudo, []string{":authority", ":method", ":path", ":scheme"}) {
						t.Fatalf("HTTP2 group leaked or differs from ordinary: %+v", got)
					}
				}
			})
		}
	}
}

func TestCompatibleMatchesPinnedPresetWithApplicationUA(t *testing.T) {
	want := captureProfile(t, func() *compatibilityBackend {
		client := req.C().SetCookieJar(nil).ImpersonateChrome()
		client.SetCommonHeader("User-Agent", userAgent)
		client.SetCommonHeader("Accept-Encoding", "gzip")
		client.Transport.DisableCompression = true
		client.Transport.DisableAutoDecode()
		return &compatibilityBackend{transport: client.Transport, headers: client.Headers.Clone(), slots: make(chan struct{}, 4)}
	})
	got := captureProfile(t, func() *compatibilityBackend { return newCompatibilityBackend(true, true) })
	delete(got.headers, ":authority")
	delete(want.headers, ":authority")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixed profile differs from pinned preset:\ngot=%+v\nwant=%+v", got, want)
	}
}

func TestCompatibleConcurrentTLSOrigins(t *testing.T) {
	pool := x509.NewCertPool()
	var servers []*httptest.Server
	for range 4 {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/robots.txt" {
				w.WriteHeader(404)
				return
			}
			io.WriteString(w, "content")
		}))
		server.EnableHTTP2 = true
		server.StartTLS()
		t.Cleanup(server.Close)
		pool.AddCert(server.Certificate())
		servers = append(servers, server)
	}
	backend := newCompatibilityBackend(true, true)
	backend.transport.TLSClientConfig.RootCAs = pool
	client, err := NewWithRoundTripper(Config{Timeout: 3 * time.Second, MaxBytes: 1024}, func(string) {}, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var workers sync.WaitGroup
	for _, server := range servers {
		workers.Go(func() {
			_, err := client.Download(context.Background(), model.Request{URL: server.URL + "/guide"}, func(r model.Resource) error {
				_, err := io.Copy(io.Discard, r.Body)
				return err
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
}

func TestCompatibleLeasesBoundConcurrencyReuseAndClose(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/held" {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "OK")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	backend := newCompatibilityBackend(true, true)
	backend.transport.TLSClientConfig.RootCAs = pool
	defer backend.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var active []*http.Response
	for range 4 {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/held", nil)
		response, err := backend.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		active = append(active, response)
	}
	if len(backend.slots) != 4 {
		t.Fatal("compatible responses were silently serialized")
	}
	waiting, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	request, _ := http.NewRequestWithContext(waiting, http.MethodGet, server.URL+"/held", nil)
	if _, err := backend.RoundTrip(request); err == nil || waiting.Err() == nil {
		t.Fatalf("queued lease did not respect cancellation: %v", err)
	}
	backend.CloseIdleConnections()
	for _, response := range active {
		response.Body.Close()
	}
	if len(backend.slots) != 0 || len(backend.idle) != 0 {
		t.Fatal("closing active generation returned stale leases to pool")
	}
	for i := range 2 {
		reused := false
		requestContext := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		})
		request, _ := http.NewRequestWithContext(requestContext, http.MethodGet, server.URL+"/ok", nil)
		response, err := backend.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if i == 1 && !reused {
			t.Fatal("warm compatible request did not reuse its connection")
		}
	}
	if len(backend.idle) != 1 || backend.transport.MaxIdleConns != 4 {
		t.Fatal("idle compatible transport pool is not bounded")
	}
}
