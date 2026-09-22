package library

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/pdfcheck"
	"aos-cx-docs-dldr/internal/pdfgen"
	"aos-cx-docs-dldr/internal/storage"
)

var (
	runPattern    = regexp.MustCompile(`^[0-9TZ.-]+-[A-Za-z0-9]+$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const finderMetadataMaxBytes = 1 << 20

type Run struct {
	Destination                                 string
	Target                                      string
	Stage                                       string
	CatalogueURL                                string
	CatalogueFetchedAt                          string
	CatalogueWarnings                           []string
	SkippedUnavailable                          []model.SkippedUnavailableGuide
	root                                        *os.Root
	parent, target, stage, lock, journal, stamp string
	platform, version                           string
	owner                                       lockOwner
	lockInfo                                    os.FileInfo
	lockReady                                   bool
	previous                                    Manifest
	guides                                      map[string]model.ArchiveResult
	history                                     []History
	attempts                                    []Attempt
	errors                                      []string
	published                                   bool
	rename                                      func(string, string) error
	versionPolicy                               nativeVersionPolicy
	migrationHook                               func(string) error
}

func (r *Run) SetSkippedUnavailable(values []model.SkippedUnavailableGuide) {
	r.SkippedUnavailable = slices.Clone(values)
	for index := range r.SkippedUnavailable {
		_, retainedPDF := r.previous.PDFGuides[r.SkippedUnavailable[index].ID]
		_, retainedHTML := r.previous.HTMLGuides[r.SkippedUnavailable[index].ID]
		r.SkippedUnavailable[index].RetainedPrevious = retainedPDF || retainedHTML
	}
}

func Open(destination, platform, version string) (_ *Run, err error) {
	return OpenContext(context.Background(), destination, platform, version)
}

func OpenContext(ctx context.Context, destination, platform, version string) (_ *Run, err error) {
	policy, err := defaultNativeVersionPolicy()
	if err != nil {
		return nil, err
	}
	return openContextWithPolicy(ctx, destination, platform, version, policy, nil)
}

func openContextWithPolicy(
	ctx context.Context,
	destination, platform, version string,
	policy nativeVersionPolicy,
	hook func(string) error,
) (_ *Run, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base, err := storage.ResolveBase(destination)
	if err != nil {
		return nil, err
	}
	p, err := storage.SafeComponent(platform)
	if err != nil {
		return nil, err
	}
	v, err := storage.SafeComponent(version)
	if err != nil {
		return nil, err
	}
	if err := preflightWithPolicy(ctx, destination, platform, version, policy); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	r := &Run{Destination: base, root: root, parent: p, target: path.Join(p, v),
		lock: path.Join(p, "."+v+".lock"), journal: path.Join(p, "."+v+".transaction.json"),
		platform: platform, version: version, versionPolicy: policy, migrationHook: hook,
		guides: map[string]model.ArchiveResult{}, attempts: []Attempt{}, errors: []string{}}
	r.rename = root.Rename
	r.resetTransaction()
	r.Target = filepath.Join(base, filepath.FromSlash(r.target))
	defer func() {
		if err != nil {
			err = errors.Join(err, r.Close())
		}
	}()
	if err := storage.EnsureDir(root, p); err != nil {
		return nil, err
	}
	for _, managed := range []string{r.target, r.lock, r.journal, path.Join(p, ".staging"), path.Join(p, ".snapshots"), path.Join(p, ".incomplete")} {
		if err := storage.Check(root, managed); err != nil {
			return nil, err
		}
	}
	if err := r.acquire(); err != nil {
		return nil, err
	}
	if err := r.recoverContext(ctx); err != nil {
		return nil, err
	}
	if _, statErr := root.Lstat(r.target); statErr == nil {
		r.previous, err = r.readLibrary(r.target)
		if err != nil {
			return nil, err
		}
		r.history, err = r.readHistory(r.target)
		if err != nil {
			return nil, err
		}
		for id, result := range r.previous.PDFGuides {
			r.guides[id] = result
		}
		for id, result := range r.previous.HTMLGuides {
			r.guides[id] = result
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if err := r.checkOwnership(); err != nil {
		return nil, err
	}
	if err := storage.EnsureDir(root, r.stage); err != nil {
		return nil, err
	}
	if r.previous.RunID != "" {
		if err := storage.CopyTreeFiltered(root, r.target, r.stage, copyPublishedEntry); err != nil {
			return nil, err
		}
	}
	if err := r.checkOwnership(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Run) resetTransaction() {
	r.stamp = time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + rand.Text()
	r.stage = path.Join(r.parent, ".staging", path.Base(r.target)+"-"+r.stamp)
	r.Stage = filepath.Join(r.Destination, filepath.FromSlash(r.stage))
}

func (r *Run) acquire() error {
	f, err := r.root.OpenFile(r.lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		var owner lockOwner
		readErr := storage.ReadJSON(r.root, r.lock, &owner)
		return fmt.Errorf("library locked at %s (PID %d, host %q, metadata error %v); confirm the owning process has stopped before manually removing this specific lock",
			filepath.Join(r.Destination, r.lock), owner.PID, owner.Host, readErr)
	}
	if err != nil {
		return err
	}
	r.lockInfo, err = f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	host, hostErr := os.Hostname()
	r.owner = lockOwner{PID: os.Getpid(), Host: host, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Token: rand.Text()}
	data, marshalErr := jsonBytes(r.owner)
	_, writeErr := f.Write(data)
	syncErr, closeErr := f.Sync(), f.Close()
	if err := errors.Join(err, hostErr, marshalErr, writeErr, syncErr, closeErr); err != nil {
		return err
	}
	r.lockReady = true
	return storage.SyncDir(r.root, r.parent)
}

func (r *Run) Close() error {
	if r.root == nil {
		return nil
	}
	var err error
	if r.lockInfo != nil {
		if r.lockReady {
			err = r.checkOwnership()
		} else {
			err = r.checkLockFile()
		}
		if err == nil {
			err = errors.Join(r.root.Remove(r.lock), storage.SyncDir(r.root, r.parent))
		}
	}
	err = errors.Join(err, r.root.Close())
	r.root = nil
	return err
}

func (r *Run) GuideOutput(id string) (string, error) {
	name, err := storage.SafeComponent(id)
	if err != nil {
		return "", err
	}
	work := path.Join(r.stage, ".work", name)
	if err := storage.EnsureDir(r.root, work); err != nil {
		return "", err
	}
	return filepath.Join(r.Destination, filepath.FromSlash(work)), nil
}

func (r *Run) Accept(result model.ArchiveResult) error {
	name, err := storage.SafeComponent(result.Document.ID)
	if err != nil {
		return err
	}
	work := path.Join(r.stage, ".work", name)
	if result.OutputDir != filepath.Join(r.Destination, filepath.FromSlash(work)) ||
		result.Document.Platform != r.platform || result.Document.Version != r.version ||
		(result.Format != "pdf" && result.Format != "html") {
		return errors.New("archive result is outside this library transaction")
	}
	generatedFailed := result.Format == "html" && result.HTML != nil &&
		result.HTML.GeneratedPDF != nil && result.HTML.GeneratedPDF.Status == "failed"
	previousGenerated := r.guides[name].HTML != nil &&
		r.guides[name].HTML.GeneratedPDF != nil &&
		r.guides[name].HTML.GeneratedPDF.Status == "complete"
	previous, hasPrevious := r.guides[name]
	accept := result.Status == "complete" || result.Status == "degraded"
	retained := false
	if hasPrevious {
		switch {
		case previous.Status == "complete" && result.Status != "complete":
			accept, retained = false, true
		case generatedFailed && previousGenerated:
			accept, retained = false, true
		case previous.Status == "degraded" && result.Status == "degraded" &&
			!missingResourceSubset(result.MissingResources, previous.MissingResources):
			accept, retained = false, true
		}
	}
	for existing, previous := range r.guides {
		if strings.EqualFold(existing, name) && previous.Document.ID != result.Document.ID {
			return fmt.Errorf("guide path collides with existing guide %q", previous.Document.ID)
		}
	}
	if accept {
		if err := r.verifyGuide(work, result); err != nil {
			return err
		}
	}
	if accept && !retained {
		target := path.Join(r.stage, name)
		if err := storage.RemoveTree(r.root, target); err != nil {
			return err
		}
		if err := r.rename(work, target); err != nil {
			return err
		}
		result.OutputDir = ""
		r.guides[name] = result
	} else {
		if err := storage.WriteJSON(r.root, path.Join(work, "attempt.json"), result); err != nil {
			return err
		}
		if retained && result.Status == "degraded" {
			if err := storage.RemoveTree(r.root, work); err != nil {
				return err
			}
		}
	}
	attemptStatus := result.Status
	if generatedFailed {
		attemptStatus = "incomplete"
	}
	r.attempts = append(r.attempts, Attempt{ID: result.Document.ID, Title: result.Document.Title, Status: attemptStatus, Format: result.Format,
		Errors: result.Errors, Notices: result.Notices, Warnings: result.Warnings,
		StylesheetRecoveries: cloneStylesheetRecoveries(result.StylesheetRecoveries),
		MissingResources:     cloneMissingResources(result.MissingResources), RetainedPrevious: retained})
	for _, message := range result.Errors {
		r.errors = append(r.errors, result.Document.Title+": "+message)
	}
	return nil
}

func (r *Run) verifyGuide(directory string, result model.ArchiveResult) error {
	if result.Format == "html" {
		return r.verifyHTMLGuide(directory, result)
	}
	return r.verifyPDFGuide(directory, result)
}

func (r *Run) verifyPDFGuide(directory string, result model.ArchiveResult) error {
	pdf := result.PDF
	if result.Status != "complete" || result.Format != "pdf" || result.Document.Kind != "pdf" || pdf == nil ||
		pdf.Status != "complete" || pdf.SHA256 == "" || pdf.SourceSHA256 != pdf.SHA256 || pdf.Size < 32 ||
		pdf.Size > 16<<30 {
		return fmt.Errorf("invalid complete publisher PDF metadata: %s", directory)
	}
	if _, err := fetch.NormalizeURL(pdf.FinalURL); err != nil {
		return err
	}
	if _, err := fetch.NormalizeURL(result.Document.URL); err != nil {
		return err
	}
	if pdf.SourceVerified {
		input, err := archive.VerifiedPublisherPDFInput(pdf.URL, pdf.Inputs)
		if err != nil || pdf.FinalURL != input.FinalURL ||
			input.Size != int(pdf.Size) || input.SHA256 != pdf.SourceSHA256 {
			return fmt.Errorf("invalid verified publisher PDF source evidence: %s", directory)
		}
	} else if pdf.URL != result.Document.URL || len(pdf.Inputs) != 0 {
		return fmt.Errorf("invalid direct publisher PDF provenance: %s", directory)
	}
	switch pdf.Origin {
	case "":
		// Native schema 1 predates the explicit origin field. The source-evidence
		// checks above remain authoritative for those retained libraries.
	case model.PDFOriginDirectMapped:
		if pdf.SourceVerified {
			return fmt.Errorf("verified publisher PDF has direct-mapped origin: %s", directory)
		}
	case model.PDFOriginSelectedRoute, model.PDFOriginSourceAdvertised, model.PDFOriginHPEExportAll:
		if !pdf.SourceVerified {
			return fmt.Errorf("unverified publisher PDF has source-verified origin: %s", directory)
		}
	default:
		return fmt.Errorf("invalid publisher PDF origin %q: %s", pdf.Origin, directory)
	}
	name, err := storage.PDFFilename(result.Document.Platform, result.Document.Version, result.Document.Title)
	if err != nil || pdf.Path != name {
		return fmt.Errorf("unexpected publisher PDF filename: %s", directory)
	}
	if err := storage.Check(r.root, directory); err != nil {
		return err
	}
	f, err := r.root.Open(directory)
	if err != nil {
		return err
	}
	entries, readErr := f.ReadDir(-1)
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	entries, err = withoutFinderMetadata(r.root, directory, entries)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != pdf.Path || !entries[0].Type().IsRegular() {
		return fmt.Errorf("publisher PDF guide must contain only its PDF: %s", directory)
	}
	hash, size, err := storage.Hash(r.root, path.Join(directory, pdf.Path), pdf.Size)
	if err != nil || hash != pdf.SHA256 || size != pdf.Size {
		return fmt.Errorf("publisher PDF integrity mismatch at %s: %v", directory, err)
	}
	body, err := r.root.Open(path.Join(directory, pdf.Path))
	if err != nil {
		return err
	}
	validErr := archive.ValidatePublisherPDF(body, pdf.MIME)
	return errors.Join(validErr, body.Close())
}

func (r *Run) verifyHTMLGuide(directory string, result model.ArchiveResult) error {
	manifest := result.HTML
	if (result.Status != "complete" && result.Status != "degraded") || result.Format != "html" ||
		(result.Document.Kind != "flare" && result.Document.Kind != "hpe" && result.Document.Kind != "static") ||
		manifest == nil || manifest.SchemaVersion != 1 || manifest.Status != result.Status ||
		manifest.SourceURL != result.Document.URL || len(manifest.Topics) == 0 || len(manifest.Errors) != 0 ||
		!reflect.DeepEqual(result.MissingResources, manifest.MissingResources) {
		return fmt.Errorf("invalid publishable HTML archive metadata: %s", directory)
	}
	if err := storage.Check(r.root, directory); err != nil {
		return err
	}
	var disk model.HTMLArchive
	if err := storage.ReadJSON(r.root, path.Join(directory, "manifest.json"), &disk); err != nil {
		return err
	}
	if !reflect.DeepEqual(disk, *manifest) {
		return fmt.Errorf("HTML guide manifest does not match accepted metadata: %s", directory)
	}
	if err := r.verifyMissingResources(directory, manifest); err != nil {
		return err
	}
	allowed := map[string]bool{
		"index.html": true, "toc.html": true, "search.json": true, "search-index.js": true, "search.js": true, "manifest.json": true,
	}
	if generated := manifest.GeneratedPDF; generated != nil {
		name, err := storage.PDFFilename(result.Document.Platform, result.Document.Version, result.Document.Title)
		if err != nil || generated.Path != name {
			return fmt.Errorf("unexpected generated PDF filename: %s", directory)
		}
		switch generated.Status {
		case "failed":
			// A failed record must not claim output, but it may carry the
			// preflight audit that ran before the failure: that is exactly
			// the evidence needed to diagnose it.
			if generated.Error == "" || generated.Renderer != pdfgen.RendererName ||
				!validGeneratedPDFTimestamp(generated.CreatedAt) || generated.SHA256 != "" ||
				generated.Size != 0 {
				return fmt.Errorf("invalid failed generated PDF metadata: %s", directory)
			}
			if v := generated.Validation; v != nil && (v.PageCount != 0 || v.LetterMediaBoxes != 0 ||
				v.FontObjects != 0 || v.OutlineObjects != 0 || v.TOCEntries != 0) {
				return fmt.Errorf("failed generated PDF metadata claims output validation: %s", directory)
			}
		case "complete":
			expectedSettings := pdfgen.ExpectedSettings()
			// Sidecar identities are those of the platform that produced the
			// library, and a library is validated only on that platform.
			pins, err := pdfgen.CurrentPlatformPins()
			if err != nil {
				return fmt.Errorf("complete generated PDF cannot be validated on this platform: %s: %w", directory, err)
			}
			if generated.Error != "" || generated.Renderer != pdfgen.RendererName ||
				generated.RendererVersion != pdfgen.ChromeVersion ||
				generated.RendererRevision != pdfgen.ChromeRevision ||
				generated.ExecutableSHA256 != pins.ChromeExecutableSHA256 ||
				generated.Postprocessor != pdfgen.QPDFPostprocessorName ||
				generated.PostprocessorVersion != pdfgen.QPDFVersion ||
				generated.PostprocessorExecutableSHA256 != pins.QPDFExecutableSHA256 ||
				generated.PostprocessorBundleSHA256 != pins.QPDFBundleSHA256 ||
				!sha256Pattern.MatchString(generated.OutlinePlanSHA256) ||
				!sha256Pattern.MatchString(generated.FooterPlanSHA256) ||
				!sha256Pattern.MatchString(generated.FooterOverlayInputSHA256) ||
				generated.FooterOverlayRenderer != pdfgen.RendererName ||
				generated.FooterOverlayRendererVersion != pdfgen.ChromeVersion ||
				generated.FooterOverlayExecutableSHA256 != pins.ChromeExecutableSHA256 ||
				!sha256Pattern.MatchString(generated.SourceManifestSHA256) ||
				!sha256Pattern.MatchString(generated.InputSHA256) ||
				!sha256Pattern.MatchString(generated.SHA256) ||
				generated.Size < 32 || generated.Size > pdfgen.DefaultMaxPDFBytes ||
				!validGeneratedPDFTimestamp(generated.CreatedAt) ||
				generated.DurationMilliseconds < 0 || generated.PeakRSSBytes <= 0 ||
				generated.PostprocessDurationMilliseconds < 0 ||
				generated.PostprocessPeakRSSBytes < 0 ||
				generated.FooterOverlayDurationMilliseconds <= 0 ||
				generated.FooterOverlayPeakRSSBytes <= 0 ||
				generated.Provenance != pdfgen.GeneratedProvenance ||
				!validGeneratedPDFInput(result, generated) ||
				generated.Settings == nil || generated.Validation == nil ||
				generated.FooterEntries != generated.Validation.PageCount ||
				!reflect.DeepEqual(*generated.Settings, expectedSettings) ||
				!validGeneratedPDFValidation(*generated.Validation) {
				return fmt.Errorf("invalid complete generated PDF metadata: %s", directory)
			}
			hash, size, err := storage.Hash(r.root, path.Join(directory, generated.Path), generated.Size)
			if err != nil || hash != generated.SHA256 || size != generated.Size {
				return fmt.Errorf("generated PDF integrity mismatch at %s: %v", directory, err)
			}
			pdf, err := r.root.Open(path.Join(directory, generated.Path))
			if err != nil {
				return err
			}
			summary, validateErr := pdfcheck.InspectGenerated(pdf, generated.Size, pdfcheck.GeneratedExpectations{
				MaxPages:       generated.Validation.PageCount,
				RequireLinks:   generated.Validation.TOCEntries > 0,
				RequireOutline: true,
				RequireImage:   generated.Validation.ImageObjects > 0,
			})
			closeErr := pdf.Close()
			if err := errors.Join(validateErr, closeErr); err != nil ||
				summary.PageCount != generated.Validation.PageCount ||
				summary.LetterMediaBoxes != generated.Validation.LetterMediaBoxes ||
				summary.FontObjects != generated.Validation.FontObjects ||
				summary.ImageObjects != generated.Validation.ImageObjects ||
				summary.AnnotationObjects != generated.Validation.AnnotationObjects ||
				summary.OutlineObjects != generated.Validation.OutlineObjects {
				return fmt.Errorf("generated PDF structural validation mismatch at %s: %v", directory, err)
			}
			allowed[generated.Path] = true
		default:
			return fmt.Errorf("invalid generated PDF status %q: %s", generated.Status, directory)
		}
	}
	folded := map[string]bool{}
	for _, record := range append(append([]model.FileRecord{}, manifest.Topics...), manifest.Assets...) {
		if record.Status != "complete" || record.Path == "" || record.SHA256 == "" || record.SourceSHA256 == "" ||
			record.Size < 0 || (record.URL == "" && !strings.HasPrefix(record.Path, "assets/")) {
			return fmt.Errorf("invalid complete HTML file record at %s", directory)
		}
		expectedPrefix := "assets/"
		if slices.ContainsFunc(manifest.Topics, func(topic model.FileRecord) bool { return topic.Path == record.Path }) {
			expectedPrefix = "pages/"
		}
		if !strings.HasPrefix(record.Path, expectedPrefix) || folded[strings.ToLower(record.Path)] || allowed[record.Path] {
			return fmt.Errorf("unsafe or colliding HTML guide path %s", record.Path)
		}
		folded[strings.ToLower(record.Path)] = true
		allowed[record.Path] = true
		hash, size, err := storage.Hash(r.root, path.Join(directory, record.Path), 1<<30)
		if err != nil || hash != record.SHA256 || size != record.Size {
			return fmt.Errorf("HTML guide file integrity mismatch at %s: %v", record.Path, err)
		}
	}
	for generated, expected := range map[string]string{
		"index.html":      manifest.Integrity.IndexSHA256,
		"toc.html":        manifest.Integrity.TOCSHA256,
		"search.json":     manifest.Integrity.SearchSHA256,
		"search-index.js": manifest.Integrity.SearchIndexSHA256,
		"search.js":       manifest.Integrity.SearchJSSHA256,
	} {
		hash, _, err := storage.Hash(r.root, path.Join(directory, generated), 64<<20)
		if err != nil || expected == "" || hash != expected {
			return fmt.Errorf("HTML generated-file integrity mismatch at %s: %v", generated, err)
		}
	}
	files, err := managedFiles(r.root, directory, "")
	if err != nil {
		return err
	}
	if len(files) != len(allowed) {
		return fmt.Errorf("unexpected/missing files in HTML guide %s", directory)
	}
	for _, name := range files {
		if !allowed[name] {
			return fmt.Errorf("unrecognized HTML guide entry %s", name)
		}
	}
	return nil
}

func (r *Run) verifyMissingResources(directory string, manifest *model.HTMLArchive) error {
	if manifest.Status == "complete" && len(manifest.MissingResources) != 0 {
		return fmt.Errorf("complete HTML guide has image placeholders: %s", directory)
	}
	if manifest.Status == "degraded" && len(manifest.MissingResources) == 0 {
		return fmt.Errorf("degraded HTML guide has no image placeholders: %s", directory)
	}
	topics := make(map[string]model.FileRecord, len(manifest.Topics))
	for _, topic := range manifest.Topics {
		topics[topic.Path] = topic
	}
	for _, missing := range manifest.MissingResources {
		if _, err := fetch.NormalizeURL(missing.RequestedURL); err != nil {
			return fmt.Errorf("invalid missing-resource URL: %w", err)
		}
		if missing.FinalURL != "" {
			if _, err := fetch.NormalizeURL(missing.FinalURL); err != nil {
				return fmt.Errorf("invalid missing-resource final URL: %w", err)
			}
		}
		topic, ok := topics[missing.GeneratedPagePath]
		allowedClass := missing.FailureClass == "http-404" || missing.FailureClass == "http-410" ||
			missing.FailureClass == "timeout" || missing.FailureClass == "retry-exhausted" ||
			missing.FailureClass == "incomplete-body" || missing.FailureClass == "invalid-raster" ||
			missing.FailureClass == "mime-mismatch"
		allowedRaster := missing.ExpectedRasterType == "png" || missing.ExpectedRasterType == "jpeg" ||
			missing.ExpectedRasterType == "gif" || missing.ExpectedRasterType == "webp" ||
			missing.ExpectedRasterType == "avif"
		_, observedErr := time.Parse(time.RFC3339Nano, missing.ObservedAt)
		if !ok || topic.SHA256 == "" || topic.SHA256 != missing.GeneratedPageSHA256 ||
			topic.URL != missing.ReferringTopicURL || !sha256Pattern.MatchString(missing.GeneratedPageSHA256) ||
			!strings.HasPrefix(missing.GeneratedPagePath, "pages/") ||
			(missing.ElementType != "img" && missing.ElementType != "picture") ||
			!allowedClass || !allowedRaster || missing.Message == "" ||
			!strings.HasPrefix(missing.PlaceholderID, "image-placeholder-") ||
			missing.ReferringTopicURL == "" || observedErr != nil ||
			(strings.HasPrefix(missing.FailureClass, "http-") &&
				missing.HTTPStatus != httpStatusFromFailureClass(missing.FailureClass)) {
			return fmt.Errorf("invalid missing-resource provenance at %s", directory)
		}
		file, err := r.root.Open(path.Join(directory, missing.GeneratedPagePath))
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(file, 1<<30))
		err = errors.Join(readErr, file.Close())
		if err != nil ||
			!bytes.Contains(body, []byte(`id="`+missing.PlaceholderID+`"`)) ||
			!bytes.Contains(body, []byte(`class="archive-image-placeholder"`)) {
			return fmt.Errorf("missing image placeholder integrity mismatch at %s: %v", missing.GeneratedPagePath, err)
		}
	}
	return nil
}

func httpStatusFromFailureClass(class string) int {
	switch class {
	case "http-404":
		return 404
	case "http-410":
		return 410
	default:
		return 0
	}
}

func validGeneratedPDFInput(result model.ArchiveResult, generated *model.GeneratedPDFRecord) bool {
	switch result.Status {
	case "complete":
		return (generated.InputStatus == "" || generated.InputStatus == "complete") &&
			generated.ImagePlaceholders == 0
	case "degraded":
		return generated.InputStatus == "degraded" &&
			generated.ImagePlaceholders == model.ImagePlaceholderCount(result.MissingResources)
	default:
		return false
	}
}

func missingResourceSubset(candidate, previous []model.MissingResource) bool {
	prior := make(map[string]bool, len(previous))
	for _, missing := range previous {
		prior[missing.RequestedURL] = true
	}
	for _, missing := range candidate {
		if !prior[missing.RequestedURL] {
			return false
		}
	}
	return true
}

func cloneMissingResources(values []model.MissingResource) []model.MissingResource {
	return slices.Clone(values)
}

func validGeneratedPDFTimestamp(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func validGeneratedPDFValidation(validation model.GeneratedPDFValidation) bool {
	return validation.StructuralValidated &&
		validation.PageCount > 0 &&
		validation.LetterMediaBoxes == validation.PageCount &&
		validation.FontObjects > 0 &&
		validation.ImageObjects >= 0 &&
		validation.AnnotationObjects >= 0 &&
		validation.OutlineObjects > 0 &&
		validation.TOCEntries >= 0 &&
		validation.TOCLinksChecked == validation.TOCEntries &&
		validation.LocalRequests > 0 &&
		validation.InlineDataRequests >= 0 &&
		validation.ScriptElements >= 0 &&
		validation.LoadedImages >= 0 &&
		validation.InternalLinksChecked >= 0 &&
		validation.FailedImages == 0 &&
		len(validation.FailedImageDetails) == 0 &&
		validation.MissingTargets == 0 &&
		validation.ExternalRequests == 0 &&
		validation.CanonicalCoverTitles == 1 &&
		validation.PublisherChromeElements == 0 &&
		validation.FixedOrStickyElements == 0 &&
		validation.OutlineEntries >= 3 &&
		validation.OutlineMaxDepth >= 1 &&
		validation.OutlineExternalURIs >= 0 &&
		validation.OutlineRepeatedTargets >= 0 &&
		validation.OutlineSupplementaryGroups >= 0 &&
		validation.FontsLoaded &&
		validation.ScriptExecutionDisabled &&
		validation.ResourceClosureValidated &&
		validation.CoverTypographyValidated &&
		validation.HeadingTypographyValidated &&
		validation.HeadingPaginationValidated &&
		validation.FooterOverlayValidated &&
		validation.OutlineValidated
}

func managedFiles(root *os.Root, base, relative string) ([]string, error) {
	name := base
	if relative != "" {
		name = path.Join(base, relative)
	}
	if err := storage.Check(root, name); err != nil {
		return nil, err
	}
	dir, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	entries, err = withoutFinderMetadata(root, name, entries)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		child := path.Join(relative, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symbolic link in HTML guide: %s", child)
		}
		if entry.IsDir() {
			nested, err := managedFiles(root, base, child)
			if err != nil {
				return nil, err
			}
			files = append(files, nested...)
		} else if entry.Type().IsRegular() {
			files = append(files, child)
		} else {
			return nil, fmt.Errorf("special file in HTML guide: %s", child)
		}
	}
	return files, nil
}

func sourceDigests(result model.ArchiveResult) map[string]string {
	digests := map[string]string{}
	if result.PDF != nil {
		digests[result.PDF.URL] = result.PDF.SourceSHA256
	}
	if result.HTML != nil {
		for _, input := range result.HTML.Inputs {
			if input.SHA256 != "" {
				digests["input:"+input.Role+":"+input.RequestedURL] = input.SHA256
			}
		}
		for _, record := range append(append([]model.FileRecord{}, result.HTML.Topics...), result.HTML.Assets...) {
			if record.SourceSHA256 != "" {
				digests[record.URL] = record.SourceSHA256
			}
		}
		for _, missing := range result.HTML.MissingResources {
			digests["missing:"+missing.RequestedURL] = missing.FailureClass + ":" + missing.FinalURL
		}
	}
	return digests
}

func (r *Run) readLibrary(directory string) (Manifest, error) {
	return r.readLibraryTree(directory)
}

func (r *Run) readLibraryForZIP(directory string) (Manifest, error) {
	return r.readLibraryTree(directory)
}

func (r *Run) readLibraryTree(directory string) (Manifest, error) {
	var manifest Manifest
	if err := storage.Check(r.root, directory); err != nil {
		return manifest, err
	}
	if err := storage.ReadJSON(r.root, path.Join(directory, "manifest.json"), &manifest); err != nil {
		return manifest, fmt.Errorf("existing directory is not a recognized native library: %s: %w", directory, err)
	}
	if manifest.Application != Application || manifest.SchemaVersion != 1 || manifest.NativeSchemaVersion != LibraryNativeSchema ||
		manifest.Platform != r.platform || manifest.Version != r.version || !runPattern.MatchString(manifest.RunID) ||
		manifest.PDFGuides == nil || len(manifest.Guides) != len(manifest.PDFGuides)+len(manifest.HTMLGuides) ||
		(manifest.Status != "complete" && manifest.Status != "degraded" && manifest.Status != "incomplete") {
		if directory == r.target {
			return manifest, incompatibleLibraryError(filepath.Join(r.Destination, filepath.FromSlash(directory)),
				"native manifest schema or identity is invalid")
		}
		return manifest, fmt.Errorf("invalid staged native library schema or identity at %s", directory)
	}
	policy := r.versionPolicy
	if policy.output == "" {
		policy, _ = defaultNativeVersionPolicy()
	}
	if err := policy.supports(manifest.ApplicationVersion); err != nil {
		if directory == r.target {
			return manifest, incompatibleLibraryError(
				filepath.Join(r.Destination, filepath.FromSlash(directory)), err.Error(),
			)
		}
		return manifest, fmt.Errorf("invalid staged native library application version at %s: %w", directory, err)
	}
	if manifest.UpgradedFromVersion != "" {
		upgradedFrom, err := parseApplicationVersion(manifest.UpgradedFromVersion)
		current, currentErr := parseApplicationVersion(manifest.ApplicationVersion)
		if err != nil || currentErr != nil || !applicationVersionBefore(upgradedFrom, current) {
			return manifest, fmt.Errorf("invalid upgraded-from application version at %s", directory)
		}
	}
	allowed := map[string]bool{"manifest.json": true, "history.json": true, "index.html": true, "search-index.js": true, "search.js": true}
	folded := map[string]bool{}
	for key, result := range manifest.PDFGuides {
		component, err := storage.SafeComponent(result.Document.ID)
		fold := strings.ToLower(key)
		if err != nil || component != key || folded[fold] || result.Document.Platform != r.platform || result.Document.Version != r.version {
			return manifest, fmt.Errorf("invalid/colliding guide identity in %s", directory)
		}
		folded[fold] = true
		allowed[key] = true
		if err := r.verifyGuide(path.Join(directory, key), result); err != nil {
			return manifest, err
		}
	}
	for key, result := range manifest.HTMLGuides {
		component, err := storage.SafeComponent(result.Document.ID)
		fold := strings.ToLower(key)
		if err != nil || component != key || folded[fold] || result.Document.Platform != r.platform || result.Document.Version != r.version {
			return manifest, fmt.Errorf("invalid/colliding guide identity in %s", directory)
		}
		folded[fold] = true
		allowed[key] = true
		if err := r.verifyGuide(path.Join(directory, key), result); err != nil {
			return manifest, err
		}
	}
	summaries := map[string]bool{}
	for _, guide := range manifest.Guides {
		result, ok := manifest.PDFGuides[guide.Path]
		if !ok {
			result, ok = manifest.HTMLGuides[guide.Path]
		}
		if !ok || summaries[guide.Path] || guide.ID != result.Document.ID || guide.Title != result.Document.Title ||
			guide.Status != result.Status || guide.Format != result.Format || guide.SourceURL != result.Document.URL ||
			guide.RouteOrigin != result.Document.RouteOrigin {
			return manifest, fmt.Errorf("inconsistent central guide summary in %s", directory)
		}
		summaries[guide.Path] = true
	}
	skippedIDs := map[string]bool{}
	for _, skipped := range manifest.SkippedUnavailable {
		if skipped.ID == "" || skipped.Title == "" || skipped.Kind == "" ||
			skipped.MappedSourceURL == "" || !skipped.Checked || skipped.Available ||
			skipped.Reason == "" || skippedIDs[skipped.ID] {
			return manifest, fmt.Errorf("invalid skipped unavailable guide metadata in %s", directory)
		}
		skippedIDs[skipped.ID] = true
		_, retainedPDF := manifest.PDFGuides[skipped.ID]
		_, retainedHTML := manifest.HTMLGuides[skipped.ID]
		if skipped.RetainedPrevious != (retainedPDF || retainedHTML) {
			return manifest, fmt.Errorf("inconsistent skipped guide retention in %s", directory)
		}
	}
	if manifest.Status != versionStatus(manifest.Errors, manifest.Attempts, manifest.Guides) {
		return manifest, fmt.Errorf("inconsistent native library status in %s", directory)
	}
	dir, err := r.root.Open(directory)
	if err != nil {
		return manifest, err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return manifest, err
	}
	entries, err = withoutFinderMetadata(r.root, directory, entries)
	if err != nil {
		return manifest, err
	}
	if len(entries) != len(allowed) {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return manifest, fmt.Errorf("unexpected/missing files in managed library %s: entries=%v expected=%v", directory, names, slices.Sorted(maps.Keys(allowed)))
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return manifest, fmt.Errorf("unrecognized library entry %s", entry.Name())
		}
		if err := storage.Check(r.root, path.Join(directory, entry.Name())); err != nil {
			return manifest, err
		}
		_, pdfGuide := manifest.PDFGuides[entry.Name()]
		_, htmlGuide := manifest.HTMLGuides[entry.Name()]
		if !pdfGuide && !htmlGuide && !entry.Type().IsRegular() {
			return manifest, fmt.Errorf("invalid generated library file %s", entry.Name())
		}
	}
	return manifest, nil
}

func versionStatus(errors []string, attempts []Attempt, guides []Guide) string {
	if len(errors) > 0 || slices.ContainsFunc(attempts, func(attempt Attempt) bool {
		return attempt.Status != "complete" && attempt.Status != "degraded"
	}) {
		return "incomplete"
	}
	if slices.ContainsFunc(attempts, func(attempt Attempt) bool { return attempt.Status == "degraded" }) ||
		slices.ContainsFunc(guides, func(guide Guide) bool { return guide.Status == "degraded" }) {
		return "degraded"
	}
	return "complete"
}

func withoutFinderMetadata(root *os.Root, directory string, entries []os.DirEntry) ([]os.DirEntry, error) {
	filtered := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() != ".DS_Store" {
			filtered = append(filtered, entry)
			continue
		}
		info, err := root.Lstat(path.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Finder metadata is not a regular managed file: %s", path.Join(directory, entry.Name()))
		}
		if info.Size() > finderMetadataMaxBytes {
			return nil, fmt.Errorf("Finder metadata exceeds %d bytes: %s", finderMetadataMaxBytes, path.Join(directory, entry.Name()))
		}
	}
	return filtered, nil
}

func copyPublishedEntry(name string, info os.FileInfo) (bool, error) {
	if path.Base(name) != ".DS_Store" {
		return true, nil
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("Finder metadata is not a regular managed file: %s", name)
	}
	if info.Size() > finderMetadataMaxBytes {
		return false, fmt.Errorf("Finder metadata exceeds %d bytes: %s", finderMetadataMaxBytes, name)
	}
	return false, nil
}

func (r *Run) recover() error {
	return r.recoverContext(context.Background())
}

func (r *Run) recoverContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkOwnership(); err != nil {
		return err
	}
	if _, err := r.root.Lstat(r.journal); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	state, err := r.readJournal()
	if err != nil {
		return fmt.Errorf("cannot inspect publication journal %s; retain it and staging for manual recovery: %w", r.journal, err)
	}
	if err := r.checkTransaction(state); err != nil {
		return err
	}
	switch state.transaction.NativeSchemaVersion {
	case LegacyJournalNativeSchema:
		return r.recoverLegacyPublication(state)
	case UpgradeJournalNativeSchema:
		return errors.New("upgrade journal schema is unsupported by this application version")
	default:
		return errors.New("invalid or incompatible recovery journal; manual inspection required")
	}
}

func (r *Run) recoverLegacyPublication(state journalState) error {
	tx := state.transaction
	v := path.Base(r.target)
	expectedStage := path.Join(r.parent, ".staging", v+"-"+tx.RunID)
	expectedSnapshot := path.Join(r.parent, ".snapshots", v, tx.RunID)
	if tx.NativeSchemaVersion != LegacyJournalNativeSchema || (tx.Application != "" && tx.Application != Application) ||
		tx.Operation != "" || tx.FromApplicationVersion != "" || tx.ToApplicationVersion != "" ||
		tx.Platform != "" || tx.Version != "" || tx.OwnerToken != "" ||
		tx.SnapshotDigest != "" || tx.StageDigest != "" ||
		!runPattern.MatchString(tx.RunID) || tx.Stage != expectedStage ||
		(tx.PreviousSnapshot != "" && tx.PreviousSnapshot != expectedSnapshot) ||
		((tx.PreviousSnapshot == "") != (tx.PreviousRun == "")) ||
		(tx.PreviousRun != "" && !runPattern.MatchString(tx.PreviousRun)) {
		return errors.New("invalid or incompatible publication recovery journal; manual inspection required")
	}
	if _, err := r.root.Lstat(r.target); err == nil {
		current, err := r.readLibrary(r.target)
		if err != nil {
			return fmt.Errorf("cannot expose interrupted publication without verified hashes: %w", err)
		}
		if current.RunID != tx.RunID && current.RunID != tx.PreviousRun {
			return errors.New("published library identity changed during interrupted transaction")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		recoverFrom := tx.PreviousSnapshot
		if recoverFrom == "" {
			recoverFrom = tx.Stage
		}
		recovered, err := r.readLibrary(recoverFrom)
		if err != nil {
			return fmt.Errorf("interrupted publication requires manual recovery: %w", err)
		}
		expectedRun := tx.PreviousRun
		if tx.PreviousSnapshot == "" {
			expectedRun = tx.RunID
		}
		if recovered.RunID != expectedRun {
			return errors.New("recovery library identity does not match publication journal")
		}
		recovery := path.Join(r.parent, ".staging", v+"-recovery-"+rand.Text())
		if err := r.checkTransaction(state); err != nil {
			return err
		}
		if err := storage.CopyTree(r.root, recoverFrom, recovery); err != nil {
			return err
		}
		if err := r.renameTransaction(state, recovery, r.target); err != nil {
			return err
		}
		if err := errors.Join(storage.SyncDir(r.root, r.parent), storage.SyncDir(r.root, path.Dir(recovery))); err != nil {
			return err
		}
	} else {
		return err
	}
	return r.removeJournal(state)
}

func (r *Run) Publish(cancelled bool) (Manifest, error) {
	if r.published {
		return Manifest{}, errors.New("library transaction was already published")
	}
	if err := r.checkOwnership(); err != nil {
		return Manifest{}, err
	}
	if err := preflightRootWithPolicy(
		context.Background(),
		r.root,
		r.Destination,
		r.parent,
		path.Base(r.target),
		r.platform,
		r.version,
		r.versionPolicy,
	); err != nil {
		return Manifest{}, err
	}
	if err := r.expectNoJournal(); err != nil {
		return Manifest{}, err
	}
	if cancelled {
		r.errors = append(r.errors, "Download cancelled; not all selected guides were processed.")
	}
	work := path.Join(r.stage, ".work")
	if _, err := r.root.Stat(work); err == nil {
		incomplete := path.Join(r.parent, ".incomplete", path.Base(r.target)+"-"+r.stamp)
		dir, err := r.root.Open(work)
		if err != nil {
			return Manifest{}, err
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return Manifest{}, err
		}
		if len(entries) > 0 {
			if err := r.checkOwnership(); err != nil {
				return Manifest{}, err
			}
			if err := storage.EnsureDir(r.root, incomplete); err != nil {
				return Manifest{}, err
			}
			if err := r.rename(work, path.Join(incomplete, "unfinished")); err != nil {
				return Manifest{}, err
			}
			if err := r.checkOwnership(); err != nil {
				return Manifest{}, err
			}
			r.errors = append(r.errors, "Unfinished work retained at "+filepath.Join(r.Destination, filepath.FromSlash(incomplete)))
		} else if err := r.root.Remove(work); err != nil {
			return Manifest{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	snapshot := ""
	if r.previous.RunID != "" {
		current, err := r.readLibrary(r.target)
		if err != nil || !reflect.DeepEqual(current, r.previous) {
			return Manifest{}, fmt.Errorf("previous library changed during transaction: %v", err)
		}
		snapshot = path.Join(r.parent, ".snapshots", path.Base(r.target), r.stamp)
		if err := r.checkOwnership(); err != nil {
			return Manifest{}, err
		}
		if err := storage.EnsureDir(r.root, path.Dir(snapshot)); err != nil {
			return Manifest{}, err
		}
	} else if _, err := r.root.Lstat(r.target); err == nil {
		return Manifest{}, errors.New("destination version appeared during transaction; refusing to replace it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	changes := map[string]Change{}
	for key, current := range r.guides {
		change := Change{Added: []string{}, Removed: []string{}, Changed: []string{}}
		old, found := r.previous.PDFGuides[key]
		if !found {
			old, found = r.previous.HTMLGuides[key]
		}
		if !found {
			change.Added = append(change.Added, current.Document.URL)
		} else if old.Document.URL != current.Document.URL || old.Format != current.Format {
			change.Added = append(change.Added, current.Document.URL)
			change.Removed = append(change.Removed, old.Document.URL)
		} else if !reflect.DeepEqual(sourceDigests(old), sourceDigests(current)) {
			change.Changed = append(change.Changed, current.Document.URL)
		}
		changes[key] = change
	}
	upgradedFrom := r.previous.UpgradedFromVersion
	if upgradedFrom == "" && r.previous.ApplicationVersion != "" && r.previous.ApplicationVersion != r.versionPolicy.output {
		upgradedFrom = r.previous.ApplicationVersion
	}
	history := append(r.history, History{At: time.Now().UTC().Format(time.RFC3339Nano), ApplicationVersion: r.versionPolicy.output,
		PreviousSnapshot: strings.TrimPrefix(snapshot, r.parent+"/"), Changes: changes, Attempts: r.attempts,
		SkippedUnavailable: slices.Clone(r.SkippedUnavailable), UpgradedFromVersion: upgradedFrom})
	manifest := Manifest{Application: Application, ApplicationVersion: r.versionPolicy.output, SchemaVersion: 1, NativeSchemaVersion: LibraryNativeSchema,
		RunID: r.stamp, Platform: r.platform, Version: r.version, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Status: "complete", PDFGuides: map[string]model.ArchiveResult{}, HTMLGuides: map[string]model.ArchiveResult{},
		Attempts: r.attempts, Errors: r.errors, Guides: []Guide{},
		CatalogueURL: r.CatalogueURL, CatalogueFetchedAt: r.CatalogueFetchedAt}
	manifest.CatalogueWarnings = r.CatalogueWarnings
	manifest.SkippedUnavailable = slices.Clone(r.SkippedUnavailable)
	manifest.UpgradedFromVersion = upgradedFrom
	keys := make([]string, 0, len(r.guides))
	for key := range r.guides {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		result := r.guides[key]
		if result.Format == "pdf" {
			manifest.PDFGuides[key] = result
		} else {
			manifest.HTMLGuides[key] = result
		}
		topicCount, assetCount := 0, 0
		if result.HTML != nil {
			topicCount, assetCount = len(result.HTML.Topics), len(result.HTML.Assets)
		}
		manifest.Guides = append(manifest.Guides, Guide{ID: result.Document.ID, Title: result.Document.Title, Path: key, Format: result.Format,
			Status: result.Status, SourceURL: result.Document.URL, RouteOrigin: result.Document.RouteOrigin,
			TopicCount: topicCount, AssetCount: assetCount,
			PlaceholderCount: model.ImagePlaceholderCount(result.MissingResources)})
	}
	manifest.Status = versionStatus(manifest.Errors, manifest.Attempts, manifest.Guides)
	if err := r.localizeCrossGuideLinks(&manifest); err != nil {
		return Manifest{}, err
	}
	if err := r.writeIndex(manifest, history); err != nil {
		return Manifest{}, err
	}
	if _, err := r.readLibrary(r.stage); err != nil {
		return Manifest{}, err
	}
	tx := journal{Application: Application, NativeSchemaVersion: LegacyJournalNativeSchema, RunID: r.stamp, PreviousRun: r.previous.RunID, Stage: r.stage, PreviousSnapshot: snapshot}
	state, err := r.createJournal(tx)
	if err != nil {
		return Manifest{}, err
	}
	if snapshot != "" {
		if err := r.renameTransaction(state, r.target, snapshot); err != nil {
			return Manifest{}, err
		}
		if err := errors.Join(storage.SyncDir(r.root, r.parent), storage.SyncDir(r.root, path.Dir(snapshot))); err != nil {
			rollback := r.rollback(state)
			return Manifest{}, errors.Join(err, rollback, storage.SyncDir(r.root, r.parent))
		}
	}
	if err := r.renameTransaction(state, r.stage, r.target); err != nil {
		var rollback error
		if snapshot != "" {
			rollback = r.rollback(state)
		}
		return Manifest{}, fmt.Errorf("publication stopped; inspect target %s, staging %s and snapshot %s: %w",
			r.Target, r.Stage, snapshot, errors.Join(err, rollback))
	}
	if err := errors.Join(storage.SyncDir(r.root, r.parent), storage.SyncDir(r.root, path.Dir(r.stage))); err != nil {
		return Manifest{}, err
	}
	if err := r.removeJournal(state); err != nil {
		return Manifest{}, err
	}
	r.published = true
	return manifest, nil
}
