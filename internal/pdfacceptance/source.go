//go:build darwin

package pdfacceptance

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

const (
	AcceptanceMaxSourceFiles     = 50_000
	AcceptanceMaxSourceFileBytes = int64(64 << 20)
	AcceptanceMaxSourceBytes     = int64(2 << 30)
	AcceptanceMaxSourceDOMDepth  = 2_048
	AcceptanceMaxSourceDOMNodes  = 5_000_000
	AcceptanceMaxSourceValues    = 5_000_000
)

func LoadSourceEvidence(root string) (SourceEvidence, error) {
	result := SourceEvidence{
		CommandRows:  map[string]struct{}{},
		TableHeaders: map[string]struct{}{},
		TextCounts:   map[string]int{},
	}
	if strings.TrimSpace(root) == "" {
		return result, nil
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return result, err
	}
	if err := requireRealDirectory(absolute, "source guide root"); err != nil {
		return result, err
	}
	pages := filepath.Join(absolute, "pages")
	if err := requireRealDirectory(pages, "source guide pages directory"); err != nil {
		return result, err
	}
	entries, err := os.ReadDir(pages)
	if err != nil {
		return result, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	fileCount := 0
	var totalBytes int64
	for _, entry := range entries {
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension != ".htm" && extension != ".html" {
			continue
		}
		fileCount++
		if fileCount > AcceptanceMaxSourceFiles {
			return result, fmt.Errorf("source guide has more than %d immediate HTML pages", AcceptanceMaxSourceFiles)
		}
		path := filepath.Join(pages, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return result, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return result, fmt.Errorf("source guide page must be a nonsymlink regular file: %s", path)
		}
		if info.Size() < 0 || info.Size() > AcceptanceMaxSourceFileBytes ||
			totalBytes > AcceptanceMaxSourceBytes-info.Size() {
			return result, fmt.Errorf("source guide HTML exceeds acceptance bounds at %s", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return result, err
		}
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) {
			_ = file.Close()
			return result, fmt.Errorf("source guide page identity changed while opening: %s", path)
		}
		body, readErr := io.ReadAll(io.LimitReader(file, AcceptanceMaxSourceFileBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return result, readErr
		}
		if closeErr != nil {
			return result, closeErr
		}
		if int64(len(body)) != info.Size() {
			return result, fmt.Errorf("source guide page size changed while reading: %s", path)
		}
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, current) {
			return result, fmt.Errorf("source guide page identity changed while reading: %s", path)
		}
		totalBytes += int64(len(body))
		document, err := html.Parse(bytes.NewReader(body))
		if err != nil {
			return result, fmt.Errorf("parse source guide page %s: %w", path, err)
		}
		if err := validateSourceDOMBounds(document); err != nil {
			return result, fmt.Errorf("parse source guide page %s: %w", path, err)
		}
		if err := collectSourceEvidence(document, &result); err != nil {
			return result, fmt.Errorf("collect source guide evidence from %s: %w", path, err)
		}
	}
	return result, nil
}

func requireRealDirectory(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s is unavailable: %s: %w", label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a nonsymlink directory: %s", label, path)
	}
	return nil
}

func collectSourceEvidence(document *html.Node, result *SourceEvidence) error {
	var walk func(*html.Node, int) error
	walk = func(node *html.Node, preformattedDepth int) error {
		if node.Type == html.ElementNode {
			if (hasAcceptanceClass(node, "screen") || hasAcceptanceClass(node, "codeblock")) &&
				!inCommandAncestor(node) {
				for _, row := range normalizedRows(nodeText(node)) {
					if err := addSourceSet(result, result.CommandRows, row); err != nil {
						return err
					}
				}
			}
			if inAncestor(node, "thead") && (node.Data == "th" || node.Data == "td") {
				if value := NormalizeAcceptanceText(nodeText(node)); value != "" {
					if err := addSourceSet(result, result.TableHeaders, value); err != nil {
						return err
					}
				}
			}
			if node.Data == "pre" || node.Data == "code" {
				if preformattedDepth == 0 {
					for _, value := range normalizedRows(nodeText(node)) {
						if _, exists := result.TextCounts[value]; !exists &&
							sourceEvidenceValues(result) >= AcceptanceMaxSourceValues {
							return fmt.Errorf("source guide text evidence exceeds %d values", AcceptanceMaxSourceValues)
						}
						result.TextCounts[value]++
						if !inCommandContainer(node) {
							if err := addSourceSet(result, result.CommandRows, value); err != nil {
								return err
							}
						}
					}
				}
				preformattedDepth++
			}
		}
		if node.Type == html.TextNode && preformattedDepth == 0 {
			for _, value := range normalizedRows(node.Data) {
				if _, exists := result.TextCounts[value]; !exists &&
					sourceEvidenceValues(result) >= AcceptanceMaxSourceValues {
					return fmt.Errorf("source guide text evidence exceeds %d values", AcceptanceMaxSourceValues)
				}
				result.TextCounts[value]++
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if err := walk(child, preformattedDepth); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(document, 0)
}

func addSourceSet(result *SourceEvidence, target map[string]struct{}, value string) error {
	if _, exists := target[value]; !exists && sourceEvidenceValues(result) >= AcceptanceMaxSourceValues {
		return fmt.Errorf("source guide text evidence exceeds %d values", AcceptanceMaxSourceValues)
	}
	target[value] = struct{}{}
	return nil
}

func sourceEvidenceValues(result *SourceEvidence) int {
	return len(result.TextCounts) + len(result.TableHeaders) + len(result.CommandRows)
}

func validateSourceDOMBounds(document *html.Node) error {
	type item struct {
		node  *html.Node
		depth int
	}
	stack := []item{{node: document, depth: 1}}
	nodes := 0
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		nodes++
		if nodes > AcceptanceMaxSourceDOMNodes {
			return fmt.Errorf("HTML DOM exceeds %d-node limit", AcceptanceMaxSourceDOMNodes)
		}
		if current.depth > AcceptanceMaxSourceDOMDepth {
			return fmt.Errorf("HTML DOM exceeds %d-level depth limit", AcceptanceMaxSourceDOMDepth)
		}
		for child := current.node.FirstChild; child != nil; child = child.NextSibling {
			stack = append(stack, item{node: child, depth: current.depth + 1})
		}
	}
	return nil
}

func normalizedRows(value string) []string {
	rows := []string{}
	for _, raw := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if normalized := NormalizeAcceptanceText(raw); normalized != "" {
			rows = append(rows, normalized)
		}
	}
	return rows
}

func nodeText(node *html.Node) string {
	var value strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			value.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return value.String()
}

func hasAcceptanceClass(node *html.Node, expected string) bool {
	for _, attribute := range node.Attr {
		if attribute.Key != "class" {
			continue
		}
		for _, value := range strings.Fields(attribute.Val) {
			if value == expected {
				return true
			}
		}
	}
	return false
}

func inAncestor(node *html.Node, name string) bool {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode && parent.Data == name {
			return true
		}
	}
	return false
}

func inCommandAncestor(node *html.Node) bool {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode &&
			(hasAcceptanceClass(parent, "screen") || hasAcceptanceClass(parent, "codeblock")) {
			return true
		}
	}
	return false
}

func inCommandContainer(node *html.Node) bool {
	return hasAcceptanceClass(node, "screen") ||
		hasAcceptanceClass(node, "codeblock") ||
		inCommandAncestor(node)
}
