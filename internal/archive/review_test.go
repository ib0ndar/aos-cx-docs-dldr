package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"github.com/tdewolff/parse/v2/css"
	"golang.org/x/net/html"
)

func TestInertPublisherSpanHrefIsNotAMissingLink(t *testing.T) {
	body := `<main id="mc-main-content"><h1>Home</h1><p><span class="CLI" href="../VSX_cmds/not-a-link.htm">vsx-sync loop-protect-global</span></p></main>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(body)),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("inert publisher attribute must not prevent publication: %+v %v", result, err)
	}
	pageBytes, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(pageBytes))
	if err != nil {
		t.Fatal(err)
	}
	span := findNode(doc, func(n *html.Node) bool { return n.Data == "span" && hasClass(n, "CLI") })
	if span == nil || span.FirstChild == nil || span.FirstChild.Type != html.TextNode ||
		span.FirstChild.Data != "vsx-sync loop-protect-global" ||
		attr(span, "href") != "../VSX_cmds/not-a-link.htm" ||
		fetcher.calls[testRoot+"VSX_cmds/not-a-link.htm"] != 0 {
		t.Fatal("inert markup was changed into an active link or fetched")
	}
}

func TestOutputIntegrityChecksOnlyActiveHTMLReferenceAttributes(t *testing.T) {
	for _, test := range []struct {
		name, markup string
		wantProblem  bool
	}{
		{"span-href", `<span href="missing.htm">Text</span>`, false},
		{"div-src", `<div src="missing.png">Text</div>`, false},
		{"anchor-href", `<a href="missing.htm">Text</a>`, true},
		{"area-href", `<map name="m"><area href="missing.htm"></map>`, true},
		{"link-href", `<link rel="stylesheet" href="missing.css">`, true},
		{"image-src", `<img src="missing.png">`, true},
		{"source-src", `<video><source src="missing.mp4"></video>`, true},
		{"iframe-src", `<iframe src="missing.htm"></iframe>`, true},
		{"svg-use-href", `<svg><use href="missing.svg#symbol"></use></svg>`, true},
		{"svg-image-href", `<svg><image href="missing.png"></image></svg>`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "index.html"),
				[]byte("<!doctype html><html><body>"+test.markup+"</body></html>"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "toc.html"), []byte("<!doctype html><p>Contents</p>"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			_, problems := validateOutput(root, &model.HTMLArchive{}, nil)
			if (len(problems) > 0) != test.wantProblem {
				t.Fatalf("reference validation: got %v, want problem=%v", problems, test.wantProblem)
			}
		})
	}
}

func TestGeneratedBookmarkAbsenceIsReferrerAndTargetSpecific(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pages"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"index.html": `<a href="pages/source.html">Guide</a><a href="pages/other-source.html">Other</a>`,
		"toc.html":   `<a href="pages/source.html">Contents</a>`,
		"pages/source.html": `<a href="target-a.html#missing%20value">A</a>` +
			`<a href="target-b.html#missing%20value">B</a>` +
			`<a href="target-c.html#missing%2520value">Literal percent</a>`,
		"pages/other-source.html": `<a href="target-a.html#missing%20value">Unverified A</a>`,
		"pages/target-a.html":     `<main><h1>A</h1></main>`,
		"pages/target-b.html":     `<main><h1>B</h1></main>`,
		"pages/target-c.html":     `<main><h1 id="missing%20value">C</h1></main>`,
	}
	records := []model.FileRecord{}
	for name, body := range files {
		body = "<!doctype html><html><body>" + body + "</body></html>"
		if err := os.WriteFile(filepath.Join(dir, name),
			[]byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(name, "pages/") {
			records = append(records, model.FileRecord{
				Path: name, Status: "complete", SHA256: sourceHash([]byte(body)), Size: int64(len(body)),
			})
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	archive := &model.HTMLArchive{Topics: records}
	allowed := map[localBookmarkReference]bool{{
		source: "pages/source.html", target: "pages/target-a.html", fragment: "missing value",
	}: true}
	_, problems := validateOutput(root, archive, allowed)
	if len(problems) != 2 ||
		!slices.ContainsFunc(problems, func(problem string) bool {
			return strings.Contains(problem, "pages/source.html") &&
				strings.Contains(problem, "target-b.html#missing%20value")
		}) ||
		!slices.ContainsFunc(problems, func(problem string) bool {
			return strings.Contains(problem, "pages/other-source.html") &&
				strings.Contains(problem, "target-a.html#missing%20value")
		}) {
		t.Fatalf("bookmark exemption leaked across targets: %v", problems)
	}
}

func TestGroupedChromeSelectorKeepsUsedContentSelector(t *testing.T) {
	seen := []string{}
	filter := func(tokens []css.Token) bool {
		return selectorGroupRelevant(tokens, map[string]bool{"warning": true}, nil)
	}
	output, _, err := rewriteCSSFiltered([]byte(`.menu-icon,.warning{background:url("warning.png")}`), false,
		func(reference string, _ dependencyKind) (string, error) {
			seen = append(seen, reference)
			return "local.png", nil
		}, filter)
	if err != nil || len(seen) != 1 || strings.Contains(string(output), "menu-icon") ||
		!strings.Contains(string(output), `.warning{background:url("local.png")`) {
		t.Fatalf("grouped selector filtering lost content style: %q seen=%v err=%v", output, seen, err)
	}
}

func TestSVGColorsAndInlinedDefinitionsArePreservedAndNamespaced(t *testing.T) {
	sprite := testRoot + "Resources/sprite.svg"
	body := `<main id="mc-main-content"><h1>Home</h1><svg><use href="../Resources/sprite.svg#shape"></use></svg>
<img src="../Resources/sprite.svg"><img src="../Resources/sprite.svg"></main>`
	svg := `<svg xmlns="http://www.w3.org/2000/svg"><style>#shape{fill:url(#paint)}</style><defs>
<linearGradient id="paint"><stop stop-color="#fff"/></linearGradient></defs>
<symbol id="shape"><path fill="#fff" d="M0 0L1 1"/></symbol></svg>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(body)),
		sprite:   fixtureResource(sprite, "image/svg+xml", []byte(svg)),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("SVG archive failed: %+v %v", result, err)
	}
	pageBytes, _ := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if !bytes.Contains(pageBytes, []byte(`fill="#fff"`)) ||
		bytes.Count(pageBytes, []byte(`id="svg-`)) < 6 ||
		bytes.Count(pageBytes, []byte(`data-archive-svg="`)) < 3 ||
		!bytes.Contains(pageBytes, []byte(`linearGradient`)) ||
		!bytes.Contains(pageBytes, []byte(`:where([data-archive-svg=`)) {
		t.Fatalf("SVG definitions, colors, or scopes were lost: %s", pageBytes)
	}
	if fetcher.calls[sprite] != 1 {
		t.Fatalf("SVG retrieval was not deduplicated: %v", fetcher.calls)
	}
}

func TestTOCFragmentsAndDataImagesRemainOffline(t *testing.T) {
	body := `<main id="mc-main-content"><h1 id="section">Home</h1>
<picture><source src="data:image/png;base64,AAAA"><img src="data:image/png;base64,AAAA"></picture></main>`
	plan := flarePlan()
	plan.TOC = []model.TocEntry{{Title: "Section", URL: testHome + "#section"}}
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(body)),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("data image archive failed: %+v %v", result, err)
	}
	toc, _ := os.ReadFile(filepath.Join(output, "toc.html"))
	page, _ := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if !bytes.Contains(toc, []byte("#section")) || bytes.Count(page, []byte("data:image/png;base64,AAAA")) != 2 {
		t.Fatalf("TOC fragment or data image was lost: toc=%s page=%s", toc, page)
	}
}

func TestSourceElementAndSharedAggregateLimitCannotHideMissingBytes(t *testing.T) {
	missing := testRoot + "Resources/missing.png"
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(`<main id="mc-main-content"><h1>Home</h1>
<picture><source src="../Resources/missing.png"><img src="data:image/png;base64,AAAA"></picture></main>`)),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
	if err != nil || result.Status != "incomplete" || fetcher.calls[missing] != 1 {
		t.Fatalf("missing source dependency was hidden: %+v calls=%v err=%v", result, fetcher.calls, err)
	}

	largeTopic := bytes.Repeat([]byte("x"), 700)
	largeAsset := append(append([]byte{}, tinyPNG...), bytes.Repeat([]byte("y"), 700)...)
	htmlBody := append([]byte(`<main id="mc-main-content"><h1>Home</h1><img src="../Resources/image.png"><p>`), largeTopic...)
	htmlBody = append(htmlBody, []byte(`</p></main>`)...)
	image := testRoot + "Resources/image.png"
	budgetFetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", htmlBody),
		image:    fixtureResource(image, "image/png", largeAsset),
	}, calls: map[string]int{}}
	if result, err := HTML(context.Background(), flarePlan(), budgetFetcher, t.TempDir(), false, 1<<20, 1200); err != nil ||
		result.Status != "incomplete" || !strings.Contains(strings.Join(result.Errors, " "), "aggregate source-byte limit") {
		t.Fatalf("split aggregate budget was accepted: %+v %v", result, err)
	}
}
