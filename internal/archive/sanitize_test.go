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
	"golang.org/x/net/html"
)

// archivePage archives one Flare topic with extra fixture resources and
// returns the result with the generated topic page and its parsed content.
func archivePage(t *testing.T, body string, resources map[string]model.Resource) (model.ArchiveResult, string, []byte, *html.Node) {
	t.Helper()
	all := map[string]model.Resource{testHome: fixtureResource(testHome, "text/html", []byte(body))}
	for key, value := range resources {
		all[key] = value
	}
	fetcher := &archiveFixture{resources: all, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 4<<20)
	if err != nil || result.HTML == nil {
		t.Fatalf("archive failed: %+v %v", result, err)
	}
	page, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		return result, output, nil, nil
	}
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	content := findNode(doc, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "main" })
	return result, output, page, content
}

func activeMarkup(root *html.Node) []string {
	var found []string
	walk(root, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if n.Data == "script" || n.Namespace == "" && (contentRemovedElements[n.Data] || n.Data == "object") {
			found = append(found, "<"+n.Data+">")
		}
		for _, item := range n.Attr {
			key := strings.ToLower(item.Key)
			if strings.HasPrefix(key, "on") || key == "srcdoc" || key == "http-equiv" ||
				strings.Contains(strings.ToLower(item.Val), "javascript:") || strings.Contains(item.Val, "evil.example") {
				found = append(found, n.Data+"@"+key)
			}
		}
	})
	return found
}

func TestContentSanitizerKeepsOnlyRewrittenActiveMarkup(t *testing.T) {
	body := `<html><head><base href="https://docs.example.test/guide/Content/"></head><body><main id="mc-main-content"><h1>Home</h1>
<meta http-equiv="refresh" content="1;url=../assets/evil.bin"><base href="https://evil.example/">
<link rel="prefetch" href="https://evil.example/x.css"><template><p>Template</p></template>
<map name="m"><area shape="default" href="javascript:alert(1)"><area shape="rect" href="https://www.example.test/online">
<area shape="rect" href="home.htm#top"></map><img usemap="#m" alt="Map" src="data:image/png;base64,AAAA">
<p id="top" cite="https://evil.example/" longdesc="https://evil.example/" ping="https://evil.example/">Kept text</p>
<span class="CLI" href="https://evil.example/x.htm" formaction="https://evil.example/">CLI text</span>
<blockquote cite="https://evil.example/" onmouseover="alert(2)">Quoted</blockquote>
<div srcdoc="&lt;script&gt;alert(3)&lt;/script&gt;" data="https://evil.example/" background="data:text/html,x">Div text</div>
<math><mi href="javascript:alert(4)" xlink:href="javascript:alert(5)">x</mi></math>
<object><p>Object fallback</p></object></main></body></html>`
	result, _, page, content := archivePage(t, body, nil)
	if result.Status != "complete" {
		t.Fatalf("sanitized content should publish: %+v", result)
	}
	if found := activeMarkup(content); len(found) > 0 {
		t.Fatalf("active publisher markup survived: %v\n%s", found, page)
	}
	text := normalizedNodeText(content)
	for _, kept := range []string{"Kept text", "CLI text", "Quoted", "Div text", "Object fallback"} {
		if !strings.Contains(text, kept) {
			t.Fatalf("sanitizing lost publisher text %q: %s", kept, text)
		}
	}
	if strings.Contains(text, "Template") {
		t.Fatal("inert template content was rendered")
	}
	areas := matchingNodes(content, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "area" })
	if len(areas) != 3 || hasAttr(areas[0], "href") ||
		attr(areas[1], "href") != "https://www.example.test/online" || !hasClass(areas[1], "archive-online") ||
		!strings.HasSuffix(attr(areas[2], "href"), "#top") {
		t.Fatalf("area links were not routed through anchor rewriting: %s", page)
	}
	if image := findNode(content, func(n *html.Node) bool { return n.Data == "img" }); attr(image, "usemap") != "#m" {
		t.Fatal("fragment-only usemap was removed")
	}
}

func TestContentSanitizerContinuesAfterErrors(t *testing.T) {
	body := `<main id="mc-main-content"><h1>Home</h1>
<img class="MCPopupThumbnail" src="../Resources/thumb.png">
<div style="background:url(../Resources/missing.png)">First failure</div>
<p onclick="alert(1)">Later text</p><a href="javascript:alert(2)">Later link</a>
<iframe srcdoc="&lt;script&gt;alert(3)&lt;/script&gt;" src="https://evil.example/"></iframe>
<img src="https://evil.example/missing.svg" onerror="alert(4)" srcset="https://evil.example/x.svg 2x">
<object data="https://evil.example/x.swf"><p>Fallback</p></object>
<svg><use href="../Resources/missing.svg#shape"></use></svg></main>`
	result, _, page, content := archivePage(t, body, nil)
	if result.Status != "incomplete" || len(result.Errors) < 4 {
		t.Fatalf("failures were not recorded: %+v", result)
	}
	if found := activeMarkup(content); len(found) > 0 {
		t.Fatalf("an early failure stopped sanitizing: %v\n%s", found, page)
	}
	if use := findNode(content, func(n *html.Node) bool { return n.Data == "use" }); use == nil || hasAttr(use, "href") {
		t.Fatalf("an external SVG reference that failed to inline was kept: %s", page)
	}
	if !strings.Contains(normalizedNodeText(content), "Later text") || !strings.Contains(normalizedNodeText(content), "Fallback") {
		t.Fatalf("sanitizing after a failure lost text: %s", page)
	}

	deep := `<main id="mc-main-content"><h1>Deep</h1>` + strings.Repeat("<div>", maxHTMLDepth+5) +
		`<script>alert(5)</script>` + strings.Repeat("</div>", maxHTMLDepth+5) + `</main>`
	result, output, page, _ := archivePage(t, deep, nil)
	if result.Status != "incomplete" || page != nil ||
		!slices.ContainsFunc(result.Errors, func(e string) bool { return strings.Contains(e, "depth exceeds") }) {
		t.Fatalf("content that could not be sanitized was written: %+v", result)
	}
	if entries, _ := os.ReadDir(filepath.Join(output, "pages")); len(entries) != 0 {
		t.Fatalf("unsanitized page left on disk: %v", entries)
	}
}

func TestSVGSanitizerResolvesNamespacesForFilesAndInlineCopies(t *testing.T) {
	vector := testRoot + "Resources/diagram.svg"
	svg := `<?xml version="1.0"?><s:svg xmlns:s="http://www.w3.org/2000/svg" xmlns="http://www.w3.org/1999/xhtml"
 xmlns:h="http://www.w3.org/1999/xhtml" xmlns:x="http://www.w3.org/1999/xlink" xmlns:ink="http://www.inkscape.org/namespaces/inkscape"
 width="10" height="10" onload="alert(1)" ink:version="1"><h:script>alert(2)</h:script><s:script>alert(3)</s:script><script>alert(4)</script>
<s:title>Diagram <h:b>title</h:b></s:title>
<s:a x:href="javascript:alert(5)"><s:rect width="10" height="10" fill="url(#g)"/></s:a>
<s:a href="https://www.example.test/doc"><s:text>Online</s:text></s:a>
<s:a><s:animate attributeName="x:href" values="javascript:alert(6)"/><s:set attributeName="onclick" to="alert(7)"/>
<s:animate attributeName="opacity" values="0;1" dur="1s"/><s:text>Label</s:text></s:a>
<s:linearGradient id="g" x:href="https://evil.example/g.svg#g"><s:stop stop-color="red"/></s:linearGradient>
<s:switch><s:foreignObject width="5" height="5"><div><iframe srcdoc="&lt;script&gt;alert(8)&lt;/script&gt;"></iframe>HTML label</div></s:foreignObject>
<s:text>SVG label</s:text></s:switch>
<s:style>rect{fill:\75 rl(https://evil.example/beacon)}</s:style><s:rect style="fill:url(https://evil.example/p.png)" width="1" height="1"/></s:svg>`
	body := `<main id="mc-main-content"><h1>Home</h1><img alt="Diagram" src="../Resources/diagram.svg">
<p><a href="../Resources/diagram.svg">Full size</a></p></main>`
	result, output, page, content := archivePage(t, body, map[string]model.Resource{
		vector:                       fixtureResource(vector, "image/svg+xml", []byte(svg)),
		"https://evil.example/p.png": fixtureResource("https://evil.example/p.png", "image/png", tinyPNG),
	})
	if result.Status != "complete" {
		t.Fatalf("sanitized SVG should publish: %+v", result)
	}
	var file []byte
	for _, asset := range result.HTML.Assets {
		if strings.HasSuffix(asset.Path, ".svg") {
			file, _ = os.ReadFile(filepath.Join(output, asset.Path))
		}
	}
	standalone, _, err := parseSVGDocument(file)
	if err != nil {
		t.Fatalf("standalone SVG is not well-formed XML: %v\n%s", err, file)
	}
	inline := findNode(content, func(n *html.Node) bool { return n.Data == "svg" })
	for name, tree := range map[string]*html.Node{"standalone": standalone, "inline": inline} {
		if tree == nil {
			t.Fatalf("%s SVG missing: %s", name, page)
		}
		if problems := svgPolicyProblems(tree); len(problems) > 0 {
			t.Fatalf("%s SVG violates the policy: %v", name, problems)
		}
		rendered, _ := renderNode(tree)
		for _, active := range []string{"script", "foreignObject", "iframe", "alert", "onload", "onclick", "evil.example", "inkscape"} {
			if bytes.Contains(rendered, []byte(active)) {
				t.Fatalf("%s SVG kept %q: %s", name, active, rendered)
			}
		}
		for _, kept := range []string{"SVG label", "Online", "Label", "Diagram title", "linearGradient", `values="0;1"`} {
			if !bytes.Contains(rendered, []byte(kept)) {
				t.Fatalf("%s SVG lost %q: %s", name, kept, rendered)
			}
		}
	}
	if !bytes.Contains(file, []byte(`href="https://www.example.test/doc"`)) {
		t.Fatalf("standalone SVG lost its online link: %s", file)
	}
	if !slices.ContainsFunc(result.Warnings, func(w string) bool { return strings.Contains(w, "foreignObject") }) {
		t.Fatalf("foreignObject fallback was not reported: %v", result.Warnings)
	}
}

func TestSVGForeignObjectWithoutFallbackFailsTheGuide(t *testing.T) {
	body := `<main id="mc-main-content"><h1>Home</h1><svg><foreignObject width="5" height="5"><div>Only label</div></foreignObject></svg>
<svg><foreignObject width="5" height="5"></foreignObject><rect width="1" height="1"></rect></svg></main>`
	result, _, page, content := archivePage(t, body, nil)
	if result.Status != "incomplete" || !slices.ContainsFunc(result.Errors, func(e string) bool {
		return strings.Contains(e, "substantive SVG foreignObject")
	}) || len(result.Errors) != 1 {
		t.Fatalf("substantive foreignObject was not reported once: %+v", result.Errors)
	}
	if findNode(content, func(n *html.Node) bool {
		return n.Data == "foreignObject" || n.Data == "div" && n.Parent.Data != "main"
	}) != nil {
		t.Fatalf("foreignObject HTML survived: %s", page)
	}
}

func TestAssetsOnlyKeepVerifiedExtensions(t *testing.T) {
	for _, test := range []struct {
		url, contentType, body, want string
	}{
		{testRoot + "x.xhtml", "application/octet-stream", `<html xmlns="http://www.w3.org/1999/xhtml"><script>x</script></html>`, ".bin"},
		{testRoot + "x.js", "application/javascript", `alert(1)`, ".bin"},
		{testRoot + "x.xml", "application/xml", `<x/>`, ".bin"},
		{testRoot + "x", "text/xml", `<x/>`, ".bin"},
		{testRoot + "x.ttf", "application/octet-stream", "\x00\x01\x00\x00", ".ttf"},
		{testRoot + "x.png", "application/octet-stream", string(tinyPNG), ".png"},
		{testRoot + "x.eot", "application/vnd.ms-fontobject", "eot", ".bin"},
	} {
		kind := classifyAsset(test.url, test.contentType, []byte(test.body), dependencyAsset)
		if got := assetExtension(kind); got != test.want {
			t.Fatalf("%s %s: got %s, want %s", test.url, test.contentType, got, test.want)
		}
	}

	evil := testRoot + "Content/evil.xhtml"
	body := `<main id="mc-main-content"><h1>Home</h1><div style="background:url(evil.xhtml)">Styled</div>
<p><a class="mcpopupthumbnaillink" href="evil.xhtml">Thumbnail</a></p></main>`
	result, output, page, content := archivePage(t, body, map[string]model.Resource{
		evil: fixtureResource(evil, "application/octet-stream", []byte(`<html xmlns="http://www.w3.org/1999/xhtml"><script>x</script></html>`)),
	})
	if len(result.HTML.Assets) != 1 || !strings.HasSuffix(result.HTML.Assets[0].Path, ".bin") {
		t.Fatalf("unverified asset kept a renderable extension: %+v", result.HTML.Assets)
	}
	if entries, _ := os.ReadDir(filepath.Join(output, "assets")); len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".bin" {
		t.Fatalf("unexpected stored assets: %v", entries)
	}
	link := findNode(content, func(n *html.Node) bool { return n.Data == "a" && hasClass(n, "mcpopupthumbnaillink") })
	if result.Status != "incomplete" || link == nil || hasAttr(link, "href") ||
		!slices.ContainsFunc(result.Errors, func(e string) bool { return strings.Contains(e, "not a verified image") }) {
		t.Fatalf("link to unverified bytes was kept: %+v\n%s", result.Errors, page)
	}
}

func TestCSSEscapesCannotBypassRewriting(t *testing.T) {
	for _, test := range []struct {
		name, input string
		inline      bool
		want        []string
		omitted     bool
	}{
		{"escaped url function", `a{background:\75 rl(https://evil.example/a.png)}`, false, nil, true},
		{"escaped url in style attribute", `background:\55 RL(https://evil.example/a.png);color:red`, true, nil, true},
		{"escaped import", `@\69mport "https://evil.example/b.css";`, false, []string{"https://evil.example/b.css"}, false},
		{"escaped media rule", `@\6d edia screen{a{background:url(https://evil.example/c.png)}}`, false, []string{"https://evil.example/c.png"}, false},
		{"unknown at-rule", `@container (min-width:1px){a{background:url(https://evil.example/d.png)}}`, false, []string{"https://evil.example/d.png"}, false},
		{"escaped image-set", `a{background:\69mage-set("https://evil.example/e.png" 1x)}`, false, []string{"https://evil.example/e.png"}, false},
		{"image-set with type()", `a{background:image-set("f.png" type("image/png") 1x,"https://evil.example/g.png" 2x)}`, false,
			[]string{"f.png", "https://evil.example/g.png"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var seen []string
			output, diagnostics, err := rewriteCSS([]byte(test.input), test.inline, func(reference string, _ dependencyKind) (string, error) {
				seen = append(seen, reference)
				return "local.png", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(seen, test.want) {
				t.Fatalf("resolved %v, want %v", seen, test.want)
			}
			if bytes.Contains(output, []byte("evil")) || bytes.Contains(output, []byte(`\75`)) || bytes.Contains(output, []byte(`\55`)) {
				t.Fatalf("remote or escaped reference survived: %s", output)
			}
			if test.omitted != (len(diagnostics.Warnings) > 0) {
				t.Fatalf("omission diagnostics %v for %s", diagnostics.Warnings, output)
			}
			if problem := cssReferenceProblem(output); problem != "" {
				t.Fatalf("rewritten CSS fails output validation: %s", problem)
			}
			if problem := cssReferenceProblem([]byte(test.input)); problem == "" {
				t.Fatalf("output validation accepts the unrewritten input %s", test.input)
			}
		})
	}
	if problem := cssReferenceProblem([]byte(`@namespace MadCap url(http://www.madcapsoftware.com/Schemas/MadCap.xsd);a{background:url("../assets/x.png")}`)); problem != "" {
		t.Fatalf("namespace URI or local reference rejected: %s", problem)
	}
}

func TestOutputValidationRejectsActiveMarkup(t *testing.T) {
	generatedHead := `<head><meta charset="utf-8">` + guideIndexCSP + `<base target="_top"><title>t</title>` +
		`<link rel="stylesheet" href="toc.html"><script src="toc.html"></script><style>a{color:red}</style></head>`
	for _, test := range []struct {
		name, markup string
		want         string
	}{
		{"generated markup", `<a href="https://docs.example.test/">Online</a><a href="mailto:x@example.test">Mail</a>` +
			`<input data-guide-search type="search"><svg><a href="#x"><rect id="x" fill="url(#x)"></rect></a></svg>`, ""},
		{"meta refresh", `<meta http-equiv="refresh" content="0;url=toc.html">`, "active <meta>"},
		{"base href", `<base href="https://evil.example/">`, "active <base>"},
		{"body script", `<script src="toc.html"></script>`, "active <script>"},
		{"form", `<form action="toc.html"><input name="x"></form>`, "active <form>"},
		{"event handler", `<p onclick="alert(1)">x</p>`, "event handler"},
		{"javascript link", `<a href="javascript:alert(1)">x</a>`, "unsanitized href"},
		{"javascript area", `<map><area href="javascript:alert(1)"></map>`, "unsanitized href"},
		{"data link", `<a href="data:text/html,x">x</a>`, "unsanitized href"},
		{"srcdoc", `<div srcdoc="x">x</div>`, "unsanitized srcdoc"},
		{"remote image", `<img src="https://evil.example/x.png">`, "unsanitized src"},
		{"remote srcset", `<img srcset="toc.html 1x, https://evil.example/x.png 2x">`, "unsanitized srcset"},
		{"mathml href", `<math><mi href="javascript:alert(1)">x</mi></math>`, "unsanitized href"},
		{"remote style", `<p style="background:url(https://evil.example/x.png)">x</p>`, "not local"},
		{"escaped style", `<p style="background:\75 rl(toc.html)">x</p>`, "unrewritten CSS function"},
		{"stylesheet import", `<style>@import "https://evil.example/x.css";</style>`, "not local"},
		{"svg script", `<svg><script>alert(1)</script></svg>`, "unsupported SVG element <script>"},
		{"svg foreignObject", `<svg><foreignObject><p>x</p></foreignObject></svg>`, "unsupported SVG element <foreignObject>"},
		{"svg link", `<svg><a href="javascript:alert(1)"><text>x</text></a></svg>`, "reference is not local"},
		{"svg xlink", `<svg><a xlink:href="javascript:alert(1)"><text>x</text></a></svg>`, "reference is not local"},
		{"svg event", `<svg onload="alert(1)"></svg>`, "unsupported SVG attribute onload"},
		{"svg animation", `<svg><a><set attributeName="href" to="javascript:alert(1)"></set></a></svg>`, "unsupported SVG element <set>"},
		{"svg paint", `<svg><rect fill="url(https://evil.example/x.svg#p)"></rect></svg>`, "not local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			page := "<!doctype html><html>" + generatedHead + "<body>" + test.markup + "</body></html>"
			if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(page), 0o600); err != nil {
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
			joined := strings.Join(problems, "\n")
			if test.want == "" && len(problems) > 0 || test.want != "" && !strings.Contains(joined, test.want) {
				t.Fatalf("got %v, want problem %q", problems, test.want)
			}
		})
	}
}

func TestOutputValidationChecksStylesheetAndSVGAssets(t *testing.T) {
	for _, test := range []struct {
		name, path, body, want string
	}{
		{"local stylesheet", "assets/a.css", `@namespace m url(http://example.test/ns);a{background:url("b.png")}`, ""},
		{"remote stylesheet", "assets/a.css", `a{background:url(https://evil.example/x.png)}`, "not local"},
		{"escaped stylesheet", "assets/a.css", `@media print{a{background:\75 rl(b.png)}}`, "unrewritten CSS function"},
		{"local svg", "assets/a.svg", `<svg xmlns="http://www.w3.org/2000/svg"><a href="https://docs.example.test/"><text>x</text></a></svg>`, ""},
		{"prefixed script", "assets/a.svg", `<svg xmlns="http://www.w3.org/2000/svg" xmlns:h="http://www.w3.org/1999/xhtml"><h:script>alert(1)</h:script></svg>`, "unsupported SVG element <script>"},
		{"svg script link", "assets/a.svg", `<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><text>x</text></a></svg>`, "reference is not local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{"index.html": "<!doctype html><p>Index</p>", "toc.html": "<!doctype html><p>Contents</p>", test.path: test.body} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			archive := &model.HTMLArchive{Assets: []model.FileRecord{{Path: test.path, Status: "complete",
				SHA256: sourceHash([]byte(test.body)), Size: int64(len(test.body))}}}
			_, problems := validateOutput(root, archive, nil)
			joined := strings.Join(problems, "\n")
			if test.want == "" && len(problems) > 0 || test.want != "" && !strings.Contains(joined, test.want) {
				t.Fatalf("got %v, want problem %q", problems, test.want)
			}
		})
	}
}

func TestGeneratedPagesUseNarrowContentSecurityPolicies(t *testing.T) {
	result, output, page, _ := archivePage(t, `<main id="mc-main-content"><h1>Home</h1><p>Text</p></main>`, nil)
	if result.Status != "complete" {
		t.Fatalf("archive failed: %+v", result)
	}
	toc, _ := os.ReadFile(filepath.Join(output, "toc.html"))
	index, _ := os.ReadFile(filepath.Join(output, "index.html"))
	for name, test := range map[string]struct {
		body       []byte
		script     string
		frame      string
		hasScripts bool
	}{
		"topic": {page, "script-src 'none'", "frame-src 'self'", false},
		"toc":   {toc, "script-src 'none'", "frame-src 'none'", false},
		"index": {index, "script-src 'self'", "frame-src 'none'", true},
	} {
		for _, directive := range []string{test.script, test.frame, "base-uri 'none'", "form-action 'none'", "object-src 'none'", "default-src 'none'"} {
			if !bytes.Contains(test.body, []byte(directive)) {
				t.Fatalf("%s page policy lacks %s", name, directive)
			}
		}
		if bytes.Contains(test.body, []byte("<script")) != test.hasScripts {
			t.Fatalf("%s page script loading does not match its policy", name)
		}
	}
}

func TestReplaceFragmentURLs(t *testing.T) {
	ids := map[string]string{"a": "svg-1-a", "b": "svg-1-b"}
	got := replaceFragmentURLs(`url(#a) url(#b) url(#c) url(#a`, ids)
	if got != `url(#svg-1-a) url(#svg-1-b) url(#c) url(#a` {
		t.Fatalf("unexpected fragment rewrite %q", got)
	}
}
