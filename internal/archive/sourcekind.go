package archive

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
	"golang.org/x/net/html"
)

const (
	hpeGraphikRegular = "https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Regular-Web.woff2"
	hpeGraphikBold    = "https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Bold-Web.woff2"
)

var hpeBarePage = regexp.MustCompile(`^[A-Za-z0-9_-]+\.html?(?:#[A-Za-z0-9_.:-]+)?$`)

type hpeReference struct {
	document string
	page     string
	extra    string
	fragment string
	host     string
}

func parseHPEReference(raw string, current *pageInput) (hpeReference, bool) {
	if current != nil && strings.HasPrefix(raw, "#") {
		reference, err := url.Parse(raw)
		if err != nil || reference.Fragment == "" || reference.RawQuery != "" || reference.Path != "" {
			return hpeReference{}, false
		}
		currentIdentity, ok := parseHPEReference(current.topic.URL, nil)
		if !ok {
			currentIdentity, ok = parseHPEReference(current.requestURL, nil)
		}
		if !ok {
			return hpeReference{}, false
		}
		currentIdentity.fragment = reference.Fragment
		return currentIdentity, true
	}
	if current != nil && strings.HasPrefix(raw, "?") {
		currentIdentity, ok := parseHPEReference(current.requestURL, nil)
		if !ok {
			return hpeReference{}, false
		}
		base, err := url.Parse(current.requestURL)
		if err != nil {
			return hpeReference{}, false
		}
		reference, err := url.Parse(raw)
		if err != nil {
			return hpeReference{}, false
		}
		query := reference.Query()
		inherited, err := url.ParseQuery(currentIdentity.extra)
		if err != nil {
			return hpeReference{}, false
		}
		for key, values := range inherited {
			if _, present := query[key]; !present {
				query[key] = values
			}
		}
		base.RawQuery = query.Encode()
		base.Fragment = reference.Fragment
		resolved, err := fetch.NormalizeURL(base.String())
		if err != nil {
			return hpeReference{}, false
		}
		raw = resolved
	}
	if hpeBarePage.MatchString(raw) && current != nil {
		pageName, fragment, _ := strings.Cut(raw, "#")
		currentIdentity, ok := parseHPEReference(current.topic.URL, nil)
		if !ok {
			currentIdentity, ok = parseHPEReference(current.requestURL, nil)
		}
		if !ok {
			return hpeReference{}, false
		}
		currentIdentity.page, currentIdentity.fragment = pageName, fragment
		return currentIdentity, true
	}
	normalized, err := fetch.NormalizeURL(raw)
	if err != nil {
		return hpeReference{}, false
	}
	parsed, err := url.Parse(normalized)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return hpeReference{}, false
	}
	query := parsed.Query()
	if len(query["docId"]) > 1 || len(query["page"]) > 1 {
		return hpeReference{}, false
	}
	identity := hpeReference{fragment: parsed.Fragment, host: strings.ToLower(parsed.Host)}
	if len(query["docId"]) == 1 {
		identity.document = query["docId"][0]
	}
	if len(query["page"]) == 1 {
		identity.page = query["page"][0]
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	switch {
	case parsed.Path == "/hpesc/public/docDisplay":
		if identity.document == "" {
			return hpeReference{}, false
		}
	case len(parts) == 5 && slices.Equal(parts[:4], []string{"hpesc", "public", "api", "document"}):
		if identity.document != "" && identity.document != parts[4] {
			return hpeReference{}, false
		}
		identity.document = parts[4]
	case len(parts) == 4 && parts[0] == "documents" && parts[2] == "html":
		if identity.document != "" && identity.document != parts[1] {
			return hpeReference{}, false
		}
		if identity.page != "" && identity.page != parts[3] {
			return hpeReference{}, false
		}
		identity.document, identity.page = parts[1], parts[3]
	default:
		return hpeReference{}, false
	}
	if identity.document == "" {
		return hpeReference{}, false
	}
	if identity.page == "" {
		if query.Get("ignorePayload") == "true" || strings.HasSuffix(parsed.Path, "/docDisplay") {
			identity.page = "index.html"
		}
	}
	if identity.page == "" {
		return hpeReference{}, false
	}
	extra := url.Values{}
	for key, values := range query {
		if key == "docId" || key == "page" || key == "mask" || key == "ignorePayload" {
			continue
		}
		extra[key] = append([]string{}, values...)
	}
	identity.extra = extra.Encode()
	return identity, true
}

func linkFragment(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Fragment
}

func (identity hpeReference) key() string {
	return identity.document + "\x00" + identity.page + "\x00" + identity.extra
}

func (a *htmlArchiver) initializeSource() error {
	switch a.plan.Document.Kind {
	case "flare":
		rootURL, err := source.GuideRoot(a.plan.Document.URL)
		if err != nil {
			return err
		}
		a.guideRoots = []string{rootURL}
	case "hpe":
		selected, ok := parseHPEReference(a.plan.Document.URL, nil)
		if !ok {
			return errors.New("invalid HPE document identity in archive plan")
		}
		a.hpeDocument = selected.document
		a.hpeAliases = map[string]*pageInput{}
		a.hpeAmbiguous = map[string]bool{}
		a.hpeHosts = map[string]bool{}
		for _, page := range a.pages {
			var identities []hpeReference
			for _, raw := range []string{page.topic.URL, page.requestURL} {
				identity, ok := parseHPEReference(raw, nil)
				if !ok || identity.document != selected.document {
					return fmt.Errorf("HPE topic identity conflicts with selected document: %s", raw)
				}
				identities = append(identities, identity)
				a.hpeHosts[identity.host] = true
			}
			if identities[0].key() != identities[1].key() {
				return fmt.Errorf("HPE public/fetch topic identity mismatch: %s", page.topic.URL)
			}
			a.addHPEAlias(identities[0], page)
		}
	case "static":
		root := a.plan.Inventory.RootURL
		if root == "" {
			parsed, err := url.Parse(a.plan.Document.URL)
			if err != nil {
				return err
			}
			parsed.RawQuery, parsed.Fragment = "", ""
			parsed.Path = path.Dir(parsed.Path) + "/"
			root = parsed.String()
		}
		a.guideRoots = []string{root}
	default:
		return fmt.Errorf("HTML archiving is not implemented for source kind %q", a.plan.Document.Kind)
	}
	return nil
}

func (a *htmlArchiver) addHPEAlias(identity hpeReference, page *pageInput) {
	key := identity.key()
	if existing := a.hpeAliases[key]; existing != nil && existing != page {
		delete(a.hpeAliases, key)
		a.hpeAmbiguous[key] = true
		return
	}
	if !a.hpeAmbiguous[key] {
		a.hpeAliases[key] = page
	}
}

func (a *htmlArchiver) topicForReference(raw string, current *pageInput) *pageInput {
	if page := a.pageByURL[canonicalNoFragment(raw)]; page != nil {
		return page
	}
	if a.plan.Document.Kind != "hpe" {
		return nil
	}
	identity, ok := parseHPEReference(raw, current)
	if !ok || identity.document != a.hpeDocument || !a.hpeHosts[identity.host] || a.hpeAmbiguous[identity.key()] {
		return nil
	}
	return a.hpeAliases[identity.key()]
}

func validateHPEPage(resource model.Resource, topic model.Topic, doc *html.Node) error {
	expected, ok := parseHPEReference(topic.FetchURL, nil)
	if !ok {
		return errors.New("invalid HPE topic fetch identity")
	}
	actual, ok := parseHPEReference(resource.URL, nil)
	expectedURL, expectedErr := url.Parse(topic.FetchURL)
	actualURL, actualErr := url.Parse(resource.URL)
	if !ok || expectedErr != nil || actualErr != nil || actual.key() != expected.key() ||
		!strings.EqualFold(actualURL.Scheme, expectedURL.Scheme) ||
		!strings.EqualFold(actualURL.Host, expectedURL.Host) || actualURL.Path != expectedURL.Path {
		return fmt.Errorf("HPE response document/page/query mismatch: requested %s, received %s", topic.FetchURL, resource.URL)
	}
	if document := resource.Headers.Get("Doc-Id"); document != "" && document != expected.document {
		return fmt.Errorf("HPE response document mismatch: requested %s, received %s", expected.document, document)
	}
	if pageName := resource.Headers.Get("Doc-Page-Name"); pageName != "" && pageName != expected.page {
		return fmt.Errorf("HPE response page mismatch: requested %s, received %s", expected.page, pageName)
	}
	mains := matchingNodes(doc, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.Data == "main" && hasClass(node, "ditasrc")
	})
	if len(mains) != 1 || strings.TrimSpace(nodeText(mains[0])) == "" ||
		findNode(mains[0], func(node *html.Node) bool {
			return node.Type == html.ElementNode && (node.Data == "h1" || node.Data == "article")
		}) == nil {
		return errors.New("expected genuine HPE main.ditasrc topic content")
	}
	return nil
}

func hpeTopicScope(expected string) func(string) error {
	return func(actual string) error {
		want, err := url.Parse(expected)
		if err != nil {
			return err
		}
		got, err := url.Parse(actual)
		if err != nil {
			return err
		}
		if !strings.EqualFold(want.Scheme, got.Scheme) || !strings.EqualFold(want.Host, got.Host) ||
			want.Path != got.Path || want.Query().Encode() != got.Query().Encode() {
			return fmt.Errorf("HPE topic request/redirect left its exact planned endpoint: %s", actual)
		}
		return nil
	}
}

func (a *htmlArchiver) readPage(page *pageInput, refresh bool) (model.Resource, []byte, error) {
	ctx := a.ctx
	if a.plan.Document.Kind == "hpe" {
		ctx = fetch.WithRequestScope(ctx, hpeTopicScope(page.requestURL))
	}
	return readResource(ctx, a.fetcher, page.requestURL, refresh, a.maxBytes)
}

func validateStaticPage(doc *html.Node) error {
	content := findContent(doc, "static")
	if content == nil || strings.TrimSpace(nodeText(content)) == "" {
		return errors.New("expected substantive static/Oxygen topic content")
	}
	return nil
}

func pageMetadata(doc *html.Node) (map[string]string, []string) {
	metadata := map[string]string{}
	var copyrights []string
	walk(doc, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		if node.Data == "meta" && attr(node, "content") != "" {
			name := attr(node, "name")
			if name == "" {
				name = attr(node, "property")
			}
			lower := strings.ToLower(name)
			for _, prefix := range []string{"dc.", "dc:", "dcterms.", "dcterms:", "og:", "article:", "copyright", "author", "description", "date", "pub", "last", "modif", "created", "updated", "generator", "product", "version"} {
				if strings.HasPrefix(lower, prefix) {
					metadata[name] = attr(node, "content")
					break
				}
			}
		}
		if hasClass(node, "publishedDate") {
			if value := strings.TrimSpace(spacePattern.ReplaceAllString(nodeText(node), " ")); value != "" {
				metadata["publishedDate"] = value
			}
		}
		identity := strings.ToLower(attr(node, "class") + " " + attr(node, "id"))
		if node.Data == "footer" || strings.Contains(identity, "copyright") {
			value := strings.TrimSpace(spacePattern.ReplaceAllString(nodeText(node), " "))
			lower := strings.ToLower(value)
			if value != "" && (strings.Contains(lower, "copyright") || strings.Contains(value, "©") || strings.Contains(lower, "(c)")) &&
				!slices.Contains(copyrights, value) {
				copyrights = append(copyrights, value)
			}
		}
	})
	if len(metadata) == 0 {
		metadata = nil
	}
	return metadata, copyrights
}

func matchingNodes(root *html.Node, predicate func(*html.Node) bool) []*html.Node {
	var result []*html.Node
	walk(root, func(node *html.Node) {
		if predicate(node) {
			result = append(result, node)
		}
	})
	return result
}

func (a *htmlArchiver) hpeFontStyles(page *pageInput) []string {
	fonts := []struct {
		weight int
		url    string
	}{{400, hpeGraphikRegular}, {700, hpeGraphikBold}}
	var rules []string
	for _, font := range fonts {
		local, err := a.assets.local(font.url, page.baseURL, page.path, dependencyAsset)
		if err != nil {
			a.errors = append(a.errors, page.topic.URL+": HPE Graphik font: "+err.Error())
			continue
		}
		format := "woff2"
		if strings.HasSuffix(strings.ToLower(local), ".ttf") {
			format = "truetype"
		}
		rules = append(rules, fmt.Sprintf(
			`@font-face{font-family:"HPE Graphik";font-style:normal;font-weight:%d;src:url("%s") format("%s")}`,
			font.weight, strings.ReplaceAll(local, `"`, `\"`), format,
		))
	}
	return rules
}
