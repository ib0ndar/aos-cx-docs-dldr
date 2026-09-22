package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/testutil"
)

type hpeHTTPFixture struct {
	contentType string
	body        []byte
}

type hpeRoundTripper map[string]hpeHTTPFixture

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackedRoundTripper struct {
	delegate http.RoundTripper
	mu       sync.Mutex
	calls    []string
}

func (t *trackedRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.calls = append(t.calls, request.URL.String())
	t.mu.Unlock()
	return t.delegate.RoundTrip(request)
}

func (t *trackedRoundTripper) count(raw string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, call := range t.calls {
		if call == raw {
			count++
		}
	}
	return count
}

func (f hpeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	trace := httptrace.ContextClientTrace(request.Context())
	if trace != nil && trace.GotConn != nil {
		trace.GotConn(httptrace.GotConnInfo{})
	}
	if trace != nil && trace.WroteHeaders != nil {
		trace.WroteHeaders()
	}
	if request.URL.Path == "/robots.txt" {
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{},
			Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
	}
	fixture, ok := f[request.URL.String()]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{},
			Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {fixture.contentType}},
		Body: io.NopCloser(bytes.NewReader(fixture.body)), Request: request}, nil
}

type hpeCLIFetcher map[string]model.Resource

func (f hpeCLIFetcher) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	resource, ok := f[raw]
	if !ok {
		return model.Resource{}, errors.New("missing HPE CLI fixture " + raw)
	}
	body, err := io.ReadAll(resource.Body)
	if err != nil {
		return model.Resource{}, err
	}
	resource.Body.Close()
	f[raw] = model.Resource{URL: resource.URL, Status: resource.Status, Headers: resource.Headers.Clone(),
		Body: io.NopCloser(bytes.NewReader(body))}
	resource.Body = io.NopCloser(bytes.NewReader(body))
	return resource, nil
}

func TestHPEPlannerArchiveAndLibraryOrchestration(t *testing.T) {
	id := "doc-A"
	public := "https://support.hpe.com/hpesc/public/docDisplay?docId=" + id
	api := "https://support.hpe.com/hpesc/public/api/document/" + id
	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18.xxxx"}, SourceURL: "fixture",
		Guides: []model.Guide{{ID: "hpe", Title: "HPE Guide",
			Mappings: map[string]map[string]string{"10.18.xxxx": {"6300": public}}}},
	}

	resource := func(raw, contentType, body string) model.Resource {
		return model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {contentType}},
			Body: io.NopCloser(strings.NewReader(body))}
	}
	fetcher := hpeCLIFetcher{
		api + "?ignorePayload=true": resource(api+"?ignorePayload=true", "multiPage;charset=UTF-8",
			`<main class="ditasrc"><h1>HPE Guide</h1></main>`),
		api + "?page=content.json": resource(api+"?page=content.json", "multiPage",
			`[{"topicName":"Topic","topicLink":"GUID-topic.html","children":null}]`),
		api + "?page=GUID-topic.html": resource(api+"?page=GUID-topic.html", "multiPage;charset=UTF-8",
			`<main class="ditasrc"><article><h1 id="topic">Topic</h1><table><tr><td>Value</td></tr></table></article></main>`),
		"https://support.hpe.com/resource3/doc-resources/css/hpesc-doc.css": resource(
			"https://support.hpe.com/resource3/doc-resources/css/hpesc-doc.css", "text/css", `.ditasrc{color:#111}`),
		"https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Regular-Web.woff2": {
			URL: "https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Regular-Web.woff2", Status: 200,
			Headers: http.Header{"Content-Type": {"font/woff2"}}, Body: io.NopCloser(bytes.NewReader([]byte("wOF2regular")))},
		"https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Bold-Web.woff2": {
			URL: "https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Bold-Web.woff2", Status: 200,
			Headers: http.Header{"Content-Type": {"font/woff2"}}, Body: io.NopCloser(bytes.NewReader([]byte("wOF2bold")))},
	}
	options := options{platform: "6300", version: "10.18.xxxx", guides: []string{"hpe"},
		destination: t.TempDir(), maxMB: 2, maxArchiveMB: 8, json: true}
	var stdout, stderr bytes.Buffer
	code, err := downloadGuides(context.Background(), options, catalog, fetcher, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("HPE orchestration failed: code=%d err=%v stdout=%s stderr=%s", code, err, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	guide := output.Manifest.HTMLGuides["hpe"]
	if output.Manifest.Status != "complete" || guide.Document.Kind != "hpe" ||
		len(guide.HTML.Topics) != 2 || len(guide.HTML.Assets) != 3 ||
		guide.HTML.Inventory == nil || !guide.HTML.Inventory.Complete {
		t.Fatalf("HPE planner/archive/library output incomplete: %+v", output)
	}
}

func TestCLIHPEAdvertisedSameDocumentPDF(t *testing.T) {
	const (
		portal = "https://portal.example.test/aoscx.html"
		id     = "doc-A"
	)
	public := "https://support.hpe.com/hpesc/public/docDisplay?docId=" + id
	api := "https://support.hpe.com/hpesc/public/api/document/" + id
	pdfURL := api + "/original.pdf?version=1"
	body := testutil.PDF("HPE advertised")
	sum := sha256.Sum256(body)
	fixtures := hpeRoundTripper{
		portal: {"text/html", []byte(`<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="hpe" onclick="openFile('HTML','hpe','aoscx')">HPE PDF Guide</td>
</tr></table></div></html>`)},
		"https://portal.example.test/json/aoscx/hpe.json": {
			"application/json", []byte(`{"10.18.xxxx":{"6300":"` + public + `"}}`),
		},
		api + "?ignorePayload=true": {
			"text/html", []byte(`<html><body><embed type="application/pdf" src="` + pdfURL + `"></body></html>`),
		},
		pdfURL: {"application/pdf", body},
	}

	tracker := &trackedRoundTripper{delegate: fixtures}
	clientFactory := func(_ string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
		return fetch.NewWithRoundTripper(config, notify, tracker)
	}
	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "6300", "--version", "10.18.xxxx",
		"--guides", "hpe", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "cache"), "--portal-url", portal,
		"--delay", "0", "--retries", "0", "--workers", "1", "--json",
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, clientFactory); code != 0 {
		t.Fatalf("HPE advertised PDF exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("stdout is not isolated JSON: %v\n%s", err, &stdout)
	}
	guide := output.Manifest.PDFGuides["hpe"]
	if output.Manifest.Status != "complete" || len(output.Manifest.HTMLGuides) != 0 ||
		guide.Status != "complete" || guide.Document.Kind != "pdf" ||
		guide.Document.URL != public || guide.Document.RouteOrigin != "publisher-url" ||
		guide.PDF == nil || guide.PDF.URL != pdfURL || guide.PDF.FinalURL != pdfURL ||
		!guide.PDF.SourceVerified || guide.PDF.SourceSHA256 != hex.EncodeToString(sum[:]) ||
		guide.PDF.Size != int64(len(body)) {
		t.Fatalf("HPE advertised PDF provenance incomplete: %+v", output.Manifest)
	}
	evidence := false
	for _, input := range guide.PDF.Inputs {
		if input.Role == "pdf" && input.RequestedURL == pdfURL && input.FinalURL == pdfURL &&
			input.SHA256 == guide.PDF.SourceSHA256 && input.Size == len(body) {
			evidence = true
		}
	}
	if !evidence {
		t.Fatalf("HPE PDF source evidence missing: %+v", guide.PDF.Inputs)
	}
	entries, err := os.ReadDir(filepath.Join(output.Library, "hpe"))
	if err != nil || len(entries) != 1 || entries[0].Name() != guide.PDF.Path {
		t.Fatalf("HPE publisher PDF is not a one-file guide: %v err=%v", entries, err)
	}
	saved, err := os.ReadFile(filepath.Join(output.Library, "hpe", guide.PDF.Path))
	if err != nil || !bytes.Equal(saved, body) {
		t.Fatal("HPE advertised publisher bytes changed")
	}
	if tracker.count(pdfURL) != 1 {
		t.Fatalf("verified HPE PDF was unexpectedly retransmitted: calls=%v", tracker.calls)
	}
}

func hpePreferenceCLIFixtures(portal, id string, exportBody []byte) hpeRoundTripper {
	public := "https://support.hpe.com/hpesc/public/docDisplay?docId=" + id
	api := "https://support.hpe.com/hpesc/public/api/document/" + id
	fixtures := hpeRoundTripper{
		portal: {"text/html", []byte(`<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="hpe" onclick="openFile('HTML','hpe','aoscx')">HPE Guide</td>
</tr></table></div></html>`)},
		"https://portal.example.test/json/aoscx/hpe.json": {
			"application/json", []byte(`{"10.18.xxxx":{"6300":"` + public + `"}}`),
		},
		api + "?ignorePayload=true": {
			"multiPage;charset=UTF-8",
			[]byte(`<main class="ditasrc"><article><h1>HPE Guide</h1></article></main>`),
		},
		api + "?page=content.json": {
			"multiPage",
			[]byte(`[{"topicName":"Topic","topicLink":"GUID-topic.html","children":null}]`),
		},
		api + "?page=GUID-topic.html": {
			"multiPage;charset=UTF-8",
			[]byte(`<main class="ditasrc"><article><h1 id="topic">Topic</h1><p>Complete body.</p></article></main>`),
		},
		"https://support.hpe.com/resource3/doc-resources/css/hpesc-doc.css": {
			"text/css", []byte(`.ditasrc{color:#111}`),
		},
		"https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Regular-Web.woff2": {
			"font/woff2", []byte("wOF2regular"),
		},
		"https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Bold-Web.woff2": {
			"font/woff2", []byte("wOF2bold"),
		},
	}
	if exportBody != nil {
		fixtures[api+"/exportpdf?exportType=all"] = hpeHTTPFixture{
			contentType: "application/pdf",
			body:        exportBody,
		}
	}
	return fixtures
}

func TestCLIHPEWholeDocumentPreferenceAndHTMLFallback(t *testing.T) {
	const (
		portal = "https://portal.example.test/aoscx.html"
		id     = "doc-preference"
	)
	api := "https://support.hpe.com/hpesc/public/api/document/" + id
	export := api + "/exportpdf?exportType=all"
	baseArgs := func(root string) []string {
		return []string{
			"--transport", "http", "--platform", "6300", "--version", "10.18.xxxx",
			"--guides", "hpe", "--destination", filepath.Join(root, "library"),
			"--raw-cache", filepath.Join(root, "cache"), "--portal-url", portal,
			"--delay", "0", "--retries", "0", "--workers", "1", "--json",
		}
	}
	runCase := func(t *testing.T, fixtures hpeRoundTripper, extra ...string) (DownloadOutput, *trackedRoundTripper) {
		t.Helper()
		tracker := &trackedRoundTripper{delegate: fixtures}
		factory := func(_ string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
			return fetch.NewWithRoundTripper(config, notify, tracker)
		}
		args := append(baseArgs(t.TempDir()), extra...)
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr, factory); code != 0 {
			t.Fatalf("preference case exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
		}
		var output DownloadOutput
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatalf("stdout is not isolated JSON: %v\n%s", err, &stdout)
		}
		return output, tracker
	}

	t.Run("preferred-export", func(t *testing.T) {
		body := testutil.PDF("HPE export all")
		output, tracker := runCase(t, hpePreferenceCLIFixtures(portal, id, body),
			"--prefer-pdf", "--convert-html-to-pdf", "--chrome-path", filepath.Join(t.TempDir(), "must-not-be-used"), "--zip")
		guide := output.Manifest.PDFGuides["hpe"]
		if output.Manifest.Status != "complete" || len(output.Manifest.HTMLGuides) != 0 ||
			guide.PDF == nil || guide.PDF.Origin != "hpe-whole-document-export" ||
			!guide.PDF.SourceVerified || guide.PDF.URL != export || guide.PDF.FinalURL != export {
			t.Fatalf("whole-document preference provenance missing: %+v", output.Manifest)
		}
		saved, err := os.ReadFile(filepath.Join(output.Library, "hpe", guide.PDF.Path))
		if err != nil || !bytes.Equal(saved, body) {
			t.Fatal("whole-document publisher PDF bytes changed")
		}
		entries, err := os.ReadDir(filepath.Join(output.Library, "hpe"))
		if err != nil || len(entries) != 1 || entries[0].Name() != guide.PDF.Path {
			t.Fatalf("preferred PDF guide is not one file: %v err=%v", entries, err)
		}
		if tracker.count(export) != 2 {
			t.Fatalf("preferred export did not use one HEAD and one body request: %v", tracker.calls)
		}
		if output.ZIP == "" {
			t.Fatal("preferred publisher PDF library omitted requested ZIP")
		}
		if _, err := os.Stat(output.ZIP); err != nil {
			t.Fatalf("preferred publisher PDF ZIP missing: %v", err)
		}
		search, err := os.ReadFile(filepath.Join(output.Library, "search-index.js"))
		if err != nil || !bytes.Contains(search, []byte("HPE Guide")) ||
			bytes.Contains(search, []byte("HPE export all")) {
			t.Fatalf("PDF search was not title-only: %s err=%v", search, err)
		}
	})

	t.Run("invalid-export-falls-back", func(t *testing.T) {
		output, tracker := runCase(t,
			hpePreferenceCLIFixtures(portal, id, []byte("<html>not a PDF</html>")),
			"--prefer-pdf",
		)
		guide := output.Manifest.HTMLGuides["hpe"]
		if output.Manifest.Status != "complete" || guide.Status != "complete" ||
			len(output.Manifest.PDFGuides) != 0 ||
			!strings.Contains(strings.Join(guide.Warnings, "\n"), "archived complete HTML instead") {
			t.Fatalf("invalid preferred PDF did not retain complete HTML: %+v", output.Manifest)
		}
		if tracker.count(export) != 2 {
			t.Fatalf("invalid exact export request count changed: %v", tracker.calls)
		}
	})

	t.Run("explicit-no-preference", func(t *testing.T) {
		output, tracker := runCase(t,
			hpePreferenceCLIFixtures(portal, id, testutil.PDF("unused export")),
			"--no-prefer-pdf",
		)
		if output.Manifest.Status != "complete" || len(output.Manifest.HTMLGuides) != 1 ||
			len(output.Manifest.PDFGuides) != 0 || tracker.count(export) != 0 {
			t.Fatalf("--no-prefer-pdf did not freeze HTML preference: manifest=%+v calls=%v", output.Manifest, tracker.calls)
		}
	})
}

func TestFailedPreferredPDFAndHTMLUpdateRetainsPreviousComplete(t *testing.T) {
	const (
		portal = "https://portal.example.test/aoscx.html"
		id     = "doc-retained"
	)
	api := "https://support.hpe.com/hpesc/public/api/document/" + id
	export := api + "/exportpdf?exportType=all"
	fixtures := hpePreferenceCLIFixtures(portal, id, testutil.PDF("retained preferred PDF"))
	tracker := &trackedRoundTripper{delegate: fixtures}
	factory := func(_ string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
		return fetch.NewWithRoundTripper(config, notify, tracker)
	}
	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "6300", "--version", "10.18.xxxx",
		"--guides", "hpe", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "cache"), "--portal-url", portal,
		"--delay", "0", "--retries", "0", "--workers", "1", "--json", "--prefer-pdf",
	}
	runOnce := func(arguments []string) (int, DownloadOutput, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), arguments, &stdout, &stderr, factory)
		var output DownloadOutput
		if len(stdout.Bytes()) > 0 {
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("stdout is not isolated JSON: %v\n%s", err, &stdout)
			}
		}
		return code, output, stderr.String()
	}
	code, first, stderr := runOnce(args)
	if code != 0 {
		t.Fatalf("initial preferred PDF failed: %d %s", code, stderr)
	}
	original := first.Manifest.PDFGuides["hpe"].PDF.SHA256

	fixtures[export] = hpeHTTPFixture{"application/pdf", []byte("<html>invalid export</html>")}
	delete(fixtures, api+"?page=content.json")
	code, second, stderr := runOnce(append(append([]string{}, args...), "--refresh"))
	if code != 2 {
		t.Fatalf("failed preference/HTML update exit=%d stderr=%s", code, stderr)
	}
	retained := second.Manifest.PDFGuides["hpe"]
	if second.Manifest.Status != "incomplete" || retained.PDF == nil || retained.PDF.SHA256 != original ||
		len(second.Manifest.Attempts) != 1 || !second.Manifest.Attempts[0].RetainedPrevious ||
		len(second.Manifest.HTMLGuides) != 0 {
		t.Fatalf("failed preference/HTML update replaced prior complete guide: %+v", second.Manifest)
	}
}

func TestCLIPreferredPDFChangedAfterVerificationFallsBackToHTML(t *testing.T) {
	const (
		portal = "https://portal.example.test/aoscx.html"
		id     = "doc-changing"
	)
	api := "https://support.hpe.com/hpesc/public/api/document/" + id
	export := api + "/exportpdf?exportType=all"
	fixtures := hpePreferenceCLIFixtures(portal, id, nil)
	var exportCalls atomic.Int32
	var exportGETs atomic.Int32
	delegate := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == export {
			exportCalls.Add(1)
			body := testutil.PDF("first verified bytes")
			if request.Method == http.MethodGet && exportGETs.Add(1) > 1 {
				body = testutil.PDF("changed publication bytes")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/pdf"}},
				Body:       io.NopCloser(bytes.NewReader(body)),
				Request:    request,
			}, nil
		}
		return fixtures.RoundTrip(request)
	})
	tracker := &trackedRoundTripper{delegate: delegate}
	factory := func(_ string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
		return fetch.NewWithRoundTripper(config, notify, tracker)
	}
	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "6300", "--version", "10.18.xxxx",
		"--guides", "hpe", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "cache"), "--portal-url", portal,
		"--delay", "0", "--retries", "0", "--workers", "1", "--json",
		"--prefer-pdf", "--refresh",
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, factory); code != 0 {
		t.Fatalf("changed-PDF fallback exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	guide := output.Manifest.HTMLGuides["hpe"]
	if output.Manifest.Status != "complete" || guide.Status != "complete" ||
		len(output.Manifest.PDFGuides) != 0 || exportCalls.Load() != 3 ||
		guide.HTML == nil || guide.HTML.GeneratedPDF != nil ||
		!strings.Contains(strings.Join(guide.Warnings, "\n"), "failed after verification") {
		t.Fatalf("changed PDF did not fall back to complete HTML: manifest=%+v calls=%d", output.Manifest, exportCalls.Load())
	}
	if _, err := os.Stat(filepath.Join(output.Library, "hpe", ".download.part")); !os.IsNotExist(err) {
		t.Fatalf("failed preferred PDF staging file leaked: %v", err)
	}
}

func TestCLIHPEAdvertisedPDFForeignIdentityFailsClosed(t *testing.T) {
	const (
		portal = "https://portal.example.test/aoscx.html"
		id     = "doc-A"
	)
	public := "https://support.hpe.com/hpesc/public/docDisplay?docId=" + id
	api := "https://support.hpe.com/hpesc/public/api/document/" + id
	for _, tc := range []struct {
		name, advertised, final string
		expectPDFRequest        bool
	}{
		{"foreign-advertisement", "https://support.hpe.com/hpesc/public/api/document/doc-B/original.pdf", "", false},
		{"foreign-final-url", api + "/original.pdf", "https://support.hpe.com/hpesc/public/api/document/doc-B/original.pdf", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtures := hpeRoundTripper{
				portal: {"text/html", []byte(`<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="hpe" onclick="openFile('HTML','hpe','aoscx')">HPE PDF Guide</td>
</tr></table></div></html>`)},
				"https://portal.example.test/json/aoscx/hpe.json": {
					"application/json", []byte(`{"10.18.xxxx":{"6300":"` + public + `"}}`),
				},
				api + "?ignorePayload=true": {
					"text/html", []byte(`<html><body><embed type="application/pdf" src="` + tc.advertised + `"></body></html>`),
				},
			}
			if tc.expectPDFRequest {
				fixtures[tc.advertised] = hpeHTTPFixture{"application/pdf", testutil.PDF("wrong final")}
			}
			tracker := &trackedRoundTripper{delegate: http.RoundTripper(fixtures)}
			if tc.expectPDFRequest {
				tracker.delegate = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					response, err := fixtures.RoundTrip(request)
					if request.URL.String() == tc.advertised {
						response.StatusCode = http.StatusFound
						response.Header.Set("Location", tc.final)
						response.Body = io.NopCloser(bytes.NewReader(nil))
					}
					return response, err
				})
			}
			clientFactory := func(_ string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
				return fetch.NewWithRoundTripper(config, notify, tracker)
			}
			root := t.TempDir()
			args := []string{
				"--transport", "http", "--platform", "6300", "--version", "10.18.xxxx",
				"--guides", "hpe", "--destination", filepath.Join(root, "library"),
				"--raw-cache", filepath.Join(root, "cache"), "--portal-url", portal,
				"--delay", "0", "--retries", "0", "--workers", "1", "--json",
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr, clientFactory); code != 2 {
				t.Fatalf("wrong HPE identity exit=%d, want 2; stdout=%s stderr=%s", code, &stdout, &stderr)
			}
			var output DownloadOutput
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("incomplete stdout is not JSON: %v\n%s", err, &stdout)
			}
			if output.Manifest.Status != "incomplete" || len(output.Manifest.PDFGuides) != 0 ||
				len(output.Manifest.HTMLGuides) != 0 || len(output.Manifest.Attempts) != 1 ||
				output.Manifest.Attempts[0].Status != "failed" ||
				output.Manifest.Attempts[0].Format != "html" {
				t.Fatalf("wrong HPE identity became a complete guide: %+v", output.Manifest)
			}
			errors := strings.Join(output.Manifest.Attempts[0].Errors, " ")
			if !strings.Contains(errors, "known HPE document") &&
				!strings.Contains(errors, "selected document scope") &&
				!strings.Contains(errors, "not a landing/error page") {
				t.Fatalf("wrong HPE identity failure lost exact reason: %s", errors)
			}
			want := 0
			if tc.expectPDFRequest {
				want = 1
			}
			if tracker.count(tc.advertised) != want {
				t.Fatalf("wrong HPE identity request count=%d, want %d: %v", tracker.count(tc.advertised), want, tracker.calls)
			}
		})
	}
}

func TestHPETransportBudgetCoversPlannerTopicsStylesAndFonts(t *testing.T) {
	id := "doc-A"
	public := "https://support.hpe.com/hpesc/public/docDisplay?docId=" + id
	api := "https://support.hpe.com/hpesc/public/api/document/" + id
	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18.xxxx"}, SourceURL: "fixture",
		Guides: []model.Guide{{ID: "hpe", Title: "HPE Guide",
			Mappings: map[string]map[string]string{"10.18.xxxx": {"6300": public}}}},
	}
	roundTripper := hpeRoundTripper{
		api + "?ignorePayload=true":                                                      {"multiPage;charset=UTF-8", []byte(`<main class="ditasrc"><h1>HPE Guide</h1></main>`)},
		api + "?page=content.json":                                                       {"multiPage", []byte(`[{"topicName":"Topic","topicLink":"GUID-topic.html","children":null}]`)},
		api + "?page=GUID-topic.html":                                                    {"multiPage;charset=UTF-8", []byte(`<main class="ditasrc"><article><h1>Topic</h1></article></main>`)},
		"https://support.hpe.com/resource3/doc-resources/css/hpesc-doc.css":              {"text/css", []byte(`.ditasrc{color:#111}`)},
		"https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Regular-Web.woff2": {"font/woff2", []byte("wOF2regular")},
		"https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Bold-Web.woff2":    {"font/woff2", []byte("wOF2bold")},
	}
	budget, err := fetch.NewAttemptBudget(8, 1)
	if err != nil {
		t.Fatal(err)
	}
	client, err := fetch.NewWithRoundTripper(fetch.Config{Timeout: time.Second, MaxBytes: 2 << 20, Attempts: budget},
		func(string) {}, roundTripper)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := cache.Open(t.TempDir(), client, 2<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	options := options{platform: "6300", version: "10.18.xxxx", guides: []string{"hpe"},
		destination: t.TempDir(), maxMB: 2, maxArchiveMB: 8, json: true}
	var stdout, stderr bytes.Buffer
	code, err := downloadGuides(context.Background(), options, catalog, store, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("budgeted HPE path failed: code=%d err=%v stderr=%s", code, err, &stderr)
	}
	stats := budget.Stats()
	if stats.AttemptedTransmissions != 8 || stats.ObservedHeaderWrites != 8 ||
		stats.AdmittedRequests != 8 || stats.ReservedAttempts != 0 || stats.Exhausted {
		t.Fatalf("HPE planner/topic/style/font requests escaped shared budget: %+v", stats)
	}
}
