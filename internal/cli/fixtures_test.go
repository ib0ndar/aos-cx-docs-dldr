package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"aos-cx-docs-dldr/internal/testutil"
)

// Fixture servers shared by platform-independent tests live here rather than in
// prompt_pty_test.go. That file is darwin-gated because it drives a real PTY,
// and hosting shared fixtures there made the whole cli test package fail to
// compile on every other platform.

// portalDataPaths are the files every catalogue refresh reads after the
// product page, in request order: the portal registry, the AOS-CX product
// configuration and the route adapter. Request-count assertions include them.
var portalDataPaths = []string{
	testutil.PortalBase + "models/products.json",
	testutil.PortalBase + "models/aoscx/product.json",
	testutil.PortalBase + "adapters/aoscx-adapter.js",
}

// addPortalFixtures registers an authored portal in a URL-keyed round-tripper
// fixture map: the product page at pageURL and the portal files on its origin.
func addPortalFixtures(fixtures map[string]hpeHTTPFixture, pageURL string, portal testutil.Portal) {
	page, err := url.Parse(pageURL)
	if err != nil {
		panic(err)
	}
	origin := page.Scheme + "://" + page.Host
	fixtures[pageURL] = hpeHTTPFixture{"text/html", []byte(portal.Page())}
	for _, path := range portalDataPaths {
		contentType, body, _ := portal.File(path, origin)
		fixtures[origin+path] = hpeHTTPFixture{contentType, body}
	}
}

// promptUnavailableFixtureServer advertises two mapped guides where exactly one
// source front is a definitive HTTP 404, so availability-aware selection and
// bulk selection can be exercised without a terminal.
func promptUnavailableFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	portal := testutil.Portal{Platforms: []string{"6000"}, Versions: []string{"10.17"},
		Guides: []testutil.PortalGuide{{ID: "webUI", Title: "Introduction to the WebUI Guide"}, {ID: "other", Title: "Other Guide"}}}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if portal.Serve(w, r, "/portal/aoscx.html") {
			return
		}
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/json/aoscx/webUI.json":
			fmt.Fprintf(w, `{"10.17":{"6000":%q}}`, server.URL+"/missing/Content/home.htm")
		case "/portal/json/aoscx/other.json":
			fmt.Fprintf(w, `{"10.17":{"6000":%q}}`, server.URL+"/working/index.html")
		case "/missing/Content/home.htm":
			w.WriteHeader(http.StatusNotFound)
		case "/working/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body><nav class="wh_publication_toc"><ul>
<li><a href="topic.html">Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>Working</h1></main></body></html>`)
		case "/working/topic.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<article><h1>Topic</h1></article>`)
		default:
			t.Errorf("unexpected unavailable fixture request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
