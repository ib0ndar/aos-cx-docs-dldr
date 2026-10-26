package source

import (
	"fmt"
	"net/url"
	"slices"

	"aos-cx-docs-dldr/internal/model"
)

// RouteOriginUserSupplied marks a document whose source the user named with
// --guide-source in place of the publisher's mapping.
const RouteOriginUserSupplied = "user-supplied"

// OverrideGuideSource makes guide id resolve to raw for platform and version,
// in place of the publisher's mapping. The publisher can map a guide to a
// document it does not serve while it publishes the guide where none of its
// data points, such as the 10.18 ACLs and Classifier Policies Guide for 4100i,
// 6000 and 6100, served only by HPE Support Center. raw must be a document
// URL that resolve accepts as an absolute publisher target, on the
// catalogue's documentation host or on HPE Support Center; the document is
// then planned and checked like any other. It returns a note naming both
// routes for the release manifest.
func OverrideGuideSource(c *model.Catalog, id, platform, version, raw string) (string, error) {
	index := slices.IndexFunc(c.Guides, func(g model.Guide) bool { return g.ID == id })
	if index < 0 {
		return "", fmt.Errorf("--guide-source names guide %q, which the catalogue does not list", id)
	}
	guide := &c.Guides[index]
	target, mapped := guide.Mappings[version][platform]
	if guide.Error != "" || !mapped {
		return "", fmt.Errorf("--guide-source names guide %q, which is not mapped for %s %s", id, platform, version)
	}
	if _, overridden := guide.Overrides[version][platform]; overridden {
		return "", fmt.Errorf("--guide-source names guide %q more than once", id)
	}
	document, err := resolve(*guide, platform, version, raw, c.RouteRoot)
	if err != nil {
		return "", fmt.Errorf("--guide-source for %q: %w", id, err)
	}
	if document.RouteOrigin != "publisher-url" {
		return "", fmt.Errorf("--guide-source for %q must be an absolute document URL", id)
	}
	source, _ := url.Parse(document.URL)
	root, err := url.Parse(c.RouteRoot)
	if err != nil || c.RouteRoot == "" || (source.Host != root.Host && document.Kind != "hpe") {
		return "", fmt.Errorf("--guide-source for %q must be a document on %s or HPE Support Center", id, c.RouteRoot)
	}
	mappedURL := target
	if mappedDocument, err := resolve(*guide, platform, version, target, c.RouteRoot); err == nil {
		mappedURL = mappedDocument.URL
	}
	if guide.Overrides == nil {
		guide.Overrides = map[string]map[string]string{}
	}
	if guide.Overrides[version] == nil {
		guide.Overrides[version] = map[string]string{}
	}
	guide.Overrides[version][platform] = document.URL
	return fmt.Sprintf("%s for %s %s: --guide-source replaces the publisher's mapping %s with %s",
		guide.Title, platform, version, mappedURL, document.URL), nil
}
