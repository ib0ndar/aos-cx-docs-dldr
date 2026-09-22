package publication

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
	xhtml "golang.org/x/net/html"
)

const (
	maxTopics      = 100_000
	maxAnchors     = 1_000_000
	maxDiagnostics = 100
	maxOutputBytes = int64(16 << 30)
)

const printCSS = `
@page{size:Letter;margin:.72in .68in}
@media screen{body{box-sizing:border-box;max-width:90rem;margin:0 auto;padding:2rem;color:#202124}.print-cover{min-height:60vh}}
html{color:#202124;background:#fff}
body{font-family:"Open Sans",Arial,sans-serif;font-size:10pt;line-height:1.35}
body.print-source-hpe{font-family:"HPE Graphik",Arial,sans-serif}
.print-cover{break-after:page;display:flex;flex-direction:column;justify-content:space-between;min-height:8.3in}
.print-cover h1{font-size:22pt}.print-cover .print-meta{color:#4b5563}
.print-destination-registry{position:absolute;left:0;top:0;width:1px;height:1px;overflow:hidden;clip-path:inset(50%)}
.print-destination-registry a{display:block;width:1px;height:1px}
.print-overview{break-after:page}.print-toc{break-before:page;break-after:page}
.print-toc ol{list-style:none;padding-left:1.1em}.print-toc>ol{padding-left:0}.print-toc li{margin:.2em 0}
.print-topic.print-chapter{break-before:page}.print-topic{min-width:0}
.print-topic.print-flare-homepage{break-after:page}
:where(.print-content h1,.print-content h2,.print-content h3){break-inside:avoid-page!important;break-after:avoid-page!important}
:where(.print-content p,.print-content li){orphans:3;widows:3}
body.print-source-flare .print-topic :where(.print-content h1){font-size:16pt!important;line-height:1.2!important}
body.print-source-flare .print-topic :where(.print-content h2){font-size:14pt!important;line-height:1.25!important}
body.print-source-flare .print-topic :where(.print-content h3){font-size:12pt!important;line-height:1.25!important}
:where(.print-content table){max-width:100%}
:where(.print-content img,.print-content svg){max-width:100%;height:auto}
.archive-image-placeholder{display:flex;box-sizing:border-box;align-items:center;justify-content:center;max-width:100%;min-width:3rem;min-height:2.5rem;max-height:12rem;padding:.6rem;border:1px dashed #8a6d3b;border-radius:.25rem;background:#fff8e5;color:#533f1f;font-size:.9rem;line-height:1.3;text-align:center;overflow:hidden;break-inside:avoid-page}
.archive-image-placeholder::before{content:"[!]";margin-right:.4rem;font-weight:700}
.archive-image-placeholder-small{min-width:0;min-height:0;padding:.1rem;font-size:0}
.archive-image-placeholder-small::before{margin:0;font-size:.8rem}
:where(.print-content pre,.print-content .pre,.print-content .codeblock,.print-content .screen){max-width:100%;overflow-x:auto;white-space:pre-wrap;overflow-wrap:normal}
:where(.print-content p.CLI){max-width:100%;overflow-x:auto}
body.print-source-flare :where(.print-content>#mc-main-content>.topichero){display:none!important;position:static!important}
:where(.print-content.print-flare-home-hero-removed>#mc-main-content>.container){margin-top:0!important}
:where(.print-content thead){display:table-header-group}
:where(.print-content tr,.print-content figure,.print-content img){break-inside:avoid-page}
body.print-source-hpe :where(.print-content table code.codeph){display:inline-block;max-width:min(60ch,calc(100vw - 3rem));overflow-x:auto;white-space:nowrap!important;word-break:normal!important;overflow-wrap:normal!important}
.print-provenance{break-before:page;color:#4b5563}
`

type topicInfo struct {
	record model.FileRecord
	full   string
	anchor string
	ids    map[string]string
	title  string
}

type assembler struct {
	ctx          context.Context
	root         *os.Root
	guide        string
	result       model.ArchiveResult
	plan         model.DocumentPlan
	archive      *model.HTMLArchive
	topics       []*topicInfo
	byPath       map[string]*topicInfo
	aliases      map[string]*topicInfo
	stylesheets  []string
	inlineStyles []string
	chapters     map[string]bool
	included     map[string]bool
	overview     string
	tocLinks     int
	checked      int
	absent       int
	warnings     []string
	anchorCount  int
}

type Result struct {
	SHA256               string
	Size                 int64
	SourceManifestSHA256 string
	TOCEntries           int
	CheckedLinks         int
	SourceAbsentLinks    int
	Warnings             []string
	Outline              OutlinePlan
	Footer               FooterSourcePlan
}

// Assemble streams transient renderer input from a publishable verified local
// guide. Resource URLs are relative to guide, so callers must use that
// directory as the renderer base. Assemble never fetches or mutates the guide.
func Assemble(ctx context.Context, root *os.Root, guide string, plan model.DocumentPlan, result model.ArchiveResult, output io.Writer) (record Result, err error) {
	if ctx == nil {
		return record, errors.New("print assembly requires a context")
	}
	if output == nil {
		return record, errors.New("print assembly requires an output writer")
	}
	if root == nil || (result.Status != "complete" && result.Status != "degraded") ||
		result.Format != "html" || result.HTML == nil || result.HTML.Status != result.Status ||
		len(result.HTML.Errors) != 0 || len(result.HTML.Topics) == 0 ||
		(result.Status == "complete" && len(result.MissingResources) != 0) ||
		(result.Status == "degraded" && (len(result.MissingResources) == 0 ||
			!equalJSON(result.MissingResources, result.HTML.MissingResources))) {
		return record, errors.New("print assembly requires a publishable verified HTML guide")
	}
	if plan.Document != result.Document || plan.Inventory == nil || !plan.Inventory.Complete || len(plan.Topics) == 0 {
		return record, errors.New("print assembly plan does not match the verified HTML guide")
	}
	if !equalSourceInputs(plan.Inputs, result.HTML.Inputs) || !equalJSON(plan.Inventory, result.HTML.Inventory) {
		return record, errors.New("print assembly plan provenance does not match the verified HTML guide")
	}
	if len(result.HTML.Topics) > maxTopics {
		return record, errors.New("print assembly topic count exceeds limit")
	}
	a := &assembler{ctx: ctx, root: root, guide: guide, result: result, plan: plan, archive: result.HTML,
		byPath: map[string]*topicInfo{}, aliases: map[string]*topicInfo{}, chapters: map[string]bool{}, included: map[string]bool{}}
	if err := a.verifyAndIndex(); err != nil {
		return record, err
	}
	sourceBytes, err := json.Marshal(result.HTML)
	if err != nil {
		return record, err
	}
	record.SourceManifestSHA256 = digest(sourceBytes)
	hash := sha256.New()
	counter := &countWriter{writer: io.MultiWriter(output, hash), limit: maxOutputBytes}
	buffer := bufio.NewWriterSize(counter, 128<<10)
	writeErr := a.render(buffer)
	flushErr := buffer.Flush()
	if err := errors.Join(writeErr, flushErr); err != nil {
		return record, err
	}
	if err := a.ctx.Err(); err != nil {
		return record, err
	}
	record.SHA256, record.Size = hex.EncodeToString(hash.Sum(nil)), counter.count
	record.TOCEntries, record.CheckedLinks, record.SourceAbsentLinks = a.tocLinks, a.checked, a.absent
	record.Warnings = slices.Clone(a.warnings)
	record.Outline, err = a.outlinePlan()
	if err != nil {
		return record, err
	}
	record.Footer, err = a.footerSourcePlan()
	if err != nil {
		return record, err
	}
	return record, nil
}

type countWriter struct {
	writer io.Writer
	count  int64
	limit  int64
}

func (w *countWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.limit-w.count {
		return 0, errors.New("combined print HTML exceeds size limit")
	}
	n, err := w.writer.Write(data)
	w.count += int64(n)
	return n, err
}

func (a *assembler) verifyAndIndex() error {
	var disk model.HTMLArchive
	if err := storage.ReadJSON(a.root, path.Join(a.guide, "manifest.json"), &disk); err != nil {
		return err
	}
	if !equalJSON(disk, *a.archive) {
		return errors.New("print source manifest does not match accepted archive metadata")
	}
	for _, generated := range []struct {
		path string
		hash string
	}{{"index.html", a.archive.Integrity.IndexSHA256}, {"toc.html", a.archive.Integrity.TOCSHA256},
		{"search.json", a.archive.Integrity.SearchSHA256}, {"search-index.js", a.archive.Integrity.SearchIndexSHA256},
		{"search.js", a.archive.Integrity.SearchJSSHA256}} {
		if err := a.verifyHash(generated.path, generated.hash, 64<<20); err != nil {
			return err
		}
	}
	anchors := map[string]bool{}
	titles := map[string]string{}
	planned := map[string]bool{}
	for _, topic := range a.plan.Topics {
		key, err := fetch.CanonicalURL(topic.URL)
		if err != nil {
			return err
		}
		titles[key] = topic.Title
		planned[key] = true
	}
	foundPlanned := map[string]bool{}
	missingByPage := map[string]map[string]bool{}
	for _, missing := range a.archive.MissingResources {
		if missing.GeneratedPagePath == "" || missing.PlaceholderID == "" ||
			missing.GeneratedPageSHA256 == "" {
			return errors.New("invalid image-placeholder provenance in print source")
		}
		if missingByPage[missing.GeneratedPagePath] == nil {
			missingByPage[missing.GeneratedPagePath] = map[string]bool{}
		}
		missingByPage[missing.GeneratedPagePath][missing.PlaceholderID] = true
	}
	for index := range a.archive.Topics {
		if err := a.ctx.Err(); err != nil {
			return err
		}
		record := a.archive.Topics[index]
		if record.Status != "complete" || record.Path == "" || record.URL == "" {
			return fmt.Errorf("invalid print topic record %q", record.Path)
		}
		if err := a.verifyHash(record.Path, record.SHA256, 1<<30); err != nil {
			return err
		}
		anchor, err := topicAnchor(record.URL)
		if err != nil {
			return err
		}
		if anchors[anchor] {
			return fmt.Errorf("combined print topic anchor collision for %s", record.URL)
		}
		anchors[anchor] = true
		key, _ := fetch.CanonicalURL(record.URL)
		if !record.Supplementary && !planned[key] {
			return fmt.Errorf("verified topic is absent from the authoritative print plan: %s", record.URL)
		}
		foundPlanned[key] = true
		info := &topicInfo{record: record, full: path.Join(a.guide, record.Path), anchor: anchor, ids: map[string]string{}, title: titles[key]}
		if a.byPath[info.full] != nil {
			return fmt.Errorf("duplicate print topic path %s", record.Path)
		}
		a.byPath[info.full] = info
		a.topics = append(a.topics, info)
		for _, raw := range []string{record.URL, record.FetchURL, record.FinalURL} {
			if raw == "" {
				continue
			}
			key, err := fetch.CanonicalURL(raw)
			if err != nil {
				return err
			}
			if existing := a.aliases[key]; existing != nil && existing != info {
				a.aliases[key] = nil
			} else if _, exists := a.aliases[key]; !exists {
				a.aliases[key] = info
			}
		}
		doc, err := a.parseTopic(info)
		if err != nil {
			return err
		}
		content := findElement(doc, "archive-content")
		if content == nil {
			return fmt.Errorf("archived topic has no .archive-content: %s", record.Path)
		}
		seen := map[string]int{}
		targets := map[string]string{}
		walk(content, func(node *xhtml.Node) {
			if node.Type != xhtml.ElementNode {
				return
			}
			if hasClass(node, "archive-image-placeholder") {
				delete(missingByPage[record.Path], attribute(node, "id"))
			}
			for _, key := range []string{"id", "name"} {
				value := attribute(node, key)
				if value == "" {
					continue
				}
				a.anchorCount++
				if a.anchorCount > maxAnchors {
					return
				}
				seen[value]++
				if _, ok := info.ids[value]; !ok {
					target := bookmarkAnchor(anchor, value)
					if previous := targets[target]; previous != "" && previous != value {
						a.anchorCount = maxAnchors + 1
						return
					}
					targets[target] = value
					info.ids[value] = target
				}
			}
		})
		if a.anchorCount > maxAnchors {
			return errors.New("combined print bookmark index exceeds limit")
		}
		if len(missingByPage[record.Path]) != 0 {
			return fmt.Errorf("archived topic is missing an expected image placeholder: %s", record.Path)
		}
		if err := a.collectHead(info, doc); err != nil {
			return err
		}
	}
	for key := range planned {
		if !foundPlanned[key] {
			return fmt.Errorf("authoritative print topic is absent from the verified archive: %s", key)
		}
	}
	for _, record := range a.archive.Assets {
		if record.Status != "complete" || record.Path == "" {
			return fmt.Errorf("invalid print asset record %q", record.Path)
		}
		if err := a.verifyHash(record.Path, record.SHA256, 1<<30); err != nil {
			return err
		}
	}
	a.indexTOC()
	return nil
}

func (a *assembler) verifyHash(relative, expected string, limit int64) error {
	hash, _, err := storage.Hash(a.root, path.Join(a.guide, relative), limit)
	if err != nil || expected == "" || hash != expected {
		return fmt.Errorf("print source integrity mismatch at %s: %w", relative, err)
	}
	return nil
}

func (a *assembler) parseTopic(info *topicInfo) (*xhtml.Node, error) {
	file, err := a.root.Open(info.full)
	if err != nil {
		return nil, err
	}
	doc, parseErr := xhtml.Parse(io.LimitReader(file, info.record.Size+1))
	closeErr := file.Close()
	if err := errors.Join(parseErr, closeErr); err != nil {
		return nil, fmt.Errorf("parse archived topic %s: %w", info.record.Path, err)
	}
	return doc, nil
}

func (a *assembler) collectHead(info *topicInfo, doc *xhtml.Node) error {
	var first error
	head := findTag(doc, "head")
	if head == nil {
		return nil
	}
	walk(head, func(node *xhtml.Node) {
		if first != nil || node.Type != xhtml.ElementNode {
			return
		}
		switch node.Data {
		case "link":
			if !slices.Contains(strings.Fields(strings.ToLower(attribute(node, "rel"))), "stylesheet") {
				return
			}
			local, err := a.rebaseRequired(info.full, attribute(node, "href"))
			if err != nil {
				first = err
				return
			}
			if !slices.Contains(a.stylesheets, local) {
				a.stylesheets = append(a.stylesheets, local)
			}
		case "style":
			text := nodeText(node)
			if strings.Contains(text, ".archive-layout{") {
				return
			}
			rewritten, err := a.rewriteCSS(info, []byte(text), false)
			if err != nil {
				first = err
				return
			}
			value := string(rewritten)
			if !slices.Contains(a.inlineStyles, value) {
				a.inlineStyles = append(a.inlineStyles, value)
			}
		}
	})
	return first
}

func (a *assembler) indexTOC() {
	entries := a.plan.TOC
	if a.result.Document.Kind == "hpe" && len(entries) > 0 {
		if topic := a.topicForURL(entries[0].URL); topic != nil {
			a.overview = topic.full
			entries = append(slices.Clone(entries[0].Children), entries[1:]...)
		}
	}
	for _, entry := range entries {
		if topic := a.topicForURL(entry.URL); topic != nil {
			a.chapters[topic.full] = true
		}
	}
}

func (a *assembler) render(out io.Writer) error {
	if err := a.ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintf(out, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; script-src 'none'; connect-src 'none'"><title>%s</title>`, stdhtml.EscapeString(a.result.Document.Title))
	for _, stylesheet := range a.stylesheets {
		fmt.Fprintf(out, `<link rel="stylesheet" href="%s">`, stdhtml.EscapeString(stylesheet))
	}
	for _, style := range a.inlineStyles {
		fmt.Fprintf(out, "<style>%s</style>", style)
	}
	fmt.Fprintf(out, "<style>%s</style></head><body class=\"print-source-%s\">", printCSS, stdhtml.EscapeString(a.result.Document.Kind))
	fmt.Fprintf(out, `<section class="print-cover"><div><h1>%s</h1><p class="print-meta">AOS-CX %s / Platform %s</p></div><p class="print-meta">Combined from the complete verified local HTML archive.</p></section>`,
		stdhtml.EscapeString(a.result.Document.Title), stdhtml.EscapeString(a.result.Document.Version), stdhtml.EscapeString(a.result.Document.Platform))
	fmt.Fprintf(out, `<nav class="print-destination-registry" aria-hidden="true"><a href="#%s"></a><a href="#%s"></a>`,
		OutlineContentsTarget, OutlineProvenanceTarget)
	for _, topic := range a.topics {
		fmt.Fprintf(out, `<a href="#%s"></a>`, stdhtml.EscapeString(topic.anchor))
	}
	_, _ = io.WriteString(out, "</nav>")
	if err := a.renderTOC(out); err != nil {
		return err
	}
	if a.overview != "" {
		if err := a.renderTopic(out, a.byPath[a.overview], "print-overview"); err != nil {
			return err
		}
	}
	for _, topic := range a.topics {
		if topic.full == a.overview {
			continue
		}
		class := "print-topic"
		if a.chapters[topic.full] {
			class += " print-chapter"
		}
		if err := a.renderTopic(out, topic, class); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, `<footer class="print-provenance" id="%s"><h2>Source and provenance</h2><p><a href="%s">Publisher source</a></p>`,
		OutlineProvenanceTarget, stdhtml.EscapeString(a.result.Document.URL))
	copyright := []string{}
	for _, topic := range a.topics {
		for _, value := range topic.record.Copyright {
			if value != "" && !slices.Contains(copyright, value) {
				copyright = append(copyright, value)
			}
		}
	}
	for _, value := range copyright {
		fmt.Fprintf(out, "<p>%s</p>", stdhtml.EscapeString(value))
	}
	_, err := io.WriteString(out, "</footer></body></html>")
	return err
}

func (a *assembler) renderTopic(out io.Writer, info *topicInfo, class string) error {
	if err := a.ctx.Err(); err != nil {
		return err
	}
	doc, err := a.parseTopic(info)
	if err != nil {
		return err
	}
	content := findElement(doc, "archive-content")
	if content == nil {
		return fmt.Errorf("archived topic has no .archive-content: %s", info.record.Path)
	}
	setAttribute(content, "class", "print-content")
	if removed, err := removeFlareHomepageHero(a.result.Document.Kind, content); err != nil {
		return fmt.Errorf("classify publisher print chrome in %s: %w", info.record.Path, err)
	} else if removed {
		setAttribute(content, "class", "print-content print-flare-home-hero-removed")
		class += " print-flare-homepage"
	}
	seen := map[string]int{}
	var first error
	walk(content, func(node *xhtml.Node) {
		if first != nil || node.Type != xhtml.ElementNode {
			return
		}
		for _, key := range []string{"id", "name"} {
			old := attribute(node, key)
			if old == "" {
				continue
			}
			seen[old]++
			replacement := info.ids[old]
			if seen[old] > 1 {
				replacement += fmt.Sprintf("-%d", seen[old])
			}
			setAttribute(node, key, replacement)
		}
		if err := a.rewriteNode(info, node); err != nil {
			first = err
		}
	})
	if first != nil {
		return first
	}
	fmt.Fprintf(out, `<section class="%s" id="%s" data-source-url="%s">`, class, info.anchor, stdhtml.EscapeString(info.record.URL))
	if !hasHeading(content) {
		fmt.Fprintf(out, "<h1>%s</h1>", stdhtml.EscapeString(topicTitle(info)))
	}
	if err := xhtml.Render(out, content); err != nil {
		return err
	}
	_, err = io.WriteString(out, "</section>")
	return err
}

func removeFlareHomepageHero(kind string, content *xhtml.Node) (bool, error) {
	if kind != "flare" {
		return false, nil
	}
	var candidates []*xhtml.Node
	walk(content, func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode && node.Data == "div" && hasClass(node, "topichero") {
			candidates = append(candidates, node)
		}
	})
	if len(candidates) == 0 {
		return false, nil
	}
	if len(candidates) != 1 || !isFlareHomepageHero(candidates[0]) {
		return false, errors.New("unrecognized Flare topichero structure")
	}
	hero := candidates[0]
	hero.Parent.RemoveChild(hero)
	return true, nil
}

func isFlareHomepageHero(hero *xhtml.Node) bool {
	parent := hero.Parent
	if parent == nil || parent.Type != xhtml.ElementNode ||
		(parent.Data != "div" && parent.Data != "main") ||
		attribute(parent, "id") != "mc-main-content" {
		return false
	}
	elementChildren := 0
	for child := hero.FirstChild; child != nil; child = child.NextSibling {
		switch child.Type {
		case xhtml.ElementNode:
			elementChildren++
			if child.Data != "div" || !hasClass(child, "docname") {
				return false
			}
		case xhtml.TextNode:
			if strings.TrimSpace(child.Data) != "" {
				return false
			}
		case xhtml.CommentNode:
		default:
			return false
		}
	}
	return elementChildren == 1
}

func (a *assembler) renderTOC(out io.Writer) error {
	fmt.Fprintf(out, `<nav class="print-toc" id="%s" aria-label="Table of contents"><h1>Table of contents</h1>`, OutlineContentsTarget)
	entries := a.plan.TOC
	if a.overview != "" && len(entries) > 0 {
		entries = append(slices.Clone(entries[0].Children), entries[1:]...)
	}
	if err := a.renderTOCEntries(out, entries, 0); err != nil {
		return err
	}
	var extra []*topicInfo
	for _, topic := range a.topics {
		if topic.full != a.overview && !a.included[topic.full] {
			extra = append(extra, topic)
		}
	}
	if len(extra) > 0 {
		_, _ = io.WriteString(out, "<h2>Additional topics</h2><ol>")
		for _, topic := range extra {
			fmt.Fprintf(out, `<li><a href="#%s">%s</a></li>`, topic.anchor, stdhtml.EscapeString(topicTitle(topic)))
			a.tocLinks++
		}
		_, _ = io.WriteString(out, "</ol>")
	}
	_, err := io.WriteString(out, "</nav>")
	return err
}

func (a *assembler) renderTOCEntries(out io.Writer, entries []model.TocEntry, depth int) error {
	if depth > 1024 {
		return errors.New("print TOC nesting exceeds limit")
	}
	_, _ = io.WriteString(out, "<ol>")
	for _, entry := range entries {
		if err := a.ctx.Err(); err != nil {
			return err
		}
		_, _ = io.WriteString(out, "<li>")
		if topic := a.topicForURL(entry.URL); topic != nil {
			fragment := ""
			if parsed, err := url.Parse(entry.URL); err == nil {
				fragment = parsed.Fragment
			}
			target, err := a.topicDestination("TOC entry "+entry.URL, topic, fragment)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, `<a href="#%s">%s</a>`, target, stdhtml.EscapeString(entry.Title))
			a.included[topic.full] = true
			a.tocLinks++
		} else if entry.URL != "" {
			fmt.Fprintf(out, `<a class="print-online" href="%s">%s</a>`, stdhtml.EscapeString(entry.URL), stdhtml.EscapeString(entry.Title))
		} else {
			fmt.Fprintf(out, "<span>%s</span>", stdhtml.EscapeString(entry.Title))
		}
		if len(entry.Children) > 0 {
			if err := a.renderTOCEntries(out, entry.Children, depth+1); err != nil {
				return err
			}
		}
		_, _ = io.WriteString(out, "</li>")
	}
	_, err := io.WriteString(out, "</ol>")
	return err
}

func (a *assembler) rewriteNode(info *topicInfo, node *xhtml.Node) error {
	for _, key := range []string{"aria-labelledby", "aria-describedby"} {
		if value := attribute(node, key); value != "" {
			parts := strings.Fields(value)
			for index, part := range parts {
				if replacement := info.ids[part]; replacement != "" {
					parts[index] = replacement
				}
			}
			setAttribute(node, key, strings.Join(parts, " "))
		}
	}
	if value := attribute(node, "style"); value != "" {
		rewritten, err := a.rewriteCSS(info, []byte(value), true)
		if err != nil {
			return err
		}
		setAttribute(node, "style", string(rewritten))
	}
	if node.Data == "style" {
		rewritten, err := a.rewriteCSS(info, []byte(nodeText(node)), false)
		if err != nil {
			return err
		}
		for node.FirstChild != nil {
			node.RemoveChild(node.FirstChild)
		}
		node.AppendChild(&xhtml.Node{Type: xhtml.TextNode, Data: string(rewritten)})
	}
	for index := range node.Attr {
		value := node.Attr[index].Val
		for original, replacement := range info.ids {
			value = strings.ReplaceAll(value, "url(#"+original+")", "url(#"+replacement+")")
		}
		node.Attr[index].Val = value
	}
	if href := attribute(node, "href"); href != "" {
		switch {
		case node.Namespace == "svg" || node.Data == "link":
			if node.Namespace == "svg" && strings.HasPrefix(href, "#") && info.ids[strings.TrimPrefix(href, "#")] == "" {
				return fmt.Errorf("missing inline SVG fragment %s in %s", href, info.record.Path)
			}
			local, err := a.rewriteRequired(info, href)
			if err != nil {
				return err
			}
			setAttribute(node, "href", local)
		case node.Data == "a" || node.Data == "area":
			local, err := a.rewriteNavigation(info, href)
			if err != nil {
				return err
			}
			setAttribute(node, "href", local)
		}
	}
	if href := attribute(node, "xlink:href"); href != "" {
		if strings.HasPrefix(href, "#") && info.ids[strings.TrimPrefix(href, "#")] == "" {
			return fmt.Errorf("missing inline SVG fragment %s in %s", href, info.record.Path)
		}
		local, err := a.rewriteRequired(info, href)
		if err != nil {
			return err
		}
		setAttribute(node, "xlink:href", local)
	}
	for _, key := range []string{"src", "poster", "data"} {
		if value := attribute(node, key); value != "" && activeResourceAttribute(node, key) {
			local, err := a.rewriteRequired(info, value)
			if err != nil {
				return err
			}
			setAttribute(node, key, local)
		}
	}
	if srcset := attribute(node, "srcset"); srcset != "" {
		parts := strings.Split(srcset, ",")
		for index, candidate := range parts {
			fields := strings.Fields(candidate)
			if len(fields) == 0 || strings.HasPrefix(strings.ToLower(fields[0]), "data:") {
				continue
			}
			local, err := a.rewriteRequired(info, fields[0])
			if err != nil {
				return err
			}
			fields[0] = local
			parts[index] = strings.Join(fields, " ")
		}
		setAttribute(node, "srcset", strings.Join(parts, ", "))
	}
	return nil
}

func (a *assembler) rewriteNavigation(info *topicInfo, raw string) (string, error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "javascript:") {
		return "", errors.New("publisher JavaScript link remains in print source")
	}
	target, fragment, query, local, err := resolveLocal(info.full, raw)
	if err != nil {
		return "", err
	}
	if local {
		if topic := a.byPath[target]; topic != nil {
			destination, err := a.topicDestination(info.record.URL, topic, fragment)
			return "#" + destination, err
		}
		return a.rebaseAndVerify(target, query, fragment, true)
	}
	if topic := a.topicForURL(raw); topic != nil {
		parsed, _ := url.Parse(raw)
		destination, err := a.topicDestination(info.record.URL, topic, parsed.Fragment)
		return "#" + destination, err
	}
	return raw, nil
}

func (a *assembler) rewriteRequired(info *topicInfo, raw string) (string, error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "data:") {
		return raw, nil
	}
	target, fragment, query, local, err := resolveLocal(info.full, raw)
	if err != nil {
		return "", err
	}
	if !local {
		return "", fmt.Errorf("required print resource remains online: %s", raw)
	}
	if topic := a.byPath[target]; topic != nil {
		destination, err := a.topicDestination(info.record.URL, topic, fragment)
		return "#" + destination, err
	}
	return a.rebaseAndVerify(target, query, fragment, false)
}

func (a *assembler) rebaseRequired(owner, raw string) (string, error) {
	target, fragment, query, local, err := resolveLocal(owner, raw)
	if err != nil {
		return "", err
	}
	if !local {
		return "", fmt.Errorf("required print stylesheet remains online: %s", raw)
	}
	return a.rebaseAndVerify(target, query, fragment, false)
}

func (a *assembler) rebaseAndVerify(target, query, fragment string, navigation bool) (string, error) {
	info, err := a.root.Lstat(target)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("missing or unsafe print target %s: %w", target, err)
	}
	if fragment != "" && strings.EqualFold(path.Ext(target), ".svg") {
		ids, err := a.fileIDs(target)
		if err != nil || !ids[fragment] {
			return "", fmt.Errorf("missing SVG fragment #%s in %s", fragment, target)
		}
	}
	if navigation && fragment != "" && strings.EqualFold(path.Ext(target), ".html") {
		ids, err := a.fileIDs(target)
		if err != nil || !ids[fragment] {
			return "", fmt.Errorf("missing cross-guide HTML fragment #%s in %s", fragment, target)
		}
	}
	if !navigation && !withinGuide(a.guide, target) {
		return "", fmt.Errorf("print resource is outside the verified guide: %s", target)
	}
	relative, err := filepath.Rel(filepath.FromSlash(a.guide), filepath.FromSlash(target))
	if err != nil {
		return "", err
	}
	result := (&url.URL{Path: filepath.ToSlash(relative)}).EscapedPath()
	if query != "" {
		result += "?" + query
	}
	if fragment != "" {
		result += "#" + (&url.URL{Fragment: fragment}).EscapedFragment()
	}
	a.checked++
	return result, nil
}

func (a *assembler) topicDestination(referrer string, topic *topicInfo, fragment string) (string, error) {
	if fragment == "" {
		return topic.anchor, nil
	}
	if target := topic.ids[fragment]; target != "" {
		return target, nil
	}
	message := fmt.Sprintf("Publisher bookmark #%s is absent from source topic %s", fragment, topic.record.URL)
	sourceAbsent := topic.record.SourceBookmarksComplete && !slices.Contains(topic.record.SourceBookmarks, fragment)
	if !sourceAbsent && referrer != "" {
		sourceAbsent = slices.Contains(a.archive.Warnings, message)
	}
	if sourceAbsent {
		a.absent++
		warning := message
		if referrer != "" {
			warning += " (referenced from " + referrer + ")"
		}
		if len(a.warnings) < maxDiagnostics && !slices.Contains(a.warnings, warning) {
			a.warnings = append(a.warnings, warning)
		}
		return topic.anchor, nil
	}
	return "", fmt.Errorf("missing local print bookmark #%s in %s", fragment, topic.record.Path)
}

func (a *assembler) topicForURL(raw string) *topicInfo {
	if raw == "" {
		return nil
	}
	base, err := url.Parse(a.result.Document.URL)
	if err != nil {
		return nil
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	resolved := base.ResolveReference(ref)
	resolved.Fragment = ""
	key, err := fetch.CanonicalURL(resolved.String())
	if err != nil {
		return nil
	}
	return a.aliases[key]
}

func (a *assembler) rewriteCSS(info *topicInfo, body []byte, inline bool) ([]byte, error) {
	p := css.NewParser(parse.NewInput(bytes.NewReader(body)), inline)
	var out bytes.Buffer
	for {
		grammar, _, data := p.Next()
		if grammar == css.ErrorGrammar {
			if p.HasParseError() {
				return nil, p.Err()
			}
			if err := p.Err(); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return out.Bytes(), nil
		}
		values := append([]css.Token{}, p.Values()...)
		imageDepth := 0
		for index := range values {
			token := &values[index]
			switch token.TokenType {
			case css.FunctionToken:
				if strings.EqualFold(string(token.Data), "image-set(") ||
					strings.EqualFold(string(token.Data), "-webkit-image-set(") {
					imageDepth++
				}
			case css.RightParenthesisToken:
				if imageDepth > 0 {
					imageDepth--
				}
			}
			if grammar == css.BeginRulesetGrammar && token.TokenType == css.HashToken {
				name := strings.TrimPrefix(string(token.Data), "#")
				if replacement := info.ids[name]; replacement != "" {
					token.Data = []byte("#" + replacement)
				}
			}
			if token.TokenType != css.URLToken {
				if grammar == css.AtRuleGrammar && strings.EqualFold(strings.TrimSpace(string(data)), "@import") &&
					token.TokenType == css.StringToken {
					raw := strings.Trim(string(token.Data), `"'`)
					local, err := a.rewriteRequired(info, raw)
					if err != nil {
						return nil, err
					}
					token.Data = []byte(`"` + strings.ReplaceAll(local, `"`, `\"`) + `"`)
				}
				if imageDepth > 0 && token.TokenType == css.StringToken {
					raw := strings.Trim(string(token.Data), `"'`)
					local, err := a.rewriteRequired(info, raw)
					if err != nil {
						return nil, err
					}
					token.Data = []byte(`"` + strings.ReplaceAll(local, `"`, `\"`) + `"`)
				}
				continue
			}
			raw, err := cssURL(token.Data)
			if err != nil {
				return nil, err
			}
			local, err := a.rewriteRequired(info, raw)
			if err != nil {
				return nil, err
			}
			token.Data = []byte(`url("` + strings.ReplaceAll(local, `"`, `\"`) + `")`)
		}
		switch grammar {
		case css.AtRuleGrammar:
			out.Write(data)
			writeTokens(&out, values)
			out.WriteByte(';')
		case css.BeginAtRuleGrammar:
			out.Write(data)
			writeTokens(&out, values)
			out.WriteByte('{')
		case css.EndAtRuleGrammar, css.EndRulesetGrammar:
			out.WriteByte('}')
		case css.BeginRulesetGrammar:
			writeTokens(&out, values)
			out.WriteByte('{')
		case css.DeclarationGrammar, css.CustomPropertyGrammar:
			out.Write(data)
			out.WriteByte(':')
			writeTokens(&out, values)
			out.WriteByte(';')
		case css.CommentGrammar, css.QualifiedRuleGrammar, css.TokenGrammar:
			out.Write(data)
		}
	}
}

func (a *assembler) fileIDs(name string) (map[string]bool, error) {
	file, err := a.root.Open(name)
	if err != nil {
		return nil, err
	}
	doc, parseErr := xhtml.Parse(io.LimitReader(file, 1<<30))
	closeErr := file.Close()
	if err := errors.Join(parseErr, closeErr); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	walk(doc, func(node *xhtml.Node) {
		for _, key := range []string{"id", "name"} {
			if value := attribute(node, key); value != "" {
				ids[value] = true
			}
		}
	})
	return ids, nil
}

func resolveLocal(owner, raw string) (target, fragment, query string, local bool, err error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", "", false, err
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		return "", parsed.Fragment, parsed.RawQuery, false, nil
	}
	if strings.Contains(parsed.Path, "\\") {
		return "", "", "", false, errors.New("backslash in local print reference")
	}
	target = owner
	if parsed.Path != "" {
		target = path.Clean(path.Join(path.Dir(owner), parsed.Path))
	}
	if target == ".." || strings.HasPrefix(target, "../") || strings.HasPrefix(target, "/") {
		return "", "", "", false, fmt.Errorf("print reference escapes version library: %s", raw)
	}
	return target, parsed.Fragment, parsed.RawQuery, true, nil
}

func topicAnchor(raw string) (string, error) {
	canonical, err := fetch.CanonicalURL(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return "topic-" + hex.EncodeToString(sum[:12]), nil
}

func bookmarkAnchor(topic, fragment string) string {
	sum := sha256.Sum256([]byte(fragment))
	return topic + "--bookmark-" + hex.EncodeToString(sum[:12])
}

func topicTitle(topic *topicInfo) string {
	if strings.TrimSpace(topic.title) != "" {
		return topic.title
	}
	return topic.record.URL
}

func withinGuide(guide, target string) bool {
	if guide == "." {
		return target != ".." && !strings.HasPrefix(target, "../") && !strings.HasPrefix(target, "/")
	}
	return target != guide && strings.HasPrefix(target, guide+"/")
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func equalJSON(left, right any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func equalSourceInputs(left, right []model.SourceInput) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		a, b := left[index], right[index]
		a.ObservedAt, b.ObservedAt = "", ""
		if a != b {
			return false
		}
	}
	return true
}

func findElement(root *xhtml.Node, class string) *xhtml.Node {
	var found *xhtml.Node
	walk(root, func(node *xhtml.Node) {
		if found == nil && node.Type == xhtml.ElementNode && hasClass(node, class) {
			found = node
		}
	})
	return found
}

func hasClass(node *xhtml.Node, class string) bool {
	return slices.Contains(strings.Fields(attribute(node, "class")), class)
}

func findTag(root *xhtml.Node, tag string) *xhtml.Node {
	var found *xhtml.Node
	walk(root, func(node *xhtml.Node) {
		if found == nil && node.Type == xhtml.ElementNode && node.Data == tag {
			found = node
		}
	})
	return found
}

func hasHeading(root *xhtml.Node) bool {
	found := false
	walk(root, func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode && slices.Contains([]string{"h1", "h2", "h3"}, node.Data) {
			found = true
		}
	})
	return found
}

func walk(node *xhtml.Node, visit func(*xhtml.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func attribute(node *xhtml.Node, key string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == key {
			return attribute.Val
		}
	}
	return ""
}

func setAttribute(node *xhtml.Node, key, value string) {
	for index := range node.Attr {
		if node.Attr[index].Key == key {
			node.Attr[index].Val = value
			return
		}
	}
	node.Attr = append(node.Attr, xhtml.Attribute{Key: key, Val: value})
}

func nodeText(node *xhtml.Node) string {
	var out strings.Builder
	walk(node, func(child *xhtml.Node) {
		if child.Type == xhtml.TextNode {
			out.WriteString(child.Data)
		}
	})
	return out.String()
}

func activeResourceAttribute(node *xhtml.Node, key string) bool {
	if key == "data" {
		return node.Data == "object"
	}
	if key == "poster" {
		return node.Data == "video"
	}
	return slices.Contains([]string{"audio", "embed", "frame", "iframe", "img", "input", "script", "source", "track", "video"}, node.Data)
}

func writeTokens(out *bytes.Buffer, tokens []css.Token) {
	for _, token := range tokens {
		out.Write(token.Data)
	}
}

func cssURL(data []byte) (string, error) {
	raw := strings.TrimSpace(string(data))
	if len(raw) < 5 || !strings.EqualFold(raw[:4], "url(") || raw[len(raw)-1] != ')' {
		return "", fmt.Errorf("invalid CSS URL %q", raw)
	}
	raw = strings.TrimSpace(raw[4 : len(raw)-1])
	if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') && raw[len(raw)-1] == raw[0] {
		raw = raw[1 : len(raw)-1]
	}
	return raw, nil
}
