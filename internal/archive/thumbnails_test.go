package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

func TestOldStyleThumbnailEmbedsAdvertisedOriginal(t *testing.T) {
	original := testRoot + "Resources/Images/full%20diagram.png?v=2"
	thumbnail := testRoot + "Resources/Images/diagram_thumb.png"
	body := `<main id="mc-main-content"><h1>Home</h1><p>
<a id="figure" class="MCPopupThumbnailLink MCPopupThumbnailHover figure-class"
href="../Resources/Images/full%20diagram.png?v=2" style="width:100px;max-height:100px;border:1px solid red" tabindex="0">
<picture><source srcset="../Resources/Images/other-thumb.png 2x">
<img id="diagram" class="MCPopupThumbnail img" alt="Full diagram" title="Figure title"
src="../Resources/Images/diagram_thumb.png" srcset="../Resources/Images/other-thumb.png 2x"
data-original="../Resources/Images/stale-thumb.png" data-srcset="../Resources/Images/stale-thumb.png 1x"
width="100" height="90" data-mc-width="900" data-mc-height="805" tabindex="0"
style="mc-thumbnail:hover;mc-thumbnail-max-height:100px;WIDTH:345px!important;height:355px;max-inline-size:100px;border:2px solid blue;--caption:'a;b'">
</picture></a><span>Figure caption remains here.</span></p></main>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome:  fixtureResource(testHome, "text/html", []byte(body)),
		original:  fixtureResource(original, "image/png", renderPNG),
		thumbnail: fixtureResource(thumbnail, "image/png", tinyPNG),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("full-image archive failed: %+v %v", result, err)
	}
	if len(result.HTML.Assets) != 1 || fetcher.calls[original] != 1 || fetcher.calls[thumbnail] != 0 {
		t.Fatalf("only the advertised original should be retrieved: assets=%v calls=%v", result.HTML.Assets, fetcher.calls)
	}
	data, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	image := findNode(doc, func(n *html.Node) bool { return attr(n, "id") == "diagram" })
	wrapper := findNode(doc, func(n *html.Node) bool { return attr(n, "id") == "figure" })
	if image == nil || image.Data != "img" || !hasClass(image, "archive-full-image") ||
		attr(image, "alt") != "Full diagram" || attr(image, "title") != "Figure title" ||
		wrapper == nil || wrapper.Data != "span" || !hasClass(wrapper, "figure-class") || hasAttr(wrapper, "href") {
		t.Fatalf("original image semantics/anchors were not preserved: %s", data)
	}
	for _, key := range []string{"width", "height", "srcset", "data-srcset", "data-original", "tabindex"} {
		if hasAttr(image, key) {
			t.Fatalf("thumbnail constraint %s remains", key)
		}
	}
	if strings.Contains(strings.ToLower(attr(image, "style")), "width:") ||
		strings.Contains(strings.ToLower(attr(image, "style")), "height:") ||
		strings.Contains(strings.ToLower(attr(image, "style")), "inline-size:") ||
		!strings.Contains(attr(image, "style"), "border:") ||
		!strings.Contains(attr(image, "style"), "--caption:") ||
		!strings.Contains(string(data), "Figure caption remains here.") {
		t.Fatalf("thumbnail style removal changed unrelated presentation: %s", data)
	}
	if findNode(doc, func(n *html.Node) bool {
		return n.Data == "source" || strings.Contains(strings.ToLower(attr(n, "class")), "mcpopupthumbnail")
	}) != nil {
		t.Fatal("popup or responsive thumbnail selection remains")
	}
	saved, err := os.ReadFile(filepath.Join(output, result.HTML.Assets[0].Path))
	if err != nil || !bytes.Equal(saved, renderPNG) {
		t.Fatal("original image bytes changed")
	}
}

func TestOldStyleThumbnailMissingOriginalIsNotComplete(t *testing.T) {
	for _, href := range []string{"../Resources/missing.png", ""} {
		t.Run(href, func(t *testing.T) {
			body := `<main id="mc-main-content"><h1>Home</h1><a class="mcpopupthumbnaillink" href="` + href +
				`"><img class="mcpopupthumbnail" src="../Resources/thumb.png"></a></main>`
			fetcher := &archiveFixture{resources: map[string]model.Resource{
				testHome:                         fixtureResource(testHome, "text/html", []byte(body)),
				testRoot + "Resources/thumb.png": fixtureResource(testRoot+"Resources/thumb.png", "image/png", renderPNG),
			}, calls: map[string]int{}}
			result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
			if err == nil && (result.Status == "complete" || len(result.Errors) == 0) {
				t.Fatalf("unavailable original silently became a complete thumbnail archive: %+v", result)
			}
		})
	}
}

func TestOldStyleStandaloneThumbnailUsesExplicitOriginal(t *testing.T) {
	for _, attribute := range []string{"data-mc-popup-src", "data-original"} {
		t.Run(attribute, func(t *testing.T) {
			original := testRoot + "Resources/original.png"
			body := `<main id="mc-main-content"><h1>Home</h1><img class="mcpopupthumbnail img" ` +
				attribute + `="../Resources/original.png" src="../Resources/thumb.png" width="100" height="90"></main>`
			fetcher := &archiveFixture{resources: map[string]model.Resource{
				testHome: fixtureResource(testHome, "text/html", []byte(body)),
				original: fixtureResource(original, "image/png", renderPNG),
			}, calls: map[string]int{}}
			output := t.TempDir()
			result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20)
			if err != nil || result.Status != "complete" || fetcher.calls[original] != 1 ||
				len(result.HTML.Assets) != 1 {
				t.Fatalf("explicit full-size metadata was not honored: %+v %v", result, err)
			}
		})
	}
}

func TestOldStyleThumbnailOriginalSVGIsInlined(t *testing.T) {
	vector := testRoot + "Resources/full.svg"
	body := `<main id="mc-main-content"><h1>Home</h1><a class="MCPopupThumbnailLink" href="../Resources/full.svg">
<img class="MCPopupThumbnail" alt="Vector diagram" src="../Resources/thumb.png" width="50" height="40"></a></main>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(body)),
		vector: fixtureResource(vector, "image/svg+xml", []byte(
			`<svg xmlns="http://www.w3.org/2000/svg" width="900" height="600" viewBox="0 0 900 600"><path id="shape" d="M0 0L900 600"/></svg>`)),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("original SVG archive failed: %+v %v", result, err)
	}
	data, _ := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	doc, _ := html.Parse(bytes.NewReader(data))
	svg := findNode(doc, func(n *html.Node) bool { return n.Data == "svg" })
	if svg == nil || !hasClass(svg, "archive-full-image") || attr(svg, "width") != "900" ||
		attr(svg, "height") != "600" || attr(svg, "aria-label") != "Vector diagram" {
		t.Fatalf("original SVG inherited thumbnail sizing: %s", data)
	}
}

func TestThumbnailExpansionDoesNotAlterNewStyleOrOrdinaryImages(t *testing.T) {
	plan := hpeArchivePlan()
	resources := hpeResources()
	front := plan.Topics[0].FetchURL
	png := "https://support.example.test/full.png"
	thumb := "https://support.example.test/thumb.png"
	resources[front] = fixtureResource(front, "multiPage;charset=UTF-8", []byte(
		`<main class="ditasrc"><h1 id="front">Front</h1><a class="MCPopupThumbnailLink" href="`+png+
			`"><img class="MCPopupThumbnail" src="`+thumb+`" width="50" height="40"></a></main>`))
	resources[png] = fixtureResource(png, "image/png", renderPNG)
	resources[thumb] = fixtureResource(thumb, "image/png", renderPNG)
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, &archiveFixture{resources: resources, calls: map[string]int{}},
		output, false, 2<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("new-style fixture failed: %+v %v", result, err)
	}
	data, _ := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	doc, _ := html.Parse(bytes.NewReader(data))
	image := findNode(doc, func(n *html.Node) bool { return n.Data == "img" && hasClass(n, "MCPopupThumbnail") })
	if image == nil || attr(image, "width") != "50" || attr(image, "height") != "40" {
		t.Fatal("old-style expansion changed new-style image behavior")
	}

	resources = map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><a href="../Resources/full.png"><img src="../Resources/thumb.png" width="50"></a></main>`)),
		testRoot + "Resources/full.png":  fixtureResource(testRoot+"Resources/full.png", "image/png", renderPNG),
		testRoot + "Resources/thumb.png": fixtureResource(testRoot+"Resources/thumb.png", "image/png", renderPNG),
	}
	output = t.TempDir()
	result, err = HTML(context.Background(), flarePlan(), &archiveFixture{resources: resources, calls: map[string]int{}},
		output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("ordinary image fixture failed: %+v %v", result, err)
	}
	data, _ = os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	doc, _ = html.Parse(bytes.NewReader(data))
	image = findNode(doc, func(n *html.Node) bool { return n.Data == "img" })
	if image == nil || attr(image, "width") != "50" || image.Parent.Data != "a" {
		t.Fatal("unmarked ordinary image/link was changed")
	}
}
