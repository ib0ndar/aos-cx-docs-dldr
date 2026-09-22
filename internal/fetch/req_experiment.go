//go:build reqexperiment

package fetch

import (
	"crypto/tls"
	"net/http"

	"github.com/imroc/req/v3"
	utls "github.com/refraction-networking/utls"
)

const ReqExperimentProfile = "req/v3.61.0; TLS HelloChrome_120; req-default HTTP/2; " + userAgent + " headers"

// NewReqExperiment is build-gated feasibility code, not a supported CLI backend.
func NewReqExperiment(c Config, notify func(string)) (*Client, error) {
	return NewWithRoundTripper(c, notify, ReqExperimentRoundTripper())
}

// ReqExperimentRoundTripper exposes only the standard boundary to the gated
// evidence harness, which wraps it with request limits and redacted observations.
func ReqExperimentRoundTripper() http.RoundTripper {
	return newReqTransport()
}

func newReqTransport() *req.Transport {
	client := req.C().SetCookieJar(nil).SetTLSFingerprint(utls.HelloChrome_120)
	transport := client.Transport
	transport.DisableAutoDecode()
	transport.DisableCompression = true
	transport.AutoDecompression = false
	transport.Headers = nil
	transport.Cookies = nil
	transport.Proxy = http.ProxyFromEnvironment
	transport.MaxResponseHeaderBytes = 1 << 20
	transport.SetHTTP2MaxHeaderListSize(1 << 20)
	transport.MaxIdleConnsPerHost = 4
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	return transport
}
