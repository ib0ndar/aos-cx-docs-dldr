package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/source"
	"aos-cx-docs-dldr/internal/testutil"
)

// peerMappingFixtureServer maps two guides for 8320 and 8325. In 10.17 both
// platforms share each guide's document; in 10.18 the publisher maps 8325 to
// documents it does not serve. The CoPP document mapped for 8320 is served;
// the VSX one is not.
func peerMappingFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	portal := testutil.Portal{Platforms: []string{"8320", "8325"}, Versions: []string{"10.18", "10.17"},
		Guides: []testutil.PortalGuide{{ID: "copp", Title: "CoPP Guide"}, {ID: "vsx", Title: "VSX Guide"}}}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if portal.Serve(w, r, "/portal/aoscx.html") {
			return
		}
		switch r.URL.Path {
		case "/robots.txt", "/acopp/index.html", "/avsx/index.html", "/vsx/index.html":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/json/aoscx/copp.json":
			fmt.Fprintf(w, `{"10.17":{"8320":%[1]q,"8325":%[1]q},"10.18":{"8320":%[2]q,"8325":%[3]q}}`,
				server.URL+"/copp17/index.html", server.URL+"/copp/index.html", server.URL+"/acopp/index.html")
		case "/portal/json/aoscx/vsx.json":
			fmt.Fprintf(w, `{"10.17":{"8320":%[1]q,"8325":%[1]q},"10.18":{"8320":%[2]q,"8325":%[3]q}}`,
				server.URL+"/vsx17/index.html", server.URL+"/vsx/index.html", server.URL+"/avsx/index.html")
		case "/copp/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body><nav class="wh_publication_toc"><ul>
<li><a href="topic.html">Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>CoPP</h1></main></body></html>`)
		case "/copp/topic.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<article><h1>Topic</h1></article>`)
		default:
			t.Errorf("unexpected peer mapping fixture request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestUnservedMappingUsesTheDocumentMappedForPeers(t *testing.T) {
	server := peerMappingFixtureServer(t)
	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "8325", "--release", "10.18",
		"--all", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "raw-v2"),
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--workers", "1", "--json",
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, newNativeClient)
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	manifest := output.Manifest
	if manifest.Status != "complete" || len(manifest.Guides) != 1 || manifest.Guides[0].ID != "copp" ||
		manifest.Guides[0].SourceURL != server.URL+"/copp/index.html" ||
		manifest.Guides[0].RouteOrigin != source.RouteOriginPeerMapping {
		t.Fatalf("CoPP was not archived from the peers' document: %+v", manifest.Guides)
	}
	if len(manifest.SkippedUnavailable) != 1 || manifest.SkippedUnavailable[0].ID != "vsx" ||
		manifest.SkippedUnavailable[0].MappedSourceURL != server.URL+"/avsx/index.html" {
		t.Fatalf("VSX, whose peers' document is not served either, was not skipped: %+v", manifest.SkippedUnavailable)
	}
	if !slices.ContainsFunc(manifest.CatalogueWarnings, func(message string) bool {
		return strings.Contains(message, server.URL+"/acopp/index.html") &&
			strings.Contains(message, server.URL+"/copp/index.html") && strings.Contains(message, "8320")
	}) {
		t.Fatalf("manifest does not record the replaced mapping: %q", manifest.CatalogueWarnings)
	}
	text := stderr.String()
	for _, expected := range []string{
		"Mapped source unavailable for CoPP Guide: mapped source HTTP 404; CoPP Guide for 8325 10.18",
		"Candidate for unavailable VSX Guide on 8325 10.18 is not usable: mapped source HTTP 404 (" + server.URL + "/vsx/index.html)",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("diagnostics omitted %q:\n%s", expected, text)
		}
	}
}

// releasePDFFixtureServer routes 10.16 guides through the portal's adapter.
// The publisher serves neither the HTML mapped for 8325 nor the one mapped for
// its peer 8320, only PDFs at the release's PDF route: the CoPP one titled for
// the guide, release and platform, the VSX one for another release.
func releasePDFFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	portal := testutil.Portal{Platforms: []string{"8320", "8325"}, Versions: []string{"10.16", "10.15"},
		Guides: []testutil.PortalGuide{{ID: "copp", Title: "CoPP Guide"}, {ID: "vsx", Title: "VSX Guide"},
			{ID: "webUI", Title: "Introduction to the Web UI Guide"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if portal.Serve(w, r, "/portal/aoscx.html") {
			return
		}
		switch r.URL.Path {
		case "/portal/json/aoscx/webUI.json":
			io.WriteString(w, `{"10.15":{"8320":"web_ui_83xx","8325":"web_ui_83xx"},"10.16":{"8320":"web_ui_83xx","8325":"web_ui_83xx"}}`)
		case "/techdocs/AOS-CX/10.16/PDF/web_ui_83xx.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(testutil.TitledPDF("web", "(AOS-CX Web UI Guide)", false))
		case "/portal/json/aoscx/copp.json":
			io.WriteString(w, `{"10.15":{"8320":"copp_83xx","8325":"copp_83xx"},"10.16":{"8320":"copp_83xx","8325":"acopp_83xx"}}`)
		case "/portal/json/aoscx/vsx.json":
			io.WriteString(w, `{"10.15":{"8320":"vsx_83xx","8325":"vsx_83xx"},"10.16":{"8320":"vsx_83xx","8325":"avsx_83xx"}}`)
		case "/techdocs/AOS-CX/10.16/PDF/copp_83xx.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(testutil.TitledPDF("copp", "(AOS-CX 10.16.xxxx CoPP Guide \\(8320, 8325 Switch Series\\))", false))
		case "/techdocs/AOS-CX/10.16/PDF/vsx_83xx.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(testutil.TitledPDF("vsx", "(AOS-CX 10.15.xxxx VSX Guide \\(8320, 8325\\))", false))
		case "/robots.txt",
			"/techdocs/AOS-CX/10.16/HTML/acopp_83xx/Content/home.htm", "/techdocs/AOS-CX/10.16/HTML/copp_83xx/Content/home.htm",
			"/techdocs/AOS-CX/10.16/HTML/avsx_83xx/Content/home.htm", "/techdocs/AOS-CX/10.16/HTML/vsx_83xx/Content/home.htm",
			"/techdocs/AOS-CX/10.16/PDF/acopp_83xx.pdf", "/techdocs/AOS-CX/10.16/PDF/avsx_83xx.pdf",
			"/techdocs/AOS-CX/10.16/HTML/web_ui_83xx/Content/home.htm":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected release PDF fixture request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestUnservedGuideUsesTheReleasePDFItsTitleIdentifies(t *testing.T) {
	server := releasePDFFixtureServer(t)
	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "8325", "--release", "10.16",
		"--all", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "raw-v2"),
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--workers", "1", "--json",
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, newNativeClient)
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	manifest := output.Manifest
	pdf := server.URL + "/techdocs/AOS-CX/10.16/PDF/copp_83xx.pdf"
	if manifest.Status != "complete" || len(manifest.Guides) != 2 || manifest.Guides[0].ID != "copp" ||
		manifest.Guides[0].Format != "pdf" || manifest.Guides[0].SourceURL != pdf ||
		manifest.Guides[0].RouteOrigin != source.RouteOriginPeerReleasePDF {
		t.Fatalf("CoPP was not archived from its peers' release PDF: %+v", manifest.Guides)
	}
	if web := manifest.Guides[1]; web.ID != "webUI" || web.Format != "pdf" ||
		web.SourceURL != server.URL+"/techdocs/AOS-CX/10.16/PDF/web_ui_83xx.pdf" ||
		web.RouteOrigin != source.RouteOriginReleasePDF {
		t.Fatalf("Web UI was not archived from its own release PDF: %+v", web)
	}
	if len(manifest.SkippedUnavailable) != 1 || manifest.SkippedUnavailable[0].ID != "vsx" {
		t.Fatalf("VSX, whose release PDF names another release, was not skipped: %+v", manifest.SkippedUnavailable)
	}
	if !slices.ContainsFunc(manifest.CatalogueWarnings, func(message string) bool {
		return strings.Contains(message, "acopp_83xx/Content/home.htm") && strings.Contains(message, pdf) &&
			strings.Contains(message, "from the mapping of 8320") &&
			strings.Contains(message, `"AOS-CX 10.16.xxxx CoPP Guide (8320, 8325 Switch Series)"`)
	}) {
		t.Fatalf("manifest does not record the release PDF: %q", manifest.CatalogueWarnings)
	}
	if text := stderr.String(); !strings.Contains(text, "is not usable: publisher PDF is not identified as this guide: "+
		`publisher PDF titled "AOS-CX 10.15.xxxx VSX Guide (8320, 8325)" does not name "10.16"`) {
		t.Fatalf("diagnostics:\n%s", text)
	}
}

func TestGuideSourceArchivesTheNamedPublisherDocument(t *testing.T) {
	portal := testutil.Portal{Platforms: []string{"6100"}, Versions: []string{"10.18"},
		Guides: []testutil.PortalGuide{{ID: "acls", Title: "ACLs Guide"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if portal.Serve(w, r, "/portal/aoscx.html") {
			return
		}
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/json/aoscx/acls.json":
			io.WriteString(w, `{"10.18":{"6100":"ACLs_unserved"}}`)
		case "/techdocs/elsewhere/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body><nav class="wh_publication_toc"><ul>
<li><a href="topic.html">Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>ACLs</h1></main></body></html>`)
		case "/techdocs/elsewhere/topic.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<article><h1>Topic</h1></article>`)
		default:
			t.Errorf("unexpected guide source fixture request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	base := []string{
		"--transport", "http", "--platform", "6100", "--release", "10.18", "--all",
		"--destination", filepath.Join(root, "library"), "--raw-cache", filepath.Join(root, "raw-v2"),
		"--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0", "--retries", "0", "--workers", "1", "--json",
	}
	named := server.URL + "/techdocs/elsewhere/index.html"
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append(base, "--guide-source", "acls="+named), &stdout, &stderr, newNativeClient)
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	manifest := output.Manifest
	if manifest.Status != "complete" || len(manifest.Guides) != 1 || manifest.Guides[0].SourceURL != named ||
		manifest.Guides[0].RouteOrigin != source.RouteOriginUserSupplied || len(manifest.SkippedUnavailable) != 0 {
		t.Fatalf("guide was not archived from the named document: %+v", manifest)
	}
	if !slices.ContainsFunc(manifest.CatalogueWarnings, func(message string) bool {
		return strings.Contains(message, "/techdocs/AOS-CX/10.18.xxxx/HTML/ACLs_unserved/index.html") &&
			strings.Contains(message, named) && strings.Contains(message, "--guide-source")
	}) {
		t.Fatalf("manifest does not record the replaced mapping: %q", manifest.CatalogueWarnings)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--guide-source", "acls"}, "must be ID=URL"},
		{[]string{"--guide-source", "vsx=" + named}, "does not list"},
		{[]string{"--guide-source", "acls=https://mirror.example/acls.pdf"}, "must be a document on"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(context.Background(), append(slices.Clone(base), tc.args...), &stdout, &stderr, newNativeClient); code != 1 ||
			!strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%v: exit=%d stderr=%s", tc.args, code, &stderr)
		}
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"--list", "--platform", "6100", "--release", "10.18",
		"--guide-source", "acls=" + named, "--portal-url", server.URL + "/portal/aoscx.html"}, &stdout, &stderr, newNativeClient); code != 1 ||
		!strings.Contains(stderr.String(), "cannot be combined with --list") {
		t.Errorf("--list with --guide-source: exit=%d stderr=%s", code, &stderr)
	}
}

func TestDownloadSummarySaysWhichGuidesArePresentAndWhatIsMissing(t *testing.T) {
	server := peerMappingFixtureServer(t)
	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "8325", "--release", "10.18",
		"--all", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "raw-v2"),
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--workers", "1",
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, newNativeClient); code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	want := "Status: complete\n" +
		"Guides: 1 of 2 selected guides is present (1 complete); 1 missing.\n" +
		"Missing:\n" +
		"  VSX Guide: not available from the publisher (mapped source HTTP 404 at " + server.URL + "/avsx/index.html)\n"
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("summary missing from output:\n%s\nwant:\n%s", &stdout, want)
	}
}
