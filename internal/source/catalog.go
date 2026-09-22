package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const maxInventoryBytes = 32 << 20
const maxEntries = 100_000

type CatalogProgressStage string

const (
	CatalogPortalStarted    CatalogProgressStage = "portal-started"
	CatalogPortalReady      CatalogProgressStage = "portal-ready"
	CatalogMappingCompleted CatalogProgressStage = "mapping-completed"
)

type CatalogProgress struct {
	Stage     CatalogProgressStage
	Completed int
	Total     int
}

type CatalogProgressObserver func(CatalogProgress)

var bookPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var handlerPattern = regexp.MustCompile(`^\s*(?:return\s+)?openFile\(\s*('HTML'|"HTML"|'PDF'|"PDF")\s*,\s*('([A-Za-z0-9_-]+)'|"([A-Za-z0-9_-]+)")\s*,\s*('aoscx'|"aoscx")\s*\)\s*;?\s*$`)
var releasePattern = regexp.MustCompile(`^\d+\.\d+(?:\.(?:\d+|[xX]+))*$`)
var errorTitle = regexp.MustCompile(`(?i)^(?:error(?:\s+\d+)?|access denied|unauthorized|forbidden|(?:404[ :\-]*)?(?:page|document|file) not found|sign in|log in|request rejected|service unavailable)(?:[.!:]|$)`)

func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

func text(n *html.Node) string {
	var b strings.Builder
	pending := []*html.Node{n}
	for len(pending) > 0 {
		n := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for child := n.LastChild; child != nil; child = child.PrevSibling {
			pending = append(pending, child)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func nodes(root *html.Node, match func(*html.Node) bool) []*html.Node {
	var found []*html.Node
	pending := []*html.Node{root}
	for len(pending) > 0 {
		n := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if n.Type == html.ElementNode && match(n) {
			found = append(found, n)
		}
		for child := n.LastChild; child != nil; child = child.PrevSibling {
			pending = append(pending, child)
		}
	}
	return found
}

func label(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= 4096
}

func options(root *html.Node, id string) ([]string, error) {
	selects := nodes(root, func(n *html.Node) bool {
		v, _ := attr(n, "id")
		return n.Data == "select" && v == id
	})
	if len(selects) != 1 {
		return nil, fmt.Errorf("portal must contain one select#%s", id)
	}
	var values []string
	for _, n := range nodes(selects[0], func(n *html.Node) bool { return n.Data == "option" }) {
		if _, disabled := attr(n, "disabled"); disabled {
			continue
		}
		if _, hidden := attr(n, "hidden"); hidden {
			continue
		}
		value, present := attr(n, "value")
		if !present {
			value = text(n)
		}
		if value == "" {
			continue
		}
		if !label(value) || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("invalid portal %s value %q", id, value)
		}
		if !slices.Contains(values, value) {
			values = append(values, value)
		}
	}
	if len(values) == 0 || len(values) > 1000 {
		return nil, fmt.Errorf("portal select#%s has no usable options or too many options", id)
	}
	return values, nil
}

func get(ctx context.Context, f model.Fetcher, raw string) (model.Resource, error) {
	return getResource(ctx, f, raw, true)
}

func getResource(ctx context.Context, f model.Fetcher, raw string, refresh bool) (model.Resource, error) {
	normalized, err := fetch.CanonicalURL(raw)
	if err != nil {
		return model.Resource{}, err
	}
	r, err := f.Get(ctx, normalized, refresh)
	if err != nil {
		return model.Resource{}, err
	}
	if r.Status != 200 {
		r.Body.Close()
		return model.Resource{}, &fetch.StatusError{URL: r.URL, Status: r.Status}
	}
	if _, err := fetch.NormalizeURL(r.URL); err != nil {
		r.Body.Close()
		return model.Resource{}, err
	}
	return r, nil
}

func parseHTML(r model.Resource) (*html.Node, error) {
	return parseHTMLContext(context.Background(), r)
}

func parseHTMLContext(ctx context.Context, r model.Resource) (*html.Node, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(contextReader{ctx, r.Body}, maxInventoryBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxInventoryBytes {
		return nil, errors.New("publisher HTML exceeds inventory byte limit")
	}
	return parsePublisherHTML(ctx, body, r.Headers.Get("Content-Type"), r.URL)
}

func parsePublisherHTML(ctx context.Context, body []byte, contentType, sourceURL string) (*html.Node, error) {
	tree, err := decodePublisherHTML(ctx, body, contentType, sourceURL)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes(tree, func(n *html.Node) bool { return n.Data == "title" || n.Data == "h1" || n.Data == "input" }) {
		t, _ := attr(n, "type")
		if errorTitle.MatchString(text(n)) || (n.Data == "input" && strings.EqualFold(t, "password")) {
			return nil, fmt.Errorf("publisher returned an error/login page: %s", sourceURL)
		}
	}
	return tree, nil
}

func decodePublisherHTML(ctx context.Context, body []byte, contentType, sourceURL string) (*html.Node, error) {
	media, params, _ := mime.ParseMediaType(contentType)
	if strings.EqualFold(media, "multipage") && params["charset"] == "" {
		contentType = "text/html; charset=utf-8"
	}
	reader, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return nil, fmt.Errorf("decode publisher HTML: %w", err)
	}
	decoded, err := io.ReadAll(io.LimitReader(contextReader{ctx, reader}, maxInventoryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decode publisher HTML: %w", err)
	}
	if len(decoded) > maxInventoryBytes {
		return nil, errors.New("decoded publisher HTML exceeds inventory byte limit")
	}
	if !htmlShape.Match(decoded) {
		return nil, fmt.Errorf("expected publisher HTML, not a landing/error response: %s", sourceURL)
	}
	tree, err := html.Parse(contextReader{ctx, bytes.NewReader(decoded)})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return tree, nil
}

func LoadCatalog(ctx context.Context, f model.Fetcher, portal string) (model.Catalog, error) {
	return LoadCatalogWithWorkers(ctx, f, portal, 1)
}

// LoadCatalogWithWorkers keeps portal discovery serial, then retrieves valid
// mapping files concurrently while preserving publisher order in the result.
func LoadCatalogWithWorkers(ctx context.Context, f model.Fetcher, portal string, workers int) (model.Catalog, error) {
	return LoadCatalogWithWorkersObserved(ctx, f, portal, workers, nil)
}

// LoadCatalogWithWorkersObserved reports typed discovery and mapping progress.
// Concurrent mapping callbacks are serialized after each fetch has completed.
func LoadCatalogWithWorkersObserved(
	ctx context.Context,
	f model.Fetcher,
	portal string,
	workers int,
	observe CatalogProgressObserver,
) (model.Catalog, error) {
	if workers < 1 {
		return model.Catalog{}, errors.New("catalog workers must be at least 1")
	}
	if observe != nil {
		observe(CatalogProgress{Stage: CatalogPortalStarted})
	}
	r, err := get(ctx, f, portal)
	if err != nil {
		return model.Catalog{}, err
	}
	tree, err := parseHTMLContext(ctx, r)
	if err != nil {
		return model.Catalog{}, err
	}
	catalog := model.Catalog{SourceURL: r.URL, FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Guides: []model.Guide{}, Warnings: []string{}}
	if catalog.Platforms, err = options(tree, "platform"); err != nil {
		return catalog, err
	}
	if catalog.Versions, err = options(tree, "ver"); err != nil {
		return catalog, err
	}
	menus := nodes(tree, func(n *html.Node) bool { id, _ := attr(n, "id"); return id == "menu1" })
	if len(menus) != 1 {
		return catalog, errors.New("missing or ambiguous Product Documentation inventory #menu1")
	}
	cells := nodes(menus[0], func(n *html.Node) bool {
		_, id := attr(n, "id")
		_, handler := attr(n, "onclick")
		return n.Data == "td" && id && handler
	})
	if len(cells) == 0 || len(cells) > 1000 {
		return catalog, errors.New("missing or oversized Product Documentation inventory")
	}
	seen := make(map[string]bool)
	base, _ := url.Parse(r.URL)
	type entry struct {
		guide      model.Guide
		mappingURL string
	}
	entries := make([]entry, 0, len(cells))
	for _, cell := range cells {
		if err := ctx.Err(); err != nil {
			return catalog, err
		}
		id, _ := attr(cell, "id")
		handler, _ := attr(cell, "onclick")
		match := handlerPattern.FindStringSubmatch(handler)
		book := id
		if match != nil {
			book = match[3] + match[4]
		}
		if !label(book) || !label(text(cell)) {
			return catalog, errors.New("invalid guide identifier or title")
		}
		if seen[book] {
			return catalog, fmt.Errorf("duplicate Product Documentation identifier %q", book)
		}
		seen[book] = true
		guide := model.Guide{ID: book, Title: text(cell), Mappings: map[string]map[string]string{}}
		item := entry{guide: guide}
		if match == nil || book != id {
			item.guide.Error = fmt.Sprintf("unsupported Product Documentation handler for %q", book)
		} else {
			item.mappingURL = base.ResolveReference(&url.URL{Path: "json/aoscx/" + book + ".json"}).String()
		}
		entries = append(entries, item)
	}
	mappingTotal := 0
	for _, item := range entries {
		if item.mappingURL != "" {
			mappingTotal++
		}
	}
	if observe != nil {
		observe(CatalogProgress{Stage: CatalogPortalReady, Total: mappingTotal})
	}
	jobs := make(chan int)
	var group sync.WaitGroup
	var progressMu sync.Mutex
	mappingCompleted := 0
	for range min(workers, len(entries)) {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				item := &entries[index]
				if item.mappingURL == "" {
					continue
				}
				mapping, err := get(ctx, f, item.mappingURL)
				if err == nil {
					item.guide.Mappings, err = parseMappings(mapping)
				}
				if err != nil {
					item.guide.Error = fmt.Sprintf("mapping %s: %s", item.mappingURL, err)
				}
				if observe != nil && ctx.Err() == nil {
					progressMu.Lock()
					mappingCompleted++
					observe(CatalogProgress{
						Stage: CatalogMappingCompleted, Completed: mappingCompleted, Total: mappingTotal,
					})
					progressMu.Unlock()
				}
			}
		}()
	}
	for index := range entries {
		if entries[index].mappingURL != "" {
			jobs <- index
		}
	}
	close(jobs)
	group.Wait()
	if err := ctx.Err(); err != nil {
		return catalog, err
	}
	for _, item := range entries {
		guide := item.guide
		if guide.Error != "" {
			catalog.Warnings = append(catalog.Warnings, guide.Title+": "+guide.Error)
		}
		catalog.Guides = append(catalog.Guides, guide)
	}
	return catalog, nil
}

// A token decoder rejects duplicate keys instead of encoding/json's last-wins
// semantics, and enforces the exact two-level publisher mapping shape.
func parseMappings(r model.Resource) (map[string]map[string]string, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxInventoryBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxInventoryBytes || !utf8.Valid(data) {
		return nil, errors.New("oversized or invalid UTF-8 mapping")
	}
	d := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return nil, errors.New("expected version/platform mapping object")
	}
	result := make(map[string]map[string]string)
	count := 0
	for d.More() {
		token, err := d.Token()
		version, ok := token.(string)
		if err != nil || !ok || !label(version) {
			return nil, errors.New("invalid mapping version")
		}
		if _, duplicate := result[version]; duplicate || len(result) >= 1000 {
			return nil, errors.New("duplicate version or oversized mapping")
		}
		token, err = d.Token()
		if err != nil || token != json.Delim('{') {
			return nil, fmt.Errorf("expected platform mapping for %q", version)
		}
		platforms := make(map[string]string)
		for d.More() {
			token, err := d.Token()
			platform, ok := token.(string)
			if err != nil || !ok || !label(platform) {
				return nil, errors.New("invalid mapping platform")
			}
			if _, duplicate := platforms[platform]; duplicate {
				return nil, fmt.Errorf("duplicate platform %q", platform)
			}
			token, err = d.Token()
			target, ok := token.(string)
			if err != nil || !ok || !label(target) || strings.TrimSpace(target) != target {
				return nil, fmt.Errorf("invalid target for %s/%s", version, platform)
			}
			platforms[platform] = target
			count++
			if count > maxEntries {
				return nil, errors.New("mapping exceeds entry limit")
			}
		}
		if token, err = d.Token(); err != nil || token != json.Delim('}') {
			return nil, errors.New("invalid platform mapping terminator")
		}
		result[version] = platforms
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid mapping terminator")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing mapping data")
	}
	return result, nil
}

func releaseAfter(version string, boundary int) (bool, error) {
	if !releasePattern.MatchString(version) {
		return false, fmt.Errorf("unsupported release label %q; cannot infer publisher route", version)
	}
	parts := strings.Split(version, ".")
	bound := []int{10, boundary, 0}
	result := 0
	for i, part := range parts {
		v := 0
		if !strings.ContainsAny(part, "xX") {
			var err error
			v, err = strconv.Atoi(part)
			if err != nil {
				return false, fmt.Errorf("invalid release component: %w", err)
			}
		}
		b := 0
		if i < len(bound) {
			b = bound[i]
		}
		if result == 0 && v != b {
			if v > b {
				result = 1
			} else {
				result = -1
			}
		}
	}
	return result > 0, nil
}

func resolve(guide model.Guide, platform, version, target string) (model.Document, error) {
	d := model.Document{ID: guide.ID, Title: guide.Title, Platform: platform, Version: version}
	if strings.Contains(target, ":") || strings.HasPrefix(target, "//") {
		normalized, err := fetch.NormalizeURL(target)
		if err != nil {
			return d, err
		}
		u, _ := url.Parse(normalized)
		d.URL = normalized
		d.RouteOrigin = "publisher-url"
		switch {
		case strings.HasSuffix(strings.ToLower(u.Path), ".pdf"):
			d.Kind = "pdf"
		case u.Hostname() == "support.hpe.com":
			ids := u.Query()["docId"]
			if u.Path != "/hpesc/public/docDisplay" || (u.Port() != "" && u.Port() != "443") ||
				len(ids) != 1 || !bookPattern.MatchString(ids[0]) {
				return d, fmt.Errorf("unsupported HPE document URL %s", normalized)
			}
			d.Kind = "hpe"
		case strings.HasSuffix(strings.ToLower(u.Path), ".html") || strings.HasSuffix(strings.ToLower(u.Path), ".htm"):
			d.Kind = "static"
			if strings.Contains(u.Path, "/Content/") {
				d.Kind = "flare"
			}
		default:
			return d, fmt.Errorf("unsupported absolute publisher target %s", normalized)
		}
		return d, nil
	}
	if !bookPattern.MatchString(target) {
		return d, fmt.Errorf("unsupported publisher target %q; refusing to guess a route", target)
	}
	newer, err := releaseAfter(version, 17)
	if err != nil {
		return d, err
	}
	modern, err := releaseAfter(version, 12)
	if err != nil {
		return d, err
	}
	base := "https://arubanetworking.hpe.com/techdocs/"
	switch {
	case newer && strings.Contains(target, "en_us"):
		d.Kind, d.URL = "hpe", "https://support.hpe.com/hpesc/public/docDisplay?mask=sh-rs&docId="+target
		d.RouteOrigin = "hpe-document-id"
	case newer:
		d.Kind, d.URL = "static", base+"AOS-CX/"+version+"/HTML/"+target+"/index.html"
		d.RouteOrigin = "adapter-derived-book-stem"
	case modern && guide.ID != "cli":
		d.Kind, d.URL = "flare", base+"AOS-CX/"+version+"/HTML/"+target+"/Content/home.htm"
		d.RouteOrigin = "adapter-derived-book-stem"
	case modern:
		d.Kind, d.URL = "pdf", base+"AOS-CX/"+version+"/PDF/"+target+".pdf"
		d.RouteOrigin = "adapter-derived-book-stem"
	default:
		d.Kind, d.URL = "pdf", base+"Archived/AOS-CX/"+version+"/PDF/"+target+".pdf"
		d.RouteOrigin = "adapter-derived-book-stem"
	}
	return d, nil
}

func ResolveDocuments(c model.Catalog, platform, version string) ([]model.Document, []string, error) {
	if !slices.Contains(c.Platforms, platform) || !slices.Contains(c.Versions, version) {
		return nil, nil, errors.New("platform/version is not in the current portal dropdowns")
	}
	documents := []model.Document{}
	diagnostics := []string{}
	for _, g := range c.Guides {
		if g.Error != "" {
			diagnostics = append(diagnostics, g.Title+": "+g.Error)
			continue
		}
		target, mapped := g.Mappings[version][platform]
		if !mapped {
			continue
		}
		d, err := resolve(g, platform, version, target)
		if err != nil {
			diagnostics = append(diagnostics, g.Title+": "+err.Error())
		} else {
			documents = append(documents, d)
		}
	}
	return documents, diagnostics, nil
}

func AvailableVersions(c model.Catalog, platform string) []string {
	result := []string{}
	for _, version := range c.Versions {
		for _, guide := range c.Guides {
			if guide.Mappings[version][platform] != "" {
				result = append(result, version)
				break
			}
		}
	}
	return result
}
