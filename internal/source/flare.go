package source

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"github.com/titanous/json5"
	"golang.org/x/net/html"
)

var (
	dataModule     = regexp.MustCompile(`(?s)^define\s*\(\s*(\{.*\})\s*\)\s*;?$`)
	xmlDeclaration = regexp.MustCompile(`^version\s*=\s*(?:"1\.[01]"|'1\.[01]')(?:\s+encoding\s*=\s*(?:"[A-Za-z][A-Za-z0-9._-]*"|'[A-Za-z][A-Za-z0-9._-]*'))?(?:\s+standalone\s*=\s*(?:"(?:yes|no)"|'(?:yes|no)'))?\s*$`)
)

type inventoryBudgetKey struct{}

const flareHelpSystemPath = "Data/HelpSystem.xml"

type missingDetailedTOC struct {
	URL    string
	Status int
}

func WithInventoryRequestBudget(ctx context.Context, check func(int) error) context.Context {
	return context.WithValue(ctx, inventoryBudgetKey{}, check)
}

func (p *planner) module(in input) (map[string]any, error) {
	media, err := mediaType(in)
	if err != nil {
		return nil, err
	}
	if media != "" && media != "application/javascript" && media != "text/javascript" &&
		media != "application/x-javascript" && media != "text/plain" && media != "application/octet-stream" {
		return nil, fmt.Errorf("expected Flare JavaScript data module, received %s: %s", media, in.url)
	}
	body, err := inventoryText(in.body, in.headers.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	match := dataModule.FindStringSubmatch(strings.TrimSpace(string(body)))
	if match == nil {
		return nil, fmt.Errorf("expected one define({...}) data module: %s", in.url)
	}
	var value any
	err = json5.UnmarshalWithOptions([]byte(match[1]), &value, json5.Options{
		UseNumber: true, DisallowDuplicateKeys: true, MaxDepth: 2*maxDepth + 16, MaxValues: 16 * maxEntries, MaxBytes: maxInventoryBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("invalid Flare data module at %s: %w", in.url, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("expected Flare module object")
	}
	return object, p.ctx.Err()
}

func (p *planner) flare(in input) (model.DocumentPlan, error) {
	tree, err := p.html(in, false)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	root, err := documentRoot(in.url)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	htmls := nodes(tree, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "html"
	})
	hasMetadata := false
	if len(htmls) > 0 {
		if metadata, found := attr(htmls[0], "data-mc-path-to-help-system"); found {
			hasMetadata = true
			root, err = joinSource(in.url, metadata, false)
			if err != nil {
				return model.DocumentPlan{}, err
			}
		}
	}
	rootURL, _ := url.Parse(root)
	sourceURL, _ := url.Parse(in.url)
	if (!hasMetadata && !strings.Contains(sourceURL.Path, "/Content/")) || rootURL.Path == "/" ||
		!strings.HasSuffix(root, "/") || !inside(in.url, root) {
		return model.DocumentPlan{}, fmt.Errorf("invalid Flare help-system root: %s", root)
	}
	if err := p.front(p.plan.Document.URL, p.plan.Document.Title, in.url); err != nil {
		return model.DocumentPlan{}, err
	}
	p.plan.Styles, err = styles(tree, in.url)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	pages := []*html.Node{tree}
	var missingDetailed []missingDetailedTOC
	links, err := tocLinks(tree, in.url, root)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	for _, link := range links {
		detailed, err := p.read(link, "toc", scopeInside(root))
		if err != nil {
			status, missing := permanentPublisherStatus(err)
			if !missing {
				return model.DocumentPlan{}, err
			}
			if eligibilityErr := flareHelpSystemEligible(in, tree, root); eligibilityErr != nil {
				return model.DocumentPlan{}, err
			}
			missingDetailed = append(missingDetailed, missingDetailedTOC{URL: link, Status: status})
			continue
		}
		page, err := p.html(detailed, false)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		pages = append(pages, page)
		if err := p.front(link, "Table of Contents", detailed.url); err != nil {
			return model.DocumentPlan{}, err
		}
		more, err := styles(page, detailed.url)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		p.plan.Styles = append(p.plan.Styles, more...)
	}
	modules := map[string]bool{}
	for _, page := range pages {
		for _, node := range nodes(page, func(n *html.Node) bool { _, found := attr(n, "data-mc-linked-toc"); return found }) {
			href, _ := attr(node, "data-mc-linked-toc")
			raw, err := joinSource(root, href, true)
			if err != nil {
				return model.DocumentPlan{}, err
			}
			if !inside(raw, root) {
				return model.DocumentPlan{}, fmt.Errorf("Flare TOC module is outside its guide: %s", raw)
			}
			modules[raw] = true
		}
	}
	helpSystemURL := ""
	if len(missingDetailed) > 0 {
		moduleURL, err := p.flareHelpSystemModule(in, tree, root)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		helpSystemURL, err = joinSource(root, flareHelpSystemPath, false)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		if len(modules) > 1 || len(modules) == 1 && !modules[moduleURL] {
			return model.DocumentPlan{}, errors.New("missing detailed TOC shortcut conflicts with linked TOC metadata")
		}
		modules = map[string]bool{moduleURL: true}
	} else if len(modules) != 1 {
		if len(modules) != 0 {
			return model.DocumentPlan{}, fmt.Errorf("expected one complete Flare TOC module, found %d", len(modules))
		}
		moduleURL, err := p.flareHelpSystemModule(in, tree, root)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		modules[moduleURL] = true
	}
	var moduleURL string
	for raw := range modules {
		moduleURL = raw
	}
	module, err := p.read(moduleURL, "module", scopeInside(root))
	if err != nil {
		return model.DocumentPlan{}, err
	}
	data, err := p.module(module)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	count, err := integer(data["numchunks"])
	prefix, validPrefix := data["prefix"].(string)
	treeData, validTree := data["tree"].(map[string]any)
	treeNodes, validNodes := treeData["n"].([]any)
	if err != nil || count < 1 || count > maxChunks || !validPrefix || !bookPattern.MatchString(prefix) ||
		!validTree || !validNodes || len(treeNodes) == 0 {
		return model.DocumentPlan{}, errors.New("invalid Flare chunk count, prefix or tree")
	}
	if check, ok := p.ctx.Value(inventoryBudgetKey{}).(func(int) error); ok {
		if err := check(count); err != nil {
			return model.DocumentPlan{}, err
		}
	}
	type entry struct {
		title, url string
		chunk      int
	}
	entries := map[int]entry{}
	var chunkURLs []string
	for chunkNumber := 0; chunkNumber < count; chunkNumber++ {
		chunkURL, err := joinSource(moduleURL, fmt.Sprintf("%s%d.js", prefix, chunkNumber), false)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		chunk, err := p.read(chunkURL, "chunk", scopeInside(root))
		if err != nil {
			return model.DocumentPlan{}, err
		}
		chunkData, err := p.module(chunk)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		chunkURLs = append(chunkURLs, chunkURL)
		keys := make([]string, 0, len(chunkData))
		for key := range chunkData {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, href := range keys {
			item, ok := chunkData[href].(map[string]any)
			if !ok {
				return model.DocumentPlan{}, errors.New("invalid Flare chunk entry")
			}
			indices, iok := item["i"].([]any)
			titles, tok := item["t"].([]any)
			bookmarks, bok := item["b"].([]any)
			if !iok || len(indices) == 0 || !tok || !bok || len(indices) != len(titles) || len(indices) != len(bookmarks) {
				return model.DocumentPlan{}, errors.New("invalid Flare index/title/bookmark arrays")
			}
			for pos, value := range indices {
				index, err := integer(value)
				if err != nil || index < 0 {
					return model.DocumentPlan{}, errors.New("invalid Flare index")
				}
				if _, found := entries[index]; found {
					return model.DocumentPlan{}, fmt.Errorf("duplicate Flare index %d", index)
				}
				title, err := stringLabel(titles[pos], "Flare topic title")
				if err != nil {
					return model.DocumentPlan{}, err
				}
				bookmark, ok := bookmarks[pos].(string)
				if !ok {
					return model.DocumentPlan{}, errors.New("invalid Flare bookmark")
				}
				raw := ""
				if href != "___" {
					raw, err = joinSource(root, href, true)
					if err != nil {
						return model.DocumentPlan{}, err
					}
					if bookmark != "" {
						raw, err = fetch.CanonicalURL(raw)
						if err != nil {
							return model.DocumentPlan{}, err
						}
						raw, err = fetch.NormalizeURL(raw + "#" + strings.TrimLeft(bookmark, "#"))
						if err != nil {
							return model.DocumentPlan{}, err
						}
					}
				}
				entries[index] = entry{title, raw, chunkNumber}
				if len(entries) > maxEntries {
					return model.DocumentPlan{}, errors.New("Flare chunk inventory exceeds entry limit")
				}
			}
		}
	}
	used := map[int]bool{}
	local := false
	var walk func([]any, int) ([]model.TocEntry, error)
	walk = func(values []any, depth int) ([]model.TocEntry, error) {
		result := []model.TocEntry{}
		for _, value := range values {
			if err := p.check(depth); err != nil {
				return nil, err
			}
			node, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("invalid Flare tree node")
			}
			index, err := integer(node["i"])
			if err != nil {
				return nil, err
			}
			e, found := entries[index]
			if !found {
				return nil, fmt.Errorf("Flare tree references missing index %d", index)
			}
			if value, found := node["c"]; found {
				chunk, err := integer(value)
				if err != nil || chunk != e.chunk {
					return nil, fmt.Errorf("Flare tree references wrong chunk at index %d", index)
				}
			}
			children := []any{}
			if value, found := node["n"]; found {
				var ok bool
				children, ok = value.([]any)
				if !ok {
					return nil, errors.New("invalid Flare tree children")
				}
			}
			used[index] = true
			if e.url != "" {
				if inside(e.url, root) {
					local = true
				}
				if err := p.navigation(e.url, e.title, root); err != nil {
					return nil, err
				}
			}
			nested, err := walk(children, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, model.TocEntry{Title: e.title, URL: e.url, Children: nested})
		}
		return result, nil
	}
	toc, err := walk(treeNodes, 0)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if len(used) != len(entries) {
		return model.DocumentPlan{}, errors.New("Flare chunks contain entries absent from the TOC tree")
	}
	if !local {
		return model.DocumentPlan{}, errors.New("Flare TOC contains no local topics")
	}
	for _, missing := range missingDetailed {
		missingURL, err := fetch.CanonicalURL(missing.URL)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		for _, entry := range entries {
			if entry.url == "" {
				continue
			}
			entryURL, err := fetch.CanonicalURL(entry.url)
			if err != nil {
				return model.DocumentPlan{}, err
			}
			if entryURL == missingURL {
				return model.DocumentPlan{}, fmt.Errorf(
					"missing detailed TOC shortcut is an authoritative local topic: %s", missing.URL)
			}
		}
		p.plan.Warnings = append(p.plan.Warnings, fmt.Sprintf(
			"Publisher detailed TOC shortcut %s returned HTTP %d; used source-verified HelpSystem %s and module %s because the missing shortcut is absent from the complete authoritative TOC.",
			missing.URL, missing.Status, helpSystemURL, moduleURL))
		p.plan.UnavailableNavigation = append(p.plan.UnavailableNavigation, model.UnavailableNavigation{
			URL: missing.URL, HTTPStatus: missing.Status, Role: "optional-detailed-toc",
		})
	}
	p.plan.TOC = append(p.plan.TOC, toc...)
	return p.finish(root, moduleURL, chunkURLs)
}

func (p *planner) flareHelpSystemModule(front input, tree *html.Node, root string) (string, error) {
	if err := flareHelpSystemEligible(front, tree, root); err != nil {
		return "", err
	}
	helpURL, err := joinSource(root, flareHelpSystemPath, false)
	if err != nil {
		return "", err
	}
	helpSystem, err := p.read(helpURL, "help-system", scopeInside(root))
	if err != nil {
		return "", err
	}
	return flareHelpSystemTOC(helpSystem, root)
}

func flareHelpSystemEligible(front input, tree *html.Node, root string) error {
	htmls := nodes(tree, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "html"
	})
	if len(htmls) != 1 {
		return errors.New("expected one complete Flare TOC module, found 0")
	}
	targetType, hasTargetType := singleFlareAttribute(htmls[0], "data-mc-target-type")
	helpName, hasHelpName := singleFlareAttribute(htmls[0], "data-mc-help-system-file-name")
	pathToHelp, hasPathToHelp := singleFlareAttribute(htmls[0], "data-mc-path-to-help-system")
	hasTOCWidget := len(nodes(tree, func(n *html.Node) bool {
		value, found := attr(n, "data-mc-toc")
		return n.Type == html.ElementNode && found && strings.EqualFold(strings.TrimSpace(value), "true")
	})) > 0
	expectedRoot, err := documentRoot(front.url)
	if err != nil {
		return err
	}
	if !hasTargetType || !strings.EqualFold(strings.TrimSpace(targetType), "WebHelp2") ||
		!hasHelpName || !strings.EqualFold(strings.TrimSpace(helpName), "index.xml") ||
		!hasPathToHelp || strings.TrimSpace(pathToHelp) == "" || !hasTOCWidget ||
		root != expectedRoot {
		return errors.New("expected one complete Flare TOC module, found 0")
	}
	return nil
}

func permanentPublisherStatus(err error) (int, bool) {
	var status *fetch.StatusError
	if errors.As(err, &status) && (status.Status == http.StatusNotFound || status.Status == http.StatusGone) {
		return status.Status, true
	}
	return 0, false
}

func singleFlareAttribute(node *html.Node, name string) (string, bool) {
	value := ""
	count := 0
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			value = attribute.Val
			count++
		}
	}
	return value, count == 1
}

func flareHelpSystemTOC(in input, root string) (string, error) {
	media, err := mediaType(in)
	if err != nil {
		return "", err
	}
	switch media {
	case "", "application/xml", "text/xml", "application/octet-stream", "text/plain":
	default:
		return "", fmt.Errorf("expected Flare WebHelpSystem XML, received %s: %s", media, in.url)
	}
	body, err := inventoryText(in.body, in.headers.Get("Content-Type"))
	if err != nil {
		return "", err
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	depth := 0
	elements := 0
	rootSeen := false
	rootClosed := false
	declarationSeen := false
	tocCount := 0
	toc := ""
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("invalid Flare WebHelpSystem XML at %s: %w", in.url, err)
		}
		switch value := token.(type) {
		case xml.Directive:
			return "", fmt.Errorf("Flare WebHelpSystem XML directives are unsupported: %s", in.url)
		case xml.ProcInst:
			if rootSeen || declarationSeen || value.Target != "xml" ||
				!xmlDeclaration.MatchString(strings.TrimSpace(string(value.Inst))) {
				return "", fmt.Errorf("Flare WebHelpSystem XML processing instructions are unsupported: %s", in.url)
			}
			declarationSeen = true
		case xml.StartElement:
			if rootClosed {
				return "", fmt.Errorf("Flare WebHelpSystem XML has multiple root elements: %s", in.url)
			}
			depth++
			elements++
			if depth > 32 || elements > 256 {
				return "", fmt.Errorf("Flare WebHelpSystem XML exceeds structural limits: %s", in.url)
			}
			if !rootSeen {
				rootSeen = true
				if value.Name.Space != "" || value.Name.Local != "WebHelpSystem" {
					return "", fmt.Errorf("expected WebHelpSystem XML root: %s", in.url)
				}
				targetTypes := 0
				for _, attribute := range value.Attr {
					if attribute.Name.Local != "TargetType" {
						continue
					}
					targetTypes++
					if attribute.Name.Space != "" || attribute.Value != "WebHelp2" {
						return "", fmt.Errorf("expected WebHelp2 HelpSystem metadata: %s", in.url)
					}
				}
				if targetTypes != 1 {
					return "", fmt.Errorf("expected one WebHelp2 TargetType attribute: %s", in.url)
				}
			}
			for _, attribute := range value.Attr {
				if attribute.Name.Local != "Toc" {
					continue
				}
				if depth != 1 {
					return "", fmt.Errorf("Flare HelpSystem Toc attribute is not on the root element: %s", in.url)
				}
				tocCount++
				if attribute.Name.Space != "" || strings.TrimSpace(attribute.Value) == "" {
					return "", fmt.Errorf("invalid Flare HelpSystem Toc attribute: %s", in.url)
				}
				toc = strings.TrimSpace(attribute.Value)
			}
		case xml.EndElement:
			depth--
			if depth < 0 {
				return "", fmt.Errorf("invalid Flare WebHelpSystem nesting: %s", in.url)
			}
			if depth == 0 {
				rootClosed = true
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(value)) != "" {
				return "", fmt.Errorf("Flare WebHelpSystem XML has data outside its root element: %s", in.url)
			}
		}
	}
	if !rootSeen || !rootClosed || depth != 0 {
		return "", fmt.Errorf("incomplete Flare WebHelpSystem XML: %s", in.url)
	}
	if tocCount != 1 {
		return "", fmt.Errorf("expected one Flare HelpSystem Toc attribute, found %d", tocCount)
	}
	moduleURL, err := joinSource(root, toc, false)
	if err != nil {
		return "", err
	}
	if !inside(moduleURL, root) {
		return "", fmt.Errorf("Flare HelpSystem TOC module is outside its guide: %s", moduleURL)
	}
	module, err := url.Parse(moduleURL)
	if err != nil || module.RawQuery != "" || module.Fragment != "" {
		return "", fmt.Errorf("Flare HelpSystem TOC module has unsupported query or fragment: %s", moduleURL)
	}
	return moduleURL, nil
}
