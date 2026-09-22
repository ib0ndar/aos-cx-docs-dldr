package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Fixture servers shared by platform-independent tests live here rather than in
// prompt_pty_test.go. That file is darwin-gated because it drives a real PTY,
// and hosting shared fixtures there made the whole cli test package fail to
// compile on every other platform.

// promptUnavailableFixtureServer advertises two mapped guides where exactly one
// source front is a definitive HTTP 404, so availability-aware selection and
// bulk selection can be exercised without a terminal.
func promptUnavailableFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>6000</option></select>
<select id="ver"><option>10.17</option></select><div id="menu1"><table><tr>
<td id="webUI" onclick="openFile('HTML','webUI','aoscx')">Introduction to the WebUI Guide</td>
<td id="other" onclick="openFile('HTML','other','aoscx')">Other Guide</td>
</tr></table></div></html>`)
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
