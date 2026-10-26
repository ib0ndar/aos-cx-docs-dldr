package library

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
	"golang.org/x/net/html"
)

const (
	maxCrossGuideAliases     = 1_000_000
	maxCrossGuideHTMLBytes   = 64 << 20
	maxCrossGuideDiagnostics = 100
)

type crossGuideTarget struct {
	guide             string
	path              string
	sourceBookmarks   map[string]bool
	bookmarksComplete bool
}

type crossGuidePage struct {
	guide    string
	path     string
	body     []byte
	rewrites int
	// restored counts local links into another guide's former pages that
	// went back online or, without a known address, lost their href.
	restored int
	warnings []string
}

type crossGuideIndex map[string]map[string]crossGuideTarget

// crossGuideFormer describes the pages a guide published by an earlier run
// may still link to locally. A guide whose HTML was kept links to the pages of
// another guide; when that guide is replaced by a publisher PDF, keeps only
// its generated PDF or is downloaded again without the page, the page is gone
// and such links must go back online.
type crossGuideFormer struct {
	guides map[string]bool   // guide directories of the staged library
	pages  map[string]bool   // release-relative paths of staged HTML pages
	online map[string]string // public topic URL of each other page path on record
}

func (r *Run) localizeCrossGuideLinks(manifest *Manifest) error {
	if err := r.checkOwnership(); err != nil {
		return err
	}
	index := crossGuideIndex{}
	former := crossGuideFormer{guides: map[string]bool{}, pages: map[string]bool{}, online: map[string]string{}}
	for guide := range manifest.PDFGuides {
		former.guides[guide] = true
	}
	aliases := 0
	for guide, result := range manifest.HTMLGuides {
		former.guides[guide] = true
		if (result.Status != "complete" && result.Status != "degraded") || result.Format != "html" ||
			result.HTML == nil || result.HTMLRemoved {
			continue
		}
		if err := r.verifyGuide(path.Join(r.stage, guide), result); err != nil {
			return fmt.Errorf("verify HTML guide before cross-guide localization: %w", err)
		}
		for _, record := range result.HTML.Topics {
			former.pages[path.Join(guide, record.Path)] = true
			target := crossGuideTarget{
				guide:             guide,
				path:              record.Path,
				sourceBookmarks:   sliceSet(record.SourceBookmarks),
				bookmarksComplete: record.SourceBookmarksComplete,
			}
			for _, raw := range []string{record.URL, record.FetchURL, record.FinalURL} {
				for _, key := range crossGuideIdentityKeys(raw) {
					targetID := guide + "\x00" + record.Path
					if index[key] == nil {
						index[key] = map[string]crossGuideTarget{}
					}
					if _, exists := index[key][targetID]; !exists {
						aliases++
						if aliases > maxCrossGuideAliases {
							return errors.New("cross-guide topic identity index exceeds limit")
						}
						index[key][targetID] = target
					}
				}
			}
		}
	}
	// HTML records outlive their pages: a guide that keeps only its generated
	// PDF keeps them, and the previous manifest holds those of a guide now
	// replaced by a publisher PDF. Current records take precedence.
	for _, guides := range []map[string]model.ArchiveResult{r.previous.HTMLGuides, manifest.HTMLGuides} {
		for guide, result := range guides {
			if result.HTML == nil {
				continue
			}
			for _, record := range result.HTML.Topics {
				if name := path.Join(guide, record.Path); !former.pages[name] {
					former.online[name] = record.URL
				}
			}
		}
	}

	var pages []crossGuidePage
	for guide, result := range manifest.HTMLGuides {
		if (result.Status != "complete" && result.Status != "degraded") || result.Format != "html" ||
			result.HTML == nil || result.HTMLRemoved {
			continue
		}
		for _, record := range result.HTML.Topics {
			page, changed, err := r.prepareCrossGuidePage(index, former, guide, record)
			if err != nil {
				return err
			}
			if changed {
				pages = append(pages, page)
			}
		}
	}

	updated := map[string]model.ArchiveResult{}
	for _, page := range pages {
		result, ok := updated[page.guide]
		if !ok {
			result = cloneArchiveResult(manifest.HTMLGuides[page.guide])
		}
		if page.body != nil {
			if err := r.checkOwnership(); err != nil {
				return err
			}
			name := path.Join(r.stage, page.guide, page.path)
			if err := storage.Atomic(r.root, name, page.body); err != nil {
				return fmt.Errorf("write cross-guide topic %s: %w", name, err)
			}
			hash, size, err := storage.Hash(r.root, name, maxCrossGuideHTMLBytes)
			if err != nil {
				return err
			}
			for index := range result.HTML.Topics {
				if result.HTML.Topics[index].Path != page.path {
					continue
				}
				result.HTML.Topics[index].SHA256 = hash
				result.HTML.Topics[index].Size = size
				break
			}
			for index := range result.HTML.MissingResources {
				if result.HTML.MissingResources[index].GeneratedPagePath == page.path {
					result.HTML.MissingResources[index].GeneratedPageSHA256 = hash
				}
			}
			for index := range result.MissingResources {
				if result.MissingResources[index].GeneratedPagePath == page.path {
					result.MissingResources[index].GeneratedPageSHA256 = hash
				}
			}
			result.HTML.Integrity.CheckedLinks = max(0, result.HTML.Integrity.CheckedLinks+page.rewrites-page.restored)
		}
		for _, warning := range page.warnings {
			result.HTML.Warnings = appendUniqueBounded(result.HTML.Warnings, warning, maxCrossGuideDiagnostics)
			result.Warnings = appendUniqueBounded(result.Warnings, warning, maxCrossGuideDiagnostics)
		}
		updated[page.guide] = result
	}
	for guide, result := range updated {
		if err := r.checkOwnership(); err != nil {
			return err
		}
		if err := storage.WriteJSON(r.root, path.Join(r.stage, guide, "manifest.json"), result.HTML); err != nil {
			return fmt.Errorf("write cross-guide manifest for %s: %w", guide, err)
		}
		if err := r.verifyGuide(path.Join(r.stage, guide), result); err != nil {
			return fmt.Errorf("verify cross-guide result for %s: %w", guide, err)
		}
		manifest.HTMLGuides[guide] = result
		r.guides[guide] = result
	}
	return r.checkOwnership()
}

func (r *Run) prepareCrossGuidePage(index crossGuideIndex, former crossGuideFormer, guide string, record model.FileRecord) (crossGuidePage, bool, error) {
	result := crossGuidePage{guide: guide, path: record.Path}
	name := path.Join(r.stage, guide, record.Path)
	file, err := r.root.Open(name)
	if err != nil {
		return result, false, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxCrossGuideHTMLBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return result, false, err
	}
	if len(body) > maxCrossGuideHTMLBytes {
		return result, false, fmt.Errorf("generated topic exceeds cross-guide rewrite limit: %s", name)
	}
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return result, false, fmt.Errorf("parse generated topic %s: %w", name, err)
	}
	targetIDs := map[string]map[string]bool{}
	var firstErr error
	walkHTML(document, func(node *html.Node) {
		if firstErr != nil || node.Type != html.ElementNode || node.Data != "a" {
			return
		}
		// A link restored online may lead to another guide's page again.
		if hasHTMLClass(node, "archive-online") {
			if excludedCrossGuideAnchor(node) {
				return
			}
		} else if !former.restore(node, guide, record.Path, &result) {
			return
		}
		raw := strings.TrimSpace(htmlAttribute(node, "href"))
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return
		}
		candidates := map[string]crossGuideTarget{}
		for _, key := range crossGuideIdentityKeys(raw) {
			for id, target := range index[key] {
				candidates[id] = target
			}
		}
		if len(candidates) != 1 {
			if len(candidates) > 1 {
				result.warnings = appendUniqueBounded(result.warnings,
					"Ambiguous cross-guide topic identity retained online: "+raw, maxCrossGuideDiagnostics)
			}
			return
		}
		var target crossGuideTarget
		for _, candidate := range candidates {
			target = candidate
		}
		if target.guide == guide {
			return
		}
		if parsed.Fragment != "" {
			ids := targetIDs[target.guide+"\x00"+target.path]
			if ids == nil {
				ids, firstErr = r.generatedPageIDs(path.Join(r.stage, target.guide, target.path))
				if firstErr != nil {
					return
				}
				targetIDs[target.guide+"\x00"+target.path] = ids
			}
			if !ids[parsed.Fragment] {
				switch {
				case target.bookmarksComplete && target.sourceBookmarks[parsed.Fragment]:
					firstErr = fmt.Errorf("%s: source bookmark #%s was lost during archival",
						target.path, parsed.Fragment)
					return
				case target.bookmarksComplete:
					result.warnings = appendUniqueBounded(result.warnings,
						fmt.Sprintf("Publisher bookmark #%s is absent from source topic linked across guides: %s",
							parsed.Fragment, raw), maxCrossGuideDiagnostics)
				default:
					result.warnings = appendUniqueBounded(result.warnings,
						"Cross-guide fragment could not be verified and was retained online: "+raw,
						maxCrossGuideDiagnostics)
					return
				}
			}
		}
		relative, relErr := filepath.Rel(
			filepath.FromSlash(path.Dir(path.Join(guide, record.Path))),
			filepath.FromSlash(path.Join(target.guide, target.path)),
		)
		if relErr != nil {
			firstErr = relErr
			return
		}
		local := (&url.URL{Path: filepath.ToSlash(relative)}).EscapedPath()
		if parsed.RawQuery != "" {
			local += "?" + parsed.RawQuery
		}
		if parsed.Fragment != "" {
			local += "#" + parsed.EscapedFragment()
		}
		setHTMLAttribute(node, "href", local)
		removeHTMLClass(node, "archive-online")
		if title := htmlAttribute(node, "title"); strings.HasSuffix(title, "(Internet required)") {
			title = strings.TrimSpace(strings.TrimSuffix(title, "(Internet required)"))
			if title == "" {
				removeHTMLAttribute(node, "title")
			} else {
				setHTMLAttribute(node, "title", title)
			}
		}
		result.rewrites++
	})
	if firstErr != nil {
		return result, false, firstErr
	}
	if result.rewrites == 0 && result.restored == 0 && len(result.warnings) == 0 {
		return result, false, nil
	}
	if result.rewrites > 0 || result.restored > 0 {
		var rendered bytes.Buffer
		if err := html.Render(&rendered, document); err != nil {
			return result, false, err
		}
		result.body = rendered.Bytes()
	}
	return result, true, nil
}

// restore gives a local link into a former page of another guide the form
// the archiver gives a link it does not keep offline, the topic's public URL
// with the archive-online class, and reports whether it did. The public URL
// identifies the topic, so the query the local link kept from the link's
// original URL is not carried over. A link whose public URL is not on record
// loses its href, as the archiver drops a link it cannot resolve.
func (f crossGuideFormer) restore(node *html.Node, guide, page string, result *crossGuidePage) bool {
	parsed, err := url.Parse(strings.TrimSpace(htmlAttribute(node, "href")))
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Path == "" || path.IsAbs(parsed.Path) {
		return false
	}
	target := path.Join(path.Dir(path.Join(guide, page)), parsed.Path)
	targetGuide, targetPath, _ := strings.Cut(target, "/")
	if targetGuide == guide || !f.guides[targetGuide] || !strings.HasPrefix(targetPath, "pages/") ||
		f.pages[target] || excludedCrossGuideAnchor(node) {
		return false
	}
	result.restored++
	public, ok := f.online[target]
	if !ok {
		removeHTMLAttribute(node, "href")
		result.warnings = appendUniqueBounded(result.warnings,
			fmt.Sprintf("Cross-guide link removed from %s: %s is no longer an HTML page and its public URL is not on record",
				page, target), maxCrossGuideDiagnostics)
		return false
	}
	public, _, _ = strings.Cut(public, "#")
	if parsed.Fragment != "" {
		public += "#" + parsed.EscapedFragment()
	}
	setHTMLAttribute(node, "href", public)
	addHTMLClass(node, "archive-online")
	return true
}

func (r *Run) generatedPageIDs(name string) (map[string]bool, error) {
	file, err := r.root.Open(name)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxCrossGuideHTMLBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if len(body) > maxCrossGuideHTMLBytes {
		return nil, fmt.Errorf("generated topic exceeds cross-guide validation limit: %s", name)
	}
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	walkHTML(document, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		for _, attribute := range []string{"id", "name"} {
			if value := htmlAttribute(node, attribute); value != "" {
				ids[value] = true
			}
		}
	})
	return ids, nil
}

func crossGuideIdentityKeys(raw string) []string {
	if raw == "" {
		return nil
	}
	canonical, err := fetch.CanonicalURL(raw)
	if err != nil {
		return nil
	}
	keys := []string{"url:" + canonical}
	if identity := hpeCrossGuideIdentity(canonical); identity != "" {
		keys = append(keys, identity)
	}
	return keys
}

func hpeCrossGuideIdentity(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	query := parsed.Query()
	if len(query["docId"]) > 1 || len(query["page"]) > 1 {
		return ""
	}
	document, pageName := query.Get("docId"), query.Get("page")
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	switch {
	case parsed.Path == "/hpesc/public/docDisplay":
	case len(parts) == 5 && slices.Equal(parts[:4], []string{"hpesc", "public", "api", "document"}):
		if document != "" && document != parts[4] {
			return ""
		}
		document = parts[4]
	case len(parts) == 4 && parts[0] == "documents" && parts[2] == "html":
		if (document != "" && document != parts[1]) || (pageName != "" && pageName != parts[3]) {
			return ""
		}
		document, pageName = parts[1], parts[3]
	default:
		return ""
	}
	if document == "" {
		return ""
	}
	if pageName == "" && (parsed.Path == "/hpesc/public/docDisplay" || query.Get("ignorePayload") == "true") {
		pageName = "index.html"
	}
	if pageName == "" {
		return ""
	}
	extra := url.Values{}
	for key, values := range query {
		if key == "docId" || key == "page" || key == "mask" || key == "ignorePayload" {
			continue
		}
		extra[key] = append([]string{}, values...)
	}
	return "hpe:" + strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host) +
		"\x00" + document + "\x00" + pageName + "\x00" + extra.Encode()
}

func cloneArchiveResult(result model.ArchiveResult) model.ArchiveResult {
	cloned := result
	cloned.Errors = slices.Clone(result.Errors)
	cloned.Notices = slices.Clone(result.Notices)
	cloned.Warnings = slices.Clone(result.Warnings)
	cloned.StylesheetRecoveries = cloneStylesheetRecoveries(result.StylesheetRecoveries)
	cloned.MissingResources = slices.Clone(result.MissingResources)
	if result.HTML != nil {
		archive := *result.HTML
		archive.Inputs = slices.Clone(result.HTML.Inputs)
		archive.Topics = slices.Clone(result.HTML.Topics)
		archive.Assets = slices.Clone(result.HTML.Assets)
		archive.Notices = slices.Clone(result.HTML.Notices)
		archive.Warnings = slices.Clone(result.HTML.Warnings)
		archive.Errors = slices.Clone(result.HTML.Errors)
		archive.StylesheetRecoveries = cloneStylesheetRecoveries(result.HTML.StylesheetRecoveries)
		archive.MissingResources = slices.Clone(result.HTML.MissingResources)
		cloned.HTML = &archive
	}
	return cloned
}

func cloneStylesheetRecoveries(values []model.StylesheetRecovery) []model.StylesheetRecovery {
	cloned := make([]model.StylesheetRecovery, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].BrokenDeclarations = slices.Clone(value.BrokenDeclarations)
		cloned[index].TableStyleFamilies = slices.Clone(value.TableStyleFamilies)
		cloned[index].AffectedTopics = slices.Clone(value.AffectedTopics)
	}
	return cloned
}

func sliceSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func appendUniqueBounded(values []string, value string, limit int) []string {
	if value == "" || slices.Contains(values, value) || len(values) >= limit {
		return values
	}
	return append(values, value)
}

func walkHTML(node *html.Node, visit func(*html.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walkHTML(child, visit)
	}
}

func htmlAttribute(node *html.Node, key string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == key {
			return attribute.Val
		}
	}
	return ""
}

func setHTMLAttribute(node *html.Node, key, value string) {
	for index := range node.Attr {
		if node.Attr[index].Key == key {
			node.Attr[index].Val = value
			return
		}
	}
	node.Attr = append(node.Attr, html.Attribute{Key: key, Val: value})
}

func removeHTMLAttribute(node *html.Node, key string) {
	node.Attr = slices.DeleteFunc(node.Attr, func(attribute html.Attribute) bool {
		return attribute.Key == key
	})
}

func hasHTMLClass(node *html.Node, class string) bool {
	return slices.Contains(strings.Fields(htmlAttribute(node, "class")), class)
}

func addHTMLClass(node *html.Node, class string) {
	if !hasHTMLClass(node, class) {
		setHTMLAttribute(node, "class", strings.Join(append(strings.Fields(htmlAttribute(node, "class")), class), " "))
	}
}

func removeHTMLClass(node *html.Node, class string) {
	classes := slices.DeleteFunc(strings.Fields(htmlAttribute(node, "class")), func(value string) bool {
		return value == class
	})
	if len(classes) == 0 {
		removeHTMLAttribute(node, "class")
		return
	}
	setHTMLAttribute(node, "class", strings.Join(classes, " "))
}

func excludedCrossGuideAnchor(node *html.Node) bool {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		for _, class := range []string{"archive-nav", "archive-provenance", "archive-footer"} {
			if hasHTMLClass(parent, class) {
				return true
			}
		}
	}
	return false
}
