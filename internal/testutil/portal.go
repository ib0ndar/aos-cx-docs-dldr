package testutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// PortalBase is where an authored Portal serves js/shell.js, the registry, the
// product configuration and the route adapter, as the live portal does under
// /techdocs/aruba-web-portal-ux/.
const PortalBase = "/aruba-web-portal-ux/"

// Portal is an authored copy of the Product Documentation portal: the AOS-CX
// product page, the portal registry, the product configuration and a route
// adapter carrying the routing statements the application verifies. Guide
// mapping files stay json/aoscx/<key>.json beside the page and are served by
// each test, exactly as before the portal moved to JSON.
type Portal struct {
	Platforms []string
	Versions  []string
	Guides    []PortalGuide
	// Root is the documentation root the route adapter builds guide URLs from.
	// Empty means the serving host's /techdocs/.
	Root string
}

type PortalGuide struct {
	ID    string
	Title string
	// Key names the mapping file; empty means ID.
	Key string
}

// Page is the product page. It names the product and loads the portal shell.
func (p Portal) Page() string {
	return `<!DOCTYPE html><html><head><title>AOS-CX</title>
<script>window.PRODUCT_ID = "aoscx"; window.PAGE_TITLE = "AOSCX";</script></head>
<body><div id="portalBootstrapStatus">Loading documentation...</div>
<script src="` + PortalBase + `js/shell.js"></script><main id="productPage"></main></body></html>`
}

// File returns the portal file at path, if path is one; origin is the serving
// scheme and host used for the default documentation root.
func (p Portal) File(path, origin string) (contentType string, body []byte, ok bool) {
	switch path {
	case PortalBase + "models/products.json":
		return "application/json", []byte(`{"schemaVersion":"1.0.0","products":[` +
			`{"id":"instant","complexity":"low","config":"models/instant/product.json"},` +
			`{"id":"aoscx","complexity":"high","config":"models/aoscx/product.json"}]}`), true
	case PortalBase + "models/aoscx/product.json":
		return "application/json", p.config(), true
	case PortalBase + "adapters/aoscx-adapter.js":
		root := p.Root
		if root == "" {
			root = origin + "/techdocs/"
		}
		return "application/javascript", []byte(PortalAdapter(root)), true
	}
	return "", nil, false
}

// Serve answers the product page at pagePath and the portal files, reporting
// whether it handled the request.
func (p Portal) Serve(w http.ResponseWriter, r *http.Request, pagePath string) bool {
	if r.URL.Path == pagePath {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, p.Page())
		return true
	}
	contentType, body, ok := p.File(r.URL.Path, "http://"+r.Host)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(body)
	return true
}

func (p Portal) config() []byte {
	type option struct {
		Value string `json:"value"`
		Label string `json:"label"`
	}
	options := func(values []string) []option {
		result := []option{}
		for _, value := range values {
			result = append(result, option{Value: value, Label: value})
		}
		return result
	}
	resources := []any{map[string]any{
		"id": "release-notes", "title": "AOS-CX Release Notes", "kind": "Release Notes",
		"source": map[string]any{"type": "platform-map", "selector": "platform", "map": map[string]string{}},
	}}
	for _, guide := range p.Guides {
		key := guide.Key
		if key == "" {
			key = guide.ID
		}
		resources = append(resources, map[string]any{
			"id": guide.ID, "title": guide.Title, "kind": "Guide", "keywords": []string{key},
			"source": map[string]string{"type": "catalog", "key": key}, "viewer": map[string]string{"mode": "external"},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": "1.0.0", "id": "aoscx", "pageMode": "journey", "adapter": "aoscx-adapter",
		"selectors": []any{
			map[string]any{"id": "platform", "label": "Switch Series", "options": options(p.Platforms)},
			map[string]any{"id": "version", "label": "AOS-CX Release", "dependsOn": "platform", "options": options(p.Versions)},
		},
		"resources": resources,
	})
	return body
}

// PortalAdapter is an authored route adapter with the given documentation
// root. Its routing statements match the publisher's adapter.
func PortalAdapter(root string) string {
	return strings.ReplaceAll(`const ROOT = "@ROOT@";

function normalizeVersion(value) {
    return String(value || "")
        .replace(/\.xxxx$/i, ".9999")
        .split(".")
        .map(part => Number.parseInt(part, 10) || 0);
}

function compare(first, second) {
    const left = normalizeVersion(first);
    const right = normalizeVersion(second);
    for (let index = 0; index < Math.max(left.length, right.length); index += 1) {
        const difference = (left[index] || 0) - (right[index] || 0);
        if (difference) {
            return difference > 0 ? 1 : -1;
        }
    }
    return 0;
}

function safe(value) {
    return String(value || "").trim().replace(/^\/+|\/+$/g, "");
}

export default {
    async resolveSource(resource, state) {
        const platform = state.selectors.platform;
        const version = state.selectors.version;
        const response = await fetch(
            `+"`json/aoscx/${safe(resource.source.key)}.json`"+`,
            { cache: "no-store" }
        );
        const data = await response.json();
        const target = data?.[version]?.[platform];
        if (!target) {
            return null;
        }
        if (compare(version, "10.17") >= 0) {
            const indexUrl =
                `+"`${ROOT}AOS-CX/${safe(version)}.xxxx/HTML/${safe(target)}/index.html`"+`;
            return await getFirstHrefFromIndex(indexUrl);
        }
        if (compare(version, "10.12") > 0) {
            return resource.id === "cli"
                ? `+"`${ROOT}AOS-CX/${safe(version)}/PDF/${safe(target)}.pdf`"+`
                : `+"`${ROOT}AOS-CX/${safe(version)}/HTML/${safe(target)}/Content/home.htm`"+`;
        }
        return `+"`${ROOT}Archived/AOS-CX/${safe(version)}/PDF/${safe(target)}.pdf`"+`;
    }
};
`, "@ROOT@", root)
}
