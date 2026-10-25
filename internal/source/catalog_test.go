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
	"aos-cx-docs-dldr/internal/testutil"
)

const (
	portalPage = "https://example.test/portal/aoscx.html"
	portalRoot = "https://example.test" + testutil.PortalBase
	docsRoot   = "https://example.test/techdocs/"
)

var portalFixture = testutil.Portal{
	Platforms: []string{"6300", "6400"},
	Versions:  []string{"10.18", "10.17", "10.16", "10.10"},
	Guides: []testutil.PortalGuide{
		{ID: "fundamentals", Title: "Fundamentals Guide"},
		{ID: "cli", Title: "CLI (PDF)"},
		{ID: "webUI", Title: "Web UI", Key: "webUI"},
	},
	Root: docsRoot,
}

// portalResources returns the authored portal at portalPage.
func portalResources(portal testutil.Portal) map[string]string {
	resources := map[string]string{portalPage: portal.Page()}
	for _, name := range []string{"models/products.json", "models/aoscx/product.json", "adapters/aoscx-adapter.js"} {
		_, body, _ := portal.File(testutil.PortalBase+name, "https://example.test")
		resources[portalRoot+name] = string(body)
	}
	return resources
}

// fixtureContentType labels fixture bodies the way the publisher serves them.
func fixtureContentType(raw string) string {
	switch {
	case strings.HasSuffix(raw, ".json"):
		return "application/json"
	case strings.HasSuffix(raw, ".js"):
		return "application/javascript"
	}
	return "text/html; charset=utf-8"
}

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
	return model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {fixtureContentType(raw)}},
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
		return model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {fixtureContentType(raw)}},
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
			context.Background(), f, portalPage, 3,
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
		context.Background(), f, portalPage, 2,
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
			ctx, f, portalPage, 3,
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
	resources := portalResources(portalFixture)
	resources["https://example.test/portal/json/aoscx/fundamentals.json"] = `{"10.16":{"6300":"test_6300-6400","6400":"test_6300-6400"},"10.18":{"6300":"Fundamentals_6300-6400"},"10.08":{"6300":"archived"}}`
	resources["https://example.test/portal/json/aoscx/cli.json"] = `{"10.16":{"6300":"cli_6300"}}`
	resources["https://example.test/portal/json/aoscx/webUI.json"] = `{}`
	return &fixtureFetcher{resources: resources}
}

func TestFreshCatalogueAndSharedExactMappings(t *testing.T) {
	f := fixtures()
	c, err := LoadCatalog(context.Background(), f, portalPage)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Platforms, ",") != "6300,6400" || len(c.Guides) != 3 || c.Guides[2].ID != "webUI" ||
		len(c.Versions) != 4 || c.Guides[0].Title != "Fundamentals Guide" || len(f.calls) != 7 ||
		c.FetchedAt == "" || len(c.Warnings) != 0 || c.RouteRoot != docsRoot {
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
			c, err := LoadCatalog(context.Background(), f, portalPage)
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

func TestPortalConfigurationNeverGuesses(t *testing.T) {
	config := portalRoot + "models/aoscx/product.json"
	for name, edit := range map[string]func(string) string{
		"traversing key": func(body string) string { return strings.Replace(body, `"key":"cli"`, `"key":"../private"`, 1) },
		"spaced id":      func(body string) string { return strings.Replace(body, `"id":"cli"`, `"id":"cli guide"`, 1) },
		"duplicate id":   func(body string) string { return strings.Replace(body, `"id":"webUI"`, `"id":"cli"`, 1) },
		"empty title":    func(body string) string { return strings.Replace(body, `"title":"Web UI"`, `"title":" "`, 1) },
		"bad release":    func(body string) string { return strings.Replace(body, `"value":"10.16"`, `"value":"latest"`, 1) },
		"two selectors":  func(body string) string { return strings.Replace(body, `"id":"version"`, `"id":"platform"`, 1) },
		"other product":  func(body string) string { return strings.Replace(body, `"id":"aoscx"`, `"id":"instant"`, 1) },
		"adapter escapes": func(body string) string {
			return strings.Replace(body, `"adapter":"aoscx-adapter"`, `"adapter":"../x"`, 1)
		},
		"trailing content": func(body string) string { return body + "{}" },
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtures()
			f.resources[config] = edit(f.resources[config])
			if _, err := LoadCatalog(context.Background(), f, portalPage); err == nil {
				t.Fatalf("invalid portal configuration accepted: %s", f.resources[config])
			}
		})
	}
	f := fixtures()
	c, err := LoadCatalog(context.Background(), f, portalPage)
	if err != nil {
		t.Fatal(err)
	}
	for _, guide := range c.Guides {
		if guide.ID == "release-notes" {
			t.Fatal("platform-map resource was listed as a mapped guide")
		}
	}
}

func TestMalformedPortalIsFatal(t *testing.T) {
	adapter := portalRoot + "adapters/aoscx-adapter.js"
	registry := portalRoot + "models/products.json"
	for name, edit := range map[string]func(map[string]string){
		"sign-in page": func(r map[string]string) { r[portalPage] = "<html><h1>Sign in</h1></html>" },
		"no product": func(r map[string]string) {
			r[portalPage] = strings.Replace(r[portalPage], "window.PRODUCT_ID", "window.OTHER", 1)
		},
		"other product": func(r map[string]string) {
			r[portalPage] = strings.Replace(r[portalPage], `PRODUCT_ID = "aoscx"`, `PRODUCT_ID = "aos8"`, 1)
		},
		"no shell": func(r map[string]string) {
			r[portalPage] = strings.Replace(r[portalPage], "js/shell.js", "js/other.js", 1)
		},
		"two shells": func(r map[string]string) {
			r[portalPage] = strings.Replace(r[portalPage], "</body>", `<script src="/x/js/shell.js"></script></body>`, 1)
		},
		"unlisted product": func(r map[string]string) {
			r[registry] = `{"products":[{"id":"instant","config":"models/instant/product.json"}]}`
		},
		"escaping config": func(r map[string]string) {
			r[registry] = `{"products":[{"id":"aoscx","config":"../../portal/json/aoscx/cli.json"}]}`
		},
		"absolute config": func(r map[string]string) {
			r[registry] = `{"products":[{"id":"aoscx","config":"https://elsewhere.test/product.json"}]}`
		},
		"changed route": func(r map[string]string) {
			r[adapter] = strings.Replace(r[adapter], `compare(version, "10.17") >= 0`, `compare(version, "10.18") >= 0`, 1)
		},
		"new route": func(r map[string]string) {
			r[adapter] = strings.Replace(r[adapter], "/Content/home.htm`", "/Content/index.htm`", 1)
		},
		"no root": func(r map[string]string) { r[adapter] = strings.Replace(r[adapter], "const ROOT", "let ROOT", 1) },
		"two roots": func(r map[string]string) {
			r[adapter] = r[adapter] + "\nconst ROOT = \"https://example.test/other/\";\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtures()
			edit(f.resources)
			if _, err := LoadCatalog(context.Background(), f, portalPage); err == nil {
				t.Fatal("invalid portal accepted")
			} else if name == "changed route" && !strings.Contains(err.Error(), "no longer matches the guide routes") {
				t.Fatalf("route change was not reported as an adapter change: %v", err)
			}
		})
	}
	f := fixtures()
	f.resources[registry+"?"] = ""
	wrongType := &contentTypeFetcher{fixtureFetcher: f, override: map[string]string{registry: "text/html"}}
	if _, err := LoadCatalog(context.Background(), wrongType, portalPage); err == nil || !strings.Contains(err.Error(), "expected JSON portal data") {
		t.Fatalf("HTML served as the portal registry was accepted: %v", err)
	}
}

type contentTypeFetcher struct {
	*fixtureFetcher
	override map[string]string
}

func (f *contentTypeFetcher) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	r, err := f.fixtureFetcher.Get(ctx, raw, refresh)
	if contentType, ok := f.override[raw]; ok && err == nil {
		r.Headers = http.Header{"Content-Type": {contentType}}
	}
	return r, err
}

func TestCompareReleaseMirrorsPublisherAdapter(t *testing.T) {
	for _, tc := range []struct {
		first, second string
		want          int
	}{
		{"10.17", "10.17", 0}, {"10.18", "10.17", 1}, {"10.16", "10.17", -1},
		{"10.17.xxxx", "10.17", 1}, {"10.16.xxxx", "10.17", -1}, {"10.17.0.0", "10.17", 0},
		{"10.12", "10.12", 0}, {"10.13", "10.12", 1}, {"10.12.1000", "10.12", 1}, {"10.9", "10.12", -1},
		{"10.17.XXXX", "10.17.9999", 0}, {"10.17.xx", "10.17", 0},
	} {
		if got := compareRelease(tc.first, tc.second); got != tc.want {
			t.Errorf("compareRelease(%q, %q)=%d, want %d", tc.first, tc.second, got, tc.want)
		}
	}
}

func TestExactRoutingFollowsPublisherAdapter(t *testing.T) {
	for _, tc := range []struct {
		version, id, target, kind, want, origin string
	}{
		{"10.18", "fundamentals", "Fundamentals_8100-83xx-93xx-100xx", "static",
			docsRoot + "AOS-CX/10.18.xxxx/HTML/Fundamentals_8100-83xx-93xx-100xx/index.html", "adapter-derived-book-stem"},
		{"10.18", "cli", "CLI_832x", "static", docsRoot + "AOS-CX/10.18.xxxx/HTML/CLI_832x/index.html", "adapter-derived-book-stem"},
		{"10.17", "iprouting", "IP Routing 6300-6400-8100-83xx-93xx-100xx", "static",
			docsRoot + "AOS-CX/10.17.xxxx/HTML/IP%20Routing%206300-6400-8100-83xx-93xx-100xx/index.html", "adapter-derived-book-stem"},
		{"10.17", "webUI", "Introduction to the Web UI Guide (8xxx, 9xxx, 100xx)", "static",
			docsRoot + "AOS-CX/10.17.xxxx/HTML/Introduction%20to%20the%20Web%20UI%20Guide%20%288xxx%2C%209xxx%2C%20100xx%29/index.html", "adapter-derived-book-stem"},
		{"10.17", "multicast", "Multicast  Guide_4100i-6000-6100", "static",
			docsRoot + "AOS-CX/10.17.xxxx/HTML/Multicast%20%20Guide_4100i-6000-6100/index.html", "adapter-derived-book-stem"},
		{"10.16", "fundamentals", "fundamentals_8100-83xx-93xx-100xx", "flare",
			docsRoot + "AOS-CX/10.16/HTML/fundamentals_8100-83xx-93xx-100xx/Content/home.htm", "adapter-derived-book-stem"},
		{"10.16.xxxx", "fundamentals", "fund_6300", "flare", docsRoot + "AOS-CX/10.16.xxxx/HTML/fund_6300/Content/home.htm", "adapter-derived-book-stem"},
		{"10.16", "cli", "cli_832x", "pdf", docsRoot + "AOS-CX/10.16/PDF/cli_832x.pdf", "adapter-derived-book-stem"},
		{"10.13", "cli", "cli_6000-6100", "pdf", docsRoot + "AOS-CX/10.13/PDF/cli_6000-6100.pdf", "adapter-derived-book-stem"},
		{"10.12", "fundamentals", "fund_6300", "pdf", docsRoot + "Archived/AOS-CX/10.12/PDF/fund_6300.pdf", "adapter-derived-book-stem"},
		{"10.10", "cli", "cli_6300", "pdf", docsRoot + "Archived/AOS-CX/10.10/PDF/cli_6300.pdf", "adapter-derived-book-stem"},
		{"10.9", "fundamentals", "fund_6300", "pdf", docsRoot + "Archived/AOS-CX/10.9/PDF/fund_6300.pdf", "adapter-derived-book-stem"},
		{"10.16", "fundamentals", "https://publisher.test/some guide%20name.html", "static",
			"https://publisher.test/some%20guide%20name.html", "publisher-url"},
		{"10.18", "hpe", "https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001234en_us", "hpe",
			"https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001234en_us", "publisher-url"},
	} {
		t.Run(tc.version+"-"+tc.target, func(t *testing.T) {
			d, err := resolve(model.Guide{ID: tc.id}, "6300", tc.version, tc.target, docsRoot)
			if err != nil || d.Kind != tc.kind || d.URL != tc.want || d.Version != tc.version || d.RouteOrigin != tc.origin {
				t.Fatalf("route differs from the publisher adapter: %+v %v", d, err)
			}
		})
	}
}

func TestPDFRouteOriginDoesNotClaimPayloadVerification(t *testing.T) {
	document, err := resolve(model.Guide{ID: "cli", Title: "CLI"}, "8320", "10.16", "cli_8320-8325", docsRoot)
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
		{"latest", "fund_6300"}, {"10.18-preview", "fund_6300"}, {"10.16", "../fund"}, {"10.16", "fund..x"},
		{"10.16", "fund/Content/home.htm"}, {"10.16", "javascript:alert(1)"}, {"10.16", "//host.test/book.html"},
		{"10.17", " IP Routing"}, {"10.17", "IP Routing "}, {"10.17", "IP%20Routing"}, {"10.17", "IP\\Routing"},
		{"10.17", "Guide?x=1"}, {"10.17", "Guide#x"}, {"10.18", "sd00001234en_us/"},
		{"10.16", "https://support.hpe.com/login"}, {"10.16", "https://publisher.test:bad/book.html"},
		{"10.16", "https://bad host.test/book.html"},
		{"10.18", "https://support.hpe.com/hpesc/public/docDisplay?docId=one&docId=two"},
	} {
		if _, err := resolve(model.Guide{ID: "guide"}, "6300", tc.version, tc.target, docsRoot); err == nil {
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
