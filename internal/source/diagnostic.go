//go:build reqexperiment

package source

import (
	"errors"

	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

// DiagnosticMappingRequestBound inspects the same Product Documentation cells
// as LoadCatalog so a live evidence harness can stop before an oversized batch.
func DiagnosticMappingRequestBound(resource model.Resource) (int, error) {
	tree, err := parseHTML(resource)
	if err != nil {
		return 0, err
	}
	menus := nodes(tree, func(n *html.Node) bool { id, _ := attr(n, "id"); return id == "menu1" })
	if len(menus) != 1 {
		return 0, errors.New("missing or ambiguous Product Documentation inventory")
	}
	cells := nodes(menus[0], func(n *html.Node) bool {
		_, id := attr(n, "id")
		_, handler := attr(n, "onclick")
		return n.Data == "td" && id && handler
	})
	if len(cells) == 0 || len(cells) > 1000 {
		return 0, errors.New("missing or oversized Product Documentation inventory")
	}
	return len(cells), nil
}
