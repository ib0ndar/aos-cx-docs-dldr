package source

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

func staticContainer(n *html.Node) bool {
	id, _ := attr(n, "id")
	role, _ := attr(n, "role")
	aria, _ := attr(n, "aria-label")
	return hasClass(n, "wh_publication_toc") || id == "toc" ||
		(n.Data == "nav" && (strings.EqualFold(role, "doc-toc") ||
			strings.EqualFold(aria, "Table of Contents") || hasClass(n, "toc")))
}

func nearest(node *html.Node, tag string) *html.Node {
	for p := node.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == tag {
			return p
		}
	}
	return nil
}

func staticAttr(node *html.Node, key string) string {
	value, _ := attr(node, key)
	return value
}

func staticBase(tree *html.Node, actual, root string) (string, error) {
	bases := nodes(tree, func(n *html.Node) bool {
		_, found := attr(n, "href")
		return n.Data == "base" && found
	})
	if len(bases) > 1 {
		return "", errors.New("ambiguous static document base URL")
	}
	if len(bases) == 0 {
		return actual, nil
	}
	base, err := joinSource(actual, staticAttr(bases[0], "href"), false)
	if err != nil {
		return "", fmt.Errorf("invalid static document base URL: %w", err)
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid static document base URL: %s", base)
	}
	parsed.Fragment = ""
	base = parsed.String()
	if !inside(base, root) {
		return "", fmt.Errorf("static document base URL is outside the selected guide: %s", base)
	}
	return base, nil
}

func staticLazy(node *html.Node) bool {
	state := strings.ToLower(strings.TrimSpace(staticAttr(node, "data-state")))
	ready := strings.ToLower(strings.TrimSpace(staticAttr(node, "data-ready")))
	loaded := strings.ToLower(strings.TrimSpace(staticAttr(node, "data-loaded")))
	lazy := strings.ToLower(strings.TrimSpace(staticAttr(node, "data-lazy")))
	if strings.EqualFold(strings.TrimSpace(staticAttr(node, "aria-busy")), "true") ||
		state == "not-ready" || state == "loading" || state == "pending" ||
		ready == "false" || loaded == "false" || lazy == "true" {
		return true
	}
	for _, class := range strings.Fields(strings.ToLower(staticAttr(node, "class"))) {
		switch class {
		case "loading", "is-loading", "lazy", "lazy-load", "not-ready", "toc-loading", "wh_toc_loading":
			return true
		}
	}
	return false
}

func staticGroupTitle(item *html.Node) string {
	var parts []string
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node != item && node.Type == html.ElementNode &&
			(node.Data == "ul" || node.Data == "ol" || node.Data == "script" || node.Data == "style") {
			return
		}
		if node.Type == html.TextNode {
			if value := strings.TrimSpace(node.Data); value != "" {
				parts = append(parts, value)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(item)
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

func (p *planner) static(in input, flare func(input) (model.DocumentPlan, error)) (model.DocumentPlan, error) {
	tree, err := p.html(in, false)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	htmls := nodes(tree, func(n *html.Node) bool { return n.Data == "html" })
	if len(htmls) > 0 {
		if _, found := attr(htmls[0], "data-mc-path-to-help-system"); found {
			p.plan.Document.Kind = "flare"
			return flare(in)
		}
	}
	root, err := joinSource(in.url, "./", false)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if err := p.front(p.plan.Document.URL, p.plan.Document.Title, in.url); err != nil {
		return model.DocumentPlan{}, err
	}
	referenceBase, err := staticBase(tree, in.url, root)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	p.plan.Styles, err = styles(tree, referenceBase)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	links, err := tocLinksAt(tree, in.url, referenceBase, root)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if len(links) > 1 {
		return model.DocumentPlan{}, errors.New("ambiguous static detailed TOC links")
	}
	tocURL := in.url
	if len(links) == 1 {
		detailed, err := p.read(links[0], "toc", scopeInside(root))
		if err != nil {
			return model.DocumentPlan{}, err
		}
		tree, err = p.html(detailed, false)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		if err := p.front(links[0], "Table of Contents", detailed.url); err != nil {
			return model.DocumentPlan{}, err
		}
		referenceBase, err = staticBase(tree, detailed.url, root)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		more, err := styles(tree, referenceBase)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		p.plan.Styles = append(p.plan.Styles, more...)
		tocURL = detailed.url
	}
	containers := nodes(tree, staticContainer)
	if len(containers) != 1 {
		return model.DocumentPlan{}, fmt.Errorf("unsupported static source: expected one explicit complete TOC: %s", tocURL)
	}
	container := containers[0]
	if len(nodes(container, staticLazy)) != 0 {
		return model.DocumentPlan{}, errors.New("unsupported lazy-loaded static/Oxygen TOC")
	}
	lists := nodes(container, func(n *html.Node) bool {
		return (n.Data == "ul" || n.Data == "ol") && nearest(n, "ul") == nil && nearest(n, "ol") == nil
	})
	if len(lists) == 0 {
		return model.DocumentPlan{}, errors.New("unsupported static TOC without complete nested list")
	}
	var walk func(*html.Node, int) ([]model.TocEntry, error)
	walk = func(list *html.Node, depth int) ([]model.TocEntry, error) {
		var result []model.TocEntry
		for item := list.FirstChild; item != nil; item = item.NextSibling {
			if item.Type != html.ElementNode || item.Data != "li" {
				continue
			}
			if err := p.check(depth); err != nil {
				return nil, err
			}
			anchors := nodes(item, func(n *html.Node) bool {
				_, found := attr(n, "href")
				return n.Data == "a" && found && nearest(n, "li") == item
			})
			if len(anchors) > 1 {
				return nil, errors.New("ambiguous static TOC entry")
			}
			childLists := nodes(item, func(n *html.Node) bool { return (n.Data == "ul" || n.Data == "ol") && nearest(n, "li") == item })
			entry := model.TocEntry{}
			if len(anchors) == 1 {
				entry.Title = text(anchors[0])
				href, _ := attr(anchors[0], "href")
				entry.URL, err = joinSource(referenceBase, href, false)
				if err != nil {
					return nil, err
				}
				if err := p.navigation(entry.URL, entry.Title, root); err != nil {
					return nil, err
				}
			} else {
				entry.Title = staticGroupTitle(item)
				if len(childLists) == 0 {
					return nil, errors.New("static TOC leaf lacks topic URL")
				}
			}
			if !label(entry.Title) {
				return nil, errors.New("static TOC has empty title")
			}
			for _, child := range childLists {
				nested, err := walk(child, depth+1)
				if err != nil {
					return nil, err
				}
				entry.Children = append(entry.Children, nested...)
			}
			result = append(result, entry)
		}
		return result, nil
	}
	for _, list := range lists {
		entries, err := walk(list, 0)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		p.plan.TOC = append(p.plan.TOC, entries...)
	}
	if p.count == 0 || len(p.plan.Topics) <= 1+len(links) {
		return model.DocumentPlan{}, errors.New("static TOC contains no document topics")
	}
	return p.finish(root, tocURL, nil)
}
