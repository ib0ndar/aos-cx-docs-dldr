package source

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

func TestDocumentScopeRejectsDecodedDotSegments(t *testing.T) {
	flare := model.Document{Kind: "flare",
		URL: "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.16/HTML/guide/Content/home.htm"}
	pdf := model.Document{Kind: "pdf", URL: "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.13/PDF/cli_6300.pdf"}
	for _, tc := range []struct {
		document model.Document
		raw      string
		allowed  bool
	}{
		{flare, "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.16/HTML/shared/Content/home.htm", true},
		{flare, "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.16/HTML/%2e%2e/%2e%2e/x/Content/y.htm", false},
		{flare, "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.16/HTML/%2E%2E/10.17/HTML/x/Content/y.htm", false},
		{flare, "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.16/HTML/guide/%2e/Content/y.htm", false},
		{flare, "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.16/HTML/guide%2F..%2F..%2Fx/Content/y.htm", false},
		{pdf, "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.13/PDF/cli_6300.pdf", true},
		{pdf, "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.13/PDF/%2e%2e/%2e%2e/private/cli.pdf", false},
		{pdf, "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.13/cli_6300.pdf", false},
	} {
		if err := DocumentScope(tc.document)(tc.raw); (err == nil) != tc.allowed {
			t.Errorf("%s allowed=%v error=%v", tc.raw, tc.allowed, err)
		}
		if err := initialScope(tc.document)(tc.raw); (err == nil) != tc.allowed {
			t.Errorf("initial scope %s allowed=%v error=%v", tc.raw, tc.allowed, err)
		}
	}
}

func TestCatalogueAcceptsPublicHTTPAndRefusesNonPublicTargets(t *testing.T) {
	for _, tc := range []struct{ target, reason string }{
		{"ftp://arubanetworking.hpe.com/techdocs/AOS-CX/10.13/PDF/cli.pdf", "unsupported publisher URL"},
		{"http://192.168.1.5/techdocs/guide.pdf", "non-public"},
		{"https://10.0.0.5/techdocs/guide/Content/home.htm", "non-public"},
		{"https://169.254.169.254/latest/guide.html", "non-public"},
		{"https://2130706433/guide.pdf", "numeric host"},
		{"https://docs.localhost/guide.pdf", "localhost"},
	} {
		_, err := resolve(model.Guide{ID: "guide"}, "6300", "10.16", tc.target)
		if err == nil || !strings.Contains(err.Error(), tc.target) || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("unsafe mapping target %s was not refused clearly: %v", tc.target, err)
		}
	}
	catalog := model.Catalog{Platforms: []string{"6300"}, Versions: []string{"10.16"}, Guides: []model.Guide{
		{ID: "plain", Title: "Plain", Mappings: map[string]map[string]string{
			"10.16": {"6300": "http://arubanetworking.hpe.com/techdocs/plain.pdf"}}},
		{ID: "private", Title: "Private", Mappings: map[string]map[string]string{
			"10.16": {"6300": "http://10.1.2.3/techdocs/private.pdf"}}},
		{ID: "fixture", Title: "Fixture", Mappings: map[string]map[string]string{
			"10.16": {"6300": "http://127.0.0.1:8080/techdocs/fixture.pdf"}}},
	}}
	documents, diagnostics, err := ResolveDocuments(catalog, "6300", "10.16")
	if err != nil || len(documents) != 2 || documents[0].ID != "plain" || documents[1].ID != "fixture" ||
		len(diagnostics) != 1 || !strings.Contains(diagnostics[0], "Private: unsupported absolute publisher target") {
		t.Fatalf("public HTTP target refused, private target not reported, or loopback fixture target refused: %+v %v %v",
			documents, diagnostics, err)
	}
}

func stylesheetPage(t *testing.T, hrefs []string) *html.Node {
	t.Helper()
	var page strings.Builder
	page.WriteString("<html><head>")
	for _, href := range hrefs {
		fmt.Fprintf(&page, `<link rel="stylesheet" href="%s">`, href)
	}
	page.WriteString("</head><body><main>Guide</main></body></html>")
	tree, err := html.Parse(strings.NewReader(page.String()))
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestStylesheetDiscoveryIsOrderedBoundedAndCancellable(t *testing.T) {
	base := "https://arubanetworking.hpe.com/techdocs/guide/Content/home.htm"
	hrefs := []string{"a.css", "b.css", "a.css", "c.css", "b.css"}
	for range 50_000 {
		hrefs = append(hrefs, "a.css")
	}
	got, err := styles(context.Background(), stylesheetPage(t, hrefs), base)
	want := []string{
		"https://arubanetworking.hpe.com/techdocs/guide/Content/a.css",
		"https://arubanetworking.hpe.com/techdocs/guide/Content/b.css",
		"https://arubanetworking.hpe.com/techdocs/guide/Content/c.css",
	}
	if err != nil || strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stylesheet order or de-duplication changed: %v %v", got, err)
	}
	distinct := make([]string, maxStyles+1)
	for i := range distinct {
		distinct[i] = fmt.Sprintf("style-%d.css", i)
	}
	if _, err := styles(context.Background(), stylesheetPage(t, distinct[:maxStyles]), base); err != nil {
		t.Fatalf("stylesheet limit is not inclusive: %v", err)
	}
	if _, err := styles(context.Background(), stylesheetPage(t, distinct), base); err == nil ||
		!strings.Contains(err.Error(), "stylesheet inventory exceeds entry limit") {
		t.Fatalf("unbounded stylesheet inventory accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := styles(ctx, stylesheetPage(t, hrefs), base); !errors.Is(err, context.Canceled) {
		t.Fatalf("stylesheet discovery ignored cancellation: %v", err)
	}
}

func TestPlanFinishAndDiagnosticsDeduplicateInOrder(t *testing.T) {
	p := newPlanner(context.Background(), nil, model.Document{Kind: "flare"}, false)
	p.plan.Styles = []string{"b", "a", "b", "c", "a"}
	for range 3 {
		p.notice(model.NoticeExternalLinkRetained, "one")
		p.notice(model.NoticeSourcePolicy, "one")
		p.warning("first")
		p.warning("second")
	}
	plan, err := p.finish("root", "toc", nil)
	if err != nil || strings.Join(plan.Styles, ",") != "b,a,c" || len(plan.Notices) != 2 ||
		strings.Join(plan.Warnings, ",") != "first,second" {
		t.Fatalf("plan de-duplication changed: %+v %v", plan, err)
	}
	p = newPlanner(context.Background(), nil, model.Document{Kind: "flare"}, false)
	for i := range maxStyles + 1 {
		p.plan.Styles = append(p.plan.Styles, fmt.Sprintf("style-%d.css", i), "style-0.css")
	}
	if _, err := p.finish("root", "toc", nil); err == nil || !strings.Contains(err.Error(), "stylesheet inventory") {
		t.Fatalf("combined stylesheet inventory is unbounded: %v", err)
	}
}
