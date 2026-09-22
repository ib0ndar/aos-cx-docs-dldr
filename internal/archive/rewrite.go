package archive

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

func (a *htmlArchiver) rewriteContent(content *html.Node, page *pageInput) error {
	if depth(content) > maxHTMLDepth {
		return errors.New("publisher HTML element depth exceeds limit")
	}
	if err := a.expandOldStyleThumbnails(content, page); err != nil {
		return err
	}
	if err := a.rewriteImageSlots(content, page); err != nil {
		return err
	}
	seenIDs := map[string]bool{}
	var remove []*html.Node
	var vectors []*html.Node
	var firstErr error
	walk(content, func(node *html.Node) {
		if firstErr != nil || node.Type != html.ElementNode {
			return
		}
		name := strings.ToLower(node.Data)
		switch name {
		case "script", "form", "input", "button", "select", "textarea", "noscript":
			remove = append(remove, node)
			return
		case "iframe", "video", "audio", "canvas", "embed":
			a.errors = append(a.errors, page.topic.URL+": unsupported substantive <"+name+"> content")
		case "details":
			setAttr(node, "open", "")
		case "img":
			if attr(node, "data-archive-image-processed") != "true" {
				vectors = append(vectors, node)
			} else {
				removeAttr(node, "data-archive-image-processed")
			}
		case "object":
			vectors = append(vectors, node)
		}
		if strings.Contains(name, ":") {
			switch {
			case strings.Contains(name, "dropdown"), strings.Contains(name, "body"):
				node.Data = "div"
			case strings.Contains(name, "xref"):
				node.Data = "a"
			default:
				node.Data = "span"
			}
		}
		node.Attr = slices.DeleteFunc(node.Attr, func(item html.Attribute) bool {
			key := strings.ToLower(item.Key)
			return strings.HasPrefix(key, "on") || key == "contenteditable"
		})
		if id := attr(node, "id"); id != "" {
			if seenIDs[id] {
				removeAttr(node, "id")
				a.warnings = append(a.warnings, page.topic.URL+": removed duplicate publisher ID #"+id)
			} else {
				seenIDs[id] = true
			}
		}
		if hasAttr(node, "hidden") || hasClass(node, "mcdropdownbody") || hasClass(node, "mctoggler") || attr(node, "data-mc-target-name") != "" {
			removeAttr(node, "hidden")
			setAttr(node, "data-archive-expanded", "true")
			if style := attr(node, "style"); style != "" {
				setAttr(node, "style", removeDisplayNone(style))
			}
		}
		if style := attr(node, "style"); style != "" {
			rewritten, diagnostics, err := rewriteCSS([]byte(style), true, func(reference string, kind dependencyKind) (string, error) {
				return a.assets.local(reference, page.baseURL, page.path, kind)
			})
			a.addCSSDiagnostics(diagnostics)
			if err != nil {
				firstErr = err
				return
			}
			setAttr(node, "style", string(rewritten))
		}
		switch node.Data {
		case "a":
			a.rewriteAnchor(node, page)
		case "img":
		case "source":
			if processedPictureAncestor(node) {
				return
			}
			if value := attr(node, "src"); value != "" && !strings.HasPrefix(strings.ToLower(value), "data:") {
				local, err := a.assets.local(value, page.baseURL, page.path, dependencyAsset)
				if err != nil {
					firstErr = err
					return
				}
				setAttr(node, "src", local)
			}
			if err := a.rewriteSrcset(node, page); err != nil {
				firstErr = err
			}
		case "style":
			rewritten, diagnostics, err := rewriteCSS([]byte(nodeText(node)), false, func(reference string, kind dependencyKind) (string, error) {
				return a.assets.local(reference, page.baseURL, page.path, kind)
			})
			a.addCSSDiagnostics(diagnostics)
			if err != nil {
				firstErr = err
				return
			}
			for node.FirstChild != nil {
				node.RemoveChild(node.FirstChild)
			}
			node.AppendChild(&html.Node{Type: html.TextNode, Data: string(rewritten)})
		case "svg":
			if err := a.assets.rewriteSVGNode(node, page.baseURL, page.path, 0, true); err != nil {
				firstErr = err
			}
		}
	})
	for _, node := range remove {
		detach(node)
	}
	if firstErr != nil {
		return firstErr
	}
	for _, node := range vectors {
		if node.Parent == nil {
			continue
		}
		raw := attr(node, "src")
		if node.Data == "object" {
			raw = attr(node, "data")
		} else {
			for _, key := range []string{"data-original", "data-mc-popup-src", "data-src", "src"} {
				if value := strings.TrimSpace(attr(node, key)); value != "" && !strings.HasPrefix(strings.ToLower(value), "data:") {
					raw = value
					break
				}
			}
		}
		if raw == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(raw), "data:") {
			if node.Data == "object" {
				a.errors = append(a.errors, page.topic.URL+": embedded data object cannot be independently verified")
			} else if err := a.rewriteSrcset(node, page); err != nil {
				return err
			}
			continue
		}
		resolved, err := resolveReference(page.baseURL, raw)
		if err != nil {
			return err
		}
		parsed, _ := url.Parse(resolved)
		state, err := a.assets.load(canonicalNoFragment(resolved), dependencyAsset, 0)
		if err != nil {
			return err
		}
		isSVG := classifyAsset(state.record.URL, state.record.ContentType, state.sourceBody, dependencyAsset) == "svg" ||
			strings.EqualFold(attr(node, "type"), "image/svg+xml")
		if !isSVG {
			if node.Data == "object" {
				a.errors = append(a.errors, page.topic.URL+": unsupported substantive object content "+resolved)
				continue
			}
			local := localReference(page.path, state.record.Path)
			if parsed.Fragment != "" {
				local += "#" + parsed.EscapedFragment()
			}
			setAttr(node, "src", local)
			for _, old := range []string{"data-original", "data-mc-popup-src", "data-src"} {
				removeAttr(node, old)
			}
			if err := a.rewriteSrcset(node, page); err != nil {
				return err
			}
			continue
		}
		vector, _, err := a.assets.inlineSVG(resolved, page.baseURL, page.path, node, 0)
		if err != nil {
			return err
		}
		replaceNode(node, vector)
	}
	if err := a.inlineExternalUses(content, page); err != nil {
		return err
	}
	var fragmentErr error
	walk(content, func(node *html.Node) {
		if fragmentErr == nil && node.Type == html.ElementNode && node.Data == "svg" {
			fragmentErr = validateSVGFragments(node)
		}
	})
	return fragmentErr
}

func (a *htmlArchiver) rewriteAnchor(node *html.Node, page *pageInput) {
	raw := strings.TrimSpace(attr(node, "href"))
	if raw == "" {
		return
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "javascript:") {
		removeAttr(node, "href")
		return
	}
	if strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "tel:") {
		setAttr(node, "href", raw)
		addClass(node, "archive-online")
		return
	}
	if strings.HasPrefix(raw, "#") {
		// Fragment-only links already point at the current generated page. Their
		// source identity is validated during the first and second archive passes.
		return
	}
	if target := a.topicForReference(raw, page); target != nil {
		local := localReference(page.path, target.path)
		if fragment := linkFragment(raw); fragment != "" {
			local += "#" + url.PathEscape(fragment)
		}
		setAttr(node, "href", local)
		return
	}
	resolved, err := resolveReference(page.baseURL, raw)
	if err != nil {
		removeAttr(node, "href")
		a.warnings = append(a.warnings, page.topic.URL+": invalid or unsupported link removed: "+raw)
		return
	}
	parsed, _ := url.Parse(resolved)
	if target := a.topicForReference(resolved, page); target != nil {
		local := localReference(page.path, target.path)
		if parsed.Fragment != "" {
			local += "#" + parsed.EscapedFragment()
		}
		setAttr(node, "href", local)
		return
	}
	if hasClass(node, "mcpopupthumbnaillink") || imageExtension(parsed.Path) {
		local, err := a.assets.local(raw, page.baseURL, page.path, dependencyAsset)
		if err != nil {
			a.errors = append(a.errors, page.topic.URL+": linked asset: "+err.Error())
			return
		}
		setAttr(node, "href", local)
		return
	}
	if a.plan.Document.Kind == "hpe" {
		_, looksHPE := parseHPEReference(raw, page)
		query := parsed.Query()
		looksHPE = looksHPE || hpeBarePage.MatchString(raw) ||
			a.hpeHosts[strings.ToLower(parsed.Host)] &&
				(query.Has("docId") || query.Has("page") ||
					strings.Contains(strings.ToLower(parsed.Path), "/document"))
		if looksHPE {
			a.warnings = append(a.warnings, "Unplanned or ambiguous HPE topic alias retained online: "+resolved)
		}
	}
	setAttr(node, "href", resolved)
	addClass(node, "archive-online")
}

func (a *htmlArchiver) rewriteImage(node *html.Node, page *pageInput) error {
	for _, key := range []string{"data-original", "data-mc-popup-src", "data-src", "src"} {
		value := strings.TrimSpace(attr(node, key))
		if value == "" || strings.HasPrefix(strings.ToLower(value), "data:") {
			continue
		}
		local, err := a.assets.local(value, page.baseURL, page.path, dependencyAsset)
		if err != nil {
			return err
		}
		setAttr(node, "src", local)
		for _, old := range []string{"data-original", "data-mc-popup-src", "data-src"} {
			removeAttr(node, old)
		}
		break
	}
	return a.rewriteSrcset(node, page)
}

func (a *htmlArchiver) rewriteSrcset(node *html.Node, page *pageInput) error {
	for _, key := range []string{"data-srcset", "srcset"} {
		value := strings.TrimSpace(attr(node, key))
		if value == "" {
			continue
		}
		candidates, err := parseSrcset(value)
		if err != nil {
			return err
		}
		for i := range candidates {
			if strings.HasPrefix(strings.ToLower(candidates[i][0]), "data:") {
				continue
			}
			candidates[i][0], err = a.assets.local(candidates[i][0], page.baseURL, page.path, dependencyAsset)
			if err != nil {
				return err
			}
		}
		var values []string
		for _, candidate := range candidates {
			values = append(values, strings.TrimSpace(strings.Join(candidate, " ")))
		}
		setAttr(node, "srcset", strings.Join(values, ", "))
		if key == "data-srcset" {
			removeAttr(node, key)
		}
	}
	return nil
}

func parseSrcset(value string) ([][]string, error) {
	var result [][]string
	for offset := 0; offset < len(value); {
		for offset < len(value) && (value[offset] == ',' || isASCIISpace(value[offset])) {
			offset++
		}
		if offset == len(value) {
			break
		}
		var source string
		separated := false
		if value[offset] == '\'' || value[offset] == '"' {
			quote := value[offset]
			offset++
			start := offset
			for offset < len(value) && value[offset] != quote {
				offset++
			}
			if offset == len(value) {
				return nil, errors.New("unterminated quoted srcset URL")
			}
			source = value[start:offset]
			offset++
		} else {
			start := offset
			for offset < len(value) && !isASCIISpace(value[offset]) {
				offset++
			}
			token := value[start:offset]
			separated = !strings.HasPrefix(strings.ToLower(token), "data:") && strings.HasSuffix(token, ",")
			source = strings.TrimRight(token, ",")
		}
		if separated {
			result = append(result, []string{source})
			continue
		}
		for offset < len(value) && isASCIISpace(value[offset]) {
			offset++
		}
		start := offset
		for offset < len(value) && value[offset] != ',' {
			offset++
		}
		descriptor := strings.TrimSpace(value[start:offset])
		if source == "" || len(strings.Fields(descriptor)) > 1 {
			return nil, fmt.Errorf("unsupported srcset candidate near %q", value[start:])
		}
		candidate := []string{source}
		if descriptor != "" {
			candidate = append(candidate, descriptor)
		}
		result = append(result, candidate)
	}
	if len(result) == 0 {
		return nil, errors.New("empty srcset")
	}
	return result, nil
}

func isASCIISpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n' || value == '\f'
}

func imageExtension(value string) bool {
	switch strings.ToLower(path.Ext(value)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg":
		return true
	default:
		return false
	}
}

func depth(node *html.Node) int {
	maximum := 0
	var visit func(*html.Node, int)
	visit = func(current *html.Node, level int) {
		if level > maximum {
			maximum = level
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			visit(child, level+1)
		}
	}
	visit(node, 1)
	return maximum
}

func removeDisplayNone(style string) string {
	parts := strings.Split(style, ";")
	var kept []string
	for _, declaration := range parts {
		name, value, found := strings.Cut(declaration, ":")
		if found && strings.EqualFold(strings.TrimSpace(name), "display") && strings.EqualFold(strings.TrimSpace(value), "none") {
			continue
		}
		if strings.TrimSpace(declaration) != "" {
			kept = append(kept, declaration)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, ";") + ";"
}

func (a *htmlArchiver) inlineExternalUses(content *html.Node, page *pageInput) error {
	var uses []*html.Node
	walk(content, func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "use" {
			raw := attr(node, "href")
			if raw == "" {
				raw = attr(node, "xlink:href")
			}
			if raw != "" && !strings.HasPrefix(raw, "#") {
				uses = append(uses, node)
			}
		}
	})
	inserted := map[string]string{}
	for _, use := range uses {
		raw := attr(use, "href")
		if raw == "" {
			raw = attr(use, "xlink:href")
		}
		resolved, err := resolveReference(page.baseURL, raw)
		if err != nil {
			return err
		}
		parsed, _ := url.Parse(resolved)
		if parsed.Fragment == "" {
			return fmt.Errorf("external SVG use has no fragment: %s", resolved)
		}
		key := canonicalNoFragment(resolved) + "#" + parsed.Fragment
		if id := inserted[key]; id != "" {
			setAttr(use, "href", "#"+id)
			removeAttr(use, "xlink:href")
			continue
		}
		vector, ids, err := a.assets.inlineSVG(resolved, page.baseURL, page.path, nil, 0)
		if err != nil {
			return err
		}
		fragment, decodeErr := url.PathUnescape(parsed.Fragment)
		if decodeErr != nil {
			return decodeErr
		}
		newID := ids[fragment]
		if newID == "" || findNode(vector, func(node *html.Node) bool { return attr(node, "id") == newID }) == nil {
			return fmt.Errorf("SVG source fragment mismatch: #%s", parsed.Fragment)
		}
		outer := use.Parent
		for outer != nil && outer.Data != "svg" {
			outer = outer.Parent
		}
		if outer == nil {
			return errors.New("SVG use is not contained in an svg element")
		}
		defs := findNode(outer, func(node *html.Node) bool { return node.Parent == outer && node.Data == "defs" })
		if defs == nil {
			defs = &html.Node{Type: html.ElementNode, Data: "defs"}
			outer.InsertBefore(defs, outer.FirstChild)
		}
		wrapper := &html.Node{Type: html.ElementNode, Data: "g",
			Attr: []html.Attribute{{Key: "data-archive-svg", Val: attr(vector, "data-archive-svg")}}}
		for child := vector.FirstChild; child != nil; child = child.NextSibling {
			wrapper.AppendChild(cloneNode(child))
		}
		defs.AppendChild(wrapper)
		inserted[key] = newID
		setAttr(use, "href", "#"+inserted[key])
		removeAttr(use, "xlink:href")
	}
	return nil
}
