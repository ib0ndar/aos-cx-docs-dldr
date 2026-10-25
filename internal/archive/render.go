package archive

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"maps"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
	"golang.org/x/net/html"
)

const archiveStyles = `
:root{font-family:"Open Sans",Arial,sans-serif;color:#222;background:#fff}
*{box-sizing:border-box}body{margin:0}.archive-header{background:#173f5f;color:#fff;padding:1rem 1.5rem}
.archive-header a{color:#fff}.archive-layout{display:grid;grid-template-columns:minmax(16rem,24rem) minmax(0,1fr);min-height:calc(100vh - 4rem)}
.archive-toc{border-right:1px solid #ccc;padding:1rem;overflow:auto}.archive-toc iframe{border:0;width:100%;height:calc(100vh - 10rem)}
.archive-content{min-width:0;max-width:90rem;padding:1.5rem 2rem}.archive-content img,.archive-content svg{max-width:100%;height:auto}
.archive-image-placeholder{display:flex;box-sizing:border-box;align-items:center;justify-content:center;max-width:100%;min-width:3rem;min-height:2.5rem;max-height:12rem;padding:.6rem;border:1px dashed #8a6d3b;border-radius:.25rem;background:#fff8e5;color:#533f1f;font-size:.9rem;line-height:1.3;text-align:center;overflow:hidden}
.archive-image-placeholder::before{content:"[!]";margin-right:.4rem;font-weight:700}
.archive-image-placeholder-small{min-width:0;min-height:0;padding:.1rem;font-size:0}
.archive-image-placeholder-small::before{margin:0;font-size:.8rem}
:where(.archive-content table){max-width:100%}
.archive-content pre,.archive-content code{white-space:pre;overflow:auto}.archive-content pre{max-width:100%}
.archive-source-flare .archive-content .archive-full-image,
.archive-source-flare .archive-content .archive-full-image-container{display:block;width:auto!important;height:auto!important;
min-width:0!important;min-height:0!important;max-width:100%!important;max-height:none!important}
.archive-nav{display:flex;gap:1rem;justify-content:space-between;padding:1rem 2rem;border-top:1px solid #ccc}
.archive-online::after{content:" ↗";font-size:.8em}.archive-footer{padding:1rem 2rem;color:#555}
.archive-overview{margin:0 0 1rem;padding:0 0 1rem;border-bottom:1px solid #ccc}
.toc-tree,.toc-tree ul{list-style:none;padding-left:1rem}.toc-tree a:target{font-weight:bold;background:#e8f3ff}
body.archive-source-hpe{font-family:"HPE Graphik",Arial,sans-serif}
body.archive-source-hpe .archive-nav{min-height:194px;box-sizing:border-box;display:flex;align-items:center}
body.archive-source-hpe .archive-toc{height:calc(100vh - 194px)}
body.archive-source-hpe .archive-content{box-sizing:border-box;padding:77px 12px 1.5rem 28px;font-family:"HPE Graphik",Arial,sans-serif}
body.archive-source-hpe .archive-content table.table{margin-top:27px!important}
body.archive-source-hpe .archive-content table code.codeph{display:inline-block;max-width:min(60ch,calc(100vw - 3rem));overflow-x:auto;
white-space:nowrap!important;word-break:normal!important;overflow-wrap:normal!important}
body.archive-source-hpe .archive-content caption,body.archive-source-hpe .archive-content figcaption{font-size:17.6px;font-weight:600;line-height:19.8px;margin-top:20px}
details:not([open])>:not(summary){display:none!important}
@media(max-width:55rem){.archive-layout{display:block}.archive-toc{border-right:0;border-bottom:1px solid #ccc}.archive-toc iframe{height:16rem}
body.archive-source-hpe .archive-nav{min-height:auto}body.archive-source-hpe .archive-toc{height:24rem}body.archive-source-hpe .archive-content{padding:1.5rem}}
`

// Every generated page gets the narrowest policy it needs. No page needs a
// base URL, forms or plugins. Only the guide index runs scripts, its own
// search files, and only topic pages frame the generated contents page.
const offlineCSPDirectives = `default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; connect-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'`

const (
	topicPageCSP  = `<meta http-equiv="Content-Security-Policy" content="` + offlineCSPDirectives + `; script-src 'none'; frame-src 'self'">`
	tocPageCSP    = `<meta http-equiv="Content-Security-Policy" content="` + offlineCSPDirectives + `; script-src 'none'; frame-src 'none'">`
	guideIndexCSP = `<meta http-equiv="Content-Security-Policy" content="` + offlineCSPDirectives + `; script-src 'self'; frame-src 'none'">`
)

const guideSearchJS = `"use strict";
document.addEventListener("DOMContentLoaded", () => {
  const input = document.querySelector("[data-guide-search]");
  const list = document.querySelector("[data-guide-results]");
  const status = document.querySelector("[data-guide-search-status]");
  if (!input || !list || !status) return;
  const records = Array.isArray(window.AOSCX_GUIDE_SEARCH) ? window.AOSCX_GUIDE_SEARCH : [];
  const render = () => {
    const q = input.value.trim().toLowerCase();
    list.replaceChildren();
    list.hidden = true;
    if (!q) {
      status.textContent = "Enter a search term to find topics.";
      return;
    }
    const matches = records.filter(r => (r.title + " " + r.text).toLowerCase().includes(q));
    status.textContent = matches.length > 100 ? "Showing the first 100 matching topics." :
      matches.length ? "" : "No matching topics.";
    list.hidden = matches.length === 0;
    for (const row of matches.slice(0, 100)) {
      let href = null;
      try { if (typeof row.url === "string" && !/^[a-z][a-z\d+.-]*:/i.test(row.url)) href = new URL(row.url, location.href).href; } catch {}
      if (!href || !href.startsWith(new URL(".", location.href).href)) continue;
      const li = document.createElement("li"), a = document.createElement("a");
      a.href = row.url; a.textContent = row.title; li.appendChild(a); list.appendChild(li);
    }
  };
  input.addEventListener("input", render); render();
});
`

type renderedTOC struct {
	overview template.HTML
	contents template.HTML
}

func (a *htmlArchiver) renderTOCLink(entry model.TocEntry) string {
	target := a.topicForReference(entry.URL, nil)
	if entry.URL != "" && target != nil {
		id := strings.TrimSuffix(fileName(target.topic.URL, "", ""), "-")
		href := target.path
		if parsed, err := url.Parse(entry.URL); err == nil && parsed.Fragment != "" {
			href += "#" + parsed.EscapedFragment()
		}
		return fmt.Sprintf(`<a id="topic-%s" href="%s">%s</a>`, template.HTMLEscapeString(id),
			template.HTMLEscapeString(href), template.HTMLEscapeString(entry.Title))
	}
	if entry.URL != "" && onlineURL(entry.URL) {
		return `<a class="archive-online" href="` + template.HTMLEscapeString(entry.URL) + `">` +
			template.HTMLEscapeString(entry.Title) + `</a>`
	}
	if entry.URL != "" {
		a.warnings = append(a.warnings, "TOC entry with an unsupported link target kept as text: "+boundedText(entry.URL, 200))
	}
	return "<span>" + template.HTMLEscapeString(entry.Title) + "</span>"
}

// onlineURL reports whether raw is an absolute http(s) URL, the only kind of
// online link that generated navigation may contain.
func onlineURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func (a *htmlArchiver) renderTOC() renderedTOC {
	result := renderedTOC{}
	entries := a.plan.TOC
	if a.plan.Document.Kind == "hpe" && len(entries) > 0 {
		front := a.topicForReference(entries[0].URL, nil)
		if front != nil {
			identity, ok := parseHPEReference(front.topic.URL, nil)
			if ok && identity.document == a.hpeDocument && identity.page == "index.html" {
				result.overview = template.HTML(`<p class="archive-overview"><strong>Guide overview:</strong> ` +
					a.renderTOCLink(entries[0]) + `</p>`)
				entries = append(slices.Clone(entries[0].Children), entries[1:]...)
			}
		}
	}
	var out strings.Builder
	out.WriteString(`<ul class="toc-tree">`)
	var renderEntries func([]model.TocEntry)
	renderEntries = func(entries []model.TocEntry) {
		for _, entry := range entries {
			out.WriteString("<li>")
			out.WriteString(a.renderTOCLink(entry))
			if len(entry.Children) > 0 {
				out.WriteString("<ul>")
				renderEntries(entry.Children)
				out.WriteString("</ul>")
			}
			out.WriteString("</li>")
		}
	}
	renderEntries(entries)
	var supplementary []*pageInput
	for _, page := range a.pages {
		if page.supplementary {
			supplementary = append(supplementary, page)
		}
	}
	if len(supplementary) > 0 {
		out.WriteString("<li><span>Supplementary topics</span><ul>")
		for _, page := range supplementary {
			fmt.Fprintf(&out, `<li><a id="topic-%s" href="%s">%s</a></li>`,
				template.HTMLEscapeString(strings.TrimSuffix(fileName(page.topic.URL, "", ""), "-")),
				template.HTMLEscapeString(page.path), template.HTMLEscapeString(page.title))
		}
		out.WriteString("</ul></li>")
	}
	out.WriteString("</ul>")
	result.contents = template.HTML(out.String())
	return result
}

func renderTOCPage(title string, toc renderedTOC) string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">` + tocPageCSP + `<base target="_top"><title>` +
		template.HTMLEscapeString(title) + ` - contents</title><style>` + archiveStyles +
		`body{padding:.5rem}</style></head><body><nav aria-label="Guide navigation">` +
		string(toc.overview) + `<h2>Contents</h2>` + string(toc.contents) + `</nav></body></html>`
}

func renderGuideIndex(document model.Document, toc renderedTOC) string {
	source := ""
	if onlineURL(document.URL) {
		source = `<p><a href="` + template.HTMLEscapeString(document.URL) + `">Publisher source</a></p>`
	}
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">` + guideIndexCSP + `<title>` + template.HTMLEscapeString(document.Title) +
		`</title><style>` + archiveStyles + `</style><script src="search-index.js"></script><script defer src="search.js"></script></head><body>` +
		`<header class="archive-header"><h1>` + template.HTMLEscapeString(document.Title) + `</h1><p>` +
		template.HTMLEscapeString(document.Platform+" / "+document.Version) + `</p></header><main class="archive-content">` +
		source + string(toc.overview) + `<h2>Contents</h2>` + string(toc.contents) +
		`<h2>Search</h2><label>Search archived topics <input data-guide-search type="search" aria-describedby="guide-search-status"></label>` +
		`<p id="guide-search-status" data-guide-search-status role="status">Enter a search term to find topics.</p>` +
		`<ul data-guide-results hidden></ul></main></body></html>`
}

func renderTopicPage(document model.Document, current, previous, next *pageInput, styles, inlineStyles []string, content string) []byte {
	var head strings.Builder
	for _, style := range styles {
		fmt.Fprintf(&head, `<link rel="stylesheet" href="%s">`, template.HTMLEscapeString(style))
	}
	for _, style := range inlineStyles {
		head.WriteString("<style>")
		head.WriteString(style)
		head.WriteString("</style>")
	}
	if current.sourceDate != "" {
		fmt.Fprintf(&head, `<meta name="dc.date" content="%s">`, template.HTMLEscapeString(current.sourceDate))
	}
	var nav strings.Builder
	if previous != nil {
		fmt.Fprintf(&nav, `<a rel="prev" href="%s">Previous: %s</a>`, template.HTMLEscapeString(localReference(current.path, previous.path)), template.HTMLEscapeString(previous.title))
	} else {
		nav.WriteString("<span></span>")
	}
	nav.WriteString(`<a href="../index.html">Guide index</a>`)
	if next != nil {
		fmt.Fprintf(&nav, `<a rel="next" href="%s">Next: %s</a>`, template.HTMLEscapeString(localReference(current.path, next.path)), template.HTMLEscapeString(next.title))
	} else {
		nav.WriteString("<span></span>")
	}
	footer := strings.Join(current.copyright, " ")
	if footer == "" {
		footer = "Archived from the publisher source without altering substantive document content."
	}
	tocID := strings.TrimSuffix(fileName(current.topic.URL, "", ""), "-")
	page := `<!doctype html><html lang="en"><head><meta charset="utf-8">` + topicPageCSP + `<title>` + template.HTMLEscapeString(current.title) +
		` - ` + template.HTMLEscapeString(document.Title) + `</title>` + head.String() + `<style>` + archiveStyles +
		`</style></head><body class="archive-source-` + template.HTMLEscapeString(document.Kind) + `"><header class="archive-header"><a href="../index.html">` +
		template.HTMLEscapeString(document.Title) + `</a></header><div class="archive-layout"><aside class="archive-toc">` +
		`<iframe title="Guide contents" src="../toc.html#topic-` +
		template.HTMLEscapeString(tocID) + `"></iframe></aside><main class="archive-content">` + content +
		`</main></div><nav class="archive-nav">` + nav.String() + `</nav><footer class="archive-footer"><p>` +
		template.HTMLEscapeString(footer) + `</p><p><a href="` + template.HTMLEscapeString(current.topic.URL) +
		`">View publisher source</a></p></footer></body></html>`
	return []byte(page)
}

type sourceBookmarkReference struct {
	referrer string
	target   string
	fragment string
}

type localBookmarkReference struct {
	source   string
	target   string
	fragment string
}

func normalizeBookmarkFragment(fragment string) string {
	// net/url has already decoded the fragment once at every call site.
	return fragment
}

func hasBookmark(ids map[string]bool, fragment string) bool {
	return ids[normalizeBookmarkFragment(fragment)]
}

func validateOutput(
	root *os.Root,
	archive *model.HTMLArchive,
	sourceAbsent map[localBookmarkReference]bool,
) (int, []string) {
	problems, err := validateOutputHashes(context.Background(), root, archive, nil)
	if err != nil {
		return 0, []string{err.Error()}
	}
	if len(problems) > 0 {
		return 0, problems
	}
	checked, problems, err := validateOutputReferences(
		context.Background(), root, archive, sourceAbsent, nil,
	)
	if err != nil {
		return 0, []string{err.Error()}
	}
	return checked, problems
}

func validateOutputHashes(
	ctx context.Context,
	root *os.Root,
	archive *model.HTMLArchive,
	progress func(),
) ([]string, error) {
	for _, record := range append(append([]model.FileRecord{}, archive.Topics...), archive.Assets...) {
		if record.Status != "complete" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hash, size, err := storage.Hash(root, record.Path, 1<<30)
		if err != nil || hash != record.SHA256 || size != record.Size {
			return []string{fmt.Sprintf("output hash mismatch for %s: %v", record.Path, err)}, nil
		}
		if progress != nil {
			progress()
		}
	}
	return nil, nil
}

func outputReferenceFiles(archive *model.HTMLArchive) map[string]bool {
	htmlFiles := map[string]bool{"index.html": true, "toc.html": true}
	for _, record := range archive.Topics {
		if record.Status != "complete" {
			continue
		}
		htmlFiles[record.Path] = true
	}
	return htmlFiles
}

func outputReferenceFileCount(archive *model.HTMLArchive) int {
	return len(outputReferenceFiles(archive))
}

func validateOutputReferences(
	ctx context.Context,
	root *os.Root,
	archive *model.HTMLArchive,
	sourceAbsent map[localBookmarkReference]bool,
	progress func(),
) (int, []string, error) {
	htmlFiles := outputReferenceFiles(archive)
	idCache := map[string]map[string]bool{}
	var problems []string
	checked := 0
	names := slices.Sorted(maps.Keys(htmlFiles))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return checked, problems, err
		}
		before := len(problems)
		file, err := root.Open(name)
		if err != nil {
			problems = append(problems, name+": "+err.Error())
			continue
		}
		doc, parseErr := html.Parse(io.LimitReader(file, 64<<20))
		closeErr := file.Close()
		if parseErr != nil || closeErr != nil {
			problems = append(problems, fmt.Sprintf("%s: invalid generated HTML: %v %v", name, parseErr, closeErr))
			continue
		}
		ids := sourceIDs(doc)
		idCache[name] = ids
		problems = append(problems, outputPolicyProblems(name, doc)...)
		walk(doc, func(node *html.Node) {
			if node.Type != html.ElementNode {
				return
			}
			for _, key := range []string{"href", "src"} {
				if node.Namespace == "" {
					if key == "href" && !slices.Contains([]string{"a", "area", "base", "link"}, node.Data) {
						continue
					}
					if key == "src" && !slices.Contains([]string{
						"audio", "embed", "frame", "iframe", "img", "input", "script", "source", "track", "video",
					}, node.Data) {
						continue
					}
				}
				value := strings.TrimSpace(attr(node, key))
				if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, "data:") ||
					strings.HasPrefix(value, "mailto:") {
					continue
				}
				parsed, err := url.Parse(value)
				if err != nil {
					problems = append(problems, name+": invalid generated reference "+value)
					continue
				}
				if parsed.IsAbs() {
					if key == "src" || node.Data == "link" || node.Data == "object" || node.Data == "source" {
						problems = append(problems, name+": required resource remains online "+value)
					}
					continue
				}
				target := path.Clean(path.Join(path.Dir(name), parsed.Path))
				if target == ".." || strings.HasPrefix(target, "../") {
					problems = append(problems, name+": generated reference escapes guide "+value)
					continue
				}
				if _, err := root.Stat(target); err != nil {
					problems = append(problems, name+": missing generated target "+value)
					continue
				}
				checked++
				if parsed.Fragment != "" && htmlFiles[target] {
					targetIDs := idCache[target]
					if targetIDs == nil {
						targetFile, err := root.Open(target)
						if err == nil {
							targetDoc, parseErr := html.Parse(io.LimitReader(targetFile, 64<<20))
							targetFile.Close()
							if parseErr == nil {
								targetIDs = sourceIDs(targetDoc)
								idCache[target] = targetIDs
							}
						}
					}
					reference := localBookmarkReference{
						source: name, target: target,
						fragment: normalizeBookmarkFragment(parsed.Fragment),
					}
					if targetIDs != nil && !hasBookmark(targetIDs, parsed.Fragment) &&
						!sourceAbsent[reference] {
						problems = append(problems, name+": local bookmark target is missing "+value)
					}
				}
			}
		})
		if len(problems) == before && progress != nil {
			progress()
		}
	}
	assetProblems, err := outputAssetProblems(ctx, root, archive)
	problems = append(problems, assetProblems...)
	return checked, problems, err
}

// outputPolicyProblems reports markup in a generated HTML file that could
// run script, load or submit to a URL outside the archive, or redirect the
// page. Content sanitizing removes all of it, so any finding is a gap that
// must fail the guide rather than reach a library.
func outputPolicyProblems(name string, doc *html.Node) []string {
	var problems []string
	var visit func(*html.Node, bool)
	visit = func(node *html.Node, head bool) {
		if node.Type == html.ElementNode {
			if node.Namespace == "svg" {
				for _, problem := range svgPolicyProblems(node) {
					problems = append(problems, name+": "+problem)
				}
				return
			}
			head = head || node.Namespace == "" && node.Data == "head"
			if !generatedElementAllowed(name, node, head) {
				problems = append(problems, name+": active <"+node.Data+"> element remains in archived output")
			}
			for _, item := range node.Attr {
				if problem := outputAttributeProblem(node, item, head); problem != "" {
					problems = append(problems, name+": "+problem)
				}
			}
			if node.Namespace == "" && node.Data == "style" {
				if problem := cssReferenceProblem([]byte(nodeText(node))); problem != "" {
					problems = append(problems, name+": stylesheet: "+problem)
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, head)
		}
	}
	visit(doc, false)
	return problems
}

// generatedElementAllowed reports whether node may appear where it is. Only
// the generated page head loads styles and the guide index search scripts,
// and only the topic page navigation frames the contents page.
func generatedElementAllowed(name string, node *html.Node, head bool) bool {
	if node.Namespace != "" {
		return true
	}
	switch node.Data {
	case "script":
		return head && name == "index.html" && attr(node, "src") != "" && node.FirstChild == nil
	case "meta":
		return head && !hasAttr(node, "http-equiv") ||
			head && strings.EqualFold(attr(node, "http-equiv"), "content-security-policy")
	case "base":
		return head && !hasAttr(node, "href")
	case "link":
		return head && slices.Contains(strings.Fields(strings.ToLower(attr(node, "rel"))), "stylesheet")
	case "iframe":
		return node.Parent != nil && hasClass(node.Parent, "archive-toc")
	case "input":
		return name == "index.html" && hasAttr(node, "data-guide-search")
	case "object", "embed", "applet":
		return false
	}
	return !contentRemovedElements[node.Data]
}

// outputAttributeProblem describes an attribute that sanitizing should have
// removed or rewritten to a local or explicit online target, or returns "".
func outputAttributeProblem(node *html.Node, item html.Attribute, head bool) string {
	key := strings.ToLower(item.Key)
	if item.Namespace != "" {
		key = item.Namespace + ":" + key
	}
	value := strings.TrimSpace(item.Val)
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(key, "on"):
		return "event handler attribute " + key + " on <" + node.Data + ">"
	case key == "style":
		if problem := cssReferenceProblem([]byte(item.Val)); problem != "" {
			return "style attribute on <" + node.Data + ">: " + problem
		}
		return ""
	case key == "http-equiv" && head && node.Data == "meta" && strings.EqualFold(value, "content-security-policy"):
		return ""
	case !contentURLAttributes[key] && !strings.HasPrefix(key, "xlink:"):
		return ""
	case node.Namespace != "":
		// MathML has no URL attributes that the archive supports.
	case key == "href":
		switch {
		case node.Data == "a" || node.Data == "area":
			if navigableReference(value) {
				return ""
			}
		case node.Data == "link" && head:
			if !strings.HasPrefix(lower, "data:") && localArchiveReference(value) {
				return ""
			}
		}
	case key == "src":
		switch node.Data {
		case "img", "source":
			if localArchiveReference(value) {
				return ""
			}
		case "iframe", "script":
			if !strings.HasPrefix(lower, "data:") && localArchiveReference(value) {
				return ""
			}
		}
	case key == "srcset":
		if node.Data == "img" || node.Data == "source" {
			candidates, err := parseSrcset(value)
			if err == nil && !slices.ContainsFunc(candidates, func(candidate []string) bool {
				return !localArchiveReference(candidate[0])
			}) {
				return ""
			}
		}
	case key == "background":
		if localArchiveReference(value) {
			return ""
		}
	case key == "usemap":
		if strings.HasPrefix(value, "#") {
			return ""
		}
	}
	return "unsanitized " + key + " attribute on <" + node.Data + ">: " + boundedText(value, 200)
}

// outputAssetProblems applies the same reference rules to archived
// stylesheets and standalone SVG files, which browsers open directly.
func outputAssetProblems(ctx context.Context, root *os.Root, archive *model.HTMLArchive) ([]string, error) {
	var problems []string
	for _, record := range archive.Assets {
		extension := strings.ToLower(path.Ext(record.Path))
		if record.Status != "complete" || extension != ".css" && extension != ".svg" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return problems, err
		}
		file, err := root.Open(record.Path)
		if err != nil {
			problems = append(problems, record.Path+": "+err.Error())
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(file, record.Size+1))
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			problems = append(problems, record.Path+": "+err.Error())
			continue
		}
		if extension == ".css" {
			if problem := cssReferenceProblem(body); problem != "" {
				problems = append(problems, record.Path+": "+problem)
			}
			continue
		}
		svg, _, err := parseSVGDocument(body)
		if err != nil {
			problems = append(problems, record.Path+": "+err.Error())
			continue
		}
		for _, problem := range svgPolicyProblems(svg) {
			problems = append(problems, record.Path+": "+problem)
		}
	}
	return problems, nil
}
