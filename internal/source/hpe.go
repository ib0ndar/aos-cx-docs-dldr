package source

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

const hpeHost = "https://support.hpe.com"
const hpeStyle = hpeHost + "/resource3/doc-resources/css/hpesc-doc.css"

var hpeTopicLink = regexp.MustCompile(`^[A-Za-z0-9_-]+\.html?(?:#[A-Za-z0-9_.:-]+)?$`)
var repeatedLocale = regexp.MustCompile(`(?:en_us){2,}$`)

func hpeIdentity(raw string) (string, string, error) {
	normalized, err := fetch.NormalizeURL(raw)
	if err != nil {
		return "", "", err
	}
	u, _ := url.Parse(normalized)
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", "", err
	}
	ids := query["docId"]
	if u.Hostname() != "support.hpe.com" || (u.Port() != "" && u.Port() != "443") ||
		u.Path != "/hpesc/public/docDisplay" || len(ids) != 1 || !bookPattern.MatchString(ids[0]) {
		return "", "", fmt.Errorf("unsupported HPE document URL: %s", raw)
	}
	var extra []string
	for _, part := range strings.Split(u.RawQuery, "&") {
		key, _, _ := strings.Cut(part, "=")
		key, err = url.QueryUnescape(key)
		if err != nil {
			return "", "", err
		}
		if key != "docId" && key != "mask" && key != "page" {
			extra = append(extra, part)
		}
	}
	return ids[0], strings.Join(extra, "&"), nil
}

func hpeScope(expected string) func(string) error {
	return func(actual string) error {
		normalized, err := fetch.CanonicalURL(actual)
		if err != nil {
			return err
		}
		want, _ := url.Parse(expected)
		got, _ := url.Parse(normalized)
		if got.Scheme != want.Scheme || got.Host != want.Host || got.Path != want.Path {
			return fmt.Errorf("HPE source redirected away from the requested document: %s", actual)
		}
		gotQuery, err := url.ParseQuery(got.RawQuery)
		if err != nil {
			return err
		}
		wantQuery := want.Query()
		delete(wantQuery, "mask")
		delete(gotQuery, "mask")
		if !reflect.DeepEqual(gotQuery, wantQuery) {
			return fmt.Errorf("HPE response changed requested query identity: %s", actual)
		}
		return nil
	}
}

func hasClass(node *html.Node, class string) bool {
	value, _ := attr(node, "class")
	for _, item := range strings.Fields(value) {
		if item == class {
			return true
		}
	}
	return false
}

func hpeResponseIdentity(in input, id, page string) error {
	if actual := in.headers.Get("Doc-Id"); actual != "" && actual != id {
		return fmt.Errorf("HPE response document mismatch: requested %s, received %s", id, actual)
	}
	if page != "" {
		if actual := in.headers.Get("Doc-Page-Name"); actual != "" && actual != page {
			return fmt.Errorf("HPE response page mismatch: requested %s, received %s", page, actual)
		}
	}
	return nil
}

func (p *planner) hpe(preferPDF bool) (model.DocumentPlan, error) {
	id, extra, err := hpeIdentity(p.plan.Document.URL)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if repeatedLocale.MatchString(id) {
		p.warning("Publisher identifier has a repeated locale suffix; used verbatim: " + id)
	}
	api := hpeHost + "/hpesc/public/api/document/" + id
	addExtra := func(raw string) string {
		if extra != "" {
			return raw + "&" + extra
		}
		return raw
	}
	frontURL := addExtra(api + "?ignorePayload=true")
	front, err := p.read(frontURL, "front", hpeScope(frontURL))
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if err := hpeResponseIdentity(front, id, ""); err != nil {
		return model.DocumentPlan{}, err
	}
	if looksPDF(front.body) {
		return p.verifiedPDF(front, PDFOriginSelectedRoute, false)
	}
	if err := hpeResponseIdentity(front, id, "index.html"); err != nil {
		return model.DocumentPlan{}, err
	}
	tree, err := p.html(front, true)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if preferPDF {
		exportURL := hpeWholeDocumentPDFURL(id, extra)
		pdf, pdfErr := p.read(exportURL, "pdf", hpeWholeDocumentPDFScope(exportURL, id, extra))
		if pdfErr == nil {
			if identityErr := hpePDFResponseIdentity(pdf, id); identityErr != nil {
				pdfErr = identityErr
			} else {
				plan, verifyErr := p.verifiedPDF(pdf, PDFOriginHPEExportAll, true)
				if verifyErr == nil {
					return plan, nil
				}
				pdfErr = verifyErr
			}
		}
		if errors.Is(pdfErr, context.Canceled) || errors.Is(pdfErr, context.DeadlineExceeded) {
			return model.DocumentPlan{}, pdfErr
		}
		p.warning(preferredPDFFallbackWarning(pdfErr.Error()))
	}
	mains := nodes(tree, func(n *html.Node) bool { return n.Data == "main" && hasClass(n, "ditasrc") })
	if len(mains) != 1 {
		pdf, err := hpeAdvertisedPDF(tree, front.url, id)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		if pdf != "" {
			resource, err := p.read(pdf, "pdf", func(raw string) error {
				if !sameHPEPDF(raw, id) {
					return errors.New("advertised PDF left the known HPE document")
				}
				return nil
			})
			if err != nil {
				return model.DocumentPlan{}, err
			}
			if err := hpeResponseIdentity(resource, id, ""); err != nil {
				return model.DocumentPlan{}, err
			}
			return p.verifiedPDF(resource, PDFOriginSourceAdvertised, false)
		}

		return model.DocumentPlan{}, fmt.Errorf("expected HPE multipage main.ditasrc, not a landing/error page: %s", front.url)
	}
	if text(mains[0]) == "" || len(nodes(mains[0], func(n *html.Node) bool { return n.Data == "h1" || n.Data == "article" })) == 0 {
		return model.DocumentPlan{}, errors.New("empty or unrecognized HPE front matter")
	}
	tocURL := addExtra(api + "?page=content.json")
	toc, err := p.read(tocURL, "toc", hpeScope(tocURL))
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if err := hpeResponseIdentity(toc, id, ""); err != nil {
		return model.DocumentPlan{}, err
	}
	data, err := p.json(toc)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	array, ok := data.([]any)
	if !ok || len(array) == 0 {
		return model.DocumentPlan{}, errors.New("expected nonempty HPE topic array")
	}
	public := addExtra(hpeHost + "/hpesc/public/docDisplay?docId=" + id)
	if err := p.front(public, p.plan.Document.Title, front.url); err != nil {
		return model.DocumentPlan{}, err
	}
	stylesheet, err := styles(tree, front.url)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	p.plan.Styles = append([]string{hpeStyle}, stylesheet...)
	var walk func([]any, int) ([]model.TocEntry, error)
	walk = func(values []any, depth int) ([]model.TocEntry, error) {
		result := []model.TocEntry{}
		for _, value := range values {
			if err := p.check(depth); err != nil {
				return nil, err
			}
			node, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("invalid HPE TOC node")
			}
			title, err := stringLabel(node["topicName"], "HPE topicName")
			if err != nil {
				return nil, err
			}
			children := []any{}
			if v := node["children"]; v != nil {
				var ok bool
				children, ok = v.([]any)
				if !ok {
					return nil, fmt.Errorf("invalid HPE children for %q", title)
				}
			}
			entry := model.TocEntry{Title: title}
			if link := node["topicLink"]; link != nil && link != "" {
				value, err := stringLabel(link, "HPE topicLink")
				if err != nil {
					return nil, err
				}
				if !hpeTopicLink.MatchString(value) {
					return nil, fmt.Errorf("unsupported HPE topicLink %q", value)
				}
				page, fragment, has := strings.Cut(value, "#")
				entry.URL = public + "&page=" + url.QueryEscape(page)
				if has {
					entry.URL += "#" + fragment
				}
				fetchURL := addExtra(api + "?page=" + url.QueryEscape(page))
				if err := p.topic(entry.URL, title, fetchURL); err != nil {
					return nil, err
				}
			} else if len(children) == 0 {
				return nil, fmt.Errorf("HPE leaf has no topicLink: %q", title)
			}
			entry.Children, err = walk(children, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, entry)
		}
		return result, nil
	}
	entries, err := walk(array, 0)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	p.plan.TOC = append(p.plan.TOC, entries...)
	if len(p.plan.Topics) < 2 {
		return model.DocumentPlan{}, errors.New("HPE TOC contains no downloadable topics")
	}
	p.notice(model.NoticeSourcePolicy, "HPE public document API is unofficial; source shapes were validated.")
	if repeatedLocale.MatchString(id) {
		p.warning("Publisher identifier has a repeated locale suffix; used verbatim: " + id)
	}
	return p.finish(api, tocURL, nil)
}

func hpeWholeDocumentPDFURL(id, extra string) string {
	raw := hpeHost + "/hpesc/public/api/document/" + id + "/exportpdf?exportType=all"
	if extra != "" {
		raw += "&" + extra
	}
	return raw
}

func hpePDFResponseIdentity(in input, id string) error {
	if err := hpeResponseIdentity(in, id, ""); err != nil {
		return err
	}
	if page := in.headers.Get("Doc-Page-Name"); page != "" {
		return fmt.Errorf("HPE whole-document PDF response unexpectedly identified topic %s", page)
	}
	return nil
}

func hpeWholeDocumentPDFScope(requested, id, extra string) func(string) error {
	expected, _ := fetch.CanonicalURL(requested)
	extraValues, _ := url.ParseQuery(extra)
	return func(raw string) error {
		normalized, err := fetch.CanonicalURL(raw)
		if err != nil {
			return err
		}
		if normalized == expected {
			return nil
		}
		if !sameHPEPDF(normalized, id) {
			return fmt.Errorf("HPE whole-document PDF redirected away from document %s: %s", id, raw)
		}
		u, _ := url.Parse(normalized)
		query := u.Query()
		if query.Get("page") != "" {
			return fmt.Errorf("HPE whole-document PDF redirected to a topic export: %s", raw)
		}
		if values, ok := query["exportType"]; ok && (len(values) != 1 || values[0] != "all") {
			return fmt.Errorf("HPE whole-document PDF changed export type: %s", raw)
		}
		for key, values := range extraValues {
			if !reflect.DeepEqual(query[key], values) {
				return fmt.Errorf("HPE whole-document PDF changed selected document query identity: %s", raw)
			}
		}
		for key := range query {
			if key == "exportType" || key == "mask" {
				continue
			}
			if _, ok := extraValues[key]; !ok {
				return fmt.Errorf("HPE whole-document PDF added document query identity: %s", raw)
			}
		}
		return nil
	}
}

func sameHPEPDF(raw, id string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "https" || u.Hostname() != "support.hpe.com" || (u.Port() != "" && u.Port() != "443") || u.User != nil {
		return false
	}
	pathID := ""
	const prefix = "/hpesc/public/api/document/"
	if strings.HasPrefix(u.Path, prefix) {
		rest := strings.TrimPrefix(u.Path, prefix)
		pathID, _, _ = strings.Cut(rest, "/")
		if pathID == "" || pathID != id {
			return false
		}
	}
	queryIDs, hasQuery := u.Query()["docId"]
	if hasQuery && (len(queryIDs) != 1 || queryIDs[0] != id) {
		return false
	}
	return pathID == id || (hasQuery && len(queryIDs) == 1)
}

func hpeAdvertisedPDF(tree *html.Node, base, id string) (string, error) {
	candidates := map[string]bool{}
	for _, node := range nodes(tree, func(n *html.Node) bool {
		return n.Data == "a" || n.Data == "iframe" || n.Data == "embed" || n.Data == "object"
	}) {
		href, _ := attr(node, "href")
		if href == "" {
			href, _ = attr(node, "src")
		}
		if href == "" {
			href, _ = attr(node, "data")
		}
		if href == "" {
			continue
		}
		raw, err := joinSource(base, href, false)
		if err != nil {
			return "", err
		}
		u, _ := url.Parse(raw)
		media, _ := attr(node, "type")
		_, download := attr(node, "download")
		if sameHPEPDF(raw, id) && (strings.HasSuffix(strings.ToLower(u.Path), ".pdf") || strings.EqualFold(media, "application/pdf") || download) {
			candidates[raw] = true
		}
	}
	if len(candidates) == 1 {
		for raw := range candidates {
			return raw, nil
		}
	}
	return "", nil
}
