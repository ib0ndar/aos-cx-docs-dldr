package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

func ProbeDocumentAvailability(
	ctx context.Context,
	fetcher model.Fetcher,
	transport model.Transport,
	document model.Document,
	probeOptionalPDF bool,
) (model.SourceAvailability, model.PDFAvailability, error) {
	sourceAvailability := model.SourceAvailability{
		URL: document.URL, ProbeURL: document.URL, Format: sourceFormat(document),
	}
	switch document.Kind {
	case "pdf":
		return probeDirectPDFAvailability(ctx, transport, document, sourceAvailability)
	case "flare", "static":
		if fetcher == nil {
			sourceAvailability.Reason = "source availability requires a raw-cache fetcher"
			return sourceAvailability, model.PDFAvailability{}, nil
		}
		p := newPlanner(ctx, fetcher, document, true)
		front, err := p.read(document.URL, "front", initialScope(document))
		if err != nil {
			sourceAvailability, err = classifySourceFailure(ctx, sourceAvailability, err)
			return sourceAvailability, model.PDFAvailability{}, err
		}
		if looksPDF(front.body) {
			sourceAvailability.Reason = "mapped HTML source returned PDF bytes without a source advertisement"
			return sourceAvailability, model.PDFAvailability{}, nil
		}
		tree, err := p.html(front, false)
		if err != nil {
			sourceAvailability, err = classifySourceFailure(ctx, sourceAvailability, err)
			return sourceAvailability, model.PDFAvailability{}, err
		}
		sourceAvailability.Checked = true
		sourceAvailability.Available = true
		sourceAvailability.FinalURL = front.url
		if !probeOptionalPDF {
			return sourceAvailability, model.PDFAvailability{}, nil
		}
		pdfAvailability, err := probeAdvertisedPDFOnFront(ctx, transport, document, tree, front.url)
		if err != nil {
			if ctx.Err() != nil {
				return model.SourceAvailability{}, model.PDFAvailability{}, ctx.Err()
			}
			pdfAvailability.Reason = err.Error()
		}
		return sourceAvailability, pdfAvailability, nil
	case "hpe":
		sourceAvailability, err := probeHPESourceAvailability(ctx, fetcher, document, sourceAvailability)
		if err != nil || !sourceAvailability.Available || !probeOptionalPDF {
			return sourceAvailability, model.PDFAvailability{}, err
		}
		pdfAvailability, pdfErr := ProbePreferredPDFAvailability(ctx, fetcher, transport, document)
		if pdfErr != nil {
			if ctx.Err() != nil {
				return model.SourceAvailability{}, model.PDFAvailability{}, ctx.Err()
			}
			pdfAvailability.Reason = pdfErr.Error()
		}
		return sourceAvailability, pdfAvailability, nil
	default:
		sourceAvailability.Reason = fmt.Sprintf("unsupported mapped source kind %q", document.Kind)
		return sourceAvailability, model.PDFAvailability{}, nil
	}
}

func probeDirectPDFAvailability(
	ctx context.Context,
	transport model.Transport,
	document model.Document,
	availability model.SourceAvailability,
) (model.SourceAvailability, model.PDFAvailability, error) {
	if transport == nil {
		availability.Reason = "source availability requires a transport"
		return availability, model.PDFAvailability{}, nil
	}
	normalized, err := fetch.NormalizeURL(document.URL)
	if err != nil {
		availability.Reason = err.Error()
		return availability, model.PDFAvailability{}, nil
	}
	availability.ProbeURL = normalized
	scope := initialScope(document)
	resource, err := transport.Download(
		fetch.WithRequestScope(ctx, scope),
		model.Request{URL: normalized, Method: http.MethodHead},
		func(model.Resource) error { return nil },
	)
	if err != nil {
		availability, err = classifySourceFailure(ctx, availability, err)
		return availability, model.PDFAvailability{}, err
	}
	if err := scope(resource.URL); err != nil {
		availability.Reason = err.Error()
		return availability, model.PDFAvailability{}, nil
	}
	availability.Checked = true
	availability.Available = true
	availability.FinalURL = resource.URL
	return availability, model.PDFAvailability{}, nil
}

func probeHPESourceAvailability(
	ctx context.Context,
	fetcher model.Fetcher,
	document model.Document,
	availability model.SourceAvailability,
) (model.SourceAvailability, error) {
	if fetcher == nil {
		availability.Reason = "source availability requires a raw-cache fetcher"
		return availability, nil
	}
	id, extra, err := hpeIdentity(document.URL)
	if err != nil {
		availability.Reason = err.Error()
		return availability, nil
	}
	frontURL := hpeHost + "/hpesc/public/api/document/" + id + "?ignorePayload=true"
	if extra != "" {
		frontURL += "&" + extra
	}
	availability.ProbeURL = frontURL
	p := newPlanner(ctx, fetcher, document, true)
	front, err := p.read(frontURL, "front", hpeScope(frontURL))
	if err != nil {
		return classifySourceFailure(ctx, availability, err)
	}
	if err := hpeResponseIdentity(front, id, ""); err != nil {
		return classifySourceFailure(ctx, availability, err)
	}
	if looksPDF(front.body) {
		availability.Checked = true
		availability.Available = true
		availability.FinalURL = front.url
		return availability, nil
	}
	if err := hpeResponseIdentity(front, id, "index.html"); err != nil {
		return classifySourceFailure(ctx, availability, err)
	}
	tree, err := p.html(front, true)
	if err != nil {
		return classifySourceFailure(ctx, availability, err)
	}
	mains := nodes(tree, func(node *html.Node) bool {
		return node.Data == "main" && hasClass(node, "ditasrc")
	})
	if len(mains) != 1 {
		pdf, pdfErr := hpeAdvertisedPDF(tree, front.url, id)
		if pdfErr != nil || pdf == "" {
			if pdfErr == nil {
				pdfErr = errors.New("HPE front matter has no unique main.ditasrc content")
			}
			return classifySourceFailure(ctx, availability, pdfErr)
		}
	}
	availability.Checked = true
	availability.Available = true
	availability.FinalURL = front.url
	return availability, nil
}

func classifySourceFailure(
	ctx context.Context,
	availability model.SourceAvailability,
	err error,
) (model.SourceAvailability, error) {
	if ctx.Err() != nil {
		return model.SourceAvailability{}, ctx.Err()
	}
	var status *fetch.StatusError
	if errors.As(err, &status) && (status.Status == http.StatusNotFound || status.Status == http.StatusGone) {
		availability.Checked = true
		availability.FinalURL = status.URL
		availability.Reason = fmt.Sprintf("mapped source HTTP %d", status.Status)
		return availability, nil
	}
	availability.Reason = err.Error()
	return availability, nil
}

func sourceFormat(document model.Document) string {
	if document.Kind == "pdf" {
		return "PDF native"
	}
	return fmt.Sprintf("HTML (%s)", document.Kind)
}
