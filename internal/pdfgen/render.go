package pdfgen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/pdfcheck"
	"aos-cx-docs-dldr/internal/publication"
	"aos-cx-docs-dldr/internal/storage"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	cdpfetch "github.com/chromedp/cdproto/fetch"
	cdpio "github.com/chromedp/cdproto/io"
	cdpnetwork "github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const (
	DefaultTimeout     = 15 * time.Minute
	DefaultMaxPDFBytes = int64(2 << 30)
	DefaultMaxPages    = 20_000
	DefaultMaxRSSBytes = int64(14 << 30)
	maxChromeLogBytes  = 1 << 20
)

var renderSlot = make(chan struct{}, 1)

type Config struct {
	ChromePath      string
	QPDFPath        string
	Timeout         time.Duration
	MaxPDFBytes     int64
	MaxPages        int
	MaxRSSBytes     int64
	MaxQPDFRSSBytes int64
	TempRoot        string
}

type browserAudit struct {
	FontsStatus          string   `json:"fonts_status"`
	ImageCount           int      `json:"image_count"`
	RasterImages         int      `json:"raster_images"`
	ImagePlaceholders    int      `json:"image_placeholders"`
	PlaceholderOverflows int      `json:"image_placeholder_overflows"`
	FailedImages         []string `json:"failed_images"`
	ScriptElements       int      `json:"script_elements"`
	ScriptProbe          bool     `json:"script_probe"`
	InternalLinks        int      `json:"internal_links"`
	MissingTargets       []string `json:"missing_targets"`
	CoverTitles          int      `json:"cover_titles"`
	CoverTypography      bool     `json:"cover_typography"`
	HeadingTypography    bool     `json:"heading_typography"`
	HeadingPagination    bool     `json:"heading_pagination"`
	FooterOverlay        bool     `json:"footer_overlay"`
	PublisherChrome      int      `json:"publisher_chrome"`
	FixedOrSticky        []string `json:"fixed_or_sticky"`
	LocalRequests        int      `json:"-"`
	DataRequests         int      `json:"-"`
	External             []string `json:"-"`
	RequestFailures      []string `json:"-"`
}

type renderMetrics struct {
	Version string
	Audit   browserAudit
	PeakRSS int64
}

func DefaultConfig(chromePath string) Config {
	return Config{
		ChromePath: chromePath, Timeout: DefaultTimeout, MaxPDFBytes: DefaultMaxPDFBytes,
		MaxPages: DefaultMaxPages, MaxRSSBytes: DefaultMaxRSSBytes,
		MaxQPDFRSSBytes: QPDFOutlineProcessMaxRSSBytes,
	}
}

func FailedRecord(plan model.DocumentPlan, err error) model.GeneratedPDFRecord {
	name, nameErr := storage.PDFFilename(plan.Document.Platform, plan.Document.Version, plan.Document.Title)
	if nameErr != nil {
		name = "generated.pdf"
	}
	message := "generated PDF conversion failed"
	if err != nil {
		message += ": " + err.Error()
	}
	return model.GeneratedPDFRecord{
		Path: name, Status: "failed", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Error: message, Renderer: RendererName,
	}
}

func Generate(
	ctx context.Context,
	guideDir string,
	plan model.DocumentPlan,
	result *model.ArchiveResult,
	config Config,
) (model.GeneratedPDFRecord, error) {
	return GenerateInRoot(ctx, nil, guideDir, plan, result, config)
}

// GenerateInRoot is Generate for a guide directory the caller already holds
// open as guide. Its own reads and writes go through guide; Chrome and qpdf
// still need guideDir as a path, which must name the same directory. A nil
// guide opens guideDir by path instead.
func GenerateInRoot(
	ctx context.Context,
	guide *os.Root,
	guideDir string,
	plan model.DocumentPlan,
	result *model.ArchiveResult,
	config Config,
) (record model.GeneratedPDFRecord, err error) {
	started := time.Now()
	record = FailedRecord(plan, nil)
	defer func() {
		if err != nil {
			record.Status = "failed"
			record.Error = "generated PDF conversion failed: " + err.Error()
			record.DurationMilliseconds = time.Since(started).Milliseconds()
		}
	}()
	if ctx == nil {
		return record, errors.New("generated PDF rendering requires a context")
	}
	if result == nil || (result.Status != "complete" && result.Status != "degraded") ||
		result.Format != "html" || result.HTML == nil || result.HTML.Status != result.Status ||
		len(result.Errors) != 0 ||
		(result.Status == "complete" && (len(result.MissingResources) != 0 || len(result.UnpublishedTopics) != 0)) ||
		(result.Status == "degraded" && len(result.MissingResources) == 0 && len(result.UnpublishedTopics) == 0) ||
		!reflect.DeepEqual(result.MissingResources, result.HTML.MissingResources) ||
		!reflect.DeepEqual(result.UnpublishedTopics, result.HTML.UnpublishedTopics) {
		return record, errors.New("generated PDF rendering requires a publishable HTML archive")
	}
	record.InputStatus = result.Status
	record.ImagePlaceholders = model.ImagePlaceholderCount(result.MissingResources)
	if result.Document != plan.Document {
		return record, errors.New("generated PDF plan does not match the HTML archive")
	}
	config = normalizedConfig(config)
	generationCtx, generationCancel := context.WithTimeout(ctx, config.Timeout)
	defer generationCancel()
	sidecar, err := ResolveSidecar(config.ChromePath)
	if err != nil {
		return record, err
	}
	qpdfSidecar, err := ResolveQPDFSidecar(config.QPDFPath)
	if err != nil {
		return record, err
	}
	if err := generationCtx.Err(); err != nil {
		return record, err
	}
	record.RendererVersion = sidecar.Version
	record.RendererRevision = sidecar.Revision
	record.ExecutableSHA256 = sidecar.ExecutableSHA256
	record.Postprocessor = QPDFPostprocessorName
	record.PostprocessorVersion = qpdfSidecar.Version
	record.PostprocessorExecutableSHA256 = qpdfSidecar.ExecutableSHA256
	record.PostprocessorBundleSHA256 = qpdfSidecar.BundleSHA256
	settings := ExpectedSettings()
	record.Settings = &settings
	select {
	case renderSlot <- struct{}{}:
		defer func() { <-renderSlot }()
	case <-generationCtx.Done():
		return record, generationCtx.Err()
	}
	root := guide
	if root == nil {
		info, err := os.Lstat(guideDir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return record, errors.New("generated PDF guide root must be an owned real staging directory")
		}
	}
	absoluteGuide, err := filepath.Abs(guideDir)
	if err != nil {
		return record, err
	}
	if root == nil {
		if root, err = os.OpenRoot(absoluteGuide); err != nil {
			return record, err
		}
		defer func() { err = errors.Join(err, root.Close()) }()
	} else if opened, rootErr := root.Stat("."); rootErr != nil {
		return record, rootErr
	} else if named, pathErr := os.Stat(absoluteGuide); pathErr != nil || !os.SameFile(opened, named) {
		return record, errors.Join(errors.New("generated PDF guide path no longer names the opened staging directory"), pathErr)
	}
	if _, existingErr := root.Lstat(record.Path); existingErr == nil {
		return record, fmt.Errorf("generated PDF destination already exists: %s", record.Path)
	} else if !errors.Is(existingErr, os.ErrNotExist) {
		return record, existingErr
	}

	input, err := os.CreateTemp(absoluteGuide, ".aos-cx-docs-dldr-print-*.html")
	if err != nil {
		return record, err
	}
	defer os.Remove(input.Name())
	assembled, assembleErr := publication.Assemble(generationCtx, root, ".", plan, *result, input)
	closeErr := errors.Join(input.Sync(), input.Close())
	if err := errors.Join(assembleErr, closeErr); err != nil {
		return record, fmt.Errorf("assemble transient print HTML: %w", err)
	}
	record.SourceManifestSHA256 = assembled.SourceManifestSHA256
	record.InputSHA256 = assembled.SHA256
	if err := generationCtx.Err(); err != nil {
		return record, err
	}

	rawPartial, err := os.CreateTemp(absoluteGuide, ".aos-cx-docs-dldr-generated-raw-*.pdf.part")
	if err != nil {
		return record, err
	}
	rawPartialPath := rawPartial.Name()
	defer os.Remove(rawPartialPath)
	metrics, renderErr := renderChrome(
		generationCtx, sidecar, input.Name(), absoluteGuide, plan.Document.Title, rawPartial, config,
	)
	record.Validation = &model.GeneratedPDFValidation{
		LocalRequests: metrics.Audit.LocalRequests, InlineDataRequests: metrics.Audit.DataRequests,
		ScriptElements: metrics.Audit.ScriptElements, LoadedImages: metrics.Audit.ImageCount,
		ImagePlaceholders:         metrics.Audit.ImagePlaceholders,
		ImagePlaceholderOverflows: metrics.Audit.PlaceholderOverflows,
		InternalLinksChecked:      metrics.Audit.InternalLinks, FailedImages: len(metrics.Audit.FailedImages),
		FailedImageDetails: append([]string(nil), metrics.Audit.FailedImages...),
		MissingTargets:     len(metrics.Audit.MissingTargets), ExternalRequests: len(metrics.Audit.External),
		CanonicalCoverTitles: metrics.Audit.CoverTitles, PublisherChromeElements: metrics.Audit.PublisherChrome,
		FixedOrStickyElements:   len(metrics.Audit.FixedOrSticky),
		FontsLoaded:             metrics.Audit.FontsStatus == "loaded",
		ScriptExecutionDisabled: !metrics.Audit.ScriptProbe,
		ResourceClosureValidated: len(metrics.Audit.External) == 0 &&
			len(metrics.Audit.RequestFailures) == 0,
		CoverTypographyValidated:   metrics.Audit.CoverTypography,
		HeadingTypographyValidated: metrics.Audit.HeadingTypography,
		HeadingPaginationValidated: metrics.Audit.HeadingPagination,
	}
	syncErr := rawPartial.Sync()
	rawStat, statErr := rawPartial.Stat()
	if err := errors.Join(renderErr, syncErr, statErr); err != nil {
		_ = rawPartial.Close()
		return record, err
	}
	expectedPlaceholders := model.ImagePlaceholderCount(result.MissingResources)
	if metrics.Audit.ImagePlaceholders != expectedPlaceholders {
		_ = rawPartial.Close()
		return record, fmt.Errorf(
			"generated PDF input has %d image placeholders, expected %d",
			metrics.Audit.ImagePlaceholders, expectedPlaceholders,
		)
	}
	if metrics.Audit.PlaceholderOverflows != 0 {
		_ = rawPartial.Close()
		return record, fmt.Errorf(
			"generated PDF input has %d overflowing image placeholders",
			metrics.Audit.PlaceholderOverflows,
		)
	}
	if rawStat.Size() < 32 || rawStat.Size() > config.MaxPDFBytes {
		_ = rawPartial.Close()
		return record, fmt.Errorf("raw generated PDF size %d is outside the allowed range", rawStat.Size())
	}
	rawValidation, validateErr := pdfcheck.InspectGenerated(rawPartial, rawStat.Size(), pdfcheck.GeneratedExpectations{
		MaxPages: config.MaxPages, RequireAnnotations: assembled.TOCEntries > 0,
		RequireImage: metrics.Audit.RasterImages > 0,
	})
	closeErr = rawPartial.Close()
	if err := errors.Join(validateErr, closeErr); err != nil {
		return record, fmt.Errorf("validate raw generated PDF: %w", err)
	}
	if err := generationCtx.Err(); err != nil {
		return record, err
	}
	if metrics.Version != "HeadlessChrome/"+ChromeVersion {
		return record, fmt.Errorf("Chrome runtime reported %q, expected HeadlessChrome/%s", metrics.Version, ChromeVersion)
	}
	footerInspectionRoot, err := os.MkdirTemp(config.TempRoot, "aos-cx-docs-dldr-footer-plan-")
	if err != nil {
		return record, err
	}
	defer os.RemoveAll(footerInspectionRoot)
	rawDocument, footerInspectPeak, err := inspectWithQPDF(
		generationCtx, qpdfSidecar, rawPartialPath, footerInspectionRoot, "raw", config.MaxQPDFRSSBytes,
	)
	if err != nil {
		return record, fmt.Errorf("inspect raw generated PDF for footer planning: %w", err)
	}
	footer, footerPlanHash, err := buildFooterPlan(rawDocument, assembled.Footer)
	if err != nil {
		return record, err
	}
	record.FooterPlanSHA256 = footerPlanHash
	record.FooterEntries = len(footer.Entries)
	record.FooterOverlayRenderer = RendererName
	record.FooterOverlayRendererVersion = sidecar.Version
	record.FooterOverlayExecutableSHA256 = sidecar.ExecutableSHA256

	footerInput, err := os.CreateTemp(absoluteGuide, ".aos-cx-docs-dldr-footer-*.html")
	if err != nil {
		return record, err
	}
	footerInputPath := footerInput.Name()
	if err := footerInput.Close(); err != nil {
		return record, err
	}
	if err := os.Remove(footerInputPath); err != nil {
		return record, err
	}
	defer os.Remove(footerInputPath)
	record.FooterOverlayInputSHA256, err = writeFooterOverlayHTML(footerInputPath, footer)
	if err != nil {
		return record, err
	}
	footerPDF, err := os.CreateTemp(absoluteGuide, ".aos-cx-docs-dldr-footer-*.pdf.part")
	if err != nil {
		return record, err
	}
	footerPDFPath := footerPDF.Name()
	defer os.Remove(footerPDFPath)
	footerStarted := time.Now()
	footerRender, footerRenderErr := renderChromeDocument(
		generationCtx, sidecar, footerInputPath, absoluteGuide, "", footerPDF, config, &footer,
	)
	footerSyncErr := footerPDF.Sync()
	footerStat, footerStatErr := footerPDF.Stat()
	if err := errors.Join(footerRenderErr, footerSyncErr, footerStatErr); err != nil {
		_ = footerPDF.Close()
		return record, fmt.Errorf("render generated PDF footer overlay: %w", err)
	}
	if footerStat.Size() < 32 || footerStat.Size() > config.MaxPDFBytes {
		_ = footerPDF.Close()
		return record, fmt.Errorf("footer overlay PDF size %d is outside the allowed range", footerStat.Size())
	}
	footerSummary, footerValidateErr := pdfcheck.InspectGenerated(
		footerPDF, footerStat.Size(), pdfcheck.GeneratedExpectations{MaxPages: len(footer.Entries)},
	)
	footerCloseErr := footerPDF.Close()
	if err := errors.Join(footerValidateErr, footerCloseErr); err != nil {
		return record, fmt.Errorf("validate generated PDF footer overlay: %w", err)
	}
	if footerSummary.PageCount != len(footer.Entries) || !footerRender.Audit.FooterOverlay ||
		footerRender.Audit.ScriptProbe || len(footerRender.Audit.External) != 0 {
		return record, errors.New("generated PDF footer overlay failed page, layout, or isolation validation")
	}
	overlaidPartial, err := os.CreateTemp(absoluteGuide, ".aos-cx-docs-dldr-generated-footer-*.pdf.part")
	if err != nil {
		return record, err
	}
	overlaidPartialPath := overlaidPartial.Name()
	if err := overlaidPartial.Close(); err != nil {
		return record, err
	}
	if err := os.Remove(overlaidPartialPath); err != nil {
		return record, err
	}
	defer os.Remove(overlaidPartialPath)
	footerMergePeak, footerMergeDuration, err := overlayFooterPDF(
		generationCtx, qpdfSidecar, rawPartialPath, footerPDFPath, overlaidPartialPath,
		config.MaxPDFBytes, config.MaxQPDFRSSBytes, config.TempRoot,
	)
	if err != nil {
		return record, err
	}
	if footerMergeDuration <= 0 {
		return record, errors.New("generated PDF footer merge duration was not recorded")
	}
	record.FooterOverlayDurationMilliseconds = time.Since(footerStarted).Milliseconds()
	record.FooterOverlayPeakRSSBytes = max(footerRender.PeakRSS, footerMergePeak, footerInspectPeak)
	overlaidPDF, err := os.Open(overlaidPartialPath)
	if err != nil {
		return record, err
	}
	overlaidStat, overlaidStatErr := overlaidPDF.Stat()
	if overlaidStatErr != nil {
		_ = overlaidPDF.Close()
		return record, overlaidStatErr
	}
	overlaidSummary, overlaidValidateErr := pdfcheck.InspectGenerated(
		overlaidPDF, overlaidStat.Size(), pdfcheck.GeneratedExpectations{
			MaxPages: config.MaxPages, RequireAnnotations: assembled.TOCEntries > 0,
			RequireImage: metrics.Audit.RasterImages > 0,
		},
	)
	overlaidCloseErr := overlaidPDF.Close()
	if err := errors.Join(overlaidValidateErr, overlaidCloseErr); err != nil {
		return record, fmt.Errorf("validate footer-overlaid generated PDF: %w", err)
	}
	if overlaidSummary.PageCount != rawValidation.PageCount ||
		overlaidSummary.LetterMediaBoxes != rawValidation.LetterMediaBoxes {
		return record, errors.New("footer overlay changed generated PDF page geometry")
	}
	postPartial, err := os.CreateTemp(absoluteGuide, ".aos-cx-docs-dldr-generated-outlined-*.pdf.part")
	if err != nil {
		return record, err
	}
	postPartialPath := postPartial.Name()
	partialName := filepath.Base(postPartialPath)
	if err := postPartial.Close(); err != nil {
		return record, err
	}
	if err := os.Remove(postPartialPath); err != nil {
		return record, err
	}
	defer os.Remove(postPartialPath)
	outlineMetrics, err := postprocessOutline(
		generationCtx, qpdfSidecar, overlaidPartialPath, postPartialPath, assembled.Outline,
		config.MaxPDFBytes, config.MaxQPDFRSSBytes, config.TempRoot,
	)
	if err != nil {
		return record, err
	}
	record.OutlinePlanSHA256 = outlineMetrics.PlanSHA256
	record.PostprocessDurationMilliseconds = outlineMetrics.Duration.Milliseconds()
	record.PostprocessPeakRSSBytes = outlineMetrics.PeakRSS
	finalPDF, err := os.Open(postPartialPath)
	if err != nil {
		return record, err
	}
	stat, statErr := finalPDF.Stat()
	if statErr != nil || stat.Size() < 32 || stat.Size() > config.MaxPDFBytes {
		_ = finalPDF.Close()
		return record, errors.Join(statErr, fmt.Errorf("postprocessed PDF size is outside the allowed range"))
	}
	validation, validateErr := pdfcheck.InspectGenerated(finalPDF, stat.Size(), pdfcheck.GeneratedExpectations{
		MaxPages: config.MaxPages, RequireLinks: assembled.TOCEntries > 0,
		RequireOutline: true, RequireImage: metrics.Audit.RasterImages > 0,
	})
	hash, hashErr := hashFileAt(finalPDF, stat.Size())
	closeErr = finalPDF.Close()
	if err := errors.Join(validateErr, hashErr, closeErr); err != nil {
		return record, fmt.Errorf("validate postprocessed generated PDF: %w", err)
	}
	record.Status = "complete"
	record.Error = ""
	record.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	record.SHA256 = hash
	record.Size = stat.Size()
	record.DurationMilliseconds = time.Since(started).Milliseconds()
	record.PeakRSSBytes = max(metrics.PeakRSS, record.FooterOverlayPeakRSSBytes, outlineMetrics.PeakRSS)
	record.Provenance = GeneratedProvenance
	record.Validation = &model.GeneratedPDFValidation{
		PageCount: validation.PageCount, LetterMediaBoxes: validation.LetterMediaBoxes,
		FontObjects: validation.FontObjects, ImageObjects: validation.ImageObjects,
		AnnotationObjects: validation.AnnotationObjects, OutlineObjects: validation.OutlineObjects,
		TOCEntries: assembled.TOCEntries, TOCLinksChecked: assembled.TOCEntries,
		LocalRequests: metrics.Audit.LocalRequests, InlineDataRequests: metrics.Audit.DataRequests,
		ScriptElements: metrics.Audit.ScriptElements, LoadedImages: metrics.Audit.ImageCount,
		ImagePlaceholders:         metrics.Audit.ImagePlaceholders,
		ImagePlaceholderOverflows: metrics.Audit.PlaceholderOverflows,
		InternalLinksChecked:      metrics.Audit.InternalLinks, FailedImages: len(metrics.Audit.FailedImages),
		FailedImageDetails: append([]string(nil), metrics.Audit.FailedImages...),
		MissingTargets:     len(metrics.Audit.MissingTargets), ExternalRequests: len(metrics.Audit.External),
		CanonicalCoverTitles: metrics.Audit.CoverTitles, PublisherChromeElements: metrics.Audit.PublisherChrome,
		FixedOrStickyElements: len(metrics.Audit.FixedOrSticky),
		OutlineEntries:        outlineMetrics.EntryCount, OutlineMaxDepth: outlineMetrics.MaxDepth,
		OutlineExternalURIs: outlineMetrics.ExternalURIs, OutlineRepeatedTargets: outlineMetrics.RepeatedTargets,
		OutlineSupplementaryGroups: outlineMetrics.SupplementaryGroups,
		FontsLoaded:                metrics.Audit.FontsStatus == "loaded", ScriptExecutionDisabled: !metrics.Audit.ScriptProbe,
		ResourceClosureValidated:   len(metrics.Audit.External) == 0 && len(metrics.Audit.RequestFailures) == 0,
		CoverTypographyValidated:   metrics.Audit.CoverTypography,
		HeadingTypographyValidated: metrics.Audit.HeadingTypography,
		HeadingPaginationValidated: metrics.Audit.HeadingPagination,
		FooterOverlayValidated:     footerRender.Audit.FooterOverlay,
		OutlineValidated:           true,
		StructuralValidated:        true,
	}
	newIndex, oldIndex, err := generatedPDFIndex(root, record.Path)
	if err != nil {
		return record, err
	}
	oldManifest := *result.HTML
	oldManifest.GeneratedPDF = result.HTML.GeneratedPDF
	if err := root.Rename(partialName, record.Path); err != nil {
		return record, err
	}
	published := true
	defer func() {
		if err != nil && published {
			rollbackErr := rollbackGeneratedPDFPublication(root, record.Path, oldIndex, &oldManifest)
			*result.HTML = oldManifest
			if rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("roll back generated PDF publication: %w", rollbackErr))
			}
		}
	}()
	if err := storage.Atomic(root, "index.html", newIndex); err != nil {
		return record, err
	}
	indexHash := sha256.Sum256(newIndex)
	result.HTML.Integrity.IndexSHA256 = hex.EncodeToString(indexHash[:])
	result.HTML.GeneratedPDF = &record
	if err := storage.WriteJSON(root, "manifest.json", result.HTML); err != nil {
		return record, err
	}
	if err := storage.SyncDir(root, "."); err != nil {
		return record, err
	}
	if err := generationCtx.Err(); err != nil {
		return record, err
	}
	published = false
	return record, nil
}

func rollbackGeneratedPDFPublication(
	root *os.Root,
	pdfPath string,
	index []byte,
	manifest *model.HTMLArchive,
) error {
	return errors.Join(
		root.Remove(pdfPath),
		storage.Atomic(root, "index.html", index),
		storage.WriteJSON(root, "manifest.json", manifest),
		storage.SyncDir(root, "."),
	)
}

func RecordFailure(guideDir string, result *model.ArchiveResult, record model.GeneratedPDFRecord) error {
	return RecordFailureInRoot(nil, guideDir, result, record)
}

// RecordFailureInRoot is RecordFailure writing through guide, a guide
// directory the caller already holds open. A nil guide opens guideDir by path.
func RecordFailureInRoot(guide *os.Root, guideDir string, result *model.ArchiveResult, record model.GeneratedPDFRecord) error {
	if result == nil || (result.Status != "complete" && result.Status != "degraded") ||
		result.Format != "html" || result.HTML == nil {
		return errors.New("cannot record generated PDF failure without a publishable HTML archive")
	}
	record.InputStatus = result.Status
	record.ImagePlaceholders = model.ImagePlaceholderCount(result.MissingResources)
	root := guide
	if root == nil {
		opened, err := os.OpenRoot(guideDir)
		if err != nil {
			return err
		}
		defer opened.Close()
		root = opened
	}
	previous := result.HTML.GeneratedPDF
	result.HTML.GeneratedPDF = &record
	if err := storage.WriteJSON(root, "manifest.json", result.HTML); err != nil {
		result.HTML.GeneratedPDF = previous
		return err
	}
	return storage.SyncDir(root, ".")
}

func normalizedConfig(config Config) Config {
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	if config.MaxPDFBytes <= 0 {
		config.MaxPDFBytes = DefaultMaxPDFBytes
	}
	if config.MaxPages <= 0 {
		config.MaxPages = DefaultMaxPages
	}
	if config.MaxRSSBytes <= 0 {
		config.MaxRSSBytes = DefaultMaxRSSBytes
	}
	if config.MaxQPDFRSSBytes <= 0 {
		config.MaxQPDFRSSBytes = QPDFOutlineProcessMaxRSSBytes
	}
	return config
}

func ExpectedSettings() model.GeneratedPDFSettings {
	return model.GeneratedPDFSettings{
		SchemaVersion: 5, OutlinePlanSchemaVersion: QPDFOutlinePlanSchemaVersion,
		FooterPlanSchemaVersion: FooterPlanSchemaVersion,
		HeadingStyleVersion:     HeadingStyleVersion, FooterStyleVersion: FooterStyleVersion,
		Paper: "Letter", MarginTopInches: .72, MarginRightInches: .68,
		MarginBottomInches: .72, MarginLeftInches: .68,
		PageNumberFormat: "decimal", PageNumberPosition: "lower-right",
		FooterCategoryPolicy:   "first-authoritative-local-occurrence-parent-or-root-self-at-page-top",
		FirstVisiblePageNumber: 2, CoverFooterVisible: false, PrintBackground: true,
		PreferCSSPageSize: true, DisplayHeaderFooter: false, GenerateTaggedPDF: true,
		GenerateDocumentOutline: false,
	}
}

func renderChrome(
	parent context.Context,
	sidecar Sidecar,
	inputPath, guideRoot, expectedTitle string,
	output *os.File,
	config Config,
) (metrics renderMetrics, err error) {
	return renderChromeDocument(parent, sidecar, inputPath, guideRoot, expectedTitle, output, config, nil)
}

func renderChromeDocument(
	parent context.Context,
	sidecar Sidecar,
	inputPath, guideRoot, expectedTitle string,
	output *os.File,
	config Config,
	expectedFooter *footerPlan,
) (renderMetrics, error) {
	return retryChromeStart(parent, output, func() (renderMetrics, error) {
		return renderChromeOnce(parent, sidecar, inputPath, guideRoot, expectedTitle, output, config, expectedFooter)
	})
}

// chromeStartError is a Chrome that was launched but did not answer its first
// DevTools command. Nothing was rendered yet.
type chromeStartError struct{ err error }

func (e *chromeStartError) Error() string { return e.err.Error() }
func (e *chromeStartError) Unwrap() error { return e.err }

// retryChromeStart runs render once more when Chrome did not start: a second
// launch finds Chrome's files already read back, for example after a sync
// client evicted them to free disk space. The sidecar is verified again
// before the second launch, and output is emptied.
func retryChromeStart(ctx context.Context, output *os.File, render func() (renderMetrics, error)) (renderMetrics, error) {
	metrics, err := render()
	var start *chromeStartError
	if err == nil || !errors.As(err, &start) || ctx.Err() != nil {
		return metrics, err
	}
	if _, seekErr := output.Seek(0, io.SeekStart); seekErr != nil {
		return metrics, errors.Join(err, seekErr)
	}
	if truncateErr := output.Truncate(0); truncateErr != nil {
		return metrics, errors.Join(err, truncateErr)
	}
	return render()
}

// fitWideTablesExpression zooms each outermost table of the guide content
// that passes the right edge of the viewport so that it ends there.
const fitWideTablesExpression = `(() => {
	const limit = document.documentElement.clientWidth;
	for (let pass = 0; pass < 3; pass++) {
		let changed = false;
		for (const table of document.querySelectorAll(".print-content table")) {
			if (table.parentElement.closest("table")) continue;
			const box = table.getBoundingClientRect();
			if (box.width <= 0 || box.left >= limit || box.right <= limit + 0.5) continue;
			const zoom = (Number.parseFloat(table.style.zoom) || 1) * (limit - box.left) / box.width;
			table.style.setProperty("zoom", String(Math.floor(zoom * 1000) / 1000), "important");
			changed = true;
		}
		if (!changed) break;
	}
})()`

func renderChromeOnce(
	parent context.Context,
	sidecar Sidecar,
	inputPath, guideRoot, expectedTitle string,
	output *os.File,
	config Config,
	expectedFooter *footerPlan,
) (metrics renderMetrics, err error) {
	renderCtx, cancel := context.WithCancel(parent)
	defer cancel()
	if err := confirmChromeSidecar(sidecar); err != nil {
		return metrics, err
	}
	tempRoot, err := os.MkdirTemp(config.TempRoot, "aos-cx-docs-dldr-pdf-")
	if err != nil {
		return metrics, fmt.Errorf("create renderer temp root: %w", err)
	}
	if err := os.Chmod(tempRoot, 0o700); err != nil {
		_ = os.RemoveAll(tempRoot)
		return metrics, err
	}
	defer os.RemoveAll(tempRoot)
	if err := createChromeLaunchDirectories(tempRoot); err != nil {
		return metrics, err
	}
	logs := &boundedLog{limit: maxChromeLogBytes}
	allocatorCtx, allocatorCancel := chromedp.NewExecAllocator(renderCtx, chromeAllocatorOptions(sidecar.Path, tempRoot, logs)...)
	defer allocatorCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocatorCtx)
	defer browserCancel()

	var auditMu sync.Mutex
	audit := browserAudit{}
	requestError := make(chan error, 1)
	chromedp.ListenTarget(browserCtx, func(event any) {
		paused, ok := event.(*cdpfetch.EventRequestPaused)
		if !ok {
			return
		}
		allowed, kind, reason := allowRenderRequest(paused.Request.URL, guideRoot, inputPath)
		auditMu.Lock()
		switch kind {
		case "file":
			audit.LocalRequests++
		case "data":
			audit.DataRequests++
		default:
			if len(audit.External) < 100 {
				audit.External = append(audit.External, paused.Request.URL)
			}
		}
		if !allowed && reason != "" && len(audit.RequestFailures) < 100 {
			audit.RequestFailures = append(audit.RequestFailures, reason)
		}
		auditMu.Unlock()
		target := chromedp.FromContext(browserCtx).Target
		go func() {
			eventCtx := cdp.WithExecutor(browserCtx, target)
			var eventErr error
			if allowed {
				eventErr = cdpfetch.ContinueRequest(paused.RequestID).Do(eventCtx)
			} else {
				eventErr = cdpfetch.FailRequest(paused.RequestID, cdpnetwork.ErrorReasonBlockedByClient).Do(eventCtx)
			}
			if eventErr != nil {
				select {
				case requestError <- eventErr:
				default:
				}
			}
		}()
	})
	if err := chromedp.Run(browserCtx, cdpfetch.Enable(), emulation.SetScriptExecutionDisabled(true)); err != nil {
		return metrics, &chromeStartError{chromeError("start pinned Chrome", err, logs)}
	}
	if expectedFooter == nil {
		// A guide's PDF shows its pages as the HTML archive does, so the
		// publisher stylesheets apply their screen rules, not the print
		// rules written for the publisher's own PDF output. Page size,
		// margins and pagination come from the print stylesheet.
		if err := chromedp.Run(browserCtx, emulation.SetEmulatedMedia().WithMedia("screen")); err != nil {
			return metrics, chromeError("emulate screen media", err, logs)
		}
	}
	process := chromedp.FromContext(browserCtx).Browser.Process()
	if process == nil {
		return metrics, errors.New("pinned Chrome process identity is unavailable")
	}
	initialRSS, err := processGroupRSS(renderCtx, process.Pid)
	if err != nil {
		return metrics, fmt.Errorf("measure initial Chrome process-group RSS: %w", err)
	}
	if initialRSS <= 0 {
		return metrics, errors.New("Chrome process-group RSS could not be established")
	}
	if initialRSS > config.MaxRSSBytes {
		return metrics, fmt.Errorf("Chrome process-group RSS %d exceeds %d-byte limit", initialRSS, config.MaxRSSBytes)
	}
	metrics.PeakRSS = initialRSS
	monitorCtx, stopMonitor := context.WithCancel(renderCtx)
	monitor := monitorProcessGroup(monitorCtx, cancel, process.Pid, config.MaxRSSBytes)
	defer func() {
		stopMonitor()
		observation := <-monitor
		if observation.PeakRSS > metrics.PeakRSS {
			metrics.PeakRSS = observation.PeakRSS
		}
		if observation.Err != nil {
			err = errors.Join(err, observation.Err)
		}
	}()
	browserExecutor := cdp.WithExecutor(browserCtx, chromedp.FromContext(browserCtx).Browser)
	_, product, _, _, _, err := browser.GetVersion().Do(browserExecutor)
	if err != nil {
		return metrics, chromeError("read pinned Chrome version", err, logs)
	}
	metrics.Version = product
	inputURL := (&url.URL{Scheme: "file", Path: inputPath}).String()
	expectedTitleJSON, err := json.Marshal(expectedTitle)
	if err != nil {
		return metrics, err
	}
	expectedFooterJSON, err := json.Marshal(expectedFooter)
	if err != nil {
		return metrics, err
	}
	auditExpression := fmt.Sprintf(`document.fonts.ready.then(async () => {
			await Promise.all(Array.from(document.images, image => image.decode().catch(() => null)));
			const expectedTitle = %s;
			const expectedFooter = %s;
			const ids = new Set(Array.from(document.querySelectorAll("[id],a[name]"), element => element.id || element.getAttribute("name")));
			const links = Array.from(document.querySelectorAll('a[href^="#"]'));
			const coverTitles = Array.from(document.querySelectorAll(".print-cover h1")).filter(element => element.textContent.trim() === expectedTitle);
			const coverTypography = coverTitles.length === 1 && (() => {
				const element = coverTitles[0];
				const style = getComputedStyle(element);
				const rect = element.getBoundingClientRect();
				const fontPixels = Number.parseFloat(style.fontSize);
				return style.display !== "none" && style.visibility === "visible" && Number.isFinite(fontPixels) &&
					fontPixels > 0 && fontPixels <= 29.5 && rect.width > 0 && rect.height > 0;
			})();
			const publisherChrome = document.querySelectorAll(
				'.print-content>#mc-main-content>.topichero,.print-content>#mc-main-content>.docname'
			).length;
			const fixedOrSticky = Array.from(document.querySelectorAll(".print-content *")).filter(element => {
				const style = getComputedStyle(element);
				return style.display !== "none" && (style.position === "fixed" || style.position === "sticky");
			}).slice(0, 100).map(element => {
				const id = element.id ? "#" + element.id : "";
				const classes = Array.from(element.classList).slice(0, 3).map(value => "." + value).join("");
				return element.localName + id + classes;
			});
			const headings = Array.from(document.querySelectorAll(".print-content h1,.print-content h2,.print-content h3"));
			const near = (value, expected) => Number.isFinite(value) && Math.abs(value - expected) <= 0.25;
			const flare = document.body.classList.contains("print-source-flare");
			const headingTypography = !flare || headings.every(element => {
				const style = getComputedStyle(element);
				const expected = element.localName === "h1" ? [21.333, 25.6] :
					element.localName === "h2" ? [18.667, 23.333] : [16, 20];
				return near(Number.parseFloat(style.fontSize), expected[0]) &&
					near(Number.parseFloat(style.lineHeight), expected[1]);
			}) && (() => {
				const command = document.querySelector('[data-aoscx-heading-fixture="command"]');
				const extreme = document.querySelector('[data-aoscx-heading-fixture="extreme"]');
				if (command === null && extreme === null) return true;
				if (command === null || extreme === null) return false;
				const commandStyle = getComputedStyle(command);
				const extremeStyle = getComputedStyle(extreme);
				const commandLineHeight = Number.parseFloat(commandStyle.lineHeight);
				const extremeLineHeight = Number.parseFloat(extremeStyle.lineHeight);
				const commandRect = command.getBoundingClientRect();
				const extremeRect = extreme.getBoundingClientRect();
				return commandRect.height <= commandLineHeight + 1 &&
					extremeRect.height > extremeLineHeight + 1 &&
					command.scrollWidth <= command.parentElement.clientWidth + 1 &&
					extreme.scrollWidth <= extreme.parentElement.clientWidth + 1 &&
					extremeRect.height < 720;
			})();
			const headingPagination = headings.every(element => {
				const style = getComputedStyle(element);
				return (style.breakInside === "avoid-page" || style.breakInside === "avoid") &&
					(style.breakAfter === "avoid-page" || style.breakAfter === "avoid");
			});
			const footerOverlay = expectedFooter === null || (() => {
				const pages = Array.from(document.querySelectorAll(".footer-page"));
				if (pages.length !== expectedFooter.entries.length) return false;
				return pages.every((section, index) => {
					const expected = expectedFooter.entries[index];
					if (Number(section.dataset.physicalPage) !== expected.physical_page ||
						section.dataset.kind !== expected.kind) return false;
					const row = section.querySelector(".footer-row");
					if (expected.physical_page === 1) return row === null;
					if (!row) return false;
					const label = row.querySelector(".footer-label");
					const number = row.querySelector(".footer-number");
					if (!label || !number || label.textContent !== expected.label ||
						number.textContent !== String(expected.decimal_number)) return false;
					const sectionRect = section.getBoundingClientRect();
					const rowRect = row.getBoundingClientRect();
					const labelRect = label.getBoundingClientRect();
					const numberRect = number.getBoundingClientRect();
					const numberStyle = getComputedStyle(number);
					return rowRect.left >= sectionRect.left + 64 &&
						rowRect.right <= sectionRect.right - 64 &&
						rowRect.bottom <= sectionRect.bottom - 20 &&
						rowRect.top >= sectionRect.bottom - 70 &&
						labelRect.right < numberRect.left &&
						near(Number.parseFloat(numberStyle.fontSize), 12) &&
						Number.parseInt(numberStyle.fontWeight, 10) >= 600;
				});
			})();
			return {
				fonts_status: document.fonts.status,
				image_count: Array.from(document.images).filter(image =>
					image.hasAttribute("src") || image.hasAttribute("srcset")
				).length,
				raster_images: Array.from(document.images).filter(image =>
					(image.hasAttribute("src") || image.hasAttribute("srcset")) &&
					!image.src.startsWith("data:image/svg")
				).length,
				image_placeholders: document.querySelectorAll(".archive-image-placeholder[role=img]").length,
				image_placeholder_overflows: Array.from(document.querySelectorAll(".archive-image-placeholder[role=img]")).filter(element => {
					const box = element.getBoundingClientRect();
					const parent = element.parentElement && element.parentElement.getBoundingClientRect();
					return !parent || box.width <= 0 || box.height <= 0 ||
						box.left < parent.left - 1 || box.right > parent.right + 1;
				}).length,
				failed_images: Array.from(document.images).filter(image =>
					(image.hasAttribute("src") || image.hasAttribute("srcset")) &&
					(!image.complete || image.naturalWidth === 0)
				).map(image => {
					const src = image.getAttribute("src");
					const srcset = image.getAttribute("srcset");
					const identity = image.currentSrc ? "currentSrc=" + image.currentSrc :
						(src !== null ? "src=" + JSON.stringify(src) : "srcset=" + JSON.stringify(srcset));
					const topic = image.closest(".print-topic");
					return (topic && topic.dataset.sourceUrl ? topic.dataset.sourceUrl + " -> " : "") + identity;
				}).slice(0, 100),
				script_elements: document.scripts.length,
				script_probe: window.__aoscxPDFScriptProbe === true,
				internal_links: links.length,
				missing_targets: links.map(link => decodeURIComponent(link.hash.slice(1))).filter(target => target !== "" && !ids.has(target)).slice(0, 100),
				cover_titles: coverTitles.length,
				cover_typography: coverTypography,
				heading_typography: headingTypography,
				heading_pagination: headingPagination,
				footer_overlay: footerOverlay,
				publisher_chrome: publisherChrome,
				fixed_or_sticky: fixedOrSticky
			};
		})`, expectedTitleJSON, expectedFooterJSON)
	var pageState browserAudit
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(inputURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Evaluate(auditExpression, &pageState, func(params *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return params.WithAwaitPromise(true).WithReturnByValue(true)
		}),
	); err != nil {
		return metrics, chromeError("load transient print HTML", err, logs)
	}
	auditMu.Lock()
	audit.FontsStatus = pageState.FontsStatus
	audit.ImageCount = pageState.ImageCount
	audit.RasterImages = pageState.RasterImages
	audit.ImagePlaceholders = pageState.ImagePlaceholders
	audit.PlaceholderOverflows = pageState.PlaceholderOverflows
	audit.FailedImages = append([]string{}, pageState.FailedImages...)
	audit.ScriptElements = pageState.ScriptElements
	audit.ScriptProbe = pageState.ScriptProbe
	audit.InternalLinks = pageState.InternalLinks
	audit.MissingTargets = append([]string{}, pageState.MissingTargets...)
	audit.CoverTitles = pageState.CoverTitles
	audit.CoverTypography = pageState.CoverTypography
	audit.HeadingTypography = pageState.HeadingTypography
	audit.HeadingPagination = pageState.HeadingPagination
	audit.FooterOverlay = pageState.FooterOverlay
	audit.PublisherChrome = pageState.PublisherChrome
	audit.FixedOrSticky = append([]string{}, pageState.FixedOrSticky...)
	metrics.Audit = audit
	auditMu.Unlock()
	if metrics.Audit.FontsStatus != "loaded" {
		return metrics, errors.New("local fonts did not reach the loaded state")
	}
	if metrics.Audit.ScriptProbe {
		return metrics, errors.New("document script executed despite pre-navigation script disablement")
	}
	if len(metrics.Audit.External) != 0 || len(metrics.Audit.RequestFailures) != 0 {
		return metrics, fmt.Errorf("generated PDF renderer blocked nonlocal or unsafe resources: %s",
			strings.Join(append(metrics.Audit.External, metrics.Audit.RequestFailures...), "; "))
	}
	if len(metrics.Audit.FailedImages) != 0 {
		return metrics, fmt.Errorf("generated PDF input has %d failed local images: %s",
			len(metrics.Audit.FailedImages), strings.Join(metrics.Audit.FailedImages, "; "))
	}
	if len(metrics.Audit.MissingTargets) != 0 {
		return metrics, fmt.Errorf("generated PDF input has %d unresolved internal targets: %s",
			len(metrics.Audit.MissingTargets), strings.Join(metrics.Audit.MissingTargets, ", "))
	}
	if expectedTitle != "" && (metrics.Audit.CoverTitles != 1 || !metrics.Audit.CoverTypography) {
		return metrics, fmt.Errorf("generated PDF input has invalid canonical cover title: count=%d typography=%t",
			metrics.Audit.CoverTitles, metrics.Audit.CoverTypography)
	}
	if !metrics.Audit.HeadingTypography || !metrics.Audit.HeadingPagination {
		return metrics, errors.New("generated PDF input has invalid heading typography or pagination rules")
	}
	if !metrics.Audit.FooterOverlay {
		return metrics, errors.New("generated PDF footer overlay has invalid text or geometry")
	}
	if metrics.Audit.PublisherChrome != 0 {
		return metrics, fmt.Errorf("generated PDF input retains %d publisher homepage chrome elements",
			metrics.Audit.PublisherChrome)
	}
	if len(metrics.Audit.FixedOrSticky) != 0 {
		return metrics, fmt.Errorf("generated PDF input retains fixed or sticky publisher elements: %s",
			strings.Join(metrics.Audit.FixedOrSticky, ", "))
	}
	select {
	case requestErr := <-requestError:
		return metrics, fmt.Errorf("generated PDF request interception failed: %w", requestErr)
	default:
	}
	if expectedFooter == nil {
		// Chrome shrinks every page of a document to fit its widest element.
		// A table whose words cannot fit the page is scaled down on its own
		// instead, measured at the width of a page's content area, so that
		// the rest of the guide prints at full size.
		settings := ExpectedSettings()
		width := int64((8.5 - settings.MarginLeftInches - settings.MarginRightInches) * 96)
		if err := chromedp.Run(browserCtx,
			emulation.SetDeviceMetricsOverride(width, 1000, 1, false),
			chromedp.Evaluate(fitWideTablesExpression, nil),
			emulation.ClearDeviceMetricsOverride(),
		); err != nil {
			return metrics, chromeError("fit wide tables to the page", err, logs)
		}
	}
	writer := &limitedFileWriter{file: output, limit: config.MaxPDFBytes}
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, stream, err := page.PrintToPDF().
			WithPrintBackground(true).
			WithPreferCSSPageSize(true).
			WithDisplayHeaderFooter(false).
			WithGenerateTaggedPDF(true).
			WithGenerateDocumentOutline(false).
			WithTransferMode(page.PrintToPDFTransferModeReturnAsStream).
			Do(ctx)
		if err != nil {
			return err
		}
		defer cdpio.Close(stream).Do(ctx)
		for {
			params := cdpio.Read(stream).WithSize(1 << 20)
			var response cdpio.ReadReturns
			if err := cdp.Execute(ctx, cdpio.CommandRead, params, &response); err != nil {
				return err
			}
			body := []byte(response.Data)
			if response.Base64encoded {
				body, err = base64.StdEncoding.DecodeString(response.Data)
				if err != nil {
					return err
				}
			}
			if _, err := writer.Write(body); err != nil {
				return err
			}
			if response.EOF {
				return output.Sync()
			}
		}
	})); err != nil {
		return metrics, chromeError("stream generated PDF", err, logs)
	}
	if err := renderCtx.Err(); err != nil {
		return metrics, err
	}
	select {
	case requestErr := <-requestError:
		return metrics, fmt.Errorf("generated PDF request interception failed: %w", requestErr)
	default:
	}
	auditMu.Lock()
	metrics.Audit.LocalRequests = audit.LocalRequests
	metrics.Audit.DataRequests = audit.DataRequests
	metrics.Audit.External = append([]string{}, audit.External...)
	metrics.Audit.RequestFailures = append([]string{}, audit.RequestFailures...)
	auditMu.Unlock()
	if len(metrics.Audit.External) != 0 || len(metrics.Audit.RequestFailures) != 0 {
		return metrics, errors.New("generated PDF renderer observed a blocked resource request")
	}
	return metrics, nil
}

func allowRenderRequest(raw, guideRoot, inputPath string) (bool, string, string) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false, "external", "malformed renderer request URL"
	}
	if parsed.Scheme == "data" {
		return true, "data", ""
	}
	if parsed.Scheme != "file" || parsed.Host != "" || parsed.RawQuery != "" ||
		parsed.Opaque != "" || parsed.User != nil {
		return false, "external", "network and non-file renderer requests are disabled: " + raw
	}
	// url.Parse has already percent-decoded Path exactly once. Decoding it
	// again would misread a literal "%" in the destination (for example
	// "100%" or "Docs%20Lib") and refuse every resource. An encoded slash
	// would let one decoded component span directories, and NUL cannot name
	// a file, so both are refused outright.
	if strings.Contains(strings.ToLower(parsed.RawPath), "%2f") || strings.ContainsRune(parsed.Path, 0) {
		return false, "file", "renderer file request uses an encoded slash or NUL: " + raw
	}
	if !filepath.IsAbs(parsed.Path) {
		return false, "file", "renderer file request path is not absolute: " + raw
	}
	requestPath := filepath.Clean(parsed.Path)
	relative, err := filepath.Rel(guideRoot, requestPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false, "file", "renderer file request escapes verified guide root: " + raw
	}
	if requestPath == inputPath {
		return true, "file", ""
	}
	current := guideRoot
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return false, "file", "unsafe renderer file request path: " + raw
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return false, "file", "missing renderer file resource: " + raw
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, "file", "renderer file resource uses a symbolic link: " + raw
		}
	}
	info, err := os.Lstat(requestPath)
	if err != nil || !info.Mode().IsRegular() {
		return false, "file", "renderer file resource is not a regular file: " + raw
	}
	return true, "file", ""
}

func hashFileAt(file *os.File, size int64) (string, error) {
	hash := sha256.New()
	n, err := io.Copy(hash, io.NewSectionReader(file, 0, size))
	if err != nil || n != size {
		return "", errors.Join(err, fmt.Errorf("hashed %d of %d generated PDF bytes", n, size))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func chromeError(stage string, err error, logs *boundedLog) error {
	detail := strings.TrimSpace(logs.String())
	if detail == "" {
		return fmt.Errorf("%s: %w", stage, err)
	}
	return fmt.Errorf("%s: %w; bounded Chrome diagnostics: %s", stage, err, detail)
}

type limitedFileWriter struct {
	file  *os.File
	count int64
	limit int64
}

func (w *limitedFileWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.limit-w.count {
		return 0, fmt.Errorf("generated PDF exceeds %d-byte limit", w.limit)
	}
	n, err := w.file.Write(data)
	w.count += int64(n)
	return n, err
}

type boundedLog struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	label     string
	truncated bool
}

func (w *boundedLog) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(data)
	remaining := w.limit - w.buffer.Len()
	if remaining > 0 {
		_, _ = w.buffer.Write(data[:min(len(data), remaining)])
	}
	if original > remaining {
		w.truncated = true
	}
	return original, nil
}

func (w *boundedLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	value := w.buffer.String()
	if w.truncated {
		label := w.label
		if label == "" {
			label = "Chrome"
		}
		value += "\n[" + label + " diagnostics truncated]"
	}
	return value
}
