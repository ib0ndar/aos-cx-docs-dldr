package archive

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// contentRemovedElements are removed from publisher content together with
// everything inside them. They render nothing an offline reader needs, or
// they run script, load or submit to a URL, or change how the page loads.
var contentRemovedElements = setOf(`base bgsound button fencedframe form frame frameset input keygen
link meta noembed noframes noscript param portal script select template textarea`)

// contentUnsupportedElements embed substantive content that the archive
// cannot verify. They make the guide incomplete and keep only their fallback.
var contentUnsupportedElements = setOf(`applet audio canvas embed iframe video`)

// contentURLAttributes make a browser fetch, navigate to, embed or submit to
// a URL, or change how a page loads. Publisher content keeps one only where
// rewriteContent rewrites it to a verified local or online target.
var contentURLAttributes = setOf(`action archive attributionsrc background cite classid codebase
data dynsrc formaction href http-equiv imagesrcset longdesc lowsrc manifest ping poster profile
src srcdoc srcset usemap xlink:href`)

// rewriteContent turns publisher content into archive content. It is the
// security boundary for topic pages: elements and URL attributes survive only
// where they are rewritten to verified targets. Problems are recorded as guide
// errors and sanitizing continues, so even an unfinished page carries no
// active publisher markup. A returned error means that the content could not
// be sanitized at all, and its page must not be written.
func (a *htmlArchiver) rewriteContent(content *html.Node, page *pageInput) error {
	if depth(content) > maxHTMLDepth {
		return errors.New("publisher HTML element depth exceeds limit")
	}
	fail := func(err error) {
		if err != nil {
			a.errors = append(a.errors, page.topic.URL+": "+err.Error())
		}
	}
	fail(a.expandOldStyleThumbnails(content, page))
	fail(a.rewriteImageSlots(content, page))
	seenIDs := map[string]bool{}
	var vectors []*html.Node
	walkContent(content, func(node *html.Node) bool {
		if node.Type != html.ElementNode {
			return true
		}
		name := strings.ToLower(node.Data)
		if node != content && (name == "script" || node.Namespace == "" && contentRemovedElements[name]) {
			detach(node)
			return false
		}
		if id := attr(node, "id"); id != "" {
			if seenIDs[id] {
				removeAttr(node, "id")
				a.warnings = append(a.warnings, page.topic.URL+": removed duplicate publisher ID #"+id)
			} else {
				seenIDs[id] = true
			}
		}
		if node.Namespace == "svg" {
			// The outermost SVG element sanitizes and rewrites its whole tree.
			if name == "svg" && (node.Parent == nil || node.Parent.Namespace != "svg") {
				fail(a.assets.rewriteSVGNode(node, page.baseURL, page.path, 0, func(link *html.Node) {
					a.rewriteAnchor(link, page)
				}))
			}
			return true
		}
		if contentUnsupportedElements[name] {
			a.errors = append(a.errors, page.topic.URL+": unsupported substantive <"+name+"> content")
		}
		switch name {
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
			if item.Namespace != "" {
				// MathML may carry xlink, xmlns and xml attributes; only
				// language and white-space handling are kept.
				return item.Namespace != "xml" || key != "lang" && key != "space"
			}
			return strings.HasPrefix(key, "on") || key == "contenteditable" ||
				contentURLAttributes[key] && !rewrittenContentAttribute(node, key, item.Val)
		})
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
				removeAttr(node, "style")
				fail(err)
			} else {
				setAttr(node, "style", string(rewritten))
			}
		}
		if value := strings.TrimSpace(attr(node, "background")); value != "" {
			fail(a.rewriteBackground(node, value, page))
		}
		if node.Namespace != "" {
			return true
		}
		switch node.Data {
		case "a", "area":
			a.rewriteAnchor(node, page)
		case "source":
			if processedPictureAncestor(node) {
				return true
			}
			if value := attr(node, "src"); value != "" && !strings.HasPrefix(strings.ToLower(value), "data:") {
				local, err := a.assets.local(value, page.baseURL, page.path, dependencyAsset)
				if err != nil {
					removeAttr(node, "src")
					fail(err)
				} else {
					setAttr(node, "src", local)
				}
			}
			if err := a.rewriteSrcset(node, page); err != nil {
				removeAttr(node, "srcset")
				fail(err)
			}
		case "style":
			rewritten, diagnostics, err := rewriteCSS([]byte(nodeText(node)), false, func(reference string, kind dependencyKind) (string, error) {
				return a.assets.local(reference, page.baseURL, page.path, kind)
			})
			a.addCSSDiagnostics(diagnostics)
			for node.FirstChild != nil {
				node.RemoveChild(node.FirstChild)
			}
			if err != nil {
				fail(err)
			} else {
				a.assets.recordFontFaceRecoveries(page.topic.URL, diagnostics.FontFaces)
				node.AppendChild(&html.Node{Type: html.TextNode, Data: string(rewritten)})
			}
		}
		return true
	})
	for _, node := range vectors {
		if node.Parent == nil {
			continue
		}
		if err := a.rewriteVector(node, page); err != nil {
			neutralizeVector(node)
			fail(err)
		}
	}
	a.inlineExternalUses(content, page, fail)
	var fragmentErr error
	walk(content, func(node *html.Node) {
		if fragmentErr == nil && node.Type == html.ElementNode && node.Data == "svg" {
			fragmentErr = validateSVGFragments(node)
		}
	})
	fail(fragmentErr)
	return nil
}

// walkContent visits nodes in document order. Returning false skips the
// node's descendants, as for a node that visit has removed.
func walkContent(node *html.Node, visit func(*html.Node) bool) {
	if !visit(node) {
		return
	}
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		walkContent(child, visit)
		child = next
	}
}

// rewrittenContentAttribute reports whether rewriteContent rewrites the URL
// attribute key of node to a verified target, rather than removing it.
func rewrittenContentAttribute(node *html.Node, key, value string) bool {
	if node.Namespace != "" {
		return false
	}
	switch key {
	case "href":
		return node.Data == "a" || node.Data == "area"
	case "src", "srcset":
		return node.Data == "img" || node.Data == "source"
	case "data":
		return node.Data == "object"
	case "background":
		return true
	case "usemap":
		return strings.HasPrefix(strings.TrimSpace(value), "#")
	}
	return false
}

func (a *htmlArchiver) rewriteBackground(node *html.Node, value string, page *pageInput) error {
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "data:image/") {
		return nil
	}
	if strings.HasPrefix(lower, "data:") {
		removeAttr(node, "background")
		return nil
	}
	local, err := a.assets.local(value, page.baseURL, page.path, dependencyAsset)
	if err != nil {
		removeAttr(node, "background")
		return err
	}
	setAttr(node, "background", local)
	return nil
}

// rewriteVector archives an <img> or <object> that the raster image pass did
// not handle, inlining SVG. After an error the caller neutralizes node.
func (a *htmlArchiver) rewriteVector(node *html.Node, page *pageInput) error {
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
		if node.Data == "object" {
			// Without data, an object shows only its fallback content.
			neutralizeVector(node)
			return nil
		}
		return a.rewriteSrcset(node, page)
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		if node.Data == "object" {
			return errors.New("embedded data object cannot be independently verified")
		}
		return a.rewriteSrcset(node, page)
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
			return errors.New("unsupported substantive object content " + resolved)
		}
		local := localReference(page.path, state.record.Path)
		if parsed.Fragment != "" {
			local += "#" + parsed.EscapedFragment()
		}
		setAttr(node, "src", local)
		for _, old := range []string{"data-original", "data-mc-popup-src", "data-src"} {
			removeAttr(node, old)
		}
		return a.rewriteSrcset(node, page)
	}
	vector, _, err := a.assets.inlineSVG(resolved, page.baseURL, page.path, node, 0)
	if err != nil {
		return err
	}
	replaceNode(node, vector)
	return nil
}

// neutralizeVector removes the references of an image or object that could
// not be archived. Alternative text and fallback content remain.
func neutralizeVector(node *html.Node) {
	for _, key := range []string{"src", "srcset", "data", "data-original", "data-mc-popup-src", "data-src", "data-srcset"} {
		removeAttr(node, key)
	}
	if node.Data == "object" {
		node.Data, node.DataAtom = "span", atom.Span
	}
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
		local, err := a.linkedImage(resolved, page)
		if err != nil {
			removeAttr(node, "href")
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

// linkedImage archives the target of a link to a full-size image. The target
// must verify as an image, so that a link never opens other publisher bytes.
func (a *htmlArchiver) linkedImage(resolved string, page *pageInput) (string, error) {
	state, err := a.assets.load(canonicalNoFragment(resolved), dependencyAsset, 0)
	if err != nil {
		return "", err
	}
	switch classifyAsset(state.record.URL, state.record.ContentType, state.sourceBody, dependencyAsset) {
	case "png", "jpeg", "gif", "webp", "avif", "svg":
	default:
		return "", fmt.Errorf("linked image is not a verified image: %s", resolved)
	}
	local := localReference(page.path, state.record.Path)
	if parsed, _ := url.Parse(resolved); parsed.Fragment != "" {
		local += "#" + url.PathEscape(parsed.Fragment)
	}
	return local, nil
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

func (a *htmlArchiver) inlineExternalUses(content *html.Node, page *pageInput, fail func(error)) {
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
		if err := a.inlineExternalUse(use, page, inserted); err != nil {
			// An external reference that could not be inlined never stays.
			removeAttr(use, "href")
			fail(err)
		}
	}
}

func (a *htmlArchiver) inlineExternalUse(use *html.Node, page *pageInput, inserted map[string]string) error {
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
		return nil
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
		defs = &html.Node{Type: html.ElementNode, Data: "defs", Namespace: "svg"}
		outer.InsertBefore(defs, outer.FirstChild)
	}
	wrapper := &html.Node{Type: html.ElementNode, Data: "g", Namespace: "svg",
		Attr: []html.Attribute{{Key: "data-archive-svg", Val: attr(vector, "data-archive-svg")}}}
	for child := vector.FirstChild; child != nil; child = child.NextSibling {
		wrapper.AppendChild(cloneNode(child))
	}
	defs.AppendChild(wrapper)
	inserted[key] = newID
	setAttr(use, "href", "#"+inserted[key])
	removeAttr(use, "xlink:href")
	return nil
}
