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
	"golang.org/x/net/html/atom"
)

const (
	maxTopics      = 100_000
	maxAnchors     = 1_000_000
	maxDiagnostics = 100
	maxOutputBytes = int64(16 << 30)
)

// Chrome prints the guide with the publisher stylesheets' screen rules, as
// the HTML archive shows it, and clips each printed page to its content
// area. Publisher lists can be indented less than their hanging markers are
// wide (DITA guides use 1em), so .print-content keeps a left gutter that
// holds the markers. Publisher rules meant for a scrolling browser window
// are overridden where print would lose or strand content: code and CLI
// output wrap instead of scrolling or being capped in height, and are never
// hyphenated; captions wrap, because a line wider than the page makes Chrome
// shrink the whole document; headings, labels, captions, table headers,
// notes, short code blocks and the text that introduces code, lists or
// figures stay with what follows them; and a topic's trailing margin cannot
// push an empty page before the next chapter. Flare table cells may break
// words to fit the page, except in tables of nine or more columns, whose
// publisher column widths would then leave one letter per line.
const printCSS = `
@page{size:Letter;margin:.72in .68in}
html{color:#202124;background:#fff}
body{font-family:"Open Sans",Arial,sans-serif;font-size:10pt;line-height:1.35}
body.print-source-hpe{font-family:"HPE Graphik",Arial,sans-serif}
.print-cover{break-after:page;display:flex;flex-direction:column;justify-content:space-between;min-height:8.3in}
.print-cover h1{font-size:22pt}.print-cover .print-meta{color:#4b5563}
.print-destination-registry{position:absolute;left:0;top:0;width:1px;height:1px;overflow:hidden;clip-path:inset(50%)}
.print-destination-registry a{display:block;width:1px;height:1px}
.print-overview{break-after:page}.print-toc{break-before:page;break-after:page}
.print-toc ol{list-style:none;padding-left:1.1em}.print-toc>ol{padding-left:0}.print-toc li{margin:.2em 0;list-style:none}
.print-topic.print-chapter{break-before:page}.print-topic{min-width:0}
.print-content{box-sizing:border-box;padding-left:27px;padding-right:1px}
.print-topic.print-heading-only{break-after:avoid-page}
.print-topic.print-flare-homepage{break-after:page}
.print-content .print-trailing{margin-bottom:0!important}.print-content .print-trailing:not(h1,h2,h3,h4,h5,h6){break-after:auto!important}
.print-content :is(h1,h2,h3,h4,h5,h6,p.subheading,p.Subheading)+.print-blank{break-after:avoid-page}
.print-content :is(span,a)[id]:empty+:is(h1,h2,h3,h4,h5,h6){break-before:avoid-page!important}
:where(.print-content h1,.print-content h2,.print-content h3,.print-content h4,.print-content h5,.print-content h6,.print-content p.subheading,.print-content p.Subheading){break-inside:avoid-page!important;break-after:avoid-page!important}
:where(.print-content h1,.print-content h2,.print-content h3,.print-content h4,.print-content h5,.print-content h6){break-before:auto!important}
:where(.print-content p,.print-content li){orphans:3;widows:3}
body.print-source-flare .print-topic :where(.print-content h1){font-size:16pt!important;line-height:1.2!important}
body.print-source-flare .print-topic :where(.print-content h2){font-size:14pt!important;line-height:1.25!important}
body.print-source-flare .print-topic :where(.print-content h3){font-size:12pt!important;line-height:1.25!important}
:where(.print-content table){max-width:100%}
.print-content caption{white-space:normal!important;break-after:avoid-page!important}
.print-content div.note{break-inside:avoid-page}.print-content .note__title{break-after:avoid-page}
.print-content div.p>:is(ul,ol){break-before:avoid-page}
body.print-source-flare .print-content p.Figure_Title{break-after:avoid-page!important}body.print-source-flare .print-content p.Figure_Title+p{break-inside:avoid-page}
body.print-source-flare .print-content p.Figure_Title:has(img){break-after:auto!important;break-inside:avoid-page!important}body.print-source-flare .print-content p.Figure_Title:has(img)+p{break-inside:auto}
.print-content p:has(+:is(div.screen,pre,.pre,.codeblock,figure,.fig)),.print-content p:not([class^="CLI"],[class*=" CLI"]):has(+:is(ul,ol)){break-after:avoid-page}.print-content p.print-label{break-after:avoid-page}
.print-content p:has(+p.print-blank+:is(ul,ol,div.screen,pre,.pre,.codeblock,figure,.fig)){break-after:avoid-page}
.print-content :is(ul,ol):has(+p.print-blank+:is(div.screen,pre,.pre,.codeblock)){break-after:avoid-page}
.print-content :is(.print-leadin,p:has(+:is(div.screen,pre,.pre,.codeblock,figure,.fig)),p:not([class^="CLI"],[class*=" CLI"]):has(+:is(ul,ol)),p:has(+p.print-blank+:is(ul,ol,div.screen,pre,.pre,.codeblock,figure,.fig)),p:has(+:is(div:not(.screen),blockquote)>:is(div.screen,pre,.pre,.codeblock):first-child)){break-inside:avoid-page}
body.print-source-flare .print-content p:not([class^="CLI"],[class*=" CLI"]):has(+:is(p[class^="CLI"],p[class*=" CLI"])){break-inside:avoid-page}
.print-content li>span.cmd:has(+div.itemgroup>:is(pre,.pre,.codeblock):first-child){display:block;break-inside:avoid-page}
.print-content li:not(:has(>:is(p,div,ul,ol,pre,table,dl,figure))):has(+:is(div.screen,pre,.pre,.codeblock)){break-inside:avoid-page}
.print-content p.print-chain{break-inside:avoid-page;break-after:avoid-page}.print-content .print-image{orphans:1;widows:1}
.print-content li:has(>.print-leadin:first-child+:is(p[class^="CLI"],p[class*=" CLI"]).print-cli-run){break-inside:avoid-page}
body.print-source-flare .print-content :is(td,th){box-decoration-break:clone}
body.print-source-flare .print-content p.print-cli-run{break-after:avoid-page}
.print-content .print-leadin{display:block;break-after:avoid-page}.print-content :is(ul,ol):has(+:is(div.screen,pre,.pre,.codeblock)){break-after:avoid-page}
.print-content p:has(+:is(div:not(.screen),blockquote)>:is(div.screen,pre,.pre,.codeblock):first-child){break-after:avoid-page}
.print-content li:has(+:is(div.screen,pre,.pre,.codeblock,p[class^="CLI"],p[class*=" CLI"])),.print-content li:has(+p>span.CLI:first-child){break-after:avoid-page}
.print-content li>div.itemgroup:has(>:is(pre,.pre,.codeblock):first-child){break-before:avoid-page}
.print-content :is(li,div,dd,section,article,blockquote,figure):has(>.print-leadin:first-child+.print-keep:last-child){break-inside:avoid-page}
.print-content section.subheading>p:first-child:has(+*){break-after:avoid-page}
body.print-source-flare .print-content div.Indent:has(>:is(ul,ol):last-child):has(+div.screen){break-after:avoid-page}
body.print-source-flare .print-content p:not([class^="CLI"],[class*=" CLI"]):has(+:is(p[class^="CLI"],p[class*=" CLI"])){break-after:avoid-page}
:where(.print-content img,.print-content svg){max-width:100%;height:auto}
.archive-image-placeholder{display:flex;box-sizing:border-box;align-items:center;justify-content:center;max-width:100%;min-width:3rem;min-height:2.5rem;max-height:12rem;padding:.6rem;border:1px dashed #8a6d3b;border-radius:.25rem;background:#fff8e5;color:#533f1f;font-size:.9rem;line-height:1.3;text-align:center;overflow:hidden;break-inside:avoid-page}
.archive-image-placeholder::before{content:"[!]";margin-right:.4rem;font-weight:700}
.archive-image-placeholder-small{min-width:0;min-height:0;padding:.1rem;font-size:0}
.archive-image-placeholder-small::before{margin:0;font-size:.8rem}
:where(.print-content pre,.print-content .pre,.print-content .codeblock,.print-content .screen){max-width:100%;overflow-x:auto;white-space:pre-wrap;overflow-wrap:anywhere}
.print-content :is(pre,.pre,.codeblock,.screen){max-height:none!important;orphans:3;widows:3}
.print-content .print-keep{break-inside:avoid-page}
.print-content .print-tail-1{widows:4}.print-content .print-tail-2{widows:5}.print-content .print-tail-3{widows:6}
.print-content .print-head-1{orphans:4}.print-content .print-head-2{orphans:5}.print-content .print-head-3{orphans:6}
.print-content div.screen>.print-screen-edge{break-before:avoid-page}.print-content div.screen>.print-screen-whole{break-inside:avoid-page}
.print-content .print-screen-gap{display:block;break-inside:avoid-page}
.print-content div.screen>:not(:first-child):is(:nth-child(-n+5),:nth-last-child(-n+5)){break-before:avoid-page}
.print-content div.screen>:is(ul,ol)>:not(:first-child):is(:nth-child(-n+5),:nth-last-child(-n+5)){break-before:avoid-page}
body.print-source-flare .print-content div.screen>p.subheading{break-inside:auto!important;break-after:auto!important}
.print-content :is(pre,.pre,.codeblock,.screen,.screen>p,p.CLI){white-space:pre-wrap!important}
body.print-source-flare .print-content :is(p[class^="CLI"],p[class*=" CLI"],p.screen,span.CLI,code,[xml\:space="preserve"]){white-space:pre-wrap!important;overflow-wrap:anywhere!important}
body.print-source-flare .print-content :is(td,th,li,p,div.note,div.screen,div.screen>p){overflow-wrap:anywhere!important}
body.print-source-flare .print-content table{max-width:100%!important}
body.print-source-flare .print-content p[style*="float"]{float:none!important;width:auto!important}
body.print-source-flare .print-content .inner-wrap{min-height:0!important}
body.print-source-flare .print-content table:not(:has(tr>:nth-child(9))) :is(td,th){min-width:4em}
body.print-source-flare .print-content table:has(tr>:nth-child(9)) :is(td,th,li,p:not([class^="CLI"],[class*=" CLI"],.screen),div.note){overflow-wrap:break-word!important}
.print-content :is(pre,.pre,.codeblock,.screen,p.CLI,p[class^="CLI"],p[class*=" CLI"],span.CLI,code,kbd,samp,.codeph){hyphens:manual!important}
:where(.print-content p.CLI){max-width:100%;overflow-x:auto}
body.print-source-flare :where(.print-content>#mc-main-content>.topichero){display:none!important;position:static!important}
:where(.print-content.print-flare-home-hero-removed>#mc-main-content>.container){margin-top:0!important}
:where(.print-content thead){display:table-header-group}
.print-content thead{break-inside:avoid-page;break-after:avoid-page}
.print-content table:has(>thead)>tbody>tr:only-child{break-inside:auto}
.print-content tbody>tr:is(:has(>td>:nth-child(6)),:has(>td li:nth-child(6))){break-inside:auto}
:where(.print-content .table){overflow-wrap:break-word}
:where(.print-content tr,.print-content figure,.print-content img){break-inside:avoid-page}
body.print-source-hpe :where(.print-content table code.codeph){display:inline-block;max-width:min(60ch,calc(100vw - 3rem));overflow-x:auto;white-space:nowrap!important;word-break:normal!important;overflow-wrap:normal!important}
.print-provenance{break-before:page;color:#4b5563}
`

// printFontFallbackCSS gives "Open Sans" and "Open Sans Extrabold", which
// Flare stylesheets name with no fallback, an installed sans-serif face; without one Chrome falls back
// to a serif font. It precedes the publisher stylesheets, so a guide that
// ships its own Open Sans faces keeps them.
const printFontFallbackCSS = `@font-face{font-family:"Open Sans Extrabold";font-weight:bold;src:local("Open Sans Extrabold"),local("OpenSans-ExtraBold"),local("Arial Black"),local("Arial-Black"),local("Arial Bold"),local("Arial-BoldMT"),local("Liberation Sans Bold"),local("LiberationSans-Bold")}` +
	`@font-face{font-family:"Open Sans";src:local("Open Sans"),local("OpenSans-Regular"),local("Arial"),local("ArialMT"),local("Liberation Sans"),local("LiberationSans")}` +
	`@font-face{font-family:"Open Sans";font-weight:bold;src:local("Open Sans Bold"),local("OpenSans-Bold"),local("Arial Bold"),local("Arial-BoldMT"),local("Liberation Sans Bold"),local("LiberationSans-Bold")}` +
	`@font-face{font-family:"Open Sans";font-style:italic;src:local("Open Sans Italic"),local("OpenSans-Italic"),local("Arial Italic"),local("Arial-ItalicMT"),local("Liberation Sans Italic"),local("LiberationSans-Italic")}` +
	`@font-face{font-family:"Open Sans";font-weight:bold;font-style:italic;src:local("Open Sans Bold Italic"),local("OpenSans-BoldItalic"),local("Arial Bold Italic"),local("Arial-BoldItalicMT"),local("Liberation Sans Bold Italic"),local("LiberationSans-BoldItalic")}`

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
		(result.Status == "complete" && (len(result.MissingResources) != 0 || len(result.UnpublishedTopics) != 0)) ||
		(result.Status == "degraded" && len(result.MissingResources) == 0 && len(result.UnpublishedTopics) == 0) ||
		!equalJSON(result.MissingResources, result.HTML.MissingResources) ||
		!equalJSON(result.UnpublishedTopics, result.HTML.UnpublishedTopics) {
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
	// A planned topic the publisher does not serve has no page; its outline
	// entry links the publisher's URL.
	for _, unpublished := range a.archive.UnpublishedTopics {
		key, err := fetch.CanonicalURL(unpublished.URL)
		if err != nil {
			return err
		}
		if foundPlanned[key] {
			return fmt.Errorf("unpublished topic has an archived page: %s", key)
		}
		foundPlanned[key] = true
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
	fmt.Fprintf(out, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; script-src 'none'; connect-src 'none'; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'none'"><title>%s</title>`, stdhtml.EscapeString(a.result.Document.Title))
	fmt.Fprintf(out, "<style>%s</style>", printFontFallbackCSS)
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
	stripActiveMarkup(content)
	if !hasHeading(content) {
		// The title goes inside the content, where the print rules that keep
		// a heading with what follows, and the renderer's check of them, apply.
		heading := &xhtml.Node{Type: xhtml.ElementNode, Data: "h1", DataAtom: atom.H1}
		heading.AppendChild(&xhtml.Node{Type: xhtml.TextNode, Data: topicTitle(info)})
		content.InsertBefore(heading, content.FirstChild)
	}
	removeEmptyDITABlocks(content)
	markShortCodeBlocks(content)
	wrapScreenGaps(content)
	markScreenEdges(content)
	markBlankParagraphs(content)
	markLabelParagraphs(content)
	markLeadIns(content)
	markLeadInChains(content)
	markImageParagraphs(content)
	markCLIRuns(content)
	if holdsOnlyHeadings(content) {
		class += " print-heading-only"
	}
	markTrailingElements(content)
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
	if err := xhtml.Render(out, content); err != nil {
		return err
	}
	_, err = io.WriteString(out, "</section>")
	return err
}

// stripActiveMarkup removes elements and attributes from archived content
// that could run script, redirect, rebase URLs or load other documents. A
// verified archive never contains them; this guards the renderer even if the
// archived input was altered after validation. Objects keep their fallback
// content, and the print policy blocks what they would load.
func stripActiveMarkup(content *xhtml.Node) {
	var remove []*xhtml.Node
	walk(content, func(node *xhtml.Node) {
		if node.Type != xhtml.ElementNode {
			return
		}
		switch node.Data {
		case "base", "embed", "frame", "frameset", "iframe", "link", "meta", "noscript", "portal", "script", "template":
			remove = append(remove, node)
			return
		}
		node.Attr = slices.DeleteFunc(node.Attr, func(attribute xhtml.Attribute) bool {
			key := strings.ToLower(attribute.Key)
			return strings.HasPrefix(key, "on") || key == "srcdoc" || key == "http-equiv" ||
				key == "action" || key == "formaction" || key == "ping"
		})
	})
	for _, node := range remove {
		if node.Parent != nil {
			node.Parent.RemoveChild(node)
		}
	}
}

// removeEmptyDITABlocks removes DITA short descriptions, and then DITA topic
// bodies, that hold nothing but HTML whitespace. Publishers leave them
// between many headings and the content that follows, where each lets
// Chrome break the page after the heading despite break-after:avoid and
// leave the heading alone at the bottom of a page. CSS :empty cannot match
// the whitespace-only ones. An element with an ID or name stays, as a link
// may target it.
func removeEmptyDITABlocks(content *xhtml.Node) {
	removeEmpty(content, func(node *xhtml.Node) bool { return node.Data == "p" && hasClass(node, "shortdesc") })
	removeEmpty(content, func(node *xhtml.Node) bool { return node.Data == "div" && hasClass(node, "body") })
}

func removeEmpty(content *xhtml.Node, match func(*xhtml.Node) bool) {
	var remove []*xhtml.Node
	walk(content, func(node *xhtml.Node) {
		if node == content || node.Type != xhtml.ElementNode || !match(node) ||
			attribute(node, "id") != "" || attribute(node, "name") != "" {
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == xhtml.ElementNode ||
				(child.Type == xhtml.TextNode && strings.Trim(child.Data, " \t\n\f\r") != "") {
				return
			}
		}
		remove = append(remove, node)
	})
	for _, node := range remove {
		node.Parent.RemoveChild(node)
	}
}

// markShortCodeBlocks marks code blocks of at most shortCodeLines lines to
// stay on one page. Longer code blocks of text alone are marked with the
// blank lines among their first and last three visible lines, which Chrome
// counts toward the three lines it keeps on either side of a break; the
// print rules keep as many more. Longer blocks may break, keeping three lines on either
// side; a shorter block cannot satisfy both, and Chrome then splits it
// anywhere, leaving one line alone on a page.
func markShortCodeBlocks(content *xhtml.Node) {
	var visit func(*xhtml.Node)
	visit = func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode && node != content &&
			(node.Data == "pre" || hasClass(node, "pre") || hasClass(node, "codeblock") || hasClass(node, "screen")) {
			if codeLines(node) <= shortCodeLines {
				setAttribute(node, "class", strings.TrimSpace(attribute(node, "class")+" print-keep"))
			} else if !hasClass(node, "screen") || !hasBlockChild(node) {
				markCodeEdges(node)
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(content)
}

const shortCodeLines = 8

// wrapScreenGaps puts the whitespace between the paragraphs of a Flare
// screen box into block spans. Under white-space:pre-wrap that whitespace
// shows as blank lines, as in the HTML, but in anonymous boxes that break
// rules cannot name; Chrome could then break between a box's top and its
// first line, or before its last line, leaving an empty grey box top or tail
// on a page. As elements the gaps can be kept with the lines they separate.
func wrapScreenGaps(content *xhtml.Node) {
	walk(content, func(node *xhtml.Node) {
		if node.Type != xhtml.ElementNode || node.Data != "div" || !hasClass(node, "screen") {
			return
		}
		hasBlock := false
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			hasBlock = hasBlock || child.Type == xhtml.ElementNode && slices.Contains([]string{"p", "ul", "ol"}, child.Data)
		}
		if !hasBlock {
			return
		}
		wrapWhitespaceChildren(node)
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == xhtml.ElementNode && (child.Data == "ul" || child.Data == "ol") {
				wrapWhitespaceChildren(child)
			}
		}
		moveLeadingLineFeeds(node)
	})
}

func wrapWhitespaceChildren(parent *xhtml.Node) {
	for child := parent.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != xhtml.TextNode || strings.Trim(child.Data, " \t\n\f\r") != "" {
			continue
		}
		gap := screenGap(child.Data)
		parent.InsertBefore(gap, child)
		parent.RemoveChild(child)
		child = gap
	}
}

func screenGap(text string) *xhtml.Node {
	gap := &xhtml.Node{Type: xhtml.ElementNode, Data: "span", DataAtom: atom.Span,
		Attr: []xhtml.Attribute{{Key: "class", Val: "print-screen-gap"}}}
	gap.AppendChild(&xhtml.Node{Type: xhtml.TextNode, Data: text})
	return gap
}

// moveLeadingLineFeeds moves the line feeds that start a screen box's first
// paragraph into the gap before it. They show as blank lines inside the
// paragraph, where Chrome may break between them and leave the box's top
// alone on a page. The gap shows as many lines as before: one more for each
// line feed moved, the last of them a space so that it renders.
func moveLeadingLineFeeds(box *xhtml.Node) {
	var first *xhtml.Node
	for child := box.FirstChild; child != nil && first == nil; child = child.NextSibling {
		if child.Type == xhtml.ElementNode && !hasClass(child, "print-screen-gap") {
			first = child
		}
	}
	if first == nil || first.Data != "p" || first.FirstChild == nil || first.FirstChild.Type != xhtml.TextNode {
		return
	}
	text := first.FirstChild
	leading := text.Data[:len(text.Data)-len(strings.TrimLeft(text.Data, " \t\n\f\r"))]
	end := strings.LastIndexByte(leading, '\n')
	if end < 0 {
		return
	}
	moved := leading[:end+1]
	text.Data = text.Data[end+1:]
	gap := first.PrevSibling
	if gap == nil || gap.Type != xhtml.ElementNode || !hasClass(gap, "print-screen-gap") || gap.FirstChild == nil {
		gap = screenGap("")
		box.InsertBefore(gap, first)
	}
	shown := gap.FirstChild.Data
	if shown == "" || strings.HasSuffix(shown, "\n") {
		moved = moved[:len(moved)-1]
	}
	gap.FirstChild.Data = shown + moved + " "
}

// markBlankParagraphs marks paragraphs that show only white space, such as
// Flare's <p>&nbsp;</p> spacers. A heading's break-after:avoid reaches only
// the blank paragraph after it, so a page could still end with the heading
// and the blank line; a blank paragraph after a heading also keeps what
// follows.
func markBlankParagraphs(content *xhtml.Node) {
	walk(content, func(node *xhtml.Node) {
		if node.Type != xhtml.ElementNode || node.Data != "p" {
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == xhtml.ElementNode ||
				(child.Type == xhtml.TextNode && strings.Trim(child.Data, " \t\n\f\r\u00a0") != "") {
				return
			}
		}
		setAttribute(node, "class", strings.TrimSpace(attribute(node, "class")+" print-blank"))
	})
}

// markLabelParagraphs marks paragraphs that hold only one bold run, such as
// Flare's <p><b>Procedure</b></p>. They act as headings for what follows,
// a list, a note or a code box, and stay with it.
func markLabelParagraphs(content *xhtml.Node) {
	walk(content, func(node *xhtml.Node) {
		if node.Type != xhtml.ElementNode || node.Data != "p" {
			return
		}
		var bold *xhtml.Node
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			switch {
			case child.Type == xhtml.TextNode && strings.Trim(child.Data, " \t\n\f\r\u00a0") == "":
			case child.Type == xhtml.ElementNode && (child.Data == "b" || child.Data == "strong") && bold == nil:
				bold = child
			default:
				return
			}
		}
		if bold != nil && strings.TrimSpace(nodeText(bold)) != "" {
			setAttribute(node, "class", strings.TrimSpace(attribute(node, "class")+" print-label"))
		}
	})
}

// markLeadIns wraps the text that introduces a code block, a Flare CLI
// paragraph or a list inside a list item, division, section or cell, such
// as "Configure the switch:" before a pre, in a span the print rules keep
// with that block. Chrome lays such text out in an anonymous block that no
// rule can name, and could leave it alone at the bottom of a page. Code
// boxes keep their content as it is.
func markLeadIns(content *xhtml.Node) {
	var visit func(*xhtml.Node)
	visit = func(node *xhtml.Node) {
		if node.Type != xhtml.ElementNode || node.Namespace != "" || isCodeBox(node) {
			return
		}
		if slices.Contains([]string{"li", "div", "section", "article", "blockquote", "figure", "dd", "td", "th"}, node.Data) {
			wrapLeadIns(node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(content)
}

func wrapLeadIns(parent *xhtml.Node) {
	var run []*xhtml.Node
	text := false
	for child := parent.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != xhtml.ElementNode || !slices.Contains(leadInBlocks, child.Data) {
			run = append(run, child)
			text = text || strings.Trim(nodeText(child), " \t\n\f\r\u00a0") != ""
			continue
		}
		if text && (isCodeBox(child) || isCLIParagraph(child) || child.Data == "ul" || child.Data == "ol") {
			span := &xhtml.Node{Type: xhtml.ElementNode, Data: "span", DataAtom: atom.Span,
				Attr: []xhtml.Attribute{{Key: "class", Val: "print-leadin"}}}
			parent.InsertBefore(span, run[0])
			for _, node := range run {
				parent.RemoveChild(node)
				span.AppendChild(node)
			}
		}
		run, text = nil, false
	}
}

// markLeadInChains marks a paragraph ending with a colon that introduces
// what follows it without a rule of its own: a table, an image block, or
// code or a list reached through one more paragraph, as "Example:" before
// a sentence and its code. The mark keeps the paragraph whole and with
// what follows, which Chrome could otherwise leave alone at the bottom of
// a page. A paragraph holding an image introduces that image instead.
func markLeadInChains(content *xhtml.Node) {
	var visit func(*xhtml.Node)
	visit = func(node *xhtml.Node) {
		if node.Type != xhtml.ElementNode || isCodeBox(node) {
			return
		}
		if node.Data == "p" && !isCLIParagraph(node) && !hasImage(node) &&
			strings.HasSuffix(strings.Join(strings.Fields(nodeText(node)), " "), ":") {
			second := nextElement(node)
			switch {
			case second == nil:
			case startsWithTable(second) || isImageBlock(second):
				setAttribute(node, "class", strings.TrimSpace(attribute(node, "class")+" print-chain"))
			case second.Data == "p" && !isCLIParagraph(second) && !hasClass(second, "print-blank") &&
				strings.Trim(nodeText(second), " \t\n\f\r\u00a0") != "":
				if third := nextElement(second); third != nil && (isCodeBox(third) || third.Data == "ul" || third.Data == "ol") {
					setAttribute(node, "class", strings.TrimSpace(attribute(node, "class")+" print-chain"))
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(content)
}

// markImageParagraphs marks paragraphs and divisions that hold only images
// outside figures. Under the paragraph orphan and widow limits Chrome would
// not break between two tall images in one paragraph and could move both,
// leaving most of a page empty.
func markImageParagraphs(content *xhtml.Node) {
	var visit func(*xhtml.Node)
	visit = func(node *xhtml.Node) {
		if node.Type != xhtml.ElementNode || isCodeBox(node) {
			return
		}
		if (node.Data == "p" || node.Data == "div") && isImageBlock(node) {
			setAttribute(node, "class", strings.TrimSpace(attribute(node, "class")+" print-image"))
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(content)
}

// isImageBlock reports whether node shows only images: an image, an
// archived full-size image container, or a paragraph or division outside
// a figure that holds images and no text.
func isImageBlock(node *xhtml.Node) bool {
	switch {
	case node.Data == "img" || (node.Data == "span" && hasClass(node, "archive-full-image-container")):
		return true
	case node.Data != "p" && node.Data != "div", hasClass(node, "fig"), hasClass(node, "figure"):
		return false
	}
	return strings.Trim(nodeText(node), " \t\n\f\r\u00a0") == "" && hasImage(node)
}

func hasImage(node *xhtml.Node) bool {
	found := false
	walk(node, func(child *xhtml.Node) {
		found = found || child.Type == xhtml.ElementNode && (child.Data == "img" || child.Data == "svg")
	})
	return found
}

// startsWithTable reports whether node is a table or a division whose
// first content is a table.
func startsWithTable(node *xhtml.Node) bool {
	if node.Data == "table" {
		return true
	}
	if node.Data != "div" {
		return false
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == xhtml.TextNode && strings.Trim(child.Data, " \t\n\f\r\u00a0") != "" {
			return false
		}
		if child.Type == xhtml.ElementNode {
			return child.Data == "table"
		}
	}
	return false
}

// markCLIRuns marks every paragraph but the last of a run of Flare CLI
// paragraphs, such as a command's syntax lines under its heading, that
// shows at most shortCodeLines lines. The print rules keep each with the
// next, so the run moves whole like a short code block instead of leaving
// one line on a page.
func markCLIRuns(content *xhtml.Node) {
	walk(content, func(node *xhtml.Node) {
		if !isCLIParagraph(node) {
			return
		}
		if previous := previousElement(node); previous != nil && isCLIParagraph(previous) {
			return
		}
		run := []*xhtml.Node{node}
		for next := nextElement(node); next != nil && isCLIParagraph(next); next = nextElement(next) {
			run = append(run, next)
		}
		lines := 0
		for _, paragraph := range run {
			lines += max(visibleLines(paragraph), 1)
		}
		if len(run) < 2 || lines > shortCodeLines {
			return
		}
		for _, paragraph := range run[:len(run)-1] {
			setAttribute(paragraph, "class", strings.TrimSpace(attribute(paragraph, "class")+" print-cli-run"))
		}
	})
}

func isCLIParagraph(node *xhtml.Node) bool {
	return node.Type == xhtml.ElementNode && node.Data == "p" &&
		slices.ContainsFunc(strings.Fields(attribute(node, "class")), func(class string) bool { return strings.HasPrefix(class, "CLI") })
}

func nextElement(node *xhtml.Node) *xhtml.Node {
	for next := node.NextSibling; next != nil; next = next.NextSibling {
		if next.Type == xhtml.ElementNode {
			return next
		}
	}
	return nil
}

func previousElement(node *xhtml.Node) *xhtml.Node {
	for previous := node.PrevSibling; previous != nil; previous = previous.PrevSibling {
		if previous.Type == xhtml.ElementNode {
			return previous
		}
	}
	return nil
}

func isCodeBox(node *xhtml.Node) bool {
	return node.Data == "pre" || hasClass(node, "pre") || hasClass(node, "codeblock") ||
		(node.Data == "div" && hasClass(node, "screen"))
}

var leadInBlocks = []string{"p", "div", "ul", "ol", "pre", "table", "figure", "h1", "h2", "h3", "h4", "h5", "h6",
	"dl", "blockquote", "section", "article", "nav", "aside", "header", "footer", "hr", "form", "fieldset", "details"}

// codeLines counts the lines a code block shows from its first non-blank line
// to its last, blank lines between them included: its text split at line
// feeds, line breaks and block elements such as Flare's paragraphs.
func codeLines(node *xhtml.Node) int {
	lines := codeLineTexts(node)
	first, last := 0, len(lines)-1
	for first <= last && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	for last >= first && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	return last - first + 1
}

// markCodeEdges marks a code block or a paragraph of a Flare screen box
// with print-head-N and print-tail-N, N up to three, for the blank lines it
// shows before its third visible line and after its third-to-last, such as
// the line of indentation an Oxygen block keeps before </code>.
func markCodeEdges(node *xhtml.Node) {
	head, tail := codeEdgeBlankLines(node)
	for _, mark := range []struct {
		name  string
		count int
	}{{"print-head", head}, {"print-tail", tail}} {
		if mark.count > 0 {
			setAttribute(node, "class", strings.TrimSpace(fmt.Sprintf("%s %s-%d", attribute(node, "class"), mark.name, min(mark.count, 3))))
		}
	}
}

// codeEdgeBlankLines counts the blank lines a code block shows before its
// third visible line and after its third-to-last. A line feed that closes
// the last line, or a block boundary that opens the first, shows none.
func codeEdgeBlankLines(node *xhtml.Node) (head, tail int) {
	lines := codeLineTexts(node)
	var visible []int
	for index, line := range lines {
		if strings.TrimSpace(line) != "" {
			visible = append(visible, index)
		}
	}
	if len(visible) == 0 {
		return 0, 0
	}
	start, end := visible[0], visible[len(visible)-1]
	if len(visible) >= 3 {
		start, end = visible[len(visible)-3], visible[2]
	}
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) == "" {
			tail++
		}
	}
	if lines[len(lines)-1] == "" && len(lines)-1 > visible[len(visible)-1] {
		tail--
	}
	for _, line := range lines[:end] {
		if strings.TrimSpace(line) == "" {
			head++
		}
	}
	if lines[0] == "" && visible[0] > 0 {
		head--
	}
	return head, tail
}

func visibleLines(node *xhtml.Node) int {
	count := 0
	for _, line := range codeLineTexts(node) {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func hasBlockChild(node *xhtml.Node) bool {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == xhtml.ElementNode && slices.Contains([]string{"p", "ul", "ol"}, child.Data) {
			return true
		}
	}
	return false
}

// markScreenEdges marks the children of a Flare screen box that may break
// near its top and bottom, where Chrome counts the gaps and blank lines between them toward
// the three lines it keeps on either side of a break. A child before which
// a break would leave fewer than three visible lines on either side is kept
// with what precedes it, a child of one or two lines there stays whole, and
// paragraphs of three or more lines are marked like code blocks.
func markScreenEdges(content *xhtml.Node) {
	walk(content, func(box *xhtml.Node) {
		if box.Type != xhtml.ElementNode || box.Data != "div" || !hasClass(box, "screen") ||
			hasClass(box, "print-keep") || !hasBlockChild(box) {
			return
		}
		var children []*xhtml.Node
		for child := box.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == xhtml.TextNode && strings.Trim(child.Data, " \t\n\f\r") != "" {
				return
			}
			if child.Type == xhtml.ElementNode {
				children = append(children, child)
			}
		}
		visible := make([]int, len(children))
		total := 0
		for index, child := range children {
			visible[index] = visibleLines(child)
			total += visible[index]
		}
		before := 0
		for index, child := range children {
			after := total - before - visible[index]
			if index > 0 && (before < 3 || total-before < 3) {
				setAttribute(child, "class", strings.TrimSpace(attribute(child, "class")+" print-screen-edge"))
			}
			if visible[index] > 0 && visible[index] < 3 && (before < 3 || after < 3) {
				setAttribute(child, "class", strings.TrimSpace(attribute(child, "class")+" print-screen-whole"))
			}
			if visible[index] >= 3 && child.Data == "p" {
				markCodeEdges(child)
			}
			before += visible[index]
		}
	})
}

func codeLineTexts(node *xhtml.Node) []string {
	var text strings.Builder
	walk(node, func(child *xhtml.Node) {
		switch {
		case child.Type == xhtml.TextNode:
			text.WriteString(child.Data)
		case child.Type == xhtml.ElementNode && slices.Contains([]string{"br", "p", "div", "li", "tr"}, child.Data):
			text.WriteByte('\n')
		}
	})
	return strings.Split(text.String(), "\n")
}

// holdsOnlyHeadings reports whether a topic shows nothing but headings, as a
// parent topic whose page is only its title does. The heading's
// break-after:avoid does not reach past the end of its topic, so Chrome
// could leave such a title alone at the bottom of a page; its section
// avoids a break after itself instead.
func holdsOnlyHeadings(content *xhtml.Node) bool {
	other := false
	var visit func(node *xhtml.Node, inHeading bool)
	visit = func(node *xhtml.Node, inHeading bool) {
		switch {
		case other:
			return
		case node.Type == xhtml.TextNode:
			other = !inHeading && strings.Trim(node.Data, " \t\n\f\r") != ""
			return
		case node.Type == xhtml.ElementNode:
			switch node.Data {
			case "style", "script", "template", "noscript":
				return
			case "h1", "h2", "h3", "h4", "h5", "h6":
				inHeading = true
			case "img", "svg", "table", "pre", "video", "audio", "object", "embed", "canvas", "math", "hr":
				if !inHeading {
					other = true
					return
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, inHeading)
		}
	}
	visit(content, false)
	return !other
}

// markTrailingElements marks the chain of block elements that end a topic:
// its last element, that element's last element, and so on, through
// containers whose bottom margin collapses with their last child's. Those
// margins become one margin after the topic, which Chrome keeps before a
// chapter's forced page break; when the topic ends near the bottom of a
// page, that margin alone fills an otherwise empty page. In the HTML
// archive the margin is the unseen end of a page.
func markTrailingElements(content *xhtml.Node) {
	node := content
	for depth := 0; depth < 1024; depth++ {
		var last *xhtml.Node
		for child := node.LastChild; child != nil; child = child.PrevSibling {
			if child.Type == xhtml.TextNode && strings.Trim(child.Data, " \t\n\f\r") != "" {
				return
			}
			if child.Type == xhtml.ElementNode && child.Data != "style" {
				last = child
				break
			}
		}
		if last == nil || last.Namespace != "" || !slices.Contains(trailingBlocks, last.Data) {
			return
		}
		setAttribute(last, "class", strings.TrimSpace(attribute(last, "class")+" print-trailing"))
		if !slices.Contains(trailingContainers, last.Data) {
			return
		}
		node = last
	}
}

var (
	trailingContainers = []string{"div", "section", "article", "main", "aside", "header", "footer", "nav",
		"ul", "ol", "li", "dl", "dd", "figure", "blockquote", "details", "form", "fieldset"}
	trailingBlocks = append([]string{"p", "pre", "table", "h1", "h2", "h3", "h4", "h5", "h6", "dt", "figcaption", "hr"},
		trailingContainers...)
)

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
	} else if normalized, err := normalizeOutlineWrappers(a.plan); err == nil {
		// The Contents page lists what the PDF outline lists: publisher
		// navigation wrappers such as the guide title, "Table of Contents"
		// or "Home" give way to their children. The topics they name are
		// still printed, so they are not additional topics.
		a.markTOCTopics(a.plan.TOC)
		entries = normalized
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

func (a *assembler) markTOCTopics(entries []model.TocEntry) {
	for _, entry := range entries {
		if topic := a.topicForURL(entry.URL); topic != nil {
			a.included[topic.full] = true
		}
		a.markTOCTopics(entry.Children)
	}
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
		node.Attr[index].Val = replaceFragmentURLs(node.Attr[index].Val, info.ids)
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
	} else if hasAttribute(node, "href") && node.Namespace == "" && (node.Data == "a" || node.Data == "area") {
		// An empty href names the topic's own page. Left empty, Chrome
		// would resolve it against the transient print file and write that
		// local file: URL into the PDF.
		local, err := a.rewriteNavigation(info, "")
		if err != nil {
			return err
		}
		setAttribute(node, "href", local)
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
		local, err := a.rebaseAndVerify(target, query, fragment, true)
		if err != nil {
			return "", err
		}
		// A link to a guide file that is not a topic, such as a full-size
		// image, would resolve against the transient print file, and Chrome
		// would write that local file: URL into the PDF. It leads to the
		// file's publisher URL instead.
		if online := a.assetURL(target); online != "" {
			if fragment != "" {
				online += "#" + (&url.URL{Fragment: fragment}).EscapedFragment()
			}
			return online, nil
		}
		return local, nil
	}
	if topic := a.topicForURL(raw); topic != nil {
		parsed, _ := url.Parse(raw)
		destination, err := a.topicDestination(info.record.URL, topic, parsed.Fragment)
		return "#" + destination, err
	}
	return raw, nil
}

// assetURL returns the publisher URL of the guide asset at target, without
// its fragment, or "" when target is not an asset of the guide.
func (a *assembler) assetURL(target string) string {
	for _, asset := range a.archive.Assets {
		if path.Join(a.guide, asset.Path) == target {
			online, _, _ := strings.Cut(asset.URL, "#")
			return online
		}
	}
	return ""
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

func hasAttribute(node *xhtml.Node, key string) bool {
	return slices.ContainsFunc(node.Attr, func(attribute xhtml.Attribute) bool { return attribute.Key == key })
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

// replaceFragmentURLs rewrites each url(#id) in value whose id is in ids, in
// a single pass over the value.
func replaceFragmentURLs(value string, ids map[string]string) string {
	if !strings.Contains(value, "url(#") {
		return value
	}
	var out strings.Builder
	for {
		start := strings.Index(value, "url(#")
		if start < 0 {
			break
		}
		rest := value[start+len("url(#"):]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			break
		}
		id := rest[:end]
		if replacement, ok := ids[id]; ok {
			id = replacement
		}
		out.WriteString(value[:start+len("url(#")])
		out.WriteString(id)
		out.WriteByte(')')
		value = rest[end+1:]
	}
	out.WriteString(value)
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
