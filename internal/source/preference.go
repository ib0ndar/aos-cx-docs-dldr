package source

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

var (
	pdfAdvertisementLabel = regexp.MustCompile(`(?i)\b(download|original|pdf)\b`)
	explicitPDFLabel      = regexp.MustCompile(`(?i)\bpdf\b`)
)

func PreferredPDFProbeURL(document model.Document) (string, string, bool, error) {
	switch document.Kind {
	case "pdf":
		normalized, err := fetch.NormalizeURL(document.URL)
		return normalized, PDFOriginDirectMapped, true, err
	case "hpe":
		id, extra, err := hpeIdentity(document.URL)
		if err != nil {
			return "", "", false, err
		}
		return hpeWholeDocumentPDFURL(id, extra), PDFOriginHPEExportAll, true, nil
	default:
		return "", "", false, nil
	}
}

func ProbePreferredPDFAvailability(
	ctx context.Context,
	fetcher model.Fetcher,
	transport model.Transport,
	document model.Document,
) (model.PDFAvailability, error) {
	raw, origin, candidate, err := PreferredPDFProbeURL(document)
	if err != nil {
		return model.PDFAvailability{}, err
	}
	if document.Kind == "pdf" {
		return model.PDFAvailability{
			URL: raw, Origin: origin,
			Reason: "direct PDF-only mapping; bytes are verified on download",
		}, nil
	}
	if transport == nil {
		return model.PDFAvailability{}, errors.New("PDF availability requires a transport")
	}
	if document.Kind == "flare" || document.Kind == "static" {
		return probeAdvertisedPDFAvailability(ctx, fetcher, transport, document)
	}
	if !candidate {
		return model.PDFAvailability{Checked: true, Reason: "source kind has no optional native PDF"}, nil
	}
	id, extra, err := hpeIdentity(document.URL)
	if err != nil {
		return model.PDFAvailability{}, err
	}
	scope := hpeWholeDocumentPDFScope(raw, id, extra)
	requestCtx := fetch.WithRequestScope(ctx, scope)
	resource, err := transport.Download(requestCtx, model.Request{URL: raw, Method: http.MethodHead}, func(model.Resource) error {
		return nil
	})
	if err != nil {
		var status *fetch.StatusError
		if errors.As(err, &status) && (status.Status == http.StatusNotFound || status.Status == http.StatusGone) {
			return model.PDFAvailability{
				URL: raw, Origin: origin, Checked: true,
				Reason: fmt.Sprintf("publisher returned HTTP %d", status.Status),
			}, nil
		}
		return model.PDFAvailability{}, err
	}
	if err := scope(resource.URL); err != nil {
		return model.PDFAvailability{}, err
	}
	headers := input{url: resource.URL, headers: resource.Headers}
	if err := hpePDFResponseIdentity(headers, id); err != nil {
		return model.PDFAvailability{}, err
	}
	media, _, err := mimeParse(resource.Headers.Get("Content-Type"))
	if err != nil {
		return model.PDFAvailability{}, err
	}
	return model.PDFAvailability{
		URL: raw, FinalURL: resource.URL, Origin: origin, Checked: true,
		Available: strings.EqualFold(media, "application/pdf"),
		Reason:    nonPDFReason(media),
	}, nil
}

func probeAdvertisedPDFAvailability(
	ctx context.Context,
	fetcher model.Fetcher,
	transport model.Transport,
	document model.Document,
) (model.PDFAvailability, error) {
	if fetcher == nil {
		return model.PDFAvailability{}, errors.New("source-backed PDF availability requires a raw-cache fetcher")
	}
	p := newPlanner(ctx, fetcher, document, true)
	front, err := p.read(document.URL, "front", initialScope(document))
	if err != nil {
		return model.PDFAvailability{}, err
	}
	if looksPDF(front.body) {
		return model.PDFAvailability{
			Origin: PDFOriginSourceAdvertised, Checked: true,
			Reason: "mapped HTML source returned PDF bytes without a source advertisement",
		}, nil
	}
	tree, err := p.html(front, false)
	if err != nil {
		return model.PDFAvailability{}, err
	}
	return probeAdvertisedPDFOnFront(ctx, transport, document, tree, front.url)
}

func probeAdvertisedPDFOnFront(
	ctx context.Context,
	transport model.Transport,
	document model.Document,
	tree *html.Node,
	frontURL string,
) (model.PDFAvailability, error) {
	raw, count, err := advertisedOriginalPDF(tree, frontURL)
	if err != nil {
		return model.PDFAvailability{
			Origin: PDFOriginSourceAdvertised, Checked: true, Reason: err.Error(),
		}, nil
	}
	if count != 1 {
		reason := "publisher does not advertise an original whole-document PDF"
		if count > 1 {
			reason = "publisher advertises multiple possible PDFs"
		}
		return model.PDFAvailability{
			Origin: PDFOriginSourceAdvertised, Checked: true, Reason: reason,
		}, nil
	}
	scope := sourceAdvertisedPDFScope(frontURL, raw)
	requestCtx := fetch.WithRequestScope(ctx, scope)
	resource, err := transport.Download(requestCtx, model.Request{URL: raw, Method: http.MethodHead}, func(model.Resource) error {
		return nil
	})
	if err != nil {
		var status *fetch.StatusError
		if errors.As(err, &status) && (status.Status == http.StatusNotFound || status.Status == http.StatusGone) {
			return model.PDFAvailability{
				URL: raw, Origin: PDFOriginSourceAdvertised, Checked: true,
				Reason: fmt.Sprintf("publisher returned HTTP %d", status.Status),
			}, nil
		}
		return model.PDFAvailability{}, err
	}
	if err := scope(resource.URL); err != nil {
		return model.PDFAvailability{}, err
	}
	media, _, err := mimeParse(resource.Headers.Get("Content-Type"))
	if err != nil {
		return model.PDFAvailability{}, err
	}
	return model.PDFAvailability{
		URL: raw, FinalURL: resource.URL, Origin: PDFOriginSourceAdvertised, Checked: true,
		Available: strings.EqualFold(media, "application/pdf"),
		Reason:    nonPDFReason(media),
	}, nil
}

func nonPDFReason(media string) string {
	if strings.EqualFold(media, "application/pdf") {
		return ""
	}
	if media == "" {
		return "publisher HEAD response did not identify PDF content"
	}
	return "publisher HEAD response identified " + media + ", not application/pdf"
}

func mimeParse(value string) (string, map[string]string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil, nil
	}
	return mime.ParseMediaType(value)
}

func advertisedOriginalPDF(tree *html.Node, base string) (string, int, error) {
	candidates := []string{}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", 0, err
	}
	for _, node := range nodes(tree, func(n *html.Node) bool {
		switch n.Data {
		case "a", "link":
			_, found := attr(n, "href")
			return found
		case "iframe", "embed":
			_, found := attr(n, "src")
			return found
		case "object":
			_, found := attr(n, "data")
			return found
		default:
			return false
		}
	}) {
		reference, _ := attr(node, "href")
		if reference == "" {
			reference, _ = attr(node, "src")
		}
		if reference == "" {
			reference, _ = attr(node, "data")
		}
		raw, err := joinSource(base, reference, false)
		if err != nil {
			return "", 0, err
		}
		u, _ := url.Parse(raw)
		if u.Scheme != baseURL.Scheme || u.Host != baseURL.Host {
			continue
		}
		media, _ := attr(node, "type")
		mediaType, _, _ := mimeParse(media)
		downloadName, download := attr(node, "download")
		rel, _ := attr(node, "rel")
		pathPDF := strings.HasSuffix(strings.ToLower(u.Path), ".pdf")
		labelPDF := pdfAdvertisementLabel.MatchString(text(node))
		advertised := strings.EqualFold(mediaType, "application/pdf") ||
			(download && (pathPDF || strings.HasSuffix(strings.ToLower(downloadName), ".pdf") ||
				explicitPDFLabel.MatchString(text(node)))) ||
			(pathPDF &&
				(node.Data == "embed" || node.Data == "iframe" || node.Data == "object" ||
					slices.Contains(strings.Fields(strings.ToLower(rel)), "alternate") ||
					labelPDF))
		if advertised && !slices.Contains(candidates, raw) {
			candidates = append(candidates, raw)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], 1, nil
	}
	return "", len(candidates), nil
}

func sourceAdvertisedPDFScope(base, advertised string) func(string) error {
	baseURL, _ := url.Parse(base)
	requested, _ := fetch.CanonicalURL(advertised)
	return func(raw string) error {
		normalized, err := fetch.CanonicalURL(raw)
		if err != nil {
			return err
		}
		u, _ := url.Parse(normalized)
		if u.Scheme != baseURL.Scheme || u.Host != baseURL.Host {
			return fmt.Errorf("source-advertised PDF redirected outside the publisher origin: %s", raw)
		}
		if normalized != requested && !strings.HasSuffix(strings.ToLower(u.Path), ".pdf") {
			return fmt.Errorf("source-advertised PDF redirect no longer identifies an original PDF: %s", raw)
		}
		return nil
	}
}

func preferredPDFFallbackWarning(reason string) string {
	return "Preferred source-verified PDF was unavailable (" + reason + "); archived complete HTML instead."
}
