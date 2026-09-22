package source

import (
	"bytes"
	"fmt"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/pdfcheck"
)

const (
	PDFOriginDirectMapped     = model.PDFOriginDirectMapped
	PDFOriginSelectedRoute    = model.PDFOriginSelectedRoute
	PDFOriginSourceAdvertised = model.PDFOriginSourceAdvertised
	PDFOriginHPEExportAll     = model.PDFOriginHPEExportAll
)

func PublisherPDFPlan(document model.Document) (model.DocumentPlan, error) {
	if document.Kind != "pdf" {
		return model.DocumentPlan{}, fmt.Errorf(
			"guide %q cannot use mapped-PDF planning with source kind %q",
			document.ID,
			document.Kind,
		)
	}

	if _, err := fetch.NormalizeURL(document.URL); err != nil {
		return model.DocumentPlan{}, err
	}
	return model.DocumentPlan{
		Document:  document,
		PDFURL:    document.URL,
		PDFOrigin: PDFOriginDirectMapped,
	}, nil
}

func looksPDF(body []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(body, "\xef\xbb\xbf\r\n\t "), []byte("%PDF-"))
}

func (p *planner) verifiedPDF(in input, origin string, preferred bool) (model.DocumentPlan, error) {
	if err := pdfcheck.Validate(bytes.NewReader(in.body), int64(len(in.body)), in.headers.Get("Content-Type")); err != nil {
		return model.DocumentPlan{}, fmt.Errorf("expected complete original PDF bytes at %s: %w", in.url, err)
	}
	p.plan.Document.Kind = "pdf"
	p.plan.PDFURL = in.url
	p.plan.PDFVerified = true
	p.plan.PDFOrigin = origin
	p.plan.PDFPreferred = preferred
	return p.finish(in.url, "", nil)
}
