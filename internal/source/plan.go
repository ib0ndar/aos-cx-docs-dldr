package source

import (
	"context"
	"errors"
	"fmt"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
)

// LoadPlan inventories a whole supported guide. It does not retrieve planned
// topic/asset bodies or publish an HTML archive.
func LoadPlan(ctx context.Context, fetcher model.Fetcher, document model.Document) (model.DocumentPlan, error) {
	return LoadPlanWithRefresh(ctx, fetcher, document, false)
}

// LoadPlanWithRefresh inventories a whole supported guide and optionally
// revalidates every source input through the shared cache/transport policy.
func LoadPlanWithRefresh(ctx context.Context, fetcher model.Fetcher, document model.Document, refresh bool) (model.DocumentPlan, error) {
	return LoadPlanWithPreference(ctx, fetcher, document, refresh, false)
}

// LoadPlanWithPreference inventories a whole supported guide while preferring
// only source-verified whole-document PDF routes. Preference failures fall back
// to complete HTML planning with a durable warning.
func LoadPlanWithPreference(ctx context.Context, fetcher model.Fetcher, document model.Document, refresh, preferPDF bool) (model.DocumentPlan, error) {
	normalized, err := fetch.NormalizeURL(document.URL)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	document.URL = normalized
	p := newPlanner(ctx, fetcher, document, refresh)
	if document.Kind == "hpe" {
		return p.hpe(preferPDF)
	}
	if document.Kind != "pdf" && document.Kind != "flare" && document.Kind != "static" {
		return model.DocumentPlan{}, fmt.Errorf("unsupported document kind %q", document.Kind)
	}
	in, err := p.read(document.URL, "front", initialScope(document))
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if document.Kind == "pdf" || looksPDF(in.body) {
		origin := PDFOriginSelectedRoute
		if document.Kind == "pdf" {
			origin = PDFOriginDirectMapped
		}
		return p.verifiedPDF(in, origin, false)
	}
	if preferPDF {
		tree, parseErr := p.html(in, false)
		if parseErr != nil {
			return model.DocumentPlan{}, parseErr
		}
		pdfURL, count, advertisedErr := advertisedOriginalPDF(tree, in.url)
		if advertisedErr != nil {
			p.warning(preferredPDFFallbackWarning(advertisedErr.Error()))
		} else {
			switch count {
			case 0:
				p.warning(preferredPDFFallbackWarning("the publisher does not advertise an original whole-document PDF"))
			case 1:
				pdf, pdfErr := p.read(pdfURL, "pdf", sourceAdvertisedPDFScope(in.url, pdfURL))
				if pdfErr == nil {
					plan, verifyErr := p.verifiedPDF(pdf, PDFOriginSourceAdvertised, true)
					if verifyErr == nil {
						return plan, nil
					}
					pdfErr = verifyErr
				}
				if errors.Is(pdfErr, context.Canceled) || errors.Is(pdfErr, context.DeadlineExceeded) {
					return model.DocumentPlan{}, pdfErr
				}
				p.warning(preferredPDFFallbackWarning(pdfErr.Error()))
			default:
				p.warning(preferredPDFFallbackWarning("the publisher advertises multiple possible PDFs"))
			}
		}
	}
	if document.Kind == "flare" {
		return p.flare(in)
	}
	return p.static(in, p.flare)
}
