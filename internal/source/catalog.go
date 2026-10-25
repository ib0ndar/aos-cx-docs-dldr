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

// folderPattern admits publisher guide folder names, which since 10.17 can
// contain spaces, parentheses and commas, but never a path separator.
var folderPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9 _().,-]*[A-Za-z0-9)_.-])?$`)
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
	catalog, entries, err := loadPortal(ctx, f, r)
	if err != nil {
		return catalog, err
	}
	catalog.FetchedAt = time.Now().UTC().Format(time.RFC3339)
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

// compareRelease orders two release labels exactly as the publisher's route
// adapter does: a trailing .xxxx counts as .9999, every other component is
// read as a decimal number, and components that are not numbers count as 0.
func compareRelease(first, second string) int {
	parts := func(value string) []int {
		if strings.HasSuffix(strings.ToLower(value), ".xxxx") {
			value = value[:len(value)-len(".xxxx")] + ".9999"
		}
		var result []int
		for _, part := range strings.Split(value, ".") {
			digits := part
			if end := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
				digits = part[:end]
			}
			n, _ := strconv.Atoi(digits)
			result = append(result, n)
		}
		return result
	}
	left, right := parts(first), parts(second)
	for index := range max(len(left), len(right)) {
		var a, b int
		if index < len(left) {
			a = left[index]
		}
		if index < len(right) {
			b = right[index]
		}
		if a != b {
			if a > b {
				return 1
			}
			return -1
		}
	}
	return 0
}

// resolve turns one publisher mapping into a guide document. Folder targets
// follow the publisher's route adapter, which the catalog has verified;
// root is the documentation root that adapter builds URLs from.
func resolve(guide model.Guide, platform, version, target, root string) (model.Document, error) {
	d := model.Document{ID: guide.ID, Title: guide.Title, Platform: platform, Version: version}
	if strings.Contains(target, ":") || strings.HasPrefix(target, "//") {
		normalized, err := fetch.NormalizeURL(target)
		if err != nil {
			return d, err
		}
		// Refuse non-public targets here with a clear reason.
		// Loopback targets pass only so fixture catalogues resolve; the
		// transport still refuses them outside fixture mode.
		if err := fetch.CheckDestination(normalized, true); err != nil {
			return d, fmt.Errorf("unsupported absolute publisher target: %w", err)
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
	if !folderPattern.MatchString(target) || strings.Contains(target, "..") {
		return d, fmt.Errorf("unsupported publisher target %q; refusing to guess a route", target)
	}
	if !releasePattern.MatchString(version) {
		return d, fmt.Errorf("unsupported release label %q; cannot infer publisher route", version)
	}
	if root == "" {
		return d, errors.New("catalog has no verified documentation root")
	}
	folder := url.PathEscape(target)
	d.RouteOrigin = "adapter-derived-book-stem"
	switch {
	case compareRelease(version, "10.17") >= 0:
		d.Kind, d.URL = "static", root+"AOS-CX/"+version+".xxxx/HTML/"+folder+"/index.html"
	case compareRelease(version, "10.12") > 0 && guide.ID == "cli":
		d.Kind, d.URL = "pdf", root+"AOS-CX/"+version+"/PDF/"+folder+".pdf"
	case compareRelease(version, "10.12") > 0:
		d.Kind, d.URL = "flare", root+"AOS-CX/"+version+"/HTML/"+folder+"/Content/home.htm"
	default:
		d.Kind, d.URL = "pdf", root+"Archived/AOS-CX/"+version+"/PDF/"+folder+".pdf"
	}
	normalized, err := fetch.NormalizeURL(d.URL)
	if err != nil {
		return d, err
	}
	d.URL = normalized
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
		d, err := resolve(g, platform, version, target, c.RouteRoot)
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
