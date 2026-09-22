package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const maxDepth = 100
const maxChunks = 1000

type input struct {
	url     string
	body    []byte
	headers http.Header
}

func (in input) resource() model.Resource {
	return model.Resource{URL: in.url, Status: 200, Headers: in.headers, Body: io.NopCloser(bytes.NewReader(in.body))}
}

type planner struct {
	ctx     context.Context
	fetcher model.Fetcher
	refresh bool
	plan    model.DocumentPlan
	seen    map[string]bool
	count   int
}

func newPlanner(ctx context.Context, f model.Fetcher, doc model.Document, refresh bool) *planner {
	return &planner{ctx: ctx, fetcher: f, refresh: refresh, seen: map[string]bool{}, plan: model.DocumentPlan{
		Document: doc, Topics: []model.Topic{}, TOC: []model.TocEntry{}, Styles: []string{},
		Notices: []model.Notice{}, Warnings: []string{}, Inputs: []model.SourceInput{},
	}}
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(b)
}

func (p *planner) read(raw, role string, scope func(string) error) (in input, err error) {
	if err := p.ctx.Err(); err != nil {
		return in, err
	}
	ctx := p.ctx
	if scope != nil {
		if err := scope(raw); err != nil {
			return in, err
		}
		ctx = fetch.WithRequestScope(ctx, scope)
	}
	r, err := getResource(ctx, p.fetcher, raw, p.refresh)
	if err != nil {
		return in, err
	}
	defer func() { err = errors.Join(err, r.Body.Close()) }()
	if scope != nil {
		if err := scope(r.URL); err != nil {
			return in, err
		}
	}
	body, err := io.ReadAll(io.LimitReader(contextReader{p.ctx, r.Body}, maxInventoryBytes+1))
	if err != nil {
		return in, err
	}
	if len(body) > maxInventoryBytes {
		return in, fmt.Errorf("source inventory exceeds byte limit: %s", r.URL)
	}
	sum := sha256.Sum256(body)
	requested, err := fetch.CanonicalURL(raw)
	if err != nil {
		return in, err
	}
	p.plan.Inputs = append(p.plan.Inputs, model.SourceInput{RequestedURL: requested, FinalURL: r.URL, Role: role,
		ContentType: r.Headers.Get("Content-Type"), SHA256: hex.EncodeToString(sum[:]), Size: len(body), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	return input{r.URL, body, r.Headers}, p.ctx.Err()
}

func inventoryText(body []byte, contentType string) ([]byte, error) {
	encodingName := "utf-8"
	if contentType != "" {
		_, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			return nil, fmt.Errorf("invalid publisher content type: %w", err)
		}
		if params["charset"] != "" {
			encodingName = params["charset"]
		}
	}
	if strings.EqualFold(encodingName, "utf-8") || strings.EqualFold(encodingName, "utf8") {
		body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
		if !utf8.Valid(body) {
			return nil, errors.New("invalid UTF-8 publisher text")
		}
		return body, nil
	}
	encoding, _ := charset.Lookup(encodingName)
	if encoding == nil {
		return nil, fmt.Errorf("unsupported publisher charset %q", encodingName)
	}
	decoded, err := encoding.NewDecoder().Bytes(body)
	if err != nil {
		return nil, fmt.Errorf("decode publisher charset: %w", err)
	}
	if len(decoded) > maxInventoryBytes {
		return nil, errors.New("decoded publisher inventory exceeds byte limit")
	}
	return decoded, nil
}

func mediaType(in input) (string, error) {
	value := in.headers.Get("Content-Type")
	if value == "" {
		return "", nil
	}
	media, _, err := mime.ParseMediaType(value)
	return strings.ToLower(media), err
}

func (p *planner) html(in input, hpe bool) (*html.Node, error) {
	media, err := mediaType(in)
	if err != nil {
		return nil, err
	}
	if media != "" && media != "text/html" && media != "application/xhtml+xml" && !(hpe && media == "multipage") {
		return nil, fmt.Errorf("expected publisher HTML, received %s: %s", media, in.url)
	}
	return parsePublisherHTML(p.ctx, in.body, in.headers.Get("Content-Type"), in.url)
}

func joinSource(base, reference string, flare bool) (string, error) {
	if !label(reference) || strings.Contains(reference, "\\") || strings.IndexFunc(reference, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("invalid publisher URL reference %q", reference)
	}
	if flare && strings.HasPrefix(reference, "/") && !strings.HasPrefix(reference, "//") {
		reference = strings.TrimLeft(reference, "/")
	}
	root, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(reference)
	if err != nil {
		return "", err
	}
	return fetch.NormalizeURL(root.ResolveReference(ref).String())
}

func inside(raw, root string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	base, err := url.Parse(root)
	if err != nil {
		return false
	}
	if u.Scheme != base.Scheme || u.Host != base.Host || !strings.HasPrefix(u.Path, base.Path) ||
		strings.Contains(u.Path, "\\") {
		return false
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func scopeInside(root string) func(string) error {
	return func(raw string) error {
		normalized, err := fetch.NormalizeURL(raw)
		if err != nil {
			return err
		}
		if !inside(normalized, root) {
			return fmt.Errorf("source redirected/requested outside the known guide: %s", raw)
		}
		return nil
	}
}

func initialScope(document model.Document) func(string) error {
	u, err := url.Parse(document.URL)
	if err != nil {
		return func(string) error { return err }
	}
	prefix := u.Path[:strings.LastIndex(u.Path, "/")+1]
	if document.Kind == "flare" {
		if at := strings.Index(u.Path, "/HTML/"); at >= 0 {
			prefix = u.Path[:at+len("/HTML/")]
		}
	}
	return func(raw string) error {
		normalized, err := fetch.NormalizeURL(raw)
		if err != nil {
			return err
		}
		actual, _ := url.Parse(normalized)
		if actual.Scheme != u.Scheme || actual.Host != u.Host || !strings.HasPrefix(actual.Path, prefix) ||
			(document.Kind == "flare" && !strings.Contains(actual.Path, "/Content/")) {
			return fmt.Errorf("source redirected outside the selected document scope: %s", raw)
		}
		return nil
	}
}

func documentRoot(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if at := strings.Index(u.EscapedPath(), "/Content/"); at >= 0 {
		path, err := url.PathUnescape(u.EscapedPath()[:at+1])
		if err != nil {
			return "", err
		}
		u.Path, u.RawPath = path, u.EscapedPath()[:at+1]
		u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
		return fetch.NormalizeURL(u.String())
	}
	return joinSource(raw, "./", false)
}

func (p *planner) check(depth int) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	p.count++
	if depth > maxDepth || p.count > maxEntries {
		return errors.New("publisher TOC exceeds nesting/entry limit")
	}
	return nil
}

func (p *planner) topic(public, title, actual string) error {
	public, err := fetch.CanonicalURL(public)
	if err != nil {
		return err
	}
	key := public
	if actual != "" {
		actual, err = fetch.CanonicalURL(actual)
		if err != nil {
			return err
		}
		key = actual
	}
	if !p.seen[key] {
		if len(p.plan.Topics) >= maxEntries {
			return errors.New("topic inventory exceeds entry limit")
		}
		p.seen[key] = true
		p.plan.Topics = append(p.plan.Topics, model.Topic{URL: public, Title: title, FetchURL: actual})
	}
	return nil
}

func (p *planner) front(public, title, actual string) error {
	if err := p.topic(public, title, actual); err != nil {
		return err
	}
	p.plan.TOC = append(p.plan.TOC, model.TocEntry{Title: title, URL: public})
	return nil
}

func (p *planner) navigation(raw, title, root string) error {
	if inside(raw, root) {
		u, _ := url.Parse(raw)
		if !strings.HasSuffix(strings.ToLower(u.Path), ".htm") && !strings.HasSuffix(strings.ToLower(u.Path), ".html") &&
			!strings.HasSuffix(strings.ToLower(u.Path), ".xhtml") {
			return fmt.Errorf("unsupported non-HTML TOC topic: %s", raw)
		}
		return p.topic(raw, title, "")
	}
	p.notice(model.NoticeExternalLinkRetained, "External navigation retained, not downloaded: "+raw)
	return nil
}

func (p *planner) notice(kind model.NoticeKind, message string) {
	notice := model.Notice{Kind: kind, Message: message}
	if !slices.Contains(p.plan.Notices, notice) {
		p.plan.Notices = append(p.plan.Notices, notice)
	}
}

func (p *planner) warning(message string) {
	if !slices.Contains(p.plan.Warnings, message) {
		p.plan.Warnings = append(p.plan.Warnings, message)
	}
}

func styles(tree *html.Node, base string) ([]string, error) {
	var result []string
	for _, node := range nodes(tree, func(n *html.Node) bool { return n.Data == "link" }) {
		rel, _ := attr(node, "rel")
		href, exists := attr(node, "href")
		if !exists || !slices.Contains(strings.Fields(strings.ToLower(rel)), "stylesheet") {
			continue
		}
		raw, err := joinSource(base, href, false)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(result, raw) {
			result = append(result, raw)
		}
	}
	return result, nil
}

func tocLinks(tree *html.Node, actual, root string) ([]string, error) {
	return tocLinksAt(tree, actual, actual, root)
}

func tocLinksAt(tree *html.Node, actual, base, root string) ([]string, error) {
	var result []string
	for _, node := range nodes(tree, func(n *html.Node) bool { return n.Data == "a" }) {
		title := strings.ToLower(text(node))
		if title != "contents" && title != "table of contents" && title != "detailed table of contents" {
			continue
		}
		href, exists := attr(node, "href")
		if !exists {
			continue
		}
		raw, err := joinSource(base, href, false)
		if err != nil {
			return nil, err
		}
		canonical, err := fetch.CanonicalURL(raw)
		if err != nil {
			return nil, err
		}
		owner, err := fetch.CanonicalURL(actual)
		if err != nil {
			return nil, err
		}
		if inside(raw, root) && canonical != owner && !slices.Contains(result, canonical) {
			result = append(result, canonical)
		}
	}
	return result, nil
}

var htmlShape = regexp.MustCompile(`(?i)<(?:html|main|article|body|div|nav)\b`)

func (p *planner) finish(root, toc string, chunks []string) (model.DocumentPlan, error) {
	if err := p.ctx.Err(); err != nil {
		return model.DocumentPlan{}, err
	}
	p.plan.Styles = slices.Compact(p.plan.Styles)
	unique := []string{}
	for _, style := range p.plan.Styles {
		if !slices.Contains(unique, style) {
			unique = append(unique, style)
		}
	}
	p.plan.Styles = unique
	count := 0
	pending := append([]model.TocEntry{}, p.plan.TOC...)
	for len(pending) > 0 {
		entry := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		count++
		pending = append(pending, entry.Children...)
	}
	p.plan.Inventory = &model.InventoryEvidence{Complete: true, Kind: p.plan.Document.Kind, RootURL: root, TOCURL: toc,
		ChunkURLs: chunks, Entries: count, UniqueTopics: len(p.plan.Topics)}
	return p.plan, p.ctx.Err()
}
