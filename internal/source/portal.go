package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

// The Product Documentation portal is a JavaScript application. Its page names
// the product and loads js/shell.js; the product registry and the product
// configuration are JSON files under the shell's portal root; guide mappings
// are JSON files beside the page; and a per-product adapter module turns one
// mapping into a guide URL. The application reads the JSON and checks that the
// adapter still implements exactly the routes resolve reproduces. It never runs
// publisher JavaScript.

const portalProduct = "aoscx"

const maxPortalOptions = 1000

var (
	productIDPattern  = regexp.MustCompile(`window\.PRODUCT_ID\s*=\s*(?:"([a-z0-9-]+)"|'([a-z0-9-]+)')`)
	portalNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	routeRootPattern  = regexp.MustCompile(`const ROOT = "([^"\s]+/)";`)
)

// adapterRoutes are the statements of the publisher's adapter that resolve
// reproduces, compared with all white space removed. A change to any of them
// means the publisher changed how guides are routed, and resolve must change
// with it rather than build URLs the portal no longer opens.
var adapterRoutes = []string{
	`.replace(/\.xxxx$/i, ".9999")`,
	"fetch(\n`json/aoscx/${safe(resource.source.key)}.json`",
	`const target = data?.[version]?.[platform];`,
	"if (compare(version, \"10.17\") >= 0) { const indexUrl = `${ROOT}AOS-CX/${safe(version)}.xxxx/HTML/${safe(target)}/index.html`;",
	"if (compare(version, \"10.12\") > 0) { return resource.id === \"cli\" ? `${ROOT}AOS-CX/${safe(version)}/PDF/${safe(target)}.pdf` : `${ROOT}AOS-CX/${safe(version)}/HTML/${safe(target)}/Content/home.htm`; }",
	"return `${ROOT}Archived/AOS-CX/${safe(version)}/PDF/${safe(target)}.pdf`;",
}

type portalRegistry struct {
	Products []struct {
		ID     string `json:"id"`
		Config string `json:"config"`
	} `json:"products"`
}

type portalConfig struct {
	ID        string `json:"id"`
	Adapter   string `json:"adapter"`
	Selectors []struct {
		ID      string `json:"id"`
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	} `json:"selectors"`
	Resources []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Source struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		} `json:"source"`
	} `json:"resources"`
}

type portalGuide struct {
	guide      model.Guide
	mappingURL string
}

// portalShellRoot returns the portal root that the page's js/shell.js is
// served from, and checks that the page is the AOS-CX product page.
func portalShellRoot(page model.Resource) (string, error) {
	tree, err := parseHTML(page)
	if err != nil {
		return "", err
	}
	var shells []string
	var products []string
	for _, script := range nodes(tree, func(n *html.Node) bool { return n.Data == "script" }) {
		if src, ok := attr(script, "src"); ok {
			if strings.HasSuffix(strings.SplitN(src, "?", 2)[0], "/js/shell.js") {
				shells = append(shells, src)
			}
			continue
		}
		for _, match := range productIDPattern.FindAllStringSubmatch(text(script), -1) {
			products = append(products, match[1]+match[2])
		}
	}
	if len(products) != 1 || products[0] != portalProduct {
		return "", fmt.Errorf("portal page must name exactly one product %q: %s", portalProduct, page.URL)
	}
	if len(shells) != 1 {
		return "", fmt.Errorf("portal page must load exactly one js/shell.js: %s", page.URL)
	}
	shell, err := joinSource(page.URL, shells[0], false)
	if err != nil {
		return "", fmt.Errorf("invalid portal shell reference: %w", err)
	}
	return joinSource(shell, "../", false)
}

// portalPath resolves a publisher-supplied path relative to the portal root
// and refuses anything that could leave it.
func portalPath(root, relative string) (string, error) {
	if !label(relative) || strings.Contains(relative, "\\") || strings.HasPrefix(relative, "/") ||
		strings.Contains(relative, ":") || !plainSegments(relative) {
		return "", fmt.Errorf("invalid portal path %q", relative)
	}
	resolved, err := joinSource(root, relative, false)
	if err != nil {
		return "", err
	}
	if !inside(resolved, root) {
		return "", fmt.Errorf("portal path leaves the portal root: %s", relative)
	}
	return resolved, nil
}

func readPortalJSON(ctx context.Context, f model.Fetcher, raw string, value any) (string, error) {
	r, err := get(ctx, f, raw)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	if media := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Headers.Get("Content-Type"), ";", 2)[0])); media != "application/json" {
		return "", fmt.Errorf("expected JSON portal data, received %q: %s", media, r.URL)
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxInventoryBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxInventoryBytes {
		return "", fmt.Errorf("portal data exceeds byte limit: %s", r.URL)
	}
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	if err := decoder.Decode(value); err != nil {
		return "", fmt.Errorf("invalid portal data at %s: %w", r.URL, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", fmt.Errorf("trailing portal data at %s", r.URL)
	}
	return r.URL, nil
}

func portalOptions(config portalConfig, id string, valid func(string) bool) ([]string, error) {
	var found []string
	matches := 0
	for _, selector := range config.Selectors {
		if selector.ID != id {
			continue
		}
		matches++
		seen := map[string]bool{}
		for _, option := range selector.Options {
			if !valid(option.Value) {
				return nil, fmt.Errorf("invalid portal %s option %q", id, option.Value)
			}
			if !seen[option.Value] {
				seen[option.Value] = true
				found = append(found, option.Value)
			}
		}
	}
	if matches != 1 || len(found) == 0 || len(found) > maxPortalOptions {
		return nil, fmt.Errorf("portal configuration must have one %s selector with 1..%d options", id, maxPortalOptions)
	}
	return found, nil
}

func plainLabel(value string) bool {
	return label(value) && strings.TrimSpace(value) == value
}

// verifyRouteAdapter checks the adapter module against adapterRoutes and
// returns the documentation root it builds guide URLs from.
func verifyRouteAdapter(r model.Resource) (string, error) {
	defer r.Body.Close()
	media := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Headers.Get("Content-Type"), ";", 2)[0]))
	if media != "" && media != "application/javascript" && media != "text/javascript" && media != "application/x-javascript" {
		return "", fmt.Errorf("expected JavaScript route adapter, received %q: %s", media, r.URL)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInventoryBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxInventoryBytes {
		return "", fmt.Errorf("route adapter exceeds byte limit: %s", r.URL)
	}
	compact := strings.Join(strings.Fields(string(body)), "")
	for _, route := range adapterRoutes {
		if !strings.Contains(compact, strings.Join(strings.Fields(route), "")) {
			return "", fmt.Errorf("publisher route adapter %s no longer matches the guide routes this version understands; "+
				"an updated aos-cx-docs-dldr is required", r.URL)
		}
	}
	roots := routeRootPattern.FindAllStringSubmatch(string(body), -1)
	if len(roots) != 1 {
		return "", fmt.Errorf("publisher route adapter must define one documentation ROOT: %s", r.URL)
	}
	root, err := joinSource(r.URL, roots[0][1], false)
	if err != nil {
		return "", fmt.Errorf("invalid documentation ROOT in route adapter: %w", err)
	}
	return root, nil
}

// loadPortal reads the portal's product configuration and returns the catalog
// selectors, the guides with their mapping URLs, and the verified route root.
func loadPortal(ctx context.Context, f model.Fetcher, page model.Resource) (model.Catalog, []portalGuide, error) {
	catalog := model.Catalog{SourceURL: page.URL, Guides: []model.Guide{}, Warnings: []string{}}
	root, err := portalShellRoot(page)
	if err != nil {
		return catalog, nil, err
	}
	registryURL, err := portalPath(root, "models/products.json")
	if err != nil {
		return catalog, nil, err
	}
	var registry portalRegistry
	if _, err := readPortalJSON(ctx, f, registryURL, &registry); err != nil {
		return catalog, nil, err
	}
	configPath := ""
	for _, product := range registry.Products {
		if product.ID == portalProduct {
			if configPath != "" {
				return catalog, nil, fmt.Errorf("portal registry lists product %q more than once", portalProduct)
			}
			configPath = product.Config
		}
	}
	if configPath == "" {
		return catalog, nil, fmt.Errorf("portal registry does not list product %q", portalProduct)
	}
	configURL, err := portalPath(root, configPath)
	if err != nil {
		return catalog, nil, err
	}
	var config portalConfig
	if _, err := readPortalJSON(ctx, f, configURL, &config); err != nil {
		return catalog, nil, err
	}
	if config.ID != portalProduct || !portalNamePattern.MatchString(config.Adapter) {
		return catalog, nil, fmt.Errorf("portal configuration is not the %q product with a route adapter: %s", portalProduct, configURL)
	}
	if catalog.Platforms, err = portalOptions(config, "platform", plainLabel); err != nil {
		return catalog, nil, err
	}
	if catalog.Versions, err = portalOptions(config, "version", releasePattern.MatchString); err != nil {
		return catalog, nil, err
	}
	adapterURL, err := portalPath(root, "adapters/"+config.Adapter+".js")
	if err != nil {
		return catalog, nil, err
	}
	adapter, err := get(ctx, f, adapterURL)
	if err != nil {
		return catalog, nil, err
	}
	if catalog.RouteRoot, err = verifyRouteAdapter(adapter); err != nil {
		return catalog, nil, err
	}
	base, _ := url.Parse(page.URL)
	seen := map[string]bool{}
	var guides []portalGuide
	for _, resource := range config.Resources {
		if resource.Source.Type != "catalog" {
			continue
		}
		if len(guides) >= maxPortalOptions {
			return catalog, nil, errors.New("oversized Product Documentation guide inventory")
		}
		if !bookPattern.MatchString(resource.ID) || !plainLabel(resource.Title) || !bookPattern.MatchString(resource.Source.Key) {
			return catalog, nil, fmt.Errorf("invalid guide identifier, key or title for %q", resource.ID)
		}
		if seen[resource.ID] {
			return catalog, nil, fmt.Errorf("duplicate Product Documentation identifier %q", resource.ID)
		}
		seen[resource.ID] = true
		mapping := base.ResolveReference(&url.URL{Path: "json/" + portalProduct + "/" + resource.Source.Key + ".json"}).String()
		guides = append(guides, portalGuide{
			guide:      model.Guide{ID: resource.ID, Title: resource.Title, Mappings: map[string]map[string]string{}},
			mappingURL: mapping,
		})
	}
	if len(guides) == 0 {
		return catalog, nil, errors.New("portal configuration lists no guides")
	}
	return catalog, guides, ctx.Err()
}
