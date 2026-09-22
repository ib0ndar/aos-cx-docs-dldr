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

func (m *assetManager) rewriteSVG(body []byte, base, owner string, depth int) ([]byte, error) {
	styles, err := validateSVGXML(body)
	if err != nil {
		return nil, err
	}
	nodes, err := html.ParseFragment(bytes.NewReader(body), &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"})
	if err != nil {
		return nil, fmt.Errorf("parse SVG: %w", err)
	}
	var svg *html.Node
	for _, node := range nodes {
		if node.Type == html.ElementNode && node.Data == "svg" {
			svg = node
			break
		}
		if svg = findNode(node, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "svg" }); svg != nil {
			break
		}
	}
	if svg == nil {
		return nil, errors.New("SVG resource has no svg root element")
	}
	if err := m.rewriteSVGNode(svg, base, owner, depth, false); err != nil {
		return nil, err
	}
	for _, reference := range styles {
		local, err := m.local(reference, base, owner, dependencySVGStyle)
		if err != nil {
			return nil, err
		}
		style := &html.Node{Type: html.ElementNode, Data: "style", Attr: []html.Attribute{{Key: "data-archive-svg", Val: "stylesheet"}}}
		style.AppendChild(&html.Node{Type: html.TextNode, Data: `@import url("` + strings.ReplaceAll(local, `"`, `\"`) + `");`})
		svg.InsertBefore(style, svg.FirstChild)
	}
	if err := validateSVGFragments(svg); err != nil {
		return nil, err
	}
	return renderNode(svg)
}

func validateSVGXML(body []byte) ([]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	var styles []string
	depth := 0
	root := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid SVG XML: %w", err)
		}
		switch value := token.(type) {
		case xml.Directive:
			if strings.Contains(strings.ToLower(string(value)), "doctype") {
				return nil, errors.New("SVG document type declarations are unsupported")
			}
		case xml.ProcInst:
			if strings.EqualFold(value.Target, "xml-stylesheet") {
				attrs, err := processingAttributes(string(value.Inst))
				if err != nil {
					return nil, err
				}
				if href := attrs["href"]; href != "" {
					styles = append(styles, href)
				}
			}
		case xml.StartElement:
			depth++
			if depth > maxHTMLDepth {
				return nil, errors.New("SVG element depth exceeds limit")
			}
			if !root {
				if value.Name.Local != "svg" {
					return nil, errors.New("SVG resource has no svg root element")
				}
				root = true
			}
		case xml.EndElement:
			depth--
		}
	}
	if !root || depth != 0 {
		return nil, errors.New("incomplete SVG document")
	}
	return styles, nil
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

func (m *assetManager) rewriteSVGNode(svg *html.Node, base, owner string, depth int, preserveExternalUses bool) error {
	var remove []*html.Node
	var firstErr error
	walk(svg, func(n *html.Node) {
		if firstErr != nil || n.Type != html.ElementNode {
			return
		}
		if n.Data == "script" {
			remove = append(remove, n)
			return
		}
		if n.Data == "foreignobject" {
			firstErr = errors.New("substantive SVG foreignObject content is not supported")
			return
		}
		n.Attr = slices.DeleteFunc(n.Attr, func(a html.Attribute) bool {
			return strings.HasPrefix(strings.ToLower(a.Key), "on")
		})
		if style := attr(n, "style"); style != "" {
			rewritten, diagnostics, err := rewriteCSS([]byte(style), true, func(reference string, kind dependencyKind) (string, error) {
				return m.local(reference, base, owner, kind)
			})
			*m.notices = append(*m.notices, diagnostics.Notices...)
			*m.warnings = append(*m.warnings, diagnostics.Warnings...)
			if err != nil {
				firstErr = err
				return
			}
			setAttr(n, "style", string(rewritten))
		}
		if n.Data == "style" {
			original := nodeText(n)
			rewritten, diagnostics, err := rewriteCSS([]byte(original), false, func(reference string, kind dependencyKind) (string, error) {
				if kind == dependencyCSS {
					kind = dependencySVGStyle
				}
				return m.local(reference, base, owner, kind)
			})
			*m.notices = append(*m.notices, diagnostics.Notices...)
			*m.warnings = append(*m.warnings, diagnostics.Warnings...)
			if err != nil {
				firstErr = err
				return
			}
			for n.FirstChild != nil {
				n.RemoveChild(n.FirstChild)
			}
			n.AppendChild(&html.Node{Type: html.TextNode, Data: string(rewritten)})
		}
		if n.Data != "image" && n.Data != "use" {
			return
		}
		if preserveExternalUses && n.Data == "use" {
			return
		}
		for _, key := range []string{"href", "xlink:href"} {
			value := attr(n, key)
			if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(strings.ToLower(value), "data:") {
				continue
			}
			local, err := m.local(value, base, owner, dependencyAsset)
			if err != nil {
				firstErr = err
				return
			}
			setAttr(n, key, local)
		}
	})
	for _, n := range remove {
		detach(n)
	}
	return firstErr
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
			for original, replacement := range ids {
				if value == "#"+original {
					value = "#" + replacement
				}
				value = strings.ReplaceAll(value, "url(#"+original+")", "url(#"+replacement+")")
			}
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
