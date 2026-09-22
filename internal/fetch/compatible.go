package fetch

import (
	"crypto/tls"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/imroc/req/v3"
	"github.com/imroc/req/v3/http2"
	utls "github.com/refraction-networking/utls"
)

const CompatibleProfile = "req/v3.61.0 Chrome120 TLS + preset header/H2 groups; " + userAgent

func NewCompatible(c Config, notify func(string)) (*Client, error) {
	return NewWithRoundTripper(c, notify, CompatibleRoundTripper())
}

// CompatibleRoundTripper is a fixed native profile, not arbitrary fingerprint
// configuration. Robots identity and wire User-Agent remain the application.
func CompatibleRoundTripper() http.RoundTripper {
	return newCompatibilityBackend(true, true)
}

type compatibilityBackend struct {
	transport *req.Transport
	headers   http.Header
	mu        sync.Mutex
	idle      []*req.Transport
	epoch     uint64
	slots     chan struct{}
}

func newCompatibilityBackend(headerPreset, h2Preset bool) *compatibilityBackend {
	// req ordering setters wrap one another rather than replace earlier
	// wrappers. Start bare so disabling one diagnostic group really removes it.
	client := req.C().SetCookieJar(nil).SetTLSFingerprint(utls.HelloChrome_120)
	if headerPreset {
		preset := req.C().SetCookieJar(nil).ImpersonateChrome()
		client.Headers = preset.Headers.Clone()
		client.SetCommonHeaderOrder("host", "pragma", "cache-control", "sec-ch-ua",
			"sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests",
			"user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-user",
			"sec-fetch-dest", "referer", "accept-encoding", "accept-language", "cookie")
	}
	if h2Preset {
		client.SetHTTP2SettingsFrame(
			http2.Setting{ID: http2.SettingHeaderTableSize, Val: 65536},
			http2.Setting{ID: http2.SettingEnablePush, Val: 0},
			http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 1000},
			http2.Setting{ID: http2.SettingInitialWindowSize, Val: 6291456},
			http2.Setting{ID: http2.SettingMaxHeaderListSize, Val: 262144},
		)
		client.SetHTTP2ConnectionFlow(15663105)
		client.SetHTTP2HeaderPriority(http2.PriorityParam{StreamDep: 0, Exclusive: true, Weight: 255})
		client.SetCommonPseudoHeaderOder(":method", ":authority", ":scheme", ":path")
	}
	client.SetCommonHeader("User-Agent", userAgent)
	client.SetCommonHeader("Accept-Encoding", "gzip")
	headers := client.Headers.Clone()
	t := client.Transport
	t.DisableAutoDecode()
	t.DisableCompression, t.AutoDecompression = true, false
	t.Headers, t.Cookies = nil, nil
	t.MaxResponseHeaderBytes = 1 << 20
	t.SetHTTP2MaxHeaderListSize(1 << 20)
	t.MaxIdleConnsPerHost = 4
	t.MaxIdleConns = 4
	t.TLSClientConfig.MinVersion = tls.VersionTLS12
	return &compatibilityBackend{transport: t, headers: headers, slots: make(chan struct{}, 4)}
}

func (b *compatibilityBackend) RoundTrip(r *http.Request) (*http.Response, error) {
	return b.RoundTripWithDispatch(r, func() error { return nil })
}

func (b *compatibilityBackend) RoundTripWithDispatch(r *http.Request, start func() error) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case b.slots <- struct{}{}:
	}
	b.mu.Lock()
	epoch := b.epoch
	var transport *req.Transport
	if len(b.idle) > 0 {
		transport = b.idle[len(b.idle)-1]
		b.idle = b.idle[:len(b.idle)-1]
	} else {
		transport = b.transport.Clone()
	}
	b.mu.Unlock()
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	for key, values := range b.headers {
		r.Header[key] = append([]string{}, values...)
	}
	if err := start(); err != nil {
		transport.CloseIdleConnections()
		<-b.slots
		return nil, err
	}
	response, err := transport.RoundTrip(r)
	if err != nil {
		transport.CloseIdleConnections()
		<-b.slots
		return response, err
	}
	response.Body = &compatibleBody{ReadCloser: response.Body, owner: b, transport: transport, epoch: epoch}
	return response, nil
}

// req's HTTP2 connection setup mutates shared settings. Lease a deep transport
// clone through body consumption, retaining reuse without concurrent mutation.
type compatibleBody struct {
	io.ReadCloser
	owner     *compatibilityBackend
	transport *req.Transport
	failed    atomic.Bool
	once      sync.Once
	closeErr  error
	epoch     uint64
}

func (b *compatibleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		b.failed.Store(true)
	}
	return n, err
}

func (b *compatibleBody) Close() error {
	b.once.Do(func() {
		defer func() { <-b.owner.slots }()
		b.closeErr = b.ReadCloser.Close()
		b.owner.mu.Lock()
		if b.owner.epoch != b.epoch || b.failed.Load() || b.closeErr != nil || len(b.owner.idle) >= 4 {
			b.owner.mu.Unlock()
			b.transport.CloseIdleConnections()
			return
		}
		b.owner.idle = append(b.owner.idle, b.transport)
		b.owner.mu.Unlock()
	})
	return b.closeErr
}

func (b *compatibilityBackend) CloseIdleConnections() {
	b.mu.Lock()
	idle := b.idle
	b.idle = nil
	b.epoch++
	b.mu.Unlock()
	for _, transport := range idle {
		transport.CloseIdleConnections()
	}
}
