package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/pdfgen"
	"aos-cx-docs-dldr/internal/source"
	"aos-cx-docs-dldr/internal/storage"
)

type preflightFreshFetcher struct {
	base  model.Fetcher
	mu    sync.Mutex
	fresh map[string]bool
}

func (f *preflightFreshFetcher) Get(
	ctx context.Context,
	raw string,
	refresh bool,
) (model.Resource, error) {
	refresh = f.consumeFresh(raw, refresh)
	return f.base.Get(ctx, raw, refresh)
}

func (f *preflightFreshFetcher) Prefetch(ctx context.Context, raw string, refresh bool) error {
	prefetcher, ok := f.base.(interface {
		Prefetch(context.Context, string, bool) error
	})
	if !ok {
		return fmt.Errorf("raw-cache fetcher does not support disk prefetch")
	}
	return prefetcher.Prefetch(ctx, raw, f.consumeFresh(raw, refresh))
}

func (f *preflightFreshFetcher) consumeFresh(raw string, refresh bool) bool {
	if refresh {
		f.mu.Lock()
		if f.fresh[raw] {
			delete(f.fresh, raw)
			refresh = false
		}
		f.mu.Unlock()
	}
	return refresh
}

func expandGuides(args []string) []string {
	var expanded []string
	for i := 0; i < len(args); i++ {
		if args[i] != "--guides" {
			expanded = append(expanded, args[i])
			continue
		}
		start := i + 1
		for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			expanded = append(expanded, "--guides="+args[i])
		}
		if i+1 == start {
			expanded = append(expanded, "--guides")
		}
	}
	return expanded
}

func pdfSelection(catalog model.Catalog, o options) ([]model.DocumentPlan, error) {
	documents, err := selectedDocuments(catalog, o)
	if err != nil {
		return nil, err
	}
	var plans []model.DocumentPlan
	for _, document := range documents {
		plan, err := source.PublisherPDFPlan(document)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func selectedDocuments(catalog model.Catalog, o options) ([]model.Document, error) {
	documents, diagnostics, err := source.ResolveDocuments(catalog, o.platform, o.version)
	if err != nil {
		return nil, err
	}
	if len(documents) == 0 {
		if len(diagnostics) > 0 {
			return nil, fmt.Errorf("cannot establish selected catalogue mappings: %s", strings.Join(diagnostics, "; "))
		}
		return nil, fmt.Errorf("version %q is not available for platform %q. Available versions: %s",
			o.version, o.platform, strings.Join(source.AvailableVersions(catalog, o.platform), ", "))
	}
	if o.all {
		if len(diagnostics) > 0 {
			return nil, fmt.Errorf(
				"cannot determine all mapped guides because catalogue mappings or routes failed: %s; retry discovery or select explicit known guide IDs",
				strings.Join(diagnostics, "; "),
			)
		}
		if !o.allAvailableSnapshot {
			o.guides = make([]string, 0, len(documents))
			for _, document := range documents {
				o.guides = append(o.guides, document.ID)
			}
		}
	}
	known := map[string]model.Document{}
	for _, document := range documents {
		known[document.ID] = document
	}
	seen := map[string]bool{}
	paths := map[string]string{}
	var selected []model.Document
	for _, id := range o.guides {
		if seen[id] {
			continue
		}
		seen[id] = true
		document, ok := known[id]
		if !ok {
			return nil, fmt.Errorf("guide %q is not resolved for %s/%s; use --list to inspect mappings and errors", id, o.platform, o.version)
		}
		if document.Kind != "pdf" && document.Kind != "flare" && document.Kind != "hpe" && document.Kind != "static" {
			return nil, fmt.Errorf("guide %q uses unsupported document kind %s", id, document.Kind)
		}
		name, err := storage.SafeComponent(id)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(name)
		if existing, ok := paths[key]; ok {
			return nil, fmt.Errorf("guide IDs %q and %q collide on a portable filesystem", existing, id)
		}
		paths[key] = id
		selected = append(selected, document)
	}
	if len(selected) == 0 {
		return nil, errors.New("select at least one guide with --guides ID ... or --all")
	}
	return selected, nil
}

type DownloadOutput struct {
	Library  string           `json:"library"`
	ZIP      string           `json:"zip,omitempty"`
	Manifest library.Manifest `json:"manifest"`
}

func downloadGuides(ctx context.Context, o options, catalog model.Catalog, fetcher model.Fetcher, out, stderr io.Writer) (code int, err error) {
	return downloadGuidesObserved(
		ctx, o, catalog, fetcher, out, stderr, nil, nil, newHumanPresentation(out),
	)
}

func downloadGuidesObserved(
	ctx context.Context,
	o options,
	catalog model.Catalog,
	fetcher model.Fetcher,
	out, stderr io.Writer,
	progress *progressReporter,
	store *cache.Store,
	outPresentation humanPresentation,
) (code int, err error) {
	errPresentation := newHumanPresentation(stderr)
	if progress != nil {
		errPresentation = progress.presentation
	}
	documents, err := selectedDocuments(catalog, o)
	if err != nil {
		return 1, err
	}
	if err := ctx.Err(); err != nil {
		return 130, err
	}
	for _, message := range catalog.Warnings {
		fmt.Fprintln(stderr, errPresentation.warning("Catalogue warning: "+message))
	}
	run, err := library.Open(o.destination, o.platform, o.version)
	if err != nil {
		return 1, err
	}
	defer func() {
		if closeErr := run.Close(); closeErr != nil {
			fmt.Fprintln(stderr, errPresentation.failure("Library cleanup error: "+closeErr.Error()))
			err = errors.Join(err, closeErr)
			if code == 0 {
				code = 1
			}
		}
	}()
	run.CatalogueURL, run.CatalogueFetchedAt = catalog.SourceURL, catalog.FetchedAt
	run.CatalogueWarnings = append([]string{}, catalog.Warnings...)
	run.SetSkippedUnavailable(o.skippedUnavailable)
	if len(run.SkippedUnavailable) > 0 {
		names := make([]string, 0, len(run.SkippedUnavailable))
		for _, skipped := range run.SkippedUnavailable {
			names = append(names, skipped.Title)
		}
		summary := fmt.Sprintf(
			"Downloading %d available mapped guides; skipped %d unavailable: %s.",
			len(documents), len(run.SkippedUnavailable), strings.Join(names, ", "),
		)
		if progress != nil {
			progress.Notice(summary)
		} else {
			fmt.Fprintln(stderr, errPresentation.notice(summary))
		}
	}
	cancelled := false
	for index, document := range documents {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		output, err := run.GuideOutput(document.ID)
		if err != nil {
			return 1, err
		}
		if progress != nil {
			progress.Guide(index+1, len(documents), document.Title, "staging at "+output)
		} else {
			fmt.Fprintln(stderr, errPresentation.phase("Staging: "+output))
		}
		var result model.ArchiveResult
		var archiveErr error
		archiveHTML := func(plan model.DocumentPlan) (model.ArchiveResult, error) {
			var archived model.ArchiveResult
			var err error
			if progress == nil {
				archived, err = archive.HTMLWithWorkers(ctx, plan, fetcher, output, o.refresh,
					int64(o.maxMB*(1<<20)), int64(o.maxArchiveMB*(1<<20)), o.workerCount())
			} else {
				progress.Guide(index+1, len(documents), document.Title,
					fmt.Sprintf("plan complete; %d authoritative topics", len(plan.Topics)))
				progress.Guide(index+1, len(documents), document.Title, "building offline guide")
				archived, err = archive.HTMLWithWorkersObserved(ctx, plan, fetcher, output, o.refresh,
					int64(o.maxMB*(1<<20)), int64(o.maxArchiveMB*(1<<20)), o.workerCount(),
					func(event archive.Progress) {
						progress.HTML(index+1, len(documents), document.Title, event)
					})
			}
			if err != nil || (archived.Status != "complete" && archived.Status != "degraded") || !o.convertHTMLToPDF {
				return archived, err
			}
			if progress != nil {
				progress.Guide(index+1, len(documents), document.Title, "rendering companion PDF with pinned Chrome")
			}
			pdfConfig := pdfgen.DefaultConfig(o.chromePath)
			pdfConfig.QPDFPath = o.qpdfPath
			record, renderErr := pdfgen.Generate(ctx, output, plan, &archived, pdfConfig)
			if renderErr == nil {
				if progress == nil {
					fmt.Fprintf(stderr, "%s\n", errPresentation.success(fmt.Sprintf(
						"Generated PDF: %s (%d pages, %.1f MiB)",
						record.Path, record.Validation.PageCount, float64(record.Size)/(1<<20),
					)))
				}
				return archived, nil
			}
			recordErr := pdfgen.RecordFailure(output, &archived, record)
			message := record.Error
			if recordErr != nil {
				message += "; record failure: " + recordErr.Error()
				archived.Status = "incomplete"
			}
			archived.Errors = append(archived.Errors, message)
			return archived, errors.Join(renderErr, recordErr)
		}
		if document.Kind == "pdf" {
			if progress != nil {
				progress.Guide(index+1, len(documents), document.Title, "downloading publisher PDF")
			}
			plan, planErr := source.PublisherPDFPlan(document)
			if planErr != nil {
				archiveErr = planErr
				result = model.ArchiveResult{Document: document, OutputDir: output, Status: "failed", Format: "pdf", Errors: []string{planErr.Error()}}
			} else {
				result, archiveErr = archive.PublisherPDF(ctx, plan, fetcher, output, o.refresh, int64(o.maxMB*(1<<20)))
			}
		} else {
			if progress != nil {
				progress.Guide(index+1, len(documents), document.Title, "discovering complete source plan")
			}
			plan, planErr := source.LoadPlanWithPreference(ctx, fetcher, document, o.refresh, o.preferPDF)
			if planErr != nil {
				archiveErr = planErr
				result = model.ArchiveResult{Document: document, OutputDir: output, Status: "failed", Format: "html", Errors: []string{planErr.Error()}}
			} else {
				switch plan.Document.Kind {
				case "pdf":
					if planErr = archive.ValidateResolvedPublisherPDFPlan(plan); planErr != nil {
						archiveErr = planErr
						result = model.ArchiveResult{Document: plan.Document, OutputDir: output, Status: "failed",
							Format: "pdf", Errors: []string{planErr.Error()}}
						break
					}
					if progress != nil {
						progress.Guide(index+1, len(documents), document.Title, "downloading source-verified publisher PDF")
					}
					result, archiveErr = archive.PublisherPDF(ctx, plan, fetcher, output, o.refresh, int64(o.maxMB*(1<<20)))
					if archiveErr != nil && plan.PDFPreferred &&
						!errors.Is(archiveErr, context.Canceled) && !errors.Is(archiveErr, context.DeadlineExceeded) &&
						ctx.Err() == nil {
						preferenceErr := archiveErr
						htmlPlan, htmlPlanErr := source.LoadPlanWithPreference(ctx, fetcher, document, false, false)
						if htmlPlanErr != nil {
							archiveErr = htmlPlanErr
							result = model.ArchiveResult{Document: document, OutputDir: output, Status: "failed",
								Format: "html", Errors: []string{htmlPlanErr.Error()}}
							break
						}
						if htmlPlan.Document.Kind != "flare" && htmlPlan.Document.Kind != "hpe" && htmlPlan.Document.Kind != "static" {
							archiveErr = fmt.Errorf("preferred PDF fallback resolved unsupported document kind %q", htmlPlan.Document.Kind)
							result = model.ArchiveResult{Document: htmlPlan.Document, OutputDir: output, Status: "failed",
								Format: "html", Errors: []string{archiveErr.Error()}}
							break
						}
						htmlPlan.Warnings = append(htmlPlan.Warnings,
							fmt.Sprintf("Preferred source-verified PDF failed after verification (%s); archived complete HTML instead.", preferenceErr))
						result, archiveErr = archiveHTML(htmlPlan)
					}
				case "flare", "hpe", "static":
					result, archiveErr = archiveHTML(plan)
				default:
					archiveErr = fmt.Errorf("source plan resolved unsupported document kind %q", plan.Document.Kind)
					result = model.ArchiveResult{Document: plan.Document, OutputDir: output, Status: "failed",
						Format: "html", Errors: []string{archiveErr.Error()}}
				}
			}
		}
		if err := run.Accept(result); err != nil {
			return 1, err
		}
		generatedPDFFailed := (result.Status == "complete" || result.Status == "degraded") && result.Format == "html" &&
			result.HTML != nil && result.HTML.GeneratedPDF != nil &&
			result.HTML.GeneratedPDF.Status == "failed"
		if archiveErr != nil {
			message := fmt.Sprintf("Guide %s failed: %s", document.ID, archiveErr)
			if generatedPDFFailed {
				message = fmt.Sprintf("Guide %s HTML %s; companion PDF failed: %s", document.ID, result.Status, archiveErr)
			}
			if progress != nil {
				progress.Error(message)
			} else {
				fmt.Fprintln(stderr, errPresentation.failure(message))
			}
		}
		reportGuideDiagnostics(progress, stderr, errPresentation, document.ID, result, archiveErr)
		if progress != nil {
			topics, assets := 0, 0
			if result.HTML != nil {
				topics, assets = len(result.HTML.Topics), len(result.HTML.Assets)
			}
			progress.GuideResult(
				index+1, len(documents), document.Title, result.Status, topics, assets,
				model.ImagePlaceholderCount(result.MissingResources),
			)
		}
		if ctx.Err() != nil || errors.Is(archiveErr, context.Canceled) {
			cancelled = true
			break
		}
	}
	if progress != nil {
		progress.Phase("Publishing accepted guides")
	}
	manifest, publishErr := run.Publish(cancelled)
	if publishErr != nil {
		fmt.Fprintln(stderr, errPresentation.failure("Publication error: "+publishErr.Error()))
		return 1, errors.Join(ctx.Err(), publishErr)
	}
	if cancelled || ctx.Err() != nil {
		message := "Cancelled; accepted guides and prior complete guides published at: " + run.Target
		if progress != nil {
			progress.Warning(message)
		} else {
			fmt.Fprintln(stderr, errPresentation.warning(message))
		}
		return 130, context.Canceled
	}
	zipPath := ""
	if o.zip {
		if progress != nil {
			progress.Phase("Exporting verified published library")
		}
		zipPath, err = run.ExportZIP(ctx)
		if err != nil {
			return 1, fmt.Errorf("export published library ZIP: %w", err)
		}
	}
	if manifest.Status != "complete" {
		code = 2
		reportIncompleteManifest(progress, stderr, errPresentation, run.Target, manifest)
	}
	if o.json {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(DownloadOutput{Library: run.Target, ZIP: zipPath, Manifest: manifest})
	} else {
		status := outPresentation.success(manifest.Status)
		if manifest.Status != "complete" {
			status = outPresentation.warning(manifest.Status)
		}
		_, err = fmt.Fprintf(out, "%s %s\n%s %s\n",
			outPresentation.heading("Library:"), outPresentation.plain(run.Target),
			outPresentation.heading("Status:"), status)
		if err == nil && zipPath != "" {
			_, err = fmt.Fprintf(out, "%s %s\n",
				outPresentation.heading("ZIP:"), outPresentation.plain(zipPath))
		}
	}
	if progress != nil {
		stats := cache.Stats{}
		if store != nil {
			stats = store.Stats()
		}
		progress.Finish(manifest.Status, fmt.Sprintf(
			"Published %s library at %s; cache: %d downloaded, %d revalidated, %d reused, %d failed",
			manifest.Status, run.Target, stats.Downloaded, stats.Revalidated, stats.UniqueReused, stats.Failed,
		))
	}
	return code, err
}

const (
	maxGuideErrorsShown   = 5
	maxGuideWarningsShown = 3
)

func reportGuideDiagnostics(
	progress *progressReporter,
	stderr io.Writer,
	presentation humanPresentation,
	guideID string,
	result model.ArchiveResult,
	archiveErr error,
) {
	errorsToShow := deduplicateDiagnostics(result.Errors, archiveErr)
	for index, message := range errorsToShow {
		if index == maxGuideErrorsShown {
			printDiagnostic(progress, stderr, presentation, "error", fmt.Sprintf(
				"Guide %s: %d more archive errors; see the published manifest and retained attempt.",
				guideID, len(errorsToShow)-index,
			))
			break
		}
		printDiagnostic(progress, stderr, presentation, "error",
			fmt.Sprintf("Guide %s archive error: %s", guideID, message))
	}
	if summary := summarizeNotices(result.Notices); summary != "" {
		printNotice(progress, stderr, presentation,
			fmt.Sprintf("Guide %s: %s; see published manifest.", guideID, summary))
	}
	warnings := deduplicateDiagnostics(result.Warnings, nil)
	for index, message := range warnings {
		if index == maxGuideWarningsShown {
			printDiagnostic(progress, stderr, presentation, "warning", fmt.Sprintf(
				"Guide %s: %d more publisher/source warnings; see the published manifest.",
				guideID, len(warnings)-index,
			))
			break
		}
		printDiagnostic(progress, stderr, presentation, "warning",
			fmt.Sprintf("Guide %s publisher/source warning: %s", guideID, message))
	}
}

func summarizeNotices(notices []model.Notice) string {
	unique := make([]model.Notice, 0, len(notices))
	seen := make(map[model.Notice]bool, len(notices))
	for _, notice := range notices {
		if notice.Kind == "" || strings.TrimSpace(notice.Message) == "" || seen[notice] {
			continue
		}
		seen[notice] = true
		unique = append(unique, notice)
	}
	if len(unique) == 0 {
		return ""
	}
	counts := map[model.NoticeKind]int{}
	for _, notice := range unique {
		counts[notice.Kind]++
	}
	parts := make([]string, 0, 5)
	if count := counts[model.NoticeExternalLinkRetained]; count > 0 {
		label := "external links retained"
		if count == 1 {
			label = "external link retained"
		}
		parts = append(parts, fmt.Sprintf("%d %s", count, label))
	}
	if count := counts[model.NoticeNavigationStyleOmitted]; count > 0 {
		label := "navigation styles omitted"
		if count == 1 {
			label = "navigation style omitted"
		}
		parts = append(parts, fmt.Sprintf("%d %s", count, label))
	}
	if count := counts[model.NoticeCSSCleanup]; count > 0 {
		parts = append(parts, fmt.Sprintf("%d CSS cleanup", count))
	}
	if count := counts[model.NoticeSourcePolicy]; count > 0 {
		parts = append(parts, fmt.Sprintf("%d source policy", count))
	}
	known := counts[model.NoticeExternalLinkRetained] +
		counts[model.NoticeNavigationStyleOmitted] +
		counts[model.NoticeCSSCleanup] +
		counts[model.NoticeSourcePolicy]
	if other := len(unique) - known; other > 0 {
		parts = append(parts, fmt.Sprintf("%d other", other))
	}
	label := "publisher notices"
	if len(unique) == 1 {
		label = "publisher notice"
	}
	return fmt.Sprintf("%d %s (%s)", len(unique), label, strings.Join(parts, "; "))
}

func deduplicateDiagnostics(messages []string, alreadyReported error) []string {
	seen := map[string]bool{}
	if alreadyReported != nil {
		seen[alreadyReported.Error()] = true
	}
	var result []string
	for _, message := range messages {
		message = strings.TrimSpace(message)
		if message == "" || seen[message] {
			continue
		}
		seen[message] = true
		result = append(result, message)
	}
	return result
}

func printDiagnostic(
	progress *progressReporter,
	stderr io.Writer,
	presentation humanPresentation,
	kind, message string,
) {
	if progress != nil {
		if kind == "error" {
			progress.Error(message)
		} else {
			progress.Warning(message)
		}
		return
	}
	if kind == "error" {
		fmt.Fprintln(stderr, presentation.failure(message))
	} else {
		fmt.Fprintln(stderr, presentation.warning(message))
	}
}

func printNotice(
	progress *progressReporter,
	stderr io.Writer,
	presentation humanPresentation,
	message string,
) {
	if progress != nil {
		progress.Notice(message)
		return
	}
	fmt.Fprintln(stderr, presentation.notice(message))
}

func reportIncompleteManifest(
	progress *progressReporter,
	stderr io.Writer,
	presentation humanPresentation,
	target string,
	manifest library.Manifest,
) {
	message := "Full diagnostics: " + filepath.Join(target, "manifest.json")
	printDiagnostic(progress, stderr, presentation, "warning", message)
	for _, manifestError := range manifest.Errors {
		if !strings.HasPrefix(manifestError, "Unfinished work retained at ") {
			continue
		}
		printDiagnostic(progress, stderr, presentation, "warning", manifestError)
	}
}
