package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/testutil"
)

type mixedRoundTripper struct {
	mu       sync.Mutex
	fixtures map[string]hpeHTTPFixture
	calls    map[string]int
}

func (m *mixedRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	trace := httptrace.ContextClientTrace(request.Context())
	if trace != nil && trace.GotConn != nil {
		trace.GotConn(httptrace.GotConnInfo{})
	}
	if trace != nil && trace.WroteHeaders != nil {
		trace.WroteHeaders()
	}
	m.mu.Lock()
	m.calls[request.URL.String()]++
	m.mu.Unlock()
	if request.URL.Path == "/robots.txt" {
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{},
			Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
	}
	fixture, ok := m.fixtures[request.URL.String()]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{},
			Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {fixture.contentType}},
		Body: io.NopCloser(bytes.NewReader(fixture.body)), Request: request}, nil
}

func TestAllMappedGuidesMixedFormatsThroughActualCLI(t *testing.T) {
	const (
		origin = "https://fixture.test"
		portal = origin + "/portal/aoscx.html"
		hpeID  = "doc-A"
	)
	hpePublic := "https://support.hpe.com/hpesc/public/docDisplay?docId=" + hpeID
	hpeAPI := "https://support.hpe.com/hpesc/public/api/document/" + hpeID
	fixtures := map[string]hpeHTTPFixture{}
	set := func(raw, mime, body string) {
		fixtures[raw] = hpeHTTPFixture{contentType: mime, body: []byte(body)}
	}
	set(portal, "text/html", `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="pdf" onclick="openFile('PDF','pdf','aoscx')">PDF Guide</td>
<td id="flare" onclick="openFile('HTML','flare','aoscx')">Flare Guide</td>
<td id="hpe" onclick="openFile('HTML','hpe','aoscx')">HPE Guide</td>
<td id="static" onclick="openFile('HTML','static','aoscx')">Static Guide</td>
</tr></table></div></html>`)
	mapping := func(id, target string) {
		body, err := json.Marshal(map[string]any{"10.18.xxxx": map[string]string{"6300": target}})
		if err != nil {
			t.Fatal(err)
		}
		set(origin+"/portal/json/aoscx/"+id+".json", "application/json", string(body))
	}
	mapping("pdf", origin+"/guide.pdf")
	mapping("flare", origin+"/flare/Content/home.htm")
	mapping("hpe", hpePublic)
	mapping("static", origin+"/static/index.html")
	fixtures[origin+"/guide.pdf"] = hpeHTTPFixture{"application/pdf", testutil.PDF("mapped-original")}

	set(origin+"/flare/Content/home.htm", "text/html",
		`<html data-mc-path-to-help-system="../"><body><a href="contents.htm">Table of Contents</a><div id="mc-main-content"><h1>Flare Home</h1></div></body></html>`)
	set(origin+"/flare/Content/contents.htm", "text/html",
		`<html data-mc-path-to-help-system="../"><body><ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul><div id="mc-main-content"><h1>Contents</h1></div></body></html>`)
	set(origin+"/flare/Data/Tocs/Guide.js", "application/javascript",
		`define({numchunks:1,prefix:'Guide_Chunk',tree:{n:[{i:0,c:0},{i:1,c:0},{i:2,c:0}]}});`)
	set(origin+"/flare/Data/Tocs/Guide_Chunk0.js", "application/javascript",
		`define({'/Content/home.htm':{i:[0],t:['Home'],b:['']},'/Content/contents.htm':{i:[1],t:['Contents'],b:['']},'/Content/topic.htm':{i:[2],t:['Topic'],b:['']}});`)
	set(origin+"/flare/Content/topic.htm", "text/html",
		`<html><body><div id="mc-main-content"><h1>Flare Topic</h1><pre>show  vlan</pre></div></body></html>`)

	set(hpeAPI+"?ignorePayload=true", "multiPage;charset=UTF-8",
		`<main class="ditasrc"><h1>HPE Guide</h1></main>`)
	set(hpeAPI+"?page=content.json", "multiPage",
		`[{"topicName":"Topic","topicLink":"GUID-topic.html","children":null}]`)
	set(hpeAPI+"?page=GUID-topic.html", "multiPage;charset=UTF-8",
		`<main class="ditasrc"><article><h1>HPE Topic</h1></article></main>`)
	set("https://support.hpe.com/resource3/doc-resources/css/hpesc-doc.css", "text/css", `.ditasrc{color:#111}`)
	fixtures["https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Regular-Web.woff2"] =
		hpeHTTPFixture{"font/woff2", []byte("wOF2regular")}
	fixtures["https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Bold-Web.woff2"] =
		hpeHTTPFixture{"font/woff2", []byte("wOF2bold")}

	set(origin+"/static/index.html", "text/html",
		`<html><body><nav class="wh_publication_toc"><ul><li><a href="topic.html">Static Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>Static Home</h1></main></body></html>`)
	set(origin+"/static/topic.html", "text/html",
		`<article><h1>Static Topic</h1><table><tr><td>preserved</td></tr></table></article>`)

	roundTripper := &mixedRoundTripper{fixtures: fixtures, calls: map[string]int{}}
	var selectedTransport string
	factory := func(transport string, config fetch.Config, warning func(string)) (*fetch.Client, error) {
		selectedTransport = transport
		return fetch.NewWithRoundTripper(config, warning, roundTripper)
	}
	root := t.TempDir()
	args := []string{
		"--platform", "6300", "--release", "10.18.xxxx", "--all",
		"--destination", filepath.Join(root, "library"), "--raw-cache", filepath.Join(root, "cache"),
		"--portal-url", portal, "--delay", "0", "--retries", "0", "--workers", "4", "--json",
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, factory)
	if code != 0 {
		t.Fatalf("mixed all failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if selectedTransport != "compatible" {
		t.Fatalf("default transport=%q, want compatible", selectedTransport)
	}
	var result DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout was not clean JSON: %v\n%s", err, stdout.String())
	}
	wantOrder := []string{"pdf", "flare", "hpe", "static"}
	if len(result.Manifest.Guides) != len(wantOrder) || len(result.Manifest.Attempts) != len(wantOrder) {
		t.Fatalf("all did not publish four guides: %+v", result.Manifest)
	}
	for index, want := range wantOrder {
		if result.Manifest.Attempts[index].ID != want {
			t.Fatalf("catalogue processing order changed: %+v", result.Manifest.Attempts)
		}
	}
	if result.Manifest.Status != "complete" || result.Manifest.PDFGuides["pdf"].Status != "complete" ||
		result.Manifest.HTMLGuides["flare"].Document.Kind != "flare" ||
		result.Manifest.HTMLGuides["hpe"].Document.Kind != "hpe" ||
		result.Manifest.HTMLGuides["static"].Document.Kind != "static" {
		t.Fatalf("mixed format result incomplete: %+v", result.Manifest)
	}
	pdfPath := filepath.Join(result.Library, "pdf", "6300 - 10.18.xxxx - PDF Guide.pdf")
	pdfBytes, err := os.ReadFile(pdfPath)
	if err != nil || !bytes.Equal(pdfBytes, testutil.PDF("mapped-original")) {
		t.Fatalf("direct PDF bytes changed: %v", err)
	}
	for raw, count := range roundTripper.calls {
		if raw == "https://fixture.test/guide.pdf" {
			if count != 2 {
				t.Fatalf("direct PDF did not use one selection HEAD and one body request: %d", count)
			}
			continue
		}
		if count > 1 && !strings.HasSuffix(raw, "/robots.txt") {
			t.Fatalf("all workflow made a duplicate logical request for %s: %d", raw, count)
		}
	}

	prompts := &scriptedPrompts{}
	stdout.Reset()
	stderr.Reset()
	code = runWithPrompts(context.Background(), args, &stdout, &stderr, factory, prompts)
	if code != 0 || len(prompts.calls) != 0 {
		t.Fatalf("fully specified TTY-style invocation unexpectedly prompted: code=%d calls=%v stderr=%s",
			code, prompts.calls, stderr.String())
	}

	delete(roundTripper.fixtures, origin+"/static/topic.html")
	refreshArgs := append(append([]string{}, args...), "--refresh")
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), refreshArgs, &stdout, &stderr, factory)
	if code != 2 {
		t.Fatalf("failed all-guides refresh did not report incomplete: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	staticResult := result.Manifest.HTMLGuides["static"]
	var retained bool
	for _, attempt := range result.Manifest.Attempts {
		if attempt.ID == "static" {
			retained = attempt.RetainedPrevious
		}
	}
	if result.Manifest.Status != "incomplete" || staticResult.Status != "complete" || !retained {
		t.Fatalf("failed all-guides refresh did not retain prior complete guide: %+v", result.Manifest)
	}
}

func TestExplicitHTTPOverridesCompatibleDefault(t *testing.T) {
	var selected string
	factory := func(transport string, config fetch.Config, warning func(string)) (*fetch.Client, error) {
		selected = transport
		return nil, fmt.Errorf("factory stop")
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--platform", "6300", "--release", "10.16", "--guides", "guide", "--destination", t.TempDir(),
		"--transport", "http", "--timeout", "1", "--attempt-timeout", "1",
	}, &stdout, &stderr, factory)
	if code != 1 || selected != "http" || !strings.Contains(stderr.String(), "factory stop") {
		t.Fatalf("explicit HTTP override not preserved: code=%d transport=%q stderr=%s", code, selected, stderr.String())
	}
}

func TestAllGuidesConflictFailsBeforeClientOrOutput(t *testing.T) {
	called := false
	factory := func(string, fetch.Config, func(string)) (*fetch.Client, error) {
		called = true
		return nil, fmt.Errorf("unexpected client")
	}
	destination := filepath.Join(t.TempDir(), "not-created")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--platform", "6300", "--release", "10.16", "--guides", "guide", "--all",
		"--destination", destination,
	}, &stdout, &stderr, factory)
	if code != 1 || called || !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("conflict was not rejected before I/O: code=%d called=%t stderr=%s", code, called, stderr.String())
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("conflicting selection created destination")
	}
}
