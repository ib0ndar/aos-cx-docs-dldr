package pdfgen

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/html"
)

func generatedPDFIndex(root *os.Root, filename string) ([]byte, []byte, error) {
	file, err := root.Open("index.html")
	if err != nil {
		return nil, nil, err
	}
	original, readErr := io.ReadAll(io.LimitReader(file, (64<<20)+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, nil, err
	}
	if len(original) > 64<<20 {
		return nil, nil, errors.New("guide index exceeds generated PDF update limit")
	}
	document, err := html.Parse(bytes.NewReader(original))
	if err != nil {
		return nil, nil, fmt.Errorf("parse guide index for generated PDF: %w", err)
	}
	var content, heading *html.Node
	walkHTML(document, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		if hasClass(node, "archive-generated-pdf") && node.Parent != nil {
			node.Parent.RemoveChild(node)
			return
		}
		if content == nil && hasClass(node, "archive-content") {
			content = node
		}
	})
	if content == nil {
		return nil, nil, errors.New("guide index has no archive content for generated PDF link")
	}
	for child := content.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == "h1" {
			heading = child
			break
		}
	}
	paragraph := &html.Node{Type: html.ElementNode, Data: "p", Attr: []html.Attribute{{Key: "class", Val: "archive-generated-pdf"}}}
	link := &html.Node{Type: html.ElementNode, Data: "a", Attr: []html.Attribute{{
		Key: "href", Val: (&url.URL{Path: filename}).EscapedPath(),
	}}}
	link.AppendChild(&html.Node{Type: html.TextNode, Data: "Open generated PDF"})
	paragraph.AppendChild(link)
	paragraph.AppendChild(&html.Node{Type: html.TextNode, Data: " (generated from the verified archived HTML guide with pinned Chrome for Testing)"})
	if heading != nil {
		content.InsertBefore(paragraph, heading.NextSibling)
	} else {
		content.InsertBefore(paragraph, content.FirstChild)
	}
	var rendered bytes.Buffer
	if err := html.Render(&rendered, document); err != nil {
		return nil, nil, fmt.Errorf("render guide index with generated PDF: %w", err)
	}
	return rendered.Bytes(), original, nil
}

func walkHTML(node *html.Node, visit func(*html.Node)) {
	var next *html.Node
	for child := node.FirstChild; child != nil; child = next {
		next = child.NextSibling
		walkHTML(child, visit)
	}
	visit(node)
}

func hasClass(node *html.Node, class string) bool {
	for _, attribute := range node.Attr {
		if attribute.Key == "class" {
			for _, value := range strings.Fields(attribute.Val) {
				if value == class {
					return true
				}
			}
		}
	}
	return false
}
