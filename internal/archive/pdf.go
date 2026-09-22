package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/pdfcheck"
	"aos-cx-docs-dldr/internal/storage"
)

// ValidatePublisherPDF checks bounded structural byte markers, not page text
// or rendering. It never repairs/re-serializes the original publisher file.
func ValidatePublisherPDF(file *os.File, contentType string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return pdfcheck.Validate(file, info.Size(), contentType)
}

// VerifiedPublisherPDFInput returns the exact source input that established a
// resolved PDF URL, including a structurally valid SHA-256 and byte count.
func VerifiedPublisherPDFInput(pdfURL string, inputs []model.SourceInput) (*model.SourceInput, error) {
	for i := range inputs {
		in := &inputs[i]
		digest, err := hex.DecodeString(in.SHA256)
		if in.FinalURL == pdfURL && in.Size >= 32 && err == nil && len(digest) == sha256.Size {
			if _, err := fetch.NormalizeURL(in.RequestedURL); err != nil {
				return nil, fmt.Errorf("invalid publisher PDF source request URL: %w", err)
			}
			if _, err := fetch.NormalizeURL(in.FinalURL); err != nil {
				return nil, fmt.Errorf("invalid publisher PDF source final URL: %w", err)
			}
			return in, nil
		}
	}
	return nil, errors.New("resolved publisher PDF plan lacks exact source input hash/size evidence")
}

func validatePublisherPDFPlan(plan model.DocumentPlan, requireVerified bool) (*model.SourceInput, error) {
	if plan.Document.Kind != "pdf" {
		return nil, fmt.Errorf("publisher PDF plan has resolved document kind %q", plan.Document.Kind)
	}
	if _, err := fetch.NormalizeURL(plan.Document.URL); err != nil {
		return nil, fmt.Errorf("invalid selected document URL: %w", err)
	}
	if _, err := fetch.NormalizeURL(plan.PDFURL); err != nil {
		return nil, fmt.Errorf("invalid publisher PDF URL: %w", err)
	}
	if requireVerified {
		if plan.Inventory == nil || !plan.Inventory.Complete || plan.Inventory.Kind != "pdf" ||
			plan.Inventory.RootURL != plan.PDFURL {
			return nil, errors.New("resolved publisher PDF plan lacks complete PDF inventory evidence")
		}
		if !plan.PDFVerified {
			return nil, errors.New("resolved publisher PDF plan is not source-verified")
		}
		if plan.PDFOrigin != "" && plan.PDFOrigin != model.PDFOriginSelectedRoute &&
			plan.PDFOrigin != model.PDFOriginSourceAdvertised &&
			plan.PDFOrigin != model.PDFOriginHPEExportAll {
			return nil, fmt.Errorf("resolved publisher PDF plan has invalid origin %q", plan.PDFOrigin)
		}
		if plan.PDFPreferred && plan.PDFOrigin != model.PDFOriginSourceAdvertised &&
			plan.PDFOrigin != model.PDFOriginHPEExportAll {
			return nil, errors.New("preferred publisher PDF plan lacks a preference-eligible source origin")
		}
		return VerifiedPublisherPDFInput(plan.PDFURL, plan.Inputs)
	}
	if plan.PDFVerified {
		return VerifiedPublisherPDFInput(plan.PDFURL, plan.Inputs)
	}
	if plan.PDFURL != plan.Document.URL {
		return nil, errors.New("direct publisher PDF plan must retain the exact mapped document URL")
	}
	if plan.PDFOrigin != "" && plan.PDFOrigin != model.PDFOriginDirectMapped {
		return nil, fmt.Errorf("direct publisher PDF plan has invalid origin %q", plan.PDFOrigin)
	}
	if plan.PDFPreferred {
		return nil, errors.New("direct mapped publisher PDF cannot be marked as preferred")
	}
	return nil, nil
}

func publisherPDFOrigin(plan model.DocumentPlan) string {
	if plan.PDFOrigin != "" {
		return plan.PDFOrigin
	}
	if plan.PDFVerified {
		return model.PDFOriginSelectedRoute
	}
	return model.PDFOriginDirectMapped
}

// ValidateResolvedPublisherPDFPlan requires the complete source evidence needed
// before an initially non-PDF catalogue route may be published as a PDF.
func ValidateResolvedPublisherPDFPlan(plan model.DocumentPlan) error {
	_, err := validatePublisherPDFPlan(plan, true)
	return err
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

func PublisherPDF(ctx context.Context, plan model.DocumentPlan, fetcher model.Fetcher, output string, refresh bool, maxBytes int64) (result model.ArchiveResult, err error) {
	result = model.ArchiveResult{Document: plan.Document, OutputDir: output, Status: "failed", Format: "pdf",
		Errors: []string{}, Notices: append([]model.Notice{}, plan.Notices...),
		Warnings: append([]string{}, plan.Warnings...)}
	defer func() {
		if err != nil {
			result.Status = "failed"
			if result.PDF != nil {
				result.PDF.Status = "failed"
			}
			result.Errors = append(result.Errors, err.Error())
		}
	}()
	evidence, err := validatePublisherPDFPlan(plan, plan.PDFVerified)
	if err != nil {
		return result, err
	}
	if maxBytes <= 0 {
		return result, errors.New("invalid PDF byte limit")
	}
	if info, err := os.Lstat(output); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("PDF output must be an owned real staging directory")
	}
	root, err := os.OpenRoot(output)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	defer func() {
		if err != nil {
			removeErr := root.Remove(".download.part")
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove failed PDF staging file: %w", removeErr))
			}
		}
	}()
	name, err := storage.PDFFilename(plan.Document.Platform, plan.Document.Version, plan.Document.Title)
	if err != nil {
		return result, err
	}
	resource, err := fetcher.Get(ctx, plan.PDFURL, refresh)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, resource.Body.Close()) }()
	if resource.Status != 200 {
		return result, fmt.Errorf("publisher PDF returned HTTP %d", resource.Status)
	}
	file, err := root.OpenFile(".download.part", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return result, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, h), io.LimitReader(contextReader{ctx, resource.Body}, maxBytes+1))
	if copyErr == nil && n > maxBytes {
		copyErr = errors.New("PDF exceeds configured byte limit")
	}
	if copyErr == nil {
		copyErr = ctx.Err()
	}
	if copyErr == nil {
		copyErr = ValidatePublisherPDF(file, resource.Headers.Get("Content-Type"))
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if copyErr == nil && evidence != nil {
		if resource.URL != evidence.FinalURL {
			copyErr = errors.New("publisher PDF final URL changed after source verification")
		} else if n != int64(evidence.Size) || digest != evidence.SHA256 {
			copyErr = errors.New("publisher PDF bytes changed after source verification")
		}
	}
	syncErr, closeErr := file.Sync(), file.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := root.Rename(".download.part", name); err != nil {
		return result, err
	}
	if err := storage.SyncDir(root, "."); err != nil {
		return result, err
	}
	result.PDF = &model.PDFRecord{URL: plan.PDFURL, FinalURL: resource.URL, Path: name,
		Status: "complete", MIME: strings.TrimSpace(resource.Headers.Get("Content-Type")), Size: n,
		SourceSHA256: digest, SHA256: digest, RetrievedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Provenance:     "unmodified publisher PDF (HTTP transfer decoding only)",
		Origin:         publisherPDFOrigin(plan),
		SourceVerified: plan.PDFVerified, Inputs: append([]model.SourceInput{}, plan.Inputs...)}
	result.Status = "complete"
	return result, nil
}
