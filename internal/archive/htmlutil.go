package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
	"golang.org/x/net/html"
)

const (
	maxHTMLTopics = 10000
	maxHTMLAssets = 10000
	maxHTMLDepth  = 100
)

var (
	spacePattern        = regexp.MustCompile(`\s+`)
	errorHeadingPattern = regexp.MustCompile(
		`(?i)^(?:error(?:\s+\d+)?|access denied|unauthorized|forbidden|` +
			`(?:404[ :\-]*)?(?:page|document|file|topic|resource) not found|` +
			`sign[ -]?in|log[ -]?in|login|request rejected|service unavailable|` +
			`authentication required|document unavailable|loading(?: (?:document|content|page))?|` +
			`please wait)(?:\s*[-|:].*)?[.!:]?$`,
	)
	errorPagePattern = regexp.MustCompile(
		`(?i)^(?:(?:error|http)\s*)?(?:401|403|404|500|502|503)` +
			`(?:\s*[-:]\s*(?:not found|unauthorized|forbidden|server error|bad gateway|service unavailable))?[.!]?$|` +
			`^(?:page |document |topic |resource )?not found[.!]?$|` +
			`^(?:access denied|forbidden|unauthorized|sign[ -]?in|log[ -]?in|login|` +
			`service unavailable|authentication required|document unavailable)(?:\s*[-|:].*)?[.!]?$`,
	)
	loadingShellPattern = regexp.MustCompile(
		`(?i)^(?:loading(?: (?:document|content|page))?|please wait|` +
			`(?:please |you need to )?enable javascript(?: to (?:view|use|access) .*)?)[.!]*$`,
	)
	contextualErrorPattern = regexp.MustCompile(
		`(?i)(?:the requested (?:url|page|document|resource) (?:was |could )?not (?:be )?found|` +
			`the (?:page|document) you (?:requested|are looking for) (?:is not|cannot be)|` +
			`please (?:sign|log) in to (?:view|access)|you do not have permission to access|` +
			`your session (?:has )?expired|something went wrong|request (?:failed|rejected)|` +
			`unable to (?:load|display|retrieve|access)|` +
			`contact (?:your )?(?:administrator|support)|try again(?: later)?)`,
	)
	contextualLoadingPattern = regexp.MustCompile(
		`(?i)(?:please wait|enable javascript|loading (?:document|content|page))`,
	)
	jsShellPattern = regexp.MustCompile(`(?i)\b(renderDocument|__NEXT_DATA__)\b`)
	releasePattern = regexp.MustCompile(`(?i)\b(\d+\.\d+)(?:\.(?:\d+|x+))*\b`)
)

type pageInput struct {
	topic                   model.Topic
	requestURL              string
	finalURL                string
	baseURL                 string
	path                    string
	title                   string
	contentType             string
	sourceSHA256            string
	sourceSize              int64
	sourceIDs               map[string]bool
	supplementary           bool
	copyright               []string
	metadata                map[string]string
	sourceDate              string
	emitted                 bool
	tableStyles             map[string]tableStyleReference
	ordinaryStyles          []ordinaryStyleReference
	ordinaryStyleRecoveries map[string]ordinaryStyleRecovery
}

type sourceBudget struct {
	total int64
	limit int64
}

func (b *sourceBudget) add(size int64) error {
	if size < 0 || b.total > b.limit-size {
		return errors.New("archive aggregate source-byte limit exceeded")
	}
	b.total += size
	return nil
}

func readResource(ctx context.Context, fetcher model.Fetcher, raw string, refresh bool, maxBytes int64) (resource model.Resource, body []byte, err error) {
	if err := ctx.Err(); err != nil {
		return resource, nil, err
	}
	resource, err = fetcher.Get(ctx, raw, refresh)
	if err != nil {
		return resource, nil, err
	}
	defer func() { err = errors.Join(err, resource.Body.Close()) }()
	if resource.Status != 200 {
		return resource, nil, &resourceStatusError{URL: resource.URL, Status: resource.Status}
	}
	body, err = io.ReadAll(io.LimitReader(contextReader{ctx: ctx, source: resource.Body}, maxBytes+1))
	if err != nil {
		return resource, nil, err
	}
	if int64(len(body)) > maxBytes {
		return resource, nil, fmt.Errorf("publisher resource exceeds configured byte limit: %s", resource.URL)
	}
	return resource, body, ctx.Err()
}

func media(value string) string {
	kind, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return strings.ToLower(kind)
}

func parseTopic(ctx context.Context, body []byte, resource model.Resource, kind string) (*html.Node, error) {
	contentType := resource.Headers.Get("Content-Type")
	kindMedia := media(contentType)
	if kindMedia != "" && kindMedia != "text/html" && kindMedia != "application/xhtml+xml" &&
		!(kind == "hpe" && kindMedia == "multipage") {
		return nil, fmt.Errorf("expected publisher HTML, received %q: %s", contentType, resource.URL)
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("%PDF-")) {
		return nil, fmt.Errorf("expected publisher HTML, received PDF: %s", resource.URL)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed) {
		return nil, fmt.Errorf("publisher returned a JSON placeholder instead of HTML documentation: %s", resource.URL)
	}
	doc, err := source.DecodeHTML(ctx, body, contentType, resource.URL)
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func validateTopicContent(doc, content *html.Node, rawURL string) error {
	text := normalizedNodeText(content)
	hasVisualContent := hasTopicVisualContent(content)
	if text == "" && !hasVisualContent {
		return fmt.Errorf("publisher returned empty static document content: %s", rawURL)
	}
	if findNode(content, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.Data == "input" &&
			strings.EqualFold(strings.TrimSpace(attr(node, "type")), "password")
	}) != nil {
		return fmt.Errorf("publisher returned a login form instead of documentation: %s", rawURL)
	}
	errorHeading := false
	for _, heading := range matchingNodes(doc, func(node *html.Node) bool {
		return node.Type == html.ElementNode && (node.Data == "title" || node.Data == "h1")
	}) {
		if errorHeadingPattern.MatchString(normalizedNodeText(heading)) {
			errorHeading = true
			break
		}
	}
	bodyText := topicBodyText(content)
	contextualProse := contextualTopicProse(content)
	if errorPagePattern.MatchString(text) || loadingShellPattern.MatchString(text) ||
		(errorHeading && (contextualProse == "" ||
			contextualErrorPattern.MatchString(contextualProse))) {
		return fmt.Errorf("publisher returned an authentication, error, or loading page: %s", rawURL)
	}
	if hasJSShellScript(doc) && ((bodyText == "" && !hasVisualContent) ||
		contextualLoadingPattern.MatchString(contextualProse)) {
		return fmt.Errorf("publisher returned a JavaScript-only shell: %s", rawURL)
	}
	return nil
}

func normalizedNodeText(node *html.Node) string {
	return strings.TrimSpace(spacePattern.ReplaceAllString(nodeText(node), " "))
}

func hasTopicVisualContent(content *html.Node) bool {
	return findNode(content, func(node *html.Node) bool {
		return node.Type == html.ElementNode &&
			slices.Contains([]string{"img", "svg", "object", "table", "picture"}, node.Data)
	}) != nil
}

func topicBodyText(content *html.Node) string {
	return textWithoutElements(content, map[string]bool{
		"title": true, "h1": true, "h2": true, "h3": true, "h4": true,
		"h5": true, "h6": true, "script": true, "style": true, "form": true,
	})
}

func contextualTopicProse(content *html.Node) string {
	return textWithoutElements(content, map[string]bool{
		"title": true, "h1": true, "h2": true, "h3": true, "h4": true,
		"h5": true, "h6": true, "script": true, "style": true, "pre": true,
		"code": true, "samp": true, "kbd": true, "table": true, "blockquote": true,
	})
}

func textWithoutElements(root *html.Node, excluded map[string]bool) string {
	var text strings.Builder
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode && excluded[node.Data] {
			return
		}
		if node.Type == html.TextNode {
			text.WriteString(node.Data)
			text.WriteByte(' ')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	return strings.TrimSpace(spacePattern.ReplaceAllString(text.String(), " "))
}

func hasJSShellScript(doc *html.Node) bool {
	return findNode(doc, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.Data == "script" &&
			jsShellPattern.MatchString(nodeText(node))
	}) != nil
}

func findContent(doc *html.Node, kind string) *html.Node {
	var predicates []func(*html.Node) bool
	switch kind {
	case "flare":
		predicates = append(predicates, func(n *html.Node) bool { return attr(n, "id") == "mc-main-content" })
	case "hpe":
		predicates = append(predicates, func(n *html.Node) bool { return n.Data == "main" && hasClass(n, "ditasrc") })
	case "static":
		predicates = append(predicates,
			func(n *html.Node) bool { return hasClass(n, "wh_topic_content") },
			func(n *html.Node) bool { return n.Data == "main" },
			func(n *html.Node) bool { return n.Data == "article" },
		)
	}
	if kind != "static" {
		predicates = append(predicates,
			func(n *html.Node) bool { return n.Data == "main" },
			func(n *html.Node) bool { return n.Data == "article" },
			func(n *html.Node) bool { return n.Data == "body" },
		)
	}
	for _, predicate := range predicates {
		if found := findNode(doc, func(n *html.Node) bool { return n.Type == html.ElementNode && predicate(n) }); found != nil {
			return found
		}
	}
	return nil
}

func findNode(root *html.Node, predicate func(*html.Node) bool) *html.Node {
	if predicate(root) {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := findNode(child, predicate); found != nil {
			return found
		}
	}
	return nil
}

func walk(root *html.Node, visit func(*html.Node)) {
	visit(root)
	for child := root.FirstChild; child != nil; {
		next := child.NextSibling
		walk(child, visit)
		child = next
	}
}

func nodeText(root *html.Node) string {
	var b strings.Builder
	walk(root, func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
	})
	return b.String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	return slices.ContainsFunc(n.Attr, func(a html.Attribute) bool { return strings.EqualFold(a.Key, key) })
}

func setAttr(n *html.Node, key, value string) {
	for i := range n.Attr {
		if strings.EqualFold(n.Attr[i].Key, key) {
			n.Attr[i].Key, n.Attr[i].Val = key, value
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: value})
}

func removeAttr(n *html.Node, key string) {
	n.Attr = slices.DeleteFunc(n.Attr, func(a html.Attribute) bool { return strings.EqualFold(a.Key, key) })
}

func hasClass(n *html.Node, class string) bool {
	return slices.Contains(strings.Fields(attr(n, "class")), class)
}

func addClass(n *html.Node, class string) {
	classes := strings.Fields(attr(n, "class"))
	if !slices.Contains(classes, class) {
		classes = append(classes, class)
	}
	setAttr(n, "class", strings.Join(classes, " "))
}

func detach(n *html.Node) {
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

func replaceNode(old, replacement *html.Node) {
	parent := old.Parent
	if parent == nil {
		return
	}
	parent.InsertBefore(replacement, old)
	parent.RemoveChild(old)
}

func cloneNode(n *html.Node) *html.Node {
	copy := &html.Node{Type: n.Type, DataAtom: n.DataAtom, Data: n.Data, Namespace: n.Namespace}
	copy.Attr = append([]html.Attribute{}, n.Attr...)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		copy.AppendChild(cloneNode(child))
	}
	return copy
}

func resolveReference(base, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" || strings.Contains(reference, "\\") || strings.IndexFunc(reference, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("invalid publisher reference %q", reference)
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(reference)
	if err != nil {
		return "", err
	}
	return fetch.NormalizeURL(b.ResolveReference(r).String())
}

func baseURL(doc *html.Node, finalURL string) (string, error) {
	base := finalURL
	if node := findNode(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "base" && attr(n, "href") != ""
	}); node != nil {
		var err error
		base, err = resolveReference(finalURL, attr(node, "href"))
		if err != nil {
			return "", fmt.Errorf("invalid publisher base URL: %w", err)
		}
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("invalid publisher base URL %q", base)
	}
	u.Fragment = ""
	return u.String(), nil
}

func staticBaseURL(doc *html.Node, finalURL string, roots []string) (string, error) {
	bases := matchingNodes(doc, func(node *html.Node) bool {
		_, found := attrFound(node, "href")
		return node.Type == html.ElementNode && node.Data == "base" && found
	})
	if len(bases) > 1 {
		return "", errors.New("ambiguous static document base URL")
	}
	if len(bases) == 0 {
		return baseURL(doc, finalURL)
	}
	if len(bases) == 1 && strings.TrimSpace(attr(bases[0], "href")) == "" {
		return "", errors.New("invalid static document base URL: empty href")
	}
	base, err := baseURL(doc, finalURL)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid static document base URL %q", base)
	}
	for _, root := range roots {
		guide, parseErr := url.Parse(root)
		if parseErr == nil && parsed.Scheme == guide.Scheme && parsed.Host == guide.Host &&
			strings.HasPrefix(parsed.Path, strings.TrimSuffix(guide.Path, "/")+"/") {
			return base, nil
		}
	}
	return "", fmt.Errorf("static document base URL is outside the selected guide: %s", base)
}

func sourceIDs(content *html.Node) map[string]bool {
	ids := map[string]bool{}
	walk(content, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		for _, key := range []string{"id", "name"} {
			if value := attr(n, key); value != "" {
				ids[value] = true
			}
		}
	})
	return ids
}

func canonicalNoFragment(raw string) string {
	value, err := fetch.CanonicalURL(raw)
	if err != nil {
		return ""
	}
	return value
}

func fileName(raw, title, fallbackExt string) string {
	u, _ := url.Parse(raw)
	ext := strings.ToLower(path.Ext(u.Path))
	if len(ext) > 10 || strings.ContainsAny(ext, "%&=?") {
		ext = ""
	}
	if ext == "" {
		ext = fallbackExt
	}
	stem := strings.TrimSuffix(path.Base(u.Path), path.Ext(u.Path))
	if title != "" {
		stem = title
	}
	stem = safeStem(stem)
	sum := sha256.Sum256([]byte(canonicalNoFragment(raw)))
	return fmt.Sprintf("%s-%s%s", stem, hex.EncodeToString(sum[:6]), ext)
}

func safeStem(value string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			b.WriteRune(r)
		case unicode.IsSpace(r), r == '.':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 60 {
			break
		}
	}
	value = strings.Trim(b.String(), "-._")
	if value == "" {
		return "resource"
	}
	return value
}

func sourceHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func localReference(from, to string) string {
	fromParts := strings.Split(path.Dir(from), "/")
	if len(fromParts) == 1 && fromParts[0] == "." {
		fromParts = nil
	}
	toParts := strings.Split(to, "/")
	for len(fromParts) > 0 && len(toParts) > 0 && fromParts[0] == toParts[0] {
		fromParts, toParts = fromParts[1:], toParts[1:]
	}
	parts := make([]string, len(fromParts))
	for i := range parts {
		parts[i] = ".."
	}
	parts = append(parts, toParts...)
	if len(parts) == 0 {
		return path.Base(to)
	}
	return strings.Join(parts, "/")
}

func renderNode(n *html.Node) ([]byte, error) {
	var b bytes.Buffer
	if err := html.Render(&b, n); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func contentTitle(content *html.Node, fallback string) string {
	for _, name := range []string{"h1", "h2"} {
		if node := findNode(content, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == name }); node != nil {
			if value := strings.TrimSpace(spacePattern.ReplaceAllString(nodeText(node), " ")); value != "" {
				return value
			}
		}
	}
	return fallback
}

func publisherReleaseLabel(doc *html.Node, selected string) (string, bool) {
	selectedMatch := releasePattern.FindStringSubmatch(selected)
	if selectedMatch == nil {
		return "", false
	}
	var mismatch string
	walk(doc, func(node *html.Node) {
		if mismatch != "" || node.Type != html.ElementNode {
			return
		}
		id := strings.ToLower(attr(node, "id"))
		if id != "docname" && id != "docnameother" && !hasClass(node, "Variables.Doc_Title") {
			return
		}
		label := strings.TrimSpace(spacePattern.ReplaceAllString(nodeText(node), " "))
		actual := releasePattern.FindStringSubmatch(label)
		if actual != nil && actual[1] != selectedMatch[1] {
			mismatch = label
		}
	})
	return mismatch, mismatch != ""
}
