package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

type fixture map[string]struct {
	mime string
	body []byte
}

func (f fixture) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	if err := ctx.Err(); err != nil {
		return model.Resource{}, err
	}
	value, ok := f[raw]
	if !ok {
		return model.Resource{}, errors.New("missing fixture " + raw)
	}
	return model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {value.mime}},
		Body: io.NopCloser(bytes.NewReader(value.body))}, nil
}

func flareGuide(t *testing.T, missingBookmark bool) (string, model.DocumentPlan, model.ArchiveResult) {
	t.Helper()
	root := "https://docs.example.test/guide/"
	home, second, extra := root+"Content/home.htm", root+"Content/second.htm", root+"Content/extra.htm"
	css, image, svg := root+"Resources/main.css", root+"Resources/photo.png", root+"Resources/icon.svg"
	fragment := "target"
	if missingBookmark {
		fragment = "publisher-missing"
	}
	plan := model.DocumentPlan{
		Document: model.Document{ID: "guide", Title: "Modern Guide", Platform: "6300", Version: "10.16", URL: home, Kind: "flare"},
		Topics:   []model.Topic{{URL: home, Title: "Introduction"}, {URL: second, Title: "Configuration"}},
		TOC: []model.TocEntry{
			{Title: "Introduction", URL: home, Children: []model.TocEntry{{Title: "Configuration", URL: second + "#target"}}},
			{Title: "Configuration repeated", URL: second + "#target"},
		},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "flare", RootURL: root, Entries: 3, UniqueTopics: 2},
	}
	fetcher := fixture{
		home: {"text/html", []byte(`<html><head><link rel="stylesheet" href="../Resources/main.css"></head><body><main id="mc-main-content">
<div class="topichero"><div class="docname" id="docname">Modern Guide Publisher Hero</div></div><div class="container">
<h1 id="same">Introduction</h1><div id="same"><p>Duplicate source ID</p></div>
<ul><li>one</li><li>two</li></ul><table><caption>Ports</caption><tr><th rowspan="2">Port</th><td colspan="2">up</td></tr></table>
<pre><code>interface 1/1/1
    description  exact
	no shutdown</code></pre><div style="background-image:image-set('../Resources/photo.png' 1x)">Styled</div><img alt="Photo" src="../Resources/photo.png"><img alt="Vector" src="../Resources/icon.svg">
<a href="second.htm#` + fragment + `">next</a><a href="extra.htm">extra</a></div></main><footer>Copyright 2026 Publisher</footer></body></html>`)},
		second: {"text/html", []byte(`<main id="mc-main-content"><div class="container" data-ordinary-container="true"><h1 id="target">Configuration</h1><ol><li>step</li></ol><a href="home.htm#same">back</a></div></main>`)},
		extra:  {"text/html", []byte(`<main id="mc-main-content"><h1>Supplement</h1><p>Extra topic.</p></main>`)},
		css:    {"text/css", []byte(`.x{background:url("photo.png")}div.topichero{position:fixed;z-index:8;top:30px;height:150px}div.container{margin-top:120px}`)},
		image:  {"image/png", []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 0}},
		svg:    {"image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><defs><linearGradient id="g"/></defs><style>#shape{fill:url(#g)}</style><rect id="shape" fill="url(#g)"/></svg>`)},
	}
	output := t.TempDir()
	result, err := archive.HTML(context.Background(), plan, fetcher, output, false, 1<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("archive failed: result=%+v err=%v", result, err)
	}
	return output, plan, result
}

func assembleGuide(t *testing.T, output string, plan model.DocumentPlan, result model.ArchiveResult) (Result, []byte) {
	t.Helper()
	root, err := os.OpenRoot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var combined bytes.Buffer
	record, err := Assemble(context.Background(), root, ".", plan, result, &combined)
	if err != nil {
		t.Fatal(err)
	}
	return record, combined.Bytes()
}

func TestAssemblePreservesSemanticsOrderLinksAndResources(t *testing.T) {
	output, plan, archived := flareGuide(t, false)
	before, _ := os.ReadFile(filepath.Join(output, "manifest.json"))
	sourcePath := filepath.Join(output, filepath.FromSlash(archived.HTML.Topics[0].Path))
	sourceBefore, _ := os.ReadFile(sourcePath)
	record, body := assembleGuide(t, output, plan, archived)
	after, _ := os.ReadFile(filepath.Join(output, "manifest.json"))
	sourceAfter, _ := os.ReadFile(sourcePath)
	if !bytes.Equal(before, after) {
		t.Fatal("transient assembly mutated the guide manifest")
	}
	if !bytes.Equal(sourceBefore, sourceAfter) {
		t.Fatal("transient assembly mutated the archived Flare homepage")
	}
	if record.TOCEntries != 4 || record.Size != int64(len(body)) || record.SHA256 == "" ||
		record.SourceManifestSHA256 == "" || record.CheckedLinks == 0 {
		t.Fatalf("bad assembly record: %+v", record)
	}
	if record.Outline.SchemaVersion != OutlineSchemaVersion ||
		record.Outline.EntryCount != 5 || record.Outline.MaxDepth != 2 ||
		len(record.Outline.Entries) != 3 ||
		record.Outline.Entries[0].Title != "Introduction" ||
		record.Outline.Entries[0].Children[0].Title != "Configuration" ||
		record.Outline.Entries[0].Children[0].Target != record.Outline.Entries[1].Target ||
		record.Outline.Entries[2].Title != "Additional archived topics" ||
		len(record.Outline.Entries[2].Children) != 1 {
		t.Fatalf("occurrence-aware outline plan is incomplete: %+v", record.Outline)
	}
	if record.Footer.SchemaVersion != FooterSourceSchemaVersion ||
		record.Footer.ContentsTarget != OutlineContentsTarget ||
		record.Footer.ProvenanceTarget != OutlineProvenanceTarget ||
		len(record.Footer.Topics) != 3 ||
		record.Footer.Topics[0].Label != "Introduction" ||
		record.Footer.Topics[1].Label != "Introduction" ||
		record.Footer.Topics[2].Label != "Additional archived topics" {
		t.Fatalf("authoritative footer source plan is incomplete: %+v", record.Footer)
	}
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	ids := map[string]bool{}
	internalLinks := 0
	var duplicate bool
	walk(document, func(node *html.Node) {
		if id := attribute(node, "id"); id != "" {
			if ids[id] {
				duplicate = true
			}
			ids[id] = true
		}
	})
	walk(document, func(node *html.Node) {
		if node.Type == html.ElementNode && (node.Data == "a" || node.Data == "area") {
			if href := attribute(node, "href"); strings.HasPrefix(href, "#") {
				internalLinks++
				if !ids[strings.TrimPrefix(href, "#")] {
					t.Errorf("unresolved internal link %s", href)
				}
			}

		}
	})
	if duplicate || internalLinks < 5 || strings.Count(string(body), `rel="stylesheet"`) != 1 ||
		!strings.Contains(string(body), `href="#aos-cx-docs-dldr-print-contents"`) ||
		!strings.Contains(string(body), `id="aos-cx-docs-dldr-print-contents"`) ||
		!strings.Contains(string(body), `src="assets/`) || strings.Contains(string(body), "../assets/") ||
		!strings.Contains(string(body), "Additional topics") ||
		strings.Contains(string(body), "Modern Guide Publisher Hero") ||
		strings.Contains(string(body), `class="topichero"`) ||
		strings.Contains(string(body), `class="docname"`) ||
		strings.Count(string(body), "print-flare-home-hero-removed") != 2 ||
		!strings.Contains(string(body), `.print-content.print-flare-home-hero-removed>#mc-main-content>.container`) ||
		!strings.Contains(string(body), `class="print-topic print-chapter print-flare-homepage"`) ||
		!strings.Contains(string(body), `.print-topic.print-flare-homepage{break-after:page}`) ||
		!strings.Contains(string(body), `data-ordinary-container="true"`) ||
		!strings.Contains(string(body), "box-sizing:border-box") ||
		!strings.Contains(string(body), `body class="print-source-flare"`) ||
		!strings.Contains(string(body), `body.print-source-flare .print-topic :where(.print-content h1){font-size:16pt!important;line-height:1.2!important}`) ||
		!strings.Contains(string(body), `:where(.print-content h1,.print-content h2,.print-content h3){break-inside:avoid-page!important;break-after:avoid-page!important}`) ||
		!strings.Contains(string(body), "max-width:100%;overflow-x:auto;white-space:pre-wrap") ||
		!strings.Contains(string(body), ":where(.print-content p.CLI){max-width:100%;overflow-x:auto}") {
		t.Fatalf("combined structure is incomplete:\n%s", body)
	}
	if got := semanticSnapshot(document); got != (semanticCounts{tables: 1, captions: 1, lists: 5, rowspans: 1, colspans: 1, images: 1, svgs: 1}) {
		t.Fatalf("semantic structure changed: %+v", got)
	}
	if !bytes.Contains(body, []byte("interface 1/1/1\n    description  exact\n\tno shutdown")) {
		t.Fatal("pre/code whitespace changed")
	}
	intro := bytes.Index(body, []byte("Introduction"))
	config := bytes.LastIndex(body, []byte("Configuration</h1>"))
	supplement := bytes.Index(body, []byte("Supplement</h1>"))
	if intro < 0 || config <= intro || supplement <= config {
		t.Fatal("topic order changed")
	}
}

func TestOutlinePlanNormalizesWrappersAndPreservesGroupsExternalAndDepth(t *testing.T) {
	output, plan, archived := flareGuide(t, false)
	home := plan.Document.URL
	second := plan.Topics[1].URL
	deep := model.TocEntry{Title: "Depth 7", URL: second + "#target"}
	for depth := 6; depth >= 1; depth-- {
		deep = model.TocEntry{Title: fmt.Sprintf("Depth %d", depth), Children: []model.TocEntry{deep}}
	}
	plan.TOC = []model.TocEntry{{
		Title: "Home", URL: home, Children: []model.TocEntry{{
			Title: plan.Document.Title, URL: home, Children: []model.TocEntry{
				{Title: "URL-less group", Children: []model.TocEntry{
					{Title: "First local\n\tlabel", URL: second + "#target"},
					{Title: "External", URL: "https://support.example.test/kb?x=1#answer"},
				}},
				{Title: "Repeated local", URL: second + "#target"},
				deep,
			},
		}},
	}}
	record, _ := assembleGuide(t, output, plan, archived)
	outline := record.Outline
	if len(outline.Entries) != 4 || outline.Entries[0].Title != "URL-less group" ||
		outline.Entries[0].Children[0].Title != "First local label" ||
		outline.Entries[0].Target.Name == "" ||
		outline.Entries[0].Target != outline.Entries[0].Children[0].Target ||
		outline.Entries[0].Children[1].Target.URI != "https://support.example.test/kb?x=1#answer" ||
		outline.Entries[1].Target != outline.Entries[0].Children[0].Target ||
		outline.MaxDepth != 7 || outline.Entries[2].Title != "Depth 1" ||
		outline.Entries[3].Title != "Additional archived topics" {
		t.Fatalf("wrapper/group/external/repeated/deep outline contract changed: %+v", outline)
	}
}

func TestRemoveFlareHomepageHeroRejectsAmbiguousChromeAndPreservesOtherSources(t *testing.T) {
	parseContent := func(raw string) *html.Node {
		t.Helper()
		doc, err := html.Parse(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		content := findElement(doc, "archive-content")
		if content == nil {
			t.Fatal("missing test content")
		}
		return content
	}
	ambiguous := parseContent(`<main class="archive-content"><div id="mc-main-content"><div class="topichero"><div class="docname">Title</div><p>Substantive</p></div></div></main>`)
	if removed, err := removeFlareHomepageHero("flare", ambiguous); err == nil || removed {
		t.Fatalf("ambiguous publisher hero was removed: removed=%v err=%v", removed, err)
	}
	hpe := parseContent(`<main class="archive-content"><div id="mc-main-content"><div class="topichero"><div class="docname">Title</div></div></div></main>`)
	if removed, err := removeFlareHomepageHero("hpe", hpe); err != nil || removed ||
		findElement(hpe, "topichero") == nil {
		t.Fatalf("non-Flare content changed: removed=%v err=%v", removed, err)
	}
}

type semanticCounts struct {
	tables, captions, lists, rowspans, colspans, images, svgs int
}

func semanticSnapshot(document *html.Node) semanticCounts {
	var result semanticCounts
	walk(document, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		switch node.Data {
		case "table":
			result.tables++
		case "caption", "figcaption":
			result.captions++
		case "ul", "ol":
			result.lists++
		case "img":
			result.images++
		case "svg":
			result.svgs++
		}
		if attribute(node, "rowspan") != "" {
			result.rowspans++
		}
		if attribute(node, "colspan") != "" {
			result.colspans++
		}
	})
	return result
}

func TestAssembleDistinguishesSourceAbsentBookmark(t *testing.T) {
	output, plan, archived := flareGuide(t, true)
	record, _ := assembleGuide(t, output, plan, archived)
	if record.SourceAbsentLinks != 1 || len(record.Warnings) != 1 ||
		!strings.Contains(record.Warnings[0], "absent from source") {
		t.Fatalf("source-absent bookmark warning was lost: %+v", record)
	}
}

func TestAssembleUsesExactSourceAbsentWarningWhenBookmarkEvidenceIsBounded(t *testing.T) {
	output, plan, result := flareGuide(t, true)
	result.HTML.Topics[1].SourceBookmarks = nil
	result.HTML.Topics[1].SourceBookmarksComplete = false
	writeManifest(t, output, result.HTML)
	if record, _ := assembleGuide(t, output, plan, result); record.SourceAbsentLinks != 1 {
		t.Fatalf("bounded publisher-absent bookmark evidence was lost: %+v", record)
	}
	result.HTML.Warnings = nil
	writeManifest(t, output, result.HTML)
	root, err := os.OpenRoot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := Assemble(context.Background(), root, ".", plan, result, io.Discard); err == nil {
		t.Fatal("unrecorded missing bookmark was accepted")
	}
}

func TestAssembleAcceptsReconstructedInputObservationTimes(t *testing.T) {
	output, plan, result := flareGuide(t, false)
	archivedInput := model.SourceInput{
		RequestedURL: "https://docs.example.test/guide/Content/home.htm",
		FinalURL:     "https://docs.example.test/guide/Content/home.htm",
		Role:         "front",
		ContentType:  "text/html",
		SHA256:       strings.Repeat("a", 64),
		Size:         123,
		ObservedAt:   "2026-09-14T17:22:01Z",
	}
	result.HTML.Inputs = []model.SourceInput{archivedInput}
	writeManifest(t, output, result.HTML)
	reconstructedInput := archivedInput
	reconstructedInput.ObservedAt = "2026-09-14T18:22:01Z"
	plan.Inputs = []model.SourceInput{reconstructedInput}
	root, err := os.OpenRoot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := Assemble(context.Background(), root, ".", plan, result, io.Discard); err != nil {
		t.Fatalf("equivalent reconstructed plan was rejected: %v", err)
	}
	plan.Inputs[0].SHA256 = strings.Repeat("f", 64)
	if _, err := Assemble(context.Background(), root, ".", plan, result, io.Discard); err == nil {
		t.Fatal("plan with altered source hash was accepted")
	}
}

func TestAssembleRejectsMissingSourceFileAndLostBookmark(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, *model.ArchiveResult)
	}{
		{"file", func(t *testing.T, output string, result *model.ArchiveResult) {
			t.Helper()
			if err := os.Remove(filepath.Join(output, result.HTML.Assets[0].Path)); err != nil {
				t.Fatal(err)
			}
		}},
		{"bookmark", func(t *testing.T, output string, result *model.ArchiveResult) {
			t.Helper()
			index := 1
			record := &result.HTML.Topics[index]
			name := filepath.Join(output, record.Path)
			body, _ := os.ReadFile(name)
			body = bytes.Replace(body, []byte(`id="target"`), nil, 1)
			if err := os.WriteFile(name, body, 0o600); err != nil {
				t.Fatal(err)
			}
			sum := digest(body)
			record.SHA256, record.Size = sum, int64(len(body))
			data, _ := json.MarshalIndent(result.HTML, "", "  ")
			if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, plan, result := flareGuide(t, false)
			test.mutate(t, output, &result)
			root, _ := os.OpenRoot(output)
			defer root.Close()
			if _, err := Assemble(context.Background(), root, ".", plan, result, io.Discard); err == nil {
				t.Fatal("broken print source was accepted")
			}
		})
	}
}

func TestAssembleCancellationAndWriterFailureLeaveGuideUntouched(t *testing.T) {
	output, plan, result := flareGuide(t, false)
	manifest, _ := os.ReadFile(filepath.Join(output, "manifest.json"))
	root, _ := os.OpenRoot(output)
	defer root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Assemble(ctx, root, ".", plan, result, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	if _, err := Assemble(context.Background(), root, ".", plan, result, failingWriter{}); err == nil {
		t.Fatal("writer failure was hidden")
	}
	after, _ := os.ReadFile(filepath.Join(output, "manifest.json"))
	if !bytes.Equal(manifest, after) {
		t.Fatal("failed transient assembly mutated guide")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("injected write failure") }

func TestAssembleHPEOverviewAndURLAliases(t *testing.T) {
	output, plan, result := manualGuide(t, 2, "hpe")
	first := &result.HTML.Topics[0]
	second := &result.HTML.Topics[1]
	first.FetchURL = "https://support.example.test/api/document/A?ignorePayload=true"
	first.FinalURL = "https://support.example.test/documents/A/html/index.html"
	second.FetchURL = "https://support.example.test/api/document/A?page=second.html"
	second.FinalURL = "https://support.example.test/documents/A/html/second.html"
	plan.TOC = []model.TocEntry{{Title: "Overview", URL: first.FetchURL, Children: []model.TocEntry{{
		Title: "Second", URL: second.FinalURL + "#section",
	}}}}
	writeManifest(t, output, result.HTML)
	_, body := assembleGuide(t, output, plan, result)
	overview := bytes.Index(body, []byte(`class="print-overview"`))
	toc := bytes.Index(body, []byte(`class="print-toc"`))
	secondBody := bytes.LastIndex(body, []byte(`data-source-url="https://example.test/topic/1.html"`))
	if toc < 0 || overview <= toc || secondBody <= overview ||
		!bytes.Contains(body[:toc], []byte(`href="#topic-`)) ||
		strings.Count(string(body), `>Topic 0</h1>`) != 1 ||
		!bytes.Contains(body, []byte(`body class="print-source-hpe"`)) ||
		bytes.Contains(body, []byte(`body.print-source-hpe .print-topic :where(.print-content h1){font-size:16pt`)) {
		t.Fatalf("HPE overview ordering/identity changed:\n%s", body)
	}
}

func TestAssembleRebasesVerifiedCrossGuideNavigation(t *testing.T) {
	output, plan, result := manualGuide(t, 1, "flare")
	parent := t.TempDir()
	guide := filepath.Join(parent, "g1")
	if err := os.Rename(output, guide); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(parent, "g2", "pages")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "other.html"), []byte(`<h1 id="other">Other</h1>`), 0o600); err != nil {
		t.Fatal(err)
	}
	record := &result.HTML.Topics[0]
	name := filepath.Join(guide, filepath.FromSlash(record.Path))
	body, _ := os.ReadFile(name)
	body = bytes.Replace(body, []byte("</p>"), []byte(` <a href="../../g2/pages/other.html#other">cross</a></p>`), 1)
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	record.SHA256, record.Size = digest(body), int64(len(body))
	writeManifest(t, guide, result.HTML)
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var combined bytes.Buffer
	if _, err := Assemble(context.Background(), root, "g1", plan, result, &combined); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(combined.String(), `href="../g2/pages/other.html#other"`) {
		t.Fatalf("cross-guide link was not rebased: %s", combined.Bytes())
	}
}

func TestAssembleRejectsMalformedContentAndMissingSVGFragment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"content", func(body []byte) []byte {
			return bytes.Replace(body, []byte(`<main class="archive-content"`), []byte(`<main class="missing-content"`), 1)
		}},
		{"svg-fragment", func(body []byte) []byte {
			return bytes.Replace(body, []byte("</svg>"), []byte(`<use href="#missing"></use></svg>`), 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, plan, result := flareGuide(t, false)
			record := &result.HTML.Topics[0]
			name := filepath.Join(output, record.Path)
			body, _ := os.ReadFile(name)
			body = tc.change(body)
			if err := os.WriteFile(name, body, 0o600); err != nil {
				t.Fatal(err)
			}
			record.SHA256, record.Size = digest(body), int64(len(body))
			writeManifest(t, output, result.HTML)
			root, _ := os.OpenRoot(output)
			defer root.Close()
			if _, err := Assemble(context.Background(), root, ".", plan, result, io.Discard); err == nil {
				t.Fatal("invalid combined source was accepted")
			}
		})
	}
}

func TestAssembleLargeGuideGrowthIsLinear(t *testing.T) {
	measure := func(topics int) (Result, uint64) {
		output, plan, result := manualGuide(t, topics, "flare")
		root, err := os.OpenRoot(output)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		record, err := Assemble(context.Background(), root, ".", plan, result, io.Discard)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		return record, after.TotalAlloc - before.TotalAlloc
	}
	small, smallAlloc := measure(100)
	large, largeAlloc := measure(1000)
	if large.Size > small.Size*12 || largeAlloc > smallAlloc*15 {
		t.Fatalf("assembly growth is not linear: size %d->%d allocations %d->%d",
			small.Size, large.Size, smallAlloc, largeAlloc)
	}
	t.Logf("100 topics: %d bytes/%d allocated; 1000 topics: %d bytes/%d allocated",
		small.Size, smallAlloc, large.Size, largeAlloc)
}

func BenchmarkAssembleLargeGuide1000(b *testing.B) {
	output, plan, result := manualGuide(b, 1000, "flare")
	root, err := os.OpenRoot(output)
	if err != nil {
		b.Fatal(err)
	}
	defer root.Close()
	b.ReportAllocs()
	b.SetBytes(int64(len(result.HTML.Topics)))
	b.ResetTimer()
	for range b.N {
		if _, err := Assemble(context.Background(), root, ".", plan, result, io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

type testingTB interface {
	Helper()
	TempDir() string
	Fatal(...any)
}

func manualGuide(t testingTB, count int, kind string) (string, model.DocumentPlan, model.ArchiveResult) {
	t.Helper()
	output := t.TempDir()
	if err := os.Mkdir(filepath.Join(output, "pages"), 0o700); err != nil {
		t.Fatal(err)
	}
	generated := map[string][]byte{
		"index.html":      []byte("<!doctype html><html><body>index</body></html>"),
		"toc.html":        []byte("<!doctype html><html><body>toc</body></html>"),
		"search.json":     []byte("[]\n"),
		"search-index.js": []byte("window.X=[];\n"),
		"search.js":       []byte(`"use strict";`),
	}
	for name, body := range generated {
		if err := os.WriteFile(filepath.Join(output, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	document := model.Document{ID: "manual", Title: "Manual", Platform: "6300", Version: "10.16",
		URL: "https://example.test/topic/0.html", Kind: kind}
	archiveManifest := &model.HTMLArchive{SchemaVersion: 1, Status: "complete", SourceURL: document.URL,
		Inventory:   &model.InventoryEvidence{Complete: true, Kind: kind, RootURL: "https://example.test/", Entries: count, UniqueTopics: count},
		GeneratedAt: "2026-09-14T00:00:00Z", Warnings: []string{}, Errors: []string{}}
	plan := model.DocumentPlan{Document: document, Inventory: archiveManifest.Inventory}
	for index := range count {
		raw := "https://example.test/topic/" + fmt.Sprint(index) + ".html"
		title := "Topic " + fmt.Sprint(index)
		name := fmt.Sprintf("pages/topic-%05d.html", index)
		body := []byte(fmt.Sprintf(`<!doctype html><html><head></head><body><main class="archive-content"><h1 id="section">%s</h1><p><a href="%s#section">self</a></p></main></body></html>`, title, filepath.Base(name)))
		if err := os.WriteFile(filepath.Join(output, filepath.FromSlash(name)), body, 0o600); err != nil {
			t.Fatal(err)
		}
		archiveManifest.Topics = append(archiveManifest.Topics, model.FileRecord{URL: raw, Path: name, Status: "complete",
			Size: int64(len(body)), SourceSize: int64(len(body)), SHA256: digest(body), SourceSHA256: digest(body),
			SourceBookmarks: []string{"section"}, SourceBookmarksComplete: true})
		plan.Topics = append(plan.Topics, model.Topic{URL: raw, Title: title})
		plan.TOC = append(plan.TOC, model.TocEntry{Title: title, URL: raw + "#section"})
	}
	archiveManifest.Integrity = model.HTMLIntegrity{
		IndexSHA256: digest(generated["index.html"]), TOCSHA256: digest(generated["toc.html"]),
		SearchSHA256: digest(generated["search.json"]), SearchIndexSHA256: digest(generated["search-index.js"]),
		SearchJSSHA256: digest(generated["search.js"])}
	writeManifest(t, output, archiveManifest)
	return output, plan, model.ArchiveResult{Document: document, Status: "complete", Format: "html", HTML: archiveManifest}
}

func writeManifest(t testingTB, output string, manifest *model.HTMLArchive) {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
