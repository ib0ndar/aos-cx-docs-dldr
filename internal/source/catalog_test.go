package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

const portalFixture = `<html><select id="platform"><option value="">Choose</option>
<option value="6300">6300</option><option>6400</option><option disabled>old</option></select>
<select id="ver"><option>10.18.xxxx</option><option>10.17.1000</option><option>10.16</option><option>10.10</option></select>
<div id="menu1"><table><tr>
<td id="fundamentals" onclick="openFile('HTML', 'fundamentals', 'aoscx')">Fundamentals
Guide</td><td id="cli" onclick="openFile('HTML', 'cli', 'aoscx')">CLI (PDF)</td>
<td id="webUI" onclick="return openFile('HTML', 'webUI', 'aoscx');">Web UI</td>
</tr></table></div><div id="menu2"><table><tr>
<td id="release-notes" onclick="openFile('PDF', 'release-notes', 'aoscx')">Excluded</td>
</tr></table></div></html>`

type fixtureFetcher struct {
	mu        sync.Mutex
	resources map[string]string
	calls     []string
	refreshes []bool
}

func (f *fixtureFetcher) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	f.mu.Lock()
	f.calls = append(f.calls, raw)
	f.refreshes = append(f.refreshes, refresh)
	body, ok := f.resources[raw]
	f.mu.Unlock()
	if !ok {
		return model.Resource{}, fmt.Errorf("missing fixture %s", raw)
	}
	return model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body: io.NopCloser(strings.NewReader(body))}, nil
}

type concurrentCatalogFetcher struct {
	resources map[string]string
	started   chan string
	release   <-chan struct{}
	mu        sync.Mutex
	active    int
	max       int
}

func (f *concurrentCatalogFetcher) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	body, ok := f.resources[raw]
	if !ok {
		return model.Resource{}, fmt.Errorf("missing fixture %s", raw)
	}
	if !strings.Contains(raw, "/json/aoscx/") {
		return model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {"text/html"}},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	f.mu.Lock()
	f.active++
	if f.active > f.max {
		f.max = f.active
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	select {
	case f.started <- raw:
	case <-ctx.Done():
		return model.Resource{}, ctx.Err()
	}
	select {
	case <-f.release:
	case <-ctx.Done():
		return model.Resource{}, ctx.Err()
	}
	return model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestCatalogueMappingsLoadConcurrentlyInPublisherOrder(t *testing.T) {
	base := fixtures()
	release := make(chan struct{})
	f := &concurrentCatalogFetcher{resources: base.resources, started: make(chan string, 3), release: release}
	var progress []CatalogProgress
	type outcome struct {
		catalog model.Catalog
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		catalog, err := LoadCatalogWithWorkersObserved(
			context.Background(), f, "https://example.test/portal/aoscx.html", 3,
			func(event CatalogProgress) { progress = append(progress, event) },
		)
		done <- outcome{catalog: catalog, err: err}
	}()
	for range 3 {
		select {
		case <-f.started:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("mapping retrieval remained serialized")
		}
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if got := []string{result.catalog.Guides[0].ID, result.catalog.Guides[1].ID, result.catalog.Guides[2].ID}; strings.Join(got, ",") != "fundamentals,cli,webUI" {
		t.Fatalf("mapping completion changed publisher order: %v", got)
	}
	f.mu.Lock()
	maximum := f.max
	f.mu.Unlock()
	if maximum != 3 {
		t.Fatalf("mapping concurrency=%d, want 3", maximum)
	}
	want := []CatalogProgress{
		{Stage: CatalogPortalStarted},
		{Stage: CatalogPortalReady, Total: 3},
		{Stage: CatalogMappingCompleted, Completed: 1, Total: 3},
		{Stage: CatalogMappingCompleted, Completed: 2, Total: 3},
		{Stage: CatalogMappingCompleted, Completed: 3, Total: 3},
	}
	if fmt.Sprint(progress) != fmt.Sprint(want) {
		t.Fatalf("catalogue progress events=%v, want %v", progress, want)
	}
}

func TestCatalogueProgressCountsMappingFailuresAsCompleted(t *testing.T) {
	f := fixtures()
	delete(f.resources, "https://example.test/portal/json/aoscx/cli.json")
	var progress []CatalogProgress
	catalog, err := LoadCatalogWithWorkersObserved(
		context.Background(), f, "https://example.test/portal/aoscx.html", 2,
		func(event CatalogProgress) { progress = append(progress, event) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Warnings) != 1 {
		t.Fatalf("mapping failure was not retained: %+v", catalog)
	}
	if got := progress[len(progress)-1]; got.Stage != CatalogMappingCompleted ||
		got.Completed != 3 || got.Total != 3 {
		t.Fatalf("failed mapping was not counted as completed work: %v", progress)
	}
}

func TestCatalogueProgressStopsOnCancellation(t *testing.T) {
	base := fixtures()
	release := make(chan struct{})
	f := &concurrentCatalogFetcher{
		resources: base.resources, started: make(chan string, 3), release: release,
	}
	ctx, cancel := context.WithCancel(context.Background())
	var progress []CatalogProgress
	type outcome struct {
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		_, err := LoadCatalogWithWorkersObserved(
			ctx, f, "https://example.test/portal/aoscx.html", 3,
			func(event CatalogProgress) { progress = append(progress, event) },
		)
		done <- outcome{err: err}
	}()
	for range 3 {
		select {
		case <-f.started:
		case <-time.After(2 * time.Second):
			cancel()
			close(release)
			t.Fatal("mapping cancellation fixture did not start")
		}
	}
	cancel()
	result := <-done
	close(release)
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("catalogue cancellation error=%v", result.err)
	}
	if len(progress) != 2 ||
		progress[0].Stage != CatalogPortalStarted ||
		progress[1] != (CatalogProgress{Stage: CatalogPortalReady, Total: 3}) {
		t.Fatalf("catalogue cancellation emitted completion progress: %v", progress)
	}
}

func fixtures() *fixtureFetcher {
	return &fixtureFetcher{resources: map[string]string{
		"https://example.test/portal/aoscx.html":                   portalFixture,
		"https://example.test/portal/json/aoscx/fundamentals.json": `{"10.16":{"6300":"test_6300-6400","6400":"test_6300-6400"},"10.18.xxxx":{"6300":"sd00007909en_usen_us"},"10.08":{"6300":"archived"}}`,
		"https://example.test/portal/json/aoscx/cli.json":          `{"10.16":{"6300":"cli_6300"}}`,
		"https://example.test/portal/json/aoscx/webUI.json":        `{}`,
	}}
}

func TestFreshCatalogueAndSharedExactMappings(t *testing.T) {
	f := fixtures()
	c, err := LoadCatalog(context.Background(), f, "https://example.test/portal/aoscx.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Platforms, ",") != "6300,6400" || len(c.Guides) != 3 || c.Guides[2].ID != "webUI" ||
		len(c.Versions) != 4 || c.Guides[0].Title != "Fundamentals Guide" || len(f.calls) != 4 ||
		c.FetchedAt == "" || len(c.Warnings) != 0 {
		t.Fatalf("invalid catalogue: %+v calls=%v", c, f.calls)
	}
	for _, refreshed := range f.refreshes {
		if !refreshed {
			t.Error("catalogue request did not request refresh")
		}
	}
	a, _, err := ResolveDocuments(c, "6300", "10.16")
	b, _, err2 := ResolveDocuments(c, "6400", "10.16")
	if err != nil || err2 != nil || len(a) != 2 || len(b) != 1 || a[0].URL != b[0].URL {
		t.Fatalf("shared mappings changed: %v %v errors=%v %v", a, b, err, err2)
	}
	if strings.Join(AvailableVersions(c, "6400"), ",") != "10.16" {
		t.Fatal("platform-specific availability is wrong")
	}
}

func TestMappingFailuresAreNotUnavailability(t *testing.T) {
	for _, bad := range []string{
		`{not json`, `[]`, `{"10.16":[]}`, `{"10.16":{"6300":null}}`, `{"10.16":{"6300":""}}`,
		`{"10.16":{"6300":" target "}}`, `{"10.16":{},"10.16":{}}`,
		`{"10.16":{"6300":"a","6300":"b"}}`, `{} {}`, `null`,
	} {
		t.Run(bad, func(t *testing.T) {
			f := fixtures()
			f.resources["https://example.test/portal/json/aoscx/cli.json"] = bad
			c, err := LoadCatalog(context.Background(), f, "https://example.test/portal/aoscx.html")
			if err != nil {
				t.Fatal(err)
			}
			docs, diagnostics, err := ResolveDocuments(c, "6300", "10.16")
			if err != nil || len(docs) != 1 || len(diagnostics) != 1 || c.Guides[1].Error == "" || len(c.Warnings) != 1 {
				t.Fatalf("mapping error hidden: %+v %v %v", c, diagnostics, err)
			}
		})
	}
}

func TestHandlerParsingNeverExecutesOrGuesses(t *testing.T) {
	for _, bad := range []string{
		`openFile('HTML', 'fundamentals', 'aoscx'); alert(1)`,
		`openFile('HTML', '../private', 'aoscx')`, `openFile('HTML', 'different', 'aoscx')`, `arbitraryFunction()`,
	} {
		f := fixtures()
		f.resources["https://example.test/portal/aoscx.html"] = strings.Replace(portalFixture,
			"openFile('HTML', 'fundamentals', 'aoscx')", bad, 1)
		c, err := LoadCatalog(context.Background(), f, "https://example.test/portal/aoscx.html")
		if err != nil || c.Guides[0].Error == "" || len(f.calls) != 3 {
			t.Fatalf("unsafe handler processed: %q calls=%v error=%v", bad, f.calls, err)
		}
	}
}

func TestMalformedPortalIsFatal(t *testing.T) {
	for _, bad := range []string{
		"<html><h1>Sign in</h1></html>",
		strings.ReplaceAll(portalFixture, `id="ver"`, `id="unknown"`),
		strings.ReplaceAll(portalFixture, `id="menu1"`, `id="menu9"`),
		strings.Replace(portalFixture, "</html>", `<select id="platform"><option>6300</option></select></html>`, 1),
	} {
		f := fixtures()
		f.resources["https://example.test/portal/aoscx.html"] = bad
		if _, err := LoadCatalog(context.Background(), f, "https://example.test/portal/aoscx.html"); err == nil {
			t.Fatalf("invalid portal accepted: %s", bad)
		}
	}
}

func TestExactRoutingFromFrozenBaseline(t *testing.T) {
	for _, tc := range []struct {
		version, id, target, kind, suffix, origin string
	}{
		{"10.18.xxxx", "cli", "sd00001234en_us", "hpe", "docId=sd00001234en_us", "hpe-document-id"},
		{"10.17.1000", "fundamentals", "sd00001234en_us", "hpe", "docId=sd00001234en_us", "hpe-document-id"},
		{"10.18.xxxx", "fundamentals", "sd00007909en_usen_us", "hpe", "docId=sd00007909en_usen_us", "hpe-document-id"},
		{"10.18.xxxx", "fundamentals", "new_static", "static", "10.18.xxxx/HTML/new_static/index.html", "adapter-derived-book-stem"},
		{"10.17", "fundamentals", "fund_6300", "flare", "10.17/HTML/fund_6300/Content/home.htm", "adapter-derived-book-stem"},
		{"10.17.0.0", "fundamentals", "fund_6300", "flare", "10.17.0.0/HTML/fund_6300/Content/home.htm", "adapter-derived-book-stem"},
		{"10.13", "cli", "cli_6300", "pdf", "AOS-CX/10.13/PDF/cli_6300.pdf", "adapter-derived-book-stem"},
		{"10.12", "fundamentals", "fund_6300", "pdf", "Archived/AOS-CX/10.12/PDF/fund_6300.pdf", "adapter-derived-book-stem"},
		{"10.10", "cli", "cli_6300", "pdf", "Archived/AOS-CX/10.10/PDF/cli_6300.pdf", "adapter-derived-book-stem"},
		{"10.9", "fundamentals", "fund_6300", "pdf", "Archived/AOS-CX/10.9/PDF/fund_6300.pdf", "adapter-derived-book-stem"},
		{"10.16", "fundamentals", "https://publisher.test/some guide%20name.html", "static", "/some%20guide%20name.html", "publisher-url"},
	} {
		t.Run(tc.version+"-"+tc.target, func(t *testing.T) {
			d, err := resolve(model.Guide{ID: tc.id}, "6300", tc.version, tc.target)
			if err != nil || d.Kind != tc.kind || !strings.HasSuffix(d.URL, tc.suffix) ||
				d.Version != tc.version || d.RouteOrigin != tc.origin {
				t.Fatalf("route differs from frozen baseline: %+v %v", d, err)
			}
		})
	}
}

func TestPDFRouteOriginDoesNotClaimPayloadVerification(t *testing.T) {
	document, err := resolve(model.Guide{ID: "cli", Title: "CLI"}, "8320", "10.17", "cli_8320-8325")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PublisherPDFPlan(document)
	if err != nil {
		t.Fatal(err)
	}
	if document.Kind != "pdf" || document.RouteOrigin != "adapter-derived-book-stem" ||
		plan.PDFVerified || plan.PDFURL != document.URL {
		t.Fatalf("route provenance implied unavailable PDF verification: document=%+v plan=%+v", document, plan)
	}
	data := []byte(`{"id":"legacy","title":"Legacy","platform":"6300","version":"10.10","url":"https://example.test/a.pdf","kind":"pdf"}`)
	var legacy model.Document
	if err := json.Unmarshal(data, &legacy); err != nil || legacy.RouteOrigin != "" {
		t.Fatalf("optional route provenance is not backward compatible: %+v %v", legacy, err)
	}
}

func TestPublisherPDFPlanRejectsUnsupportedSourceKindWithProductionWording(t *testing.T) {
	_, err := PublisherPDFPlan(model.Document{ID: "html", Kind: "flare"})
	if err == nil || !strings.Contains(err.Error(), `cannot use mapped-PDF planning with source kind "flare"`) {
		t.Fatalf("unsupported source-kind error is not actionable: %v", err)
	}
	if strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("production error retained checkpoint framing: %v", err)
	}
}

func TestUnsupportedRoutesAreErrors(t *testing.T) {
	for _, tc := range []struct{ version, target string }{
		{"latest", "fund_6300"}, {"10.18-preview", "fund_6300"}, {"10.16", "../fund"},
		{"10.16", "fund/Content/home.htm"}, {"10.16", "javascript:alert(1)"}, {"10.16", "//host.test/book.html"},
		{"10.16", "https://support.hpe.com/login"}, {"10.16", "https://publisher.test:bad/book.html"},
		{"10.16", "https://bad host.test/book.html"},
		{"10.18", "https://support.hpe.com/hpesc/public/docDisplay?docId=one&docId=two"},
	} {
		if _, err := resolve(model.Guide{ID: "guide"}, "6300", tc.version, tc.target); err == nil {
			t.Errorf("unsupported mapping guessed: %+v", tc)
		}
	}
}

func TestMappingUTF8AndBOM(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{"\xef\xbb\xbf" + `{"10.16":{"6300":"guide"}}`, true},
		{`{"10.16":{"6300":"` + "\xff" + `"}}`, false},
	} {
		r := model.Resource{Body: io.NopCloser(strings.NewReader(tc.body))}
		result, err := parseMappings(r)
		if (err == nil) != tc.ok || (tc.ok && result["10.16"]["6300"] != "guide") {
			t.Fatalf("mapping encoding: result=%v err=%v", result, err)
		}
	}
}
