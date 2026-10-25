package archive

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	svgNamespaceURI   = "http://www.w3.org/2000/svg"
	xlinkNamespaceURI = "http://www.w3.org/1999/xlink"
	xmlNamespaceURI   = "http://www.w3.org/XML/1998/namespace"
	// foreignNamespace marks parsed elements and attributes outside the SVG,
	// XLink and XML namespaces. Browsers do not render them as SVG, and the
	// sanitizer removes them.
	foreignNamespace = "foreign"
)

// svgElements lists the SVG-namespace elements an archived SVG may contain.
// Script, foreignObject, event listeners and elements that browsers no longer
// render are absent. Animation elements are also checked by safeSVGAnimation.
var svgElements = setOf(`a animate animateMotion animateTransform circle clipPath defs desc ellipse
feBlend feColorMatrix feComponentTransfer feComposite feConvolveMatrix feDiffuseLighting
feDisplacementMap feDistantLight feDropShadow feFlood feFuncA feFuncB feFuncG feFuncR
feGaussianBlur feImage feMerge feMergeNode feMorphology feOffset fePointLight
feSpecularLighting feSpotLight feTile feTurbulence filter g image line linearGradient marker
mask metadata mpath path pattern polygon polyline radialGradient rect set stop style svg
switch symbol text textPath title tspan use view`)

var svgAnimationElements = setOf(`animate animateMotion animateTransform set`)

// svgAttributes lists, in lower case, the unnamespaced attributes an archived
// SVG element may keep besides href, style, aria-* and data-*.
var svgAttributes = setOf(`accumulate additive alignment-baseline amplitude attributename
attributetype azimuth basefrequency baseline-shift baseprofile begin bias by calcmode class clip
clip-path clip-rule clippathunits color color-interpolation color-interpolation-filters
color-profile color-rendering contentstyletype cursor cx cy d decoding diffuseconstant direction
display divisor dominant-baseline dur dx dy edgemode elevation enable-background end exponent
externalresourcesrequired fill fill-opacity fill-rule filter filterres filterunits flood-color
flood-opacity focusable font font-family font-feature-settings font-kerning font-size
font-size-adjust font-stretch font-style font-variant font-weight fr from fx fy
glyph-orientation-horizontal glyph-orientation-vertical gradienttransform gradientunits height
hreflang id image-rendering in in2 intercept isolation k k1 k2 k3 k4 kernelmatrix
kernelunitlength kerning keypoints keysplines keytimes lang lengthadjust letter-spacing
lighting-color limitingconeangle line-height marker marker-end marker-mid marker-start
markerheight markerunits markerwidth mask mask-type maskcontentunits maskunits max media method
min mix-blend-mode mode numoctaves offset opacity operator order orient origin overflow
paint-order path pathlength patterncontentunits patterntransform patternunits pointer-events
points pointsatx pointsaty pointsatz preservealpha preserveaspectratio primitiveunits r radius
refx refy rel repeatcount repeatdur requiredextensions requiredfeatures restart result role
rotate rx ry scale seed shape-rendering side slope solid-color solid-opacity spacing
specularconstant specularexponent spreadmethod startoffset stddeviation stitchtiles stop-color
stop-opacity stroke stroke-dasharray stroke-dashoffset stroke-linecap stroke-linejoin
stroke-miterlimit stroke-opacity stroke-width systemlanguage tabindex tablevalues target targetx
targety text-anchor text-decoration text-overflow text-rendering textlength title to transform
transform-box transform-origin type unicode-bidi values vector-effect version viewbox
visibility white-space width word-spacing writing-mode x x1 x2 xchannelselector y y1 y2
ychannelselector z zoomandpan`)

// svgURLAttributes are presentation attributes whose CSS values can load a
// URL. They pass through the same CSS rewriting as style attributes.
var svgURLAttributes = setOf(`clip-path cursor fill filter marker marker-end marker-mid marker-start mask stroke`)

func setOf(values string) map[string]bool {
	result := map[string]bool{}
	for _, value := range strings.Fields(values) {
		result[value] = true
	}
	return result
}

func (m *assetManager) rewriteSVG(body []byte, base, owner string, depth int) ([]byte, error) {
	svg, styles, err := parseSVGDocument(body)
	if err != nil {
		return nil, err
	}
	if err := m.rewriteSVGNode(svg, base, owner, depth, nil); err != nil {
		return nil, err
	}
	for _, reference := range styles {
		local, err := m.local(reference, base, owner, dependencySVGStyle)
		if err != nil {
			return nil, err
		}
		style := &html.Node{Type: html.ElementNode, Data: "style", Namespace: "svg",
			Attr: []html.Attribute{{Key: "data-archive-svg", Val: "stylesheet"}}}
		style.AppendChild(&html.Node{Type: html.TextNode, Data: `@import url("` + strings.ReplaceAll(local, `"`, `\"`) + `");`})
		svg.InsertBefore(style, svg.FirstChild)
	}
	if err := validateSVGFragments(svg); err != nil {
		return nil, err
	}
	// Standalone files open as XML, so the root declares the namespaces that
	// sanitizing kept; the original declarations were not carried over.
	namespaces := []html.Attribute{{Key: "xmlns", Val: svgNamespaceURI}}
	if findNode(svg, func(n *html.Node) bool {
		return slices.ContainsFunc(n.Attr, func(item html.Attribute) bool { return item.Namespace == "xlink" })
	}) != nil {
		namespaces = append(namespaces, html.Attribute{Namespace: "xmlns", Key: "xlink", Val: xlinkNamespaceURI})
	}
	svg.Attr = append(namespaces, svg.Attr...)
	return renderNode(svg)
}

// parseSVGDocument builds an element tree from the XML token stream with
// namespaces resolved, as a browser does when it opens the file. Elements in
// other namespaces are marked foreignNamespace, so XHTML or SVG markup behind
// any prefix is recognized for what it is. A document that uses no namespace
// at all is read as SVG, as it would be when inlined into HTML.
func parseSVGDocument(body []byte) (*html.Node, []string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	var styles []string
	var root, current *html.Node
	depth := 0
	unqualified := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("invalid SVG XML: %w", err)
		}
		switch value := token.(type) {
		case xml.Directive:
			if strings.Contains(strings.ToLower(string(value)), "doctype") {
				return nil, nil, errors.New("SVG document type declarations are unsupported")
			}
		case xml.ProcInst:
			if strings.EqualFold(value.Target, "xml-stylesheet") {
				attrs, err := processingAttributes(string(value.Inst))
				if err != nil {
					return nil, nil, err
				}
				if href := attrs["href"]; href != "" {
					styles = append(styles, href)
				}
			}
		case xml.StartElement:
			depth++
			if depth > maxHTMLDepth {
				return nil, nil, errors.New("SVG element depth exceeds limit")
			}
			if root == nil {
				if value.Name.Local != "svg" || value.Name.Space != svgNamespaceURI && value.Name.Space != "" {
					return nil, nil, errors.New("SVG resource has no svg root element")
				}
				unqualified = value.Name.Space == ""
			} else if current == nil {
				return nil, nil, errors.New("SVG resource has more than one root element")
			}
			node := &html.Node{Type: html.ElementNode, Data: value.Name.Local, Namespace: "svg"}
			if value.Name.Space != svgNamespaceURI && (!unqualified || value.Name.Space != "") {
				node.Namespace = foreignNamespace
			}
			for _, item := range value.Attr {
				switch {
				case item.Name.Space == "xmlns" || item.Name.Space == "" && item.Name.Local == "xmlns":
					// The decoder has already applied namespace declarations.
				case item.Name.Space == "":
					node.Attr = append(node.Attr, html.Attribute{Key: item.Name.Local, Val: item.Value})
				case item.Name.Space == xlinkNamespaceURI || item.Name.Space == "xlink":
					// An undeclared xlink prefix is read as inline SVG in HTML reads it.
					node.Attr = append(node.Attr, html.Attribute{Namespace: "xlink", Key: item.Name.Local, Val: item.Value})
				case item.Name.Space == xmlNamespaceURI:
					node.Attr = append(node.Attr, html.Attribute{Namespace: "xml", Key: item.Name.Local, Val: item.Value})
				default:
					node.Attr = append(node.Attr, html.Attribute{Namespace: foreignNamespace, Key: item.Name.Local, Val: item.Value})
				}
			}
			if current == nil {
				root = node
			} else {
				current.AppendChild(node)
			}
			current = node
		case xml.EndElement:
			depth--
			current = current.Parent
		case xml.CharData:
			if current != nil {
				current.AppendChild(&html.Node{Type: html.TextNode, Data: string(value)})
			}
		}
	}
	if root == nil || depth != 0 {
		return nil, nil, errors.New("incomplete SVG document")
	}
	return root, styles, nil
}

func processingAttributes(value string) (map[string]string, error) {
	wrapped := "<span " + value + "></span>"
	nodes, err := html.ParseFragment(strings.NewReader(wrapped), &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"})
	if err != nil || len(nodes) != 1 {
		return nil, errors.New("invalid SVG stylesheet processing instruction")
	}
	attrs := map[string]string{}
	for _, item := range nodes[0].Attr {
		attrs[strings.ToLower(item.Key)] = item.Val
	}
	return attrs, nil
}

// rewriteSVGNode sanitizes an SVG tree and rewrites its references. For SVG
// in topic content, anchor handles links as for HTML anchors, and external
// <use> references are left for inlineExternalUses. Standalone SVG files pass
// a nil anchor. Every node is processed even after an error, so a failed SVG
// never keeps an unsanitized reference.
func (m *assetManager) rewriteSVGNode(svg *html.Node, base, owner string, depth int, anchor func(*html.Node)) error {
	errs := m.sanitizeSVG(svg, base)
	resolve := func(reference string, kind dependencyKind) (string, error) {
		return m.local(reference, base, owner, kind)
	}
	walk(svg, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		kept := n.Attr[:0]
		for _, item := range n.Attr {
			key := strings.ToLower(item.Key)
			if item.Namespace == "" && (key == "style" || svgURLAttributes[key] &&
				(strings.Contains(strings.ToLower(item.Val), "url(") || strings.Contains(item.Val, `\`))) {
				var rewritten []byte
				var diagnostics cssDiagnostics
				bad := false
				var err error
				if key == "style" {
					rewritten, diagnostics, err = rewriteCSS([]byte(item.Val), true, resolve)
				} else {
					rewritten, bad, err = rewriteCSSValue([]byte(item.Val), resolve)
				}
				*m.notices = append(*m.notices, diagnostics.Notices...)
				*m.warnings = append(*m.warnings, diagnostics.Warnings...)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if bad {
					*m.warnings = append(*m.warnings, base+": omitted SVG "+key+" attribute containing an invalid publisher URL")
					continue
				}
				item.Val = string(rewritten)
			}
			kept = append(kept, item)
		}
		n.Attr = kept
		if n.Data == "style" {
			rewritten, diagnostics, err := rewriteCSS([]byte(nodeText(n)), false, func(reference string, kind dependencyKind) (string, error) {
				if kind == dependencyCSS {
					kind = dependencySVGStyle
				}
				return m.local(reference, base, owner, kind)
			})
			*m.notices = append(*m.notices, diagnostics.Notices...)
			*m.warnings = append(*m.warnings, diagnostics.Warnings...)
			for n.FirstChild != nil {
				n.RemoveChild(n.FirstChild)
			}
			if err != nil {
				errs = append(errs, err)
			} else {
				m.recordFontFaceRecoveries(base, diagnostics.FontFaces)
				n.AppendChild(&html.Node{Type: html.TextNode, Data: string(rewritten)})
			}
		}
		if err := m.rewriteSVGReference(n, base, owner, anchor); err != nil {
			errs = append(errs, err)
		}
	})
	return errors.Join(errs...)
}

// rewriteSVGReference keeps an SVG href only as a fragment, a verified local
// asset, an inline raster data URL or a link. Anything else is removed.
func (m *assetManager) rewriteSVGReference(n *html.Node, base, owner string, anchor func(*html.Node)) error {
	raw := strings.TrimSpace(attr(n, "href"))
	if raw == "" || strings.HasPrefix(raw, "#") {
		return nil
	}
	lower := strings.ToLower(raw)
	switch n.Data {
	case "a":
		if anchor != nil {
			anchor(n)
			return nil
		}
		if strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "tel:") {
			return nil
		}
		if resolved, err := resolveReference(base, raw); err == nil {
			setAttr(n, "href", resolved)
			return nil
		}
	case "image", "feImage":
		if strings.HasPrefix(lower, "data:image/") {
			return nil
		}
		if !strings.HasPrefix(lower, "data:") {
			return m.localSVGReference(n, raw, base, owner)
		}
	case "use":
		if strings.HasPrefix(lower, "data:") {
			break
		}
		if anchor != nil {
			// inlineExternalUses copies the referenced definitions into the topic.
			return nil
		}
		return m.localSVGReference(n, raw, base, owner)
	}
	removeAttr(n, "href")
	*m.warnings = append(*m.warnings, base+": removed unsupported SVG <"+n.Data+"> reference "+boundedText(raw, 200))
	return nil
}

func (m *assetManager) localSVGReference(n *html.Node, raw, base, owner string) error {
	local, err := m.local(raw, base, owner, dependencyAsset)
	if err != nil {
		removeAttr(n, "href")
		return err
	}
	setAttr(n, "href", local)
	return nil
}

// sanitizeSVG keeps only allowed SVG-namespace elements and attributes. The
// tree comes from parseSVGDocument or, for inline SVG, from the HTML parser.
// Both resolve namespaces, so no prefix can disguise HTML or script.
func (m *assetManager) sanitizeSVG(svg *html.Node, source string) []error {
	var errs []error
	removed := map[string]int{}
	replaced := 0
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		sanitizeSVGAttributes(node, removed)
		if node.Data == "title" || node.Data == "desc" {
			flattenText(node)
			return
		}
		for child := node.FirstChild; child != nil; {
			next := child.NextSibling
			switch {
			case child.Type != html.ElementNode:
			case child.Namespace == "svg" && child.Data == "foreignObject":
				// HTML inside SVG is not archived. Diagram editors pair HTML
				// labels with a plain SVG fallback, which then renders instead.
				if normalizedNodeText(child) != "" || hasTopicVisualContent(child) || findNode(child, func(n *html.Node) bool {
					return n.Type == html.ElementNode && contentUnsupportedElements[strings.ToLower(n.Data)]
				}) != nil {
					if svgFallback(child) {
						replaced++
					} else {
						errs = append(errs, errors.New("substantive SVG foreignObject content is not supported"))
					}
				}
				node.RemoveChild(child)
			case !svgElementAllowed(child):
				// Browsers render neither other namespaces nor unknown elements in SVG.
				node.RemoveChild(child)
			default:
				visit(child)
			}
			child = next
		}
	}
	visit(svg)
	if replaced > 0 {
		*m.warnings = append(*m.warnings, fmt.Sprintf(
			"%s: replaced %d SVG foreignObject HTML labels with the publisher's SVG text fallback", source, replaced))
	}
	if len(removed) > 0 {
		*m.warnings = append(*m.warnings, source+": removed unsupported SVG attributes: "+summarizeCSSCounts(removed))
	}
	return errs
}

func sanitizeSVGAttributes(node *html.Node, removed map[string]int) {
	plainHref := slices.ContainsFunc(node.Attr, func(item html.Attribute) bool {
		return item.Namespace == "" && strings.EqualFold(item.Key, "href")
	})
	node.Attr = slices.DeleteFunc(node.Attr, func(item html.Attribute) bool {
		if item.Namespace == "xlink" && plainHref {
			return true
		}
		if svgAttributeAllowed(item) {
			return false
		}
		// Event handlers, namespace declarations and foreign metadata are
		// expected in publisher SVG and are removed without a diagnostic.
		key := strings.ToLower(item.Key)
		if item.Namespace == "" && !strings.HasPrefix(key, "on") && key != "xmlns" && !strings.Contains(key, ":") {
			removed[key]++
		}
		return true
	})
}

func svgElementAllowed(node *html.Node) bool {
	return node.Type == html.ElementNode && node.Namespace == "svg" && svgElements[node.Data] &&
		(!svgAnimationElements[node.Data] || safeSVGAnimation(node))
}

func svgAttributeAllowed(item html.Attribute) bool {
	key := strings.ToLower(item.Key)
	switch item.Namespace {
	case "":
	case "xlink":
		return key == "href"
	case "xml":
		return key == "space" || key == "lang"
	default:
		return false
	}
	return !strings.HasPrefix(key, "on") && (key == "href" || key == "style" || svgAttributes[key] ||
		strings.HasPrefix(key, "aria-") || strings.HasPrefix(key, "data-"))
}

// safeSVGAnimation rejects animations that could turn a link into script or
// an event handler, or that animate to a URL outside the archive.
func safeSVGAnimation(node *html.Node) bool {
	target := strings.ToLower(strings.TrimSpace(attr(node, "attributeName")))
	if index := strings.LastIndexByte(target, ':'); index >= 0 {
		target = target[index+1:]
	}
	if target == "href" || target == "style" || strings.HasPrefix(target, "on") {
		return false
	}
	for _, key := range []string{"from", "to", "by", "values"} {
		if cssReferenceProblem([]byte(attr(node, key))) != "" {
			return false
		}
	}
	return true
}

// svgFallback reports whether a <switch> around node offers another
// rendering, as diagram editors emit for HTML labels with an SVG text copy.
func svgFallback(node *html.Node) bool {
	branch := node
	for branch.Parent != nil && branch.Parent.Namespace == "svg" &&
		(branch.Parent.Data == "g" || branch.Parent.Data == "a") {
		branch = branch.Parent
	}
	choice := branch.Parent
	if choice == nil || choice.Namespace != "svg" || choice.Data != "switch" {
		return false
	}
	for child := choice.FirstChild; child != nil; child = child.NextSibling {
		if child != branch && svgElementAllowed(child) {
			return true
		}
	}
	return false
}

// flattenText replaces element children with their text, so that a title or
// description stays readable once foreign markup in it is removed.
func flattenText(node *html.Node) {
	if findNode(node, func(n *html.Node) bool { return n != node && n.Type == html.ElementNode }) == nil {
		return
	}
	var text strings.Builder
	walk(node, func(n *html.Node) {
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
	})
	for node.FirstChild != nil {
		node.RemoveChild(node.FirstChild)
	}
	node.AppendChild(&html.Node{Type: html.TextNode, Data: text.String()})
}

// svgPolicyProblems reports SVG markup that sanitizing would have removed or
// rewritten. Output validation uses it so that no gap reaches a library.
func svgPolicyProblems(svg *html.Node) []string {
	var problems []string
	walk(svg, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if !svgElementAllowed(n) {
			problems = append(problems, "unsupported SVG element <"+n.Data+">")
			return
		}
		for _, item := range n.Attr {
			key := strings.ToLower(item.Key)
			switch {
			case !svgAttributeAllowed(item):
				problems = append(problems, "unsupported SVG attribute "+key+" on <"+n.Data+">")
			case key == "href":
				if !svgReferenceAllowed(n, item.Val) {
					problems = append(problems, "SVG <"+n.Data+"> reference is not local "+item.Val)
				}
			case item.Namespace == "" && (key == "style" || svgURLAttributes[key]):
				if problem := cssReferenceProblem([]byte(item.Val)); problem != "" {
					problems = append(problems, "SVG "+key+" attribute: "+problem)
				}
			}
		}
		if n.Data == "style" {
			if problem := cssReferenceProblem([]byte(nodeText(n))); problem != "" {
				problems = append(problems, "SVG stylesheet: "+problem)
			}
		}
	})
	return problems
}

func svgReferenceAllowed(n *html.Node, value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	switch {
	case value == "" || strings.HasPrefix(value, "#"):
		return true
	case n.Data == "a":
		return navigableReference(value)
	case n.Data == "image" || n.Data == "feImage":
		return strings.HasPrefix(lower, "data:image/") || !strings.HasPrefix(lower, "data:") && localArchiveReference(value)
	case n.Data == "use":
		return !strings.HasPrefix(lower, "data:") && localArchiveReference(value)
	}
	return false
}

// navigableReference reports whether a link in archived output is a local
// relative path or fragment, or an explicit http(s), mailto or tel link.
func navigableReference(value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "mailto:"), strings.HasPrefix(lower, "tel:"):
		return true
	case strings.HasPrefix(lower, "data:"):
		return false
	case localArchiveReference(value):
		return true
	}
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func validateSVGFragments(svg *html.Node) error {
	ids := sourceIDs(svg)
	var missing string
	walk(svg, func(n *html.Node) {
		if missing != "" || n.Type != html.ElementNode {
			return
		}
		for _, a := range n.Attr {
			value := strings.TrimSpace(a.Val)
			var fragment string
			if (strings.EqualFold(a.Key, "href") || strings.EqualFold(a.Key, "xlink:href")) && strings.HasPrefix(value, "#") {
				fragment = strings.TrimPrefix(value, "#")
			} else if start := strings.Index(value, "url(#"); start >= 0 {
				rest := value[start+5:]
				if end := strings.IndexByte(rest, ')'); end >= 0 {
					fragment = strings.Trim(rest[:end], `"' `)
				}
			}
			if fragment != "" {
				decoded, err := url.PathUnescape(fragment)
				if err != nil || !ids[decoded] {
					missing = fragment
					return
				}
			}
		}
	})
	if missing != "" {
		return fmt.Errorf("SVG source fragment mismatch: #%s", missing)
	}
	return nil
}

func (m *assetManager) inlineSVG(raw, base, owner string, original *html.Node, depth int) (*html.Node, map[string]string, error) {
	resolved, err := resolveReference(base, raw)
	if err != nil {
		return nil, nil, err
	}
	parsed, _ := url.Parse(resolved)
	fragment := parsed.Fragment
	parsed.Fragment = ""
	state, err := m.load(parsed.String(), dependencyAsset, depth)
	if err != nil {
		return nil, nil, err
	}
	if classifyAsset(state.record.URL, state.record.ContentType, state.sourceBody, dependencyAsset) != "svg" {
		return nil, nil, errors.New("vector element did not return SVG")
	}
	rewritten, err := m.rewriteSVG(state.sourceBody, state.record.FinalURL, owner, depth+1)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := html.ParseFragment(bytes.NewReader(rewritten), &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"})
	if err != nil || len(nodes) == 0 {
		return nil, nil, errors.New("cannot inline rewritten SVG")
	}
	svg := nodes[0]
	if svg.Data != "svg" {
		svg = findNode(svg, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "svg" })
	}
	if svg == nil {
		return nil, nil, errors.New("rewritten vector has no SVG root")
	}
	// Namespace declarations matter only to the standalone XML file.
	svg.Attr = slices.DeleteFunc(svg.Attr, func(item html.Attribute) bool {
		return item.Namespace == "xmlns" || item.Namespace == "" && item.Key == "xmlns"
	})
	if fragment != "" {
		if findNode(svg, func(n *html.Node) bool { return attr(n, "id") == fragment }) == nil {
			return nil, nil, fmt.Errorf("SVG source fragment mismatch: #%s", fragment)
		}
	}
	m.inlineSVGs++
	scope := sourceHash([]byte(parsed.String() + "\x00" + owner + "\x00" + strconv.Itoa(m.inlineSVGs)))[:12]
	ids, err := namespaceSVG(svg, scope)
	if err != nil {
		return nil, nil, err
	}
	if original != nil {
		for _, key := range []string{"id", "class", "width", "height", "role"} {
			if value := attr(original, key); value != "" {
				setAttr(svg, key, value)
			}
		}
		if alt := attr(original, "alt"); alt != "" {
			setAttr(svg, "aria-label", alt)
			setAttr(svg, "role", "img")
		}
	}
	return svg, ids, nil
}

func namespaceSVG(svg *html.Node, scope string) (map[string]string, error) {
	ids := map[string]string{}
	walk(svg, func(node *html.Node) {
		if id := attr(node, "id"); id != "" {
			ids[id] = "svg-" + scope + "-" + id
			setAttr(node, "id", ids[id])
		}
	})
	setAttr(svg, "data-archive-svg", scope)
	var firstErr error
	walk(svg, func(node *html.Node) {
		if firstErr != nil || node.Type != html.ElementNode {
			return
		}
		for i := range node.Attr {
			value := node.Attr[i].Val
			if replacement := ids[strings.TrimPrefix(value, "#")]; replacement != "" && strings.HasPrefix(value, "#") {
				value = "#" + replacement
			}
			value = replaceFragmentURLs(value, ids)
			if strings.EqualFold(node.Attr[i].Key, "aria-labelledby") || strings.EqualFold(node.Attr[i].Key, "aria-describedby") {
				parts := strings.Fields(value)
				for j, part := range parts {
					if replacement := ids[part]; replacement != "" {
						parts[j] = replacement
					}
				}
				value = strings.Join(parts, " ")
			}
			node.Attr[i].Val = value
		}
		if node.Data == "style" {
			rewritten, err := scopeSVGStylesheet([]byte(nodeText(node)), scope, ids)
			if err != nil {
				firstErr = err
				return
			}
			for node.FirstChild != nil {
				node.RemoveChild(node.FirstChild)
			}
			node.AppendChild(&html.Node{Type: html.TextNode, Data: string(rewritten)})
			setAttr(node, "data-archive-svg", scope)
		}
	})
	if firstErr != nil {
		return nil, firstErr
	}
	if err := validateSVGFragments(svg); err != nil {
		return nil, err
	}
	return ids, nil
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

func scopeSVGStylesheet(body []byte, scope string, ids map[string]string) ([]byte, error) {
	p := css.NewParser(parse.NewInput(bytes.NewReader(body)), false)
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
		rewriteSVGTokens(values, ids)
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
			groups := splitSelectorTokens(values)
			for i, group := range groups {
				if i > 0 {
					out.WriteByte(',')
				}
				out.WriteString(`:where([data-archive-svg="` + scope + `"]) `)
				writeTokens(&out, group)
			}
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

func rewriteSVGTokens(tokens []css.Token, ids map[string]string) {
	for i := range tokens {
		if tokens[i].TokenType == css.HashToken {
			name := strings.TrimPrefix(string(tokens[i].Data), "#")
			if replacement := ids[name]; replacement != "" {
				tokens[i].Data = []byte("#" + replacement)
			}
			continue
		}
		if tokens[i].TokenType == css.URLToken {
			value, err := cssURL(tokens[i].Data)
			if err == nil && strings.HasPrefix(value, "#") {
				if replacement := ids[strings.TrimPrefix(value, "#")]; replacement != "" {
					tokens[i].Data = []byte(`url("#` + replacement + `")`)
				}
			}
		}
	}
}
