package archive

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Publishing tools can rename the identifiers of sections and nested topics
// while the publication's own TOC data, and links built from it, keep the
// original identifiers. Such a link names a bookmark no element carries. Two
// kinds of evidence in the target page itself say which element it stands
// for:
//
//   - a same-page link that carries the original identifier as data-tocid and
//     points at the renamed element, as Oxygen WebHelp's "On this page" list
//     does;
//   - DITA-OT's identifier scheme, which prefixes every identifier inside a
//     topic with "<topic id>__": when every element with that prefix has the
//     same nearest topic element, that element is the topic;
//   - an identifier on an element that encloses the archived content, such as
//     the page's body, names the start of the content.
//
// An identifier with conflicting evidence, or evidence pointing outside the
// archived content, has no alias, and the missing bookmark stays a warning.

// contentStart is the alias target that stands for the start of the content.
const contentStart = ""

// bookmarkAliases maps identifiers that content lacks to the identifier of
// the content element they stand for, or to contentStart. ids are the
// identifiers content has.
func bookmarkAliases(doc, content *html.Node, ids map[string]bool) map[string]string {
	aliases := map[string]string{}
	conflicting := map[string]bool{}
	add := func(name, target string) {
		if name == "" || ids[name] || (target != contentStart && !ids[target]) || conflicting[name] {
			return
		}
		if previous, found := aliases[name]; found && previous != target {
			delete(aliases, name)
			conflicting[name] = true
			return
		}
		aliases[name] = target
	}
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode || n.Data != "a" {
			return
		}
		name, href := attr(n, "data-tocid"), attr(n, "href")
		if name == "" || !strings.HasPrefix(href, "#") {
			return
		}
		if target, err := url.Parse(href); err == nil && target.Fragment != "" {
			add(name, target.Fragment)
		}
	})
	owners := map[string]string{}
	mixed := map[string]bool{}
	var visit func(n *html.Node, topic string)
	visit = func(n *html.Node, topic string) {
		if n.Type == html.ElementNode {
			if slices.Contains(strings.Fields(attr(n, "class")), "topic/topic") {
				topic = attr(n, "id")
			}
			if prefix, _, found := strings.Cut(attr(n, "id"), "__"); found && prefix != "" {
				if owner, seen := owners[prefix]; !seen {
					owners[prefix] = topic
				} else if owner != topic {
					mixed[prefix] = true
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child, topic)
		}
	}
	visit(content, "")
	for prefix, owner := range owners {
		if !mixed[prefix] && owner != "" {
			add(prefix, owner)
		}
	}
	for n := content.Parent; n != nil; n = n.Parent {
		if n.Type == html.ElementNode {
			add(attr(n, "id"), contentStart)
			add(attr(n, "name"), contentStart)
		}
	}
	return aliases
}

// insertBookmarkAnchors gives each aliased element an empty anchor carrying
// the original identifier, so every link to the original identifier, from
// the TOC, other topics or a generated PDF, lands on the element.
func insertBookmarkAnchors(content *html.Node, anchors map[string]string) error {
	if len(anchors) == 0 {
		return nil
	}
	elements := map[string]*html.Node{}
	walk(content, func(n *html.Node) {
		if n.Type == html.ElementNode {
			if id := attr(n, "id"); id != "" && elements[id] == nil {
				elements[id] = n
			}
		}
	})
	names := make([]string, 0, len(anchors))
	for name := range anchors {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		anchor := &html.Node{Type: html.ElementNode, Data: "span", DataAtom: atom.Span, Attr: []html.Attribute{{Key: "id", Val: name}}}
		if anchors[name] == contentStart {
			content.InsertBefore(anchor, content.FirstChild)
			continue
		}
		target := elements[anchors[name]]
		if target == nil {
			return fmt.Errorf("bookmark #%s stands for #%s, which was lost during archival", name, anchors[name])
		}
		if !anchorOutside[target.DataAtom] {
			target.InsertBefore(anchor, target.FirstChild)
		} else if target.Parent != nil && target != content {
			target.Parent.InsertBefore(anchor, target)
		} else {
			return fmt.Errorf("bookmark #%s stands for #%s, which cannot hold an anchor", name, anchors[name])
		}
	}
	return nil
}

// anchorOutside lists elements that cannot hold a span, either because they
// have no content or because a span child would be moved when the page is
// parsed again; their anchor goes immediately before them.
var anchorOutside = map[atom.Atom]bool{
	atom.Area: true, atom.Br: true, atom.Col: true, atom.Embed: true, atom.Hr: true, atom.Img: true,
	atom.Input: true, atom.Source: true, atom.Track: true, atom.Wbr: true,
	atom.Table: true, atom.Thead: true, atom.Tbody: true, atom.Tfoot: true, atom.Tr: true, atom.Colgroup: true,
	atom.Ul: true, atom.Ol: true, atom.Dl: true, atom.Select: true,
}
