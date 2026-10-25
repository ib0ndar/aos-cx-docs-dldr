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
	"sync"
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

// maxStyles bounds distinct stylesheets per plan; publisher guides use a few.
const maxStyles = 1000

type input struct {
	url     string
	body    []byte
	headers http.Header
}

func (in input) resource() model.Resource {
	return model.Resource{URL: in.url, Status: 200, Headers: in.headers, Body: io.NopCloser(bytes.NewReader(in.body))}
}

type planner struct {
	ctx      context.Context
	fetcher  model.Fetcher
	refresh  bool
	plan     model.DocumentPlan
	seen     map[string]bool
	notices  map[model.Notice]bool
	warnings map[string]bool
	count    int
	// workers bounds the source inputs readAll requests at once.
	workers int
}

func newPlanner(ctx context.Context, f model.Fetcher, doc model.Document, refresh bool) *planner {
	return &planner{ctx: ctx, fetcher: f, refresh: refresh, seen: map[string]bool{}, workers: 1,
		notices: map[model.Notice]bool{}, warnings: map[string]bool{}, plan: model.DocumentPlan{
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

func (p *planner) read(raw, role string, scope func(string) error) (input, error) {
	in, record, err := p.fetchInput(p.ctx, raw, role, scope)
	if err != nil {
		return in, err
	}
	p.plan.Inputs = append(p.plan.Inputs, record)
	return in, p.ctx.Err()
}

// fetchInput retrieves one source input and its evidence record without
// recording it, so that concurrent reads can be recorded in a fixed order.
func (p *planner) fetchInput(ctx context.Context, raw, role string, scope func(string) error) (in input, record model.SourceInput, err error) {
	if err := ctx.Err(); err != nil {
		return in, record, err
	}
	requestCtx := ctx
	if scope != nil {
		if err := scope(raw); err != nil {
			return in, record, err
		}
		requestCtx = fetch.WithRequestScope(ctx, scope)
	}
	r, err := getResource(requestCtx, p.fetcher, raw, p.refresh)
	if err != nil {
		return in, record, err
	}
	defer func() { err = errors.Join(err, r.Body.Close()) }()
	if scope != nil {
		if err := scope(r.URL); err != nil {
			return in, record, err
		}
	}
	body, err := io.ReadAll(io.LimitReader(contextReader{ctx, r.Body}, maxInventoryBytes+1))
	if err != nil {
		return in, record, err
	}
	if len(body) > maxInventoryBytes {
		return in, record, fmt.Errorf("source inventory exceeds byte limit: %s", r.URL)
	}
	sum := sha256.Sum256(body)
	requested, err := fetch.CanonicalURL(raw)
	if err != nil {
		return in, record, err
	}
	record = model.SourceInput{RequestedURL: requested, FinalURL: r.URL, Role: role,
		ContentType: r.Headers.Get("Content-Type"), SHA256: hex.EncodeToString(sum[:]), Size: len(body), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	return input{r.URL, body, r.Headers}, record, ctx.Err()
}

type readJob struct{ url, role string }

// readAll reads independent source inputs with up to p.workers requests in
// flight and records them in the order given. The first failure cancels the
// reads still running.
func (p *planner) readAll(jobs []readJob, scope func(string) error) ([]input, error) {
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	inputs := make([]input, len(jobs))
	records := make([]model.SourceInput, len(jobs))
	errs := make([]error, len(jobs))
	next := make(chan int)
	var group sync.WaitGroup
	for range min(max(p.workers, 1), len(jobs)) {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range next {
				inputs[index], records[index], errs[index] = p.fetchInput(ctx, jobs[index].url, jobs[index].role, scope)
				if errs[index] != nil {
					cancel()
				}
			}
		}()
	}
feed:
	for index := range jobs {
		select {
		case next <- index:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	group.Wait()
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	// Report the first failure that is not the cancellation of another read.
	var cancelled error
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return nil, err
		}
		if err != nil && cancelled == nil {
			cancelled = err
		}
	}
	if cancelled != nil {
		return nil, cancelled
	}
	p.plan.Inputs = append(p.plan.Inputs, records...)
	return inputs, nil
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
	return u.Scheme == base.Scheme && u.Host == base.Host && strings.HasPrefix(u.Path, base.Path) &&
		plainSegments(u.Path)
}

// plainSegments rejects backslashes and decoded dot segments, including
// %2e%2e, which a server may collapse to escape a path prefix.
func plainSegments(path string) bool {
	if strings.Contains(path, "\\") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
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
			!plainSegments(actual.Path) || (document.Kind == "flare" && !strings.Contains(actual.Path, "/Content/")) {
			return fmt.Errorf("source redirected outside the selected document scope: %s", raw)
		}
		return nil
	}
}

// DocumentScope is the request scope of a mapped document's first retrieval.
// The direct mapped-PDF download keeps the scope of its availability probe.
func DocumentScope(document model.Document) func(string) error {
	return initialScope(document)
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
	if !p.notices[notice] {
		p.notices[notice] = true
		p.plan.Notices = append(p.plan.Notices, notice)
	}
}

func (p *planner) warning(message string) {
	if !p.warnings[message] {
		p.warnings[message] = true
		p.plan.Warnings = append(p.plan.Warnings, message)
	}
}

func styles(ctx context.Context, tree *html.Node, base string) ([]string, error) {
	var result []string
	seen := map[string]bool{}
	for _, node := range nodes(tree, func(n *html.Node) bool { return n.Data == "link" }) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel, _ := attr(node, "rel")
		href, exists := attr(node, "href")
		if !exists || !slices.Contains(strings.Fields(strings.ToLower(rel)), "stylesheet") {
			continue
		}
		raw, err := joinSource(base, href, false)
		if err != nil {
			return nil, err
		}
		if !seen[raw] {
			if len(result) >= maxStyles {
				return nil, errors.New("publisher stylesheet inventory exceeds entry limit")
			}
			seen[raw] = true
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
	seen := map[string]bool{}
	owner, err := fetch.CanonicalURL(actual)
	if err != nil {
		return nil, err
	}
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
		if inside(raw, root) && canonical != owner && !seen[canonical] {
			seen[canonical] = true
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
	unique := []string{}
	seen := map[string]bool{}
	for _, style := range p.plan.Styles {
		if !seen[style] {
			seen[style] = true
			unique = append(unique, style)
		}
	}
	if len(unique) > maxStyles {
		return model.DocumentPlan{}, errors.New("publisher stylesheet inventory exceeds entry limit")
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
