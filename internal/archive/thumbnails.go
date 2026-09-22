package archive

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func hasThumbnailClass(node *html.Node, class string) bool {
	return slices.ContainsFunc(strings.Fields(attr(node, "class")), func(value string) bool {
		return strings.EqualFold(value, class)
	})
}

func (a *htmlArchiver) expandOldStyleThumbnails(content *html.Node, page *pageInput) error {
	if a.plan.Document.Kind != "flare" {
		return nil
	}
	var firstErr error
	walk(content, func(node *html.Node) {
		if firstErr != nil || node.Type != html.ElementNode {
			return
		}
		var image *html.Node
		var original string
		switch {
		case node.Data == "a" && hasThumbnailClass(node, "MCPopupThumbnailLink"):
			count := 0
			walk(node, func(child *html.Node) {
				if child.Type == html.ElementNode && child.Data == "img" {
					image = child
					count++
				}
			})
			if count != 1 {
				firstErr = errors.New("old-style popup must advertise exactly one image to expand inline")
				return
			}
			original = attr(node, "href")
		case node.Data == "img" && hasThumbnailClass(node, "MCPopupThumbnail"):
			image = node
		default:
			return
		}
		if strings.TrimSpace(original) == "" {
			for _, key := range []string{"data-mc-popup-src", "data-original"} {
				if value := strings.TrimSpace(attr(image, key)); value != "" {
					original = value
					break
				}
			}
		}
		if strings.TrimSpace(original) == "" {
			firstErr = errors.New("old-style thumbnail has no source-advertised full-size image")
			return
		}
		resolved, err := resolveReference(page.baseURL, original)
		if err != nil {
			firstErr = fmt.Errorf("invalid old-style full-size image: %w", err)
			return
		}
		parsed, err := url.Parse(resolved)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			firstErr = fmt.Errorf("unsupported old-style full-size image URL: %s", original)
			return
		}
		if err := a.clearThumbnailPresentation(image); err != nil {
			firstErr = err
			return
		}
		setAttr(image, "src", resolved)
		addClass(image, "archive-full-image")
		for _, key := range []string{"srcset", "data-srcset", "sizes", "data-original", "data-mc-popup-src", "data-src"} {
			removeAttr(image, key)
		}
		if picture := image.Parent; picture != nil && picture.Data == "picture" {
			for child := picture.FirstChild; child != nil; {
				next := child.NextSibling
				if child.Type == html.ElementNode && child.Data == "source" {
					detach(child)
				}
				child = next
			}
		}
		if node != image {
			if err := a.clearThumbnailPresentation(node); err != nil {
				firstErr = err
				return
			}
			node.Data, node.DataAtom = "span", atom.Span
			addClass(node, "archive-full-image-container")
			for _, key := range []string{"href", "target", "rel", "download", "role", "aria-haspopup", "aria-expanded", "aria-controls"} {
				removeAttr(node, key)
			}
		}
	})
	return firstErr
}

func (a *htmlArchiver) clearThumbnailPresentation(node *html.Node) error {
	classes := slices.DeleteFunc(strings.Fields(attr(node, "class")), func(value string) bool {
		return strings.HasPrefix(strings.ToLower(value), "mcpopupthumbnail")
	})
	if len(classes) == 0 {
		removeAttr(node, "class")
	} else {
		setAttr(node, "class", strings.Join(classes, " "))
	}
	for _, key := range []string{"width", "height", "data-mc-width", "data-mc-height", "tabindex"} {
		removeAttr(node, key)
	}
	if style := attr(node, "style"); style != "" {
		rewritten, diagnostics, err := rewriteCSSWithProperties([]byte(style), true,
			func(reference string, _ dependencyKind) (string, error) { return reference, nil },
			nil, func(property string) bool {
				switch property {
				case "width", "height", "min-width", "min-height", "max-width", "max-height",
					"inline-size", "block-size", "min-inline-size", "min-block-size", "max-inline-size", "max-block-size":
					return false
				}
				return true
			})
		a.addCSSDiagnostics(diagnostics)
		if err != nil {
			return fmt.Errorf("invalid old-style thumbnail styling: %w", err)
		}
		if len(rewritten) == 0 {
			removeAttr(node, "style")
		} else {
			setAttr(node, "style", string(rewritten))
		}
	}
	return nil
}
