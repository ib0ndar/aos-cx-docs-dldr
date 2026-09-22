//go:build reqexperiment

package fetch

import (
	"crypto/x509"
	"net/http"
)

// GroupedCompatibilityRoundTripper is diagnostic-only. Production exposes one
// fixed compatible profile; roots may be supplied only by local fixture tests.
func GroupedCompatibilityRoundTripper(headers, h2 bool, roots *x509.CertPool) http.RoundTripper {
	backend := newCompatibilityBackend(headers, h2)
	backend.transport.TLSClientConfig.RootCAs = roots
	return backend
}
