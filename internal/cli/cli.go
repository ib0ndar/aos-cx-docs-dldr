package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
	"github.com/spf13/cobra"
)

type options struct {
	list, appVersion, json, zip          bool
	platform, version                    string
	guides                               []string
	destination                          string
	refresh, all                         bool
	preferPDF, noPreferPDF               bool
	convertHTMLToPDF, noConvertHTMLToPDF bool
	chromePath, qpdfPath                 string
	portal, rawCache                     string
	transport                            string
	delay, timeout, attemptTimeout       float64
	maxMB, maxArchiveMB                  float64
	retries, workers                     int
	preflightFresh                       map[string]bool
	sourceStatus                         map[string]model.SourceAvailability
	skippedUnavailable                   []model.SkippedUnavailableGuide
	allAvailableSnapshot                 bool
}

type Listing struct {
	Catalog            model.Catalog                       `json:"catalog"`
	Documents          []model.Document                    `json:"documents,omitempty"`
	SourceAvailability map[string]model.SourceAvailability `json:"source_availability,omitempty"`
	SourceWarnings     []string                            `json:"source_availability_warnings,omitempty"`
	PDFAvailability    map[string]model.PDFAvailability    `json:"optional_pdf_availability,omitempty"`
	PDFWarnings        []string                            `json:"optional_pdf_availability_warnings,omitempty"`
	Complete           bool                                `json:"complete"`
	SkippedUnavailable []model.SkippedUnavailableGuide     `json:"skipped_unavailable_guides,omitempty"`
}

func defaultRawCache() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "aos-cx-docs-dldr", "raw-v2"), nil
}

func Run(ctx context.Context, args []string, out, stderr io.Writer) int {
	return runWithPrompts(ctx, args, out, stderr, newNativeClient, newTerminalPrompts(os.Stdin, stderr))
}

func newNativeClient(backend string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
	switch backend {
	case "http":
		return fetch.New(config, notify)
	case "compatible":
		return fetch.NewCompatible(config, notify)
	default:
		return nil, fmt.Errorf("unsupported native transport %q", backend)
	}
}

func run(ctx context.Context, args []string, out, stderr io.Writer, newClient func(string, fetch.Config, func(string)) (*fetch.Client, error)) int {
	return runWithPrompts(ctx, args, out, stderr, newClient, nil)
}

func runWithPrompts(ctx context.Context, args []string, out, stderr io.Writer, newClient func(string, fetch.Config, func(string)) (*fetch.Client, error), prompts promptDriver) int {
	outPresentation := newHumanPresentation(out)
	errPresentation := newHumanPresentation(stderr)
	errTerminal := inspectProgressTerminal(stderr)
	out = contextWriter{ctx: ctx, writer: out}
	safeStderr := &synchronizedWriter{writer: stderr}
	stderr = safeStderr
	o := options{}
	code := 0
	cmd := &cobra.Command{
		Use: model.ExecutableName, Short: "Native AOS-CX catalogue and documentation archiver",
		Long: "Native AOS-CX Product Documentation catalogue client.\n" +
			"This native application supports catalogue listing, directly mapped original\n" +
			"PDFs, complete Flare and HPE HTML guides, and explicit-plan static/Oxygen\n" +
			"HTML support (fixture-verified; current catalogue snapshot has no mappings),\n" +
			"source-verified whole-document PDF preference with complete-HTML fallback,\n" +
			"optional generated-PDF companions through pinned Chrome and qpdf sidecars,\n" +
			"fresh mapped-source checks that disable definitive HTTP 404/410 routes,\n" +
			"interactive selection, all mapped guides for one platform/release, and\n" +
			"verified portable ZIP export.\n" +
			"Retrieval is browser-free; PDF conversion never auto-installs sidecars.",
		SilenceUsage: true, SilenceErrors: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.appVersion && o.zip {
				return errors.New("--app-version cannot be combined with --zip")
			}
			if o.appVersion {
				_, err := fmt.Fprintln(out, model.ExecutableName+" "+model.Version)
				return err
			}
			if cmd.Flags().Changed("workers") && o.workers == 0 {
				return errors.New("--workers must be between 1 and 64")
			}
			preferChanged := cmd.Flags().Changed("prefer-pdf")
			noPreferChanged := cmd.Flags().Changed("no-prefer-pdf")
			if preferChanged && noPreferChanged {
				return errors.New("--prefer-pdf and --no-prefer-pdf are mutually exclusive")
			}
			if noPreferChanged {
				o.preferPDF = false
			}
			convertChanged := cmd.Flags().Changed("convert-html-to-pdf")
			noConvertChanged := cmd.Flags().Changed("no-convert-html-to-pdf")
			if convertChanged && noConvertChanged {
				return errors.New("--convert-html-to-pdf and --no-convert-html-to-pdf are mutually exclusive")
			}
			if noConvertChanged {
				o.convertHTMLToPDF = false
			}
			config, err := o.validateFor(prompts != nil)
			if err != nil {
				return err
			}
			if o.rawCache == "" {
				o.rawCache, err = defaultRawCache()
				if err != nil {
					return fmt.Errorf("find cache directory (or set --raw-cache): %w", err)
				}
			}
			if !o.list && o.destination != "" && o.platform != "" && o.version != "" {
				if err := library.Preflight(o.destination, o.platform, o.version); err != nil {
					return err
				}
			}
			if err := cache.Prepare(ctx, o.rawCache); err != nil {
				return fmt.Errorf("prepare raw cache: %w", err)
			}
			reporter := newProgressReporterWithTerminal(stderr, errTerminal, errPresentation)
			defer reporter.Close()
			var notifyMu sync.Mutex
			notify := func(message string) {
				notifyMu.Lock()
				defer notifyMu.Unlock()
				reporter.Warning("Warning: " + message)
			}
			transport, err := newClient(o.transport, config, notify)
			if err != nil {
				return err
			}
			defer transport.Close()
			store, err := cache.OpenContext(ctx, o.rawCache, transport, config.MaxBytes, notify)
			if err != nil {
				return fmt.Errorf("open raw cache: %w", err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					notify("cache cleanup: " + err.Error())
					if code == 0 {
						code = 1
					}
				}
			}()
			catalog, err := source.LoadCatalogWithWorkersObserved(
				ctx, store, o.portal, o.workerCount(), reporter.Catalog,
			)
			if err != nil {
				return fmt.Errorf("live catalogue refresh failed: %w; the native client has no stale-catalogue fallback", err)
			}
			reporter.Clear()
			if !o.list {
				fixed := selectionFixed{
					Platform:         cmd.Flags().Changed("platform"),
					Version:          cmd.Flags().Changed("version"),
					Guides:           cmd.Flags().Changed("guides"),
					All:              cmd.Flags().Changed("all") && o.all,
					Destination:      cmd.Flags().Changed("destination"),
					Refresh:          cmd.Flags().Changed("refresh"),
					PreferPDF:        preferChanged || noPreferChanged,
					ConvertHTMLToPDF: convertChanged || noConvertChanged,
				}
				guided := o.platform == "" || o.version == "" ||
					(len(o.guides) == 0 && !o.all) || o.destination == ""
				reporter.Phase(fmt.Sprintf(
					"Catalogue ready: %d platforms, %d releases, %d guide mappings",
					len(catalog.Platforms), len(catalog.Versions), len(catalog.Guides),
				))
				if _, err := completeInteractiveSelection(
					ctx, catalog, &o, prompts, reporter.Warning, fixed, guided,
					availabilityBackend{
						fetcher: store, transport: transport,
						observe: reporter.Availability, clearLive: reporter.Clear,
					},
				); err != nil {
					return err
				}
				if err := closePromptDriver(prompts); err != nil {
					return fmt.Errorf("close interactive prompts: %w", err)
				}
				fetcher := model.Fetcher(store)
				if len(o.preflightFresh) > 0 {
					fetcher = &preflightFreshFetcher{base: store, fresh: o.preflightFresh}
				}
				code, err = downloadGuidesObserved(ctx, o, catalog, fetcher, out, stderr, reporter, store, outPresentation)
				return err
			}
			listing := Listing{Catalog: catalog, Complete: len(catalog.Warnings) == 0}
			if o.platform != "" {
				docs, diagnostics, err := source.ResolveDocuments(catalog, o.platform, o.version)
				if err != nil {
					return err
				}
				listing.Documents = docs
				listing.SourceAvailability = make(map[string]model.SourceAvailability, len(docs))
				listing.PDFAvailability = make(map[string]model.PDFAvailability, len(docs))
				scanner := selectionWizard{
					state: o, fetcher: store, transport: transport,
					sourceStatus: map[string]model.SourceAvailability{},
					availability: map[string]model.PDFAvailability{},
					observe:      reporter.Availability,
					message: func(message string) {
						if strings.HasPrefix(message, "Could not verify optional PDF availability") {
							listing.PDFWarnings = append(listing.PDFWarnings, message)
						} else {
							listing.SourceWarnings = append(listing.SourceWarnings, message)
						}
					},
				}
				if err := scanner.scanAvailability(ctx, docs, true); err != nil {
					return err
				}
				reporter.Clear()
				for _, document := range docs {
					if availability, found := scanner.sourceStatus[document.URL]; found {
						listing.SourceAvailability[document.ID] = availability
						if availability.Checked && !availability.Available {
							listing.SkippedUnavailable = append(listing.SkippedUnavailable,
								model.SkippedUnavailableGuide{
									ID: document.ID, Title: document.Title, Kind: document.Kind,
									MappedSourceURL: document.URL, ProbeURL: availability.ProbeURL,
									FinalURL: availability.FinalURL, Checked: true,
									Reason: availability.Reason,
								})
						}
					}
					if availability, found := scanner.availability[document.URL]; found {
						listing.PDFAvailability[document.ID] = availability
					}
				}
				for _, message := range diagnostics {
					if !contains(listing.Catalog.Warnings, message) {
						listing.Catalog.Warnings = append(listing.Catalog.Warnings, message)
					}
				}
				listing.Complete = len(listing.Catalog.Warnings) == 0 &&
					len(listing.SourceWarnings) == 0 && len(listing.PDFWarnings) == 0
				if len(docs) == 0 && listing.Complete {
					return fmt.Errorf("version %q is not available for platform %q. Available versions: %s",
						o.version, o.platform, strings.Join(source.AvailableVersions(catalog, o.platform), ", "))
				}
			}
			if !listing.Complete {
				code = 2
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if o.json {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(listing)
			}
			return printListingStyled(out, listing, o, outPresentation)
		},
	}
	cmd.SetOut(out)
	cmd.SetErr(stderr)
	cmd.SetArgs(expandGuides(args))
	cmd.SetContext(ctx)
	cmd.CompletionOptions.DisableDefaultCmd = true
	flags := cmd.Flags()
	flags.BoolVarP(&o.appVersion, "app-version", "V", false, "Print application version without network access")
	flags.BoolVar(&o.list, "list", false, "Refresh and display the Product Documentation catalogue")
	flags.StringVar(&o.platform, "platform", "", "Exact platform label from the portal")
	flags.StringVar(&o.version, "version", "", "Exact AOS-CX documentation release (not application version)")
	flags.StringArrayVar(&o.guides, "guides", nil, "Space-separated exact mapped guide IDs")
	flags.StringVar(&o.destination, "destination", "", "Base output directory; platform/version are always appended")
	flags.BoolVar(&o.refresh, "refresh", false, "Revalidate cached guide bytes; catalogue is always fresh")
	flags.BoolVar(&o.preferPDF, "prefer-pdf", false, "Prefer a source-verified whole-document publisher PDF; retain complete HTML if unavailable")
	flags.BoolVar(&o.noPreferPDF, "no-prefer-pdf", false, "Prefer complete HTML when available (the default)")
	flags.BoolVar(&o.convertHTMLToPDF, "convert-html-to-pdf", false, "Create a companion PDF from complete archived HTML using the pinned Chrome sidecar")
	flags.BoolVar(&o.noConvertHTMLToPDF, "no-convert-html-to-pdf", false, "Do not create a PDF from archived HTML (the default)")
	flags.StringVar(&o.chromePath, "chrome-path", "", "Exact pinned Chrome for Testing headless-shell executable")
	flags.StringVar(&o.qpdfPath, "qpdf-path", "", "Exact pinned qpdf outline-postprocessor executable")
	flags.BoolVar(&o.zip, "zip", false, "Export the verified published version library as an adjacent ZIP")
	flags.BoolVar(&o.all, "all", false, "Download every resolved mapped guide for the selected platform/release")
	flags.BoolVar(&o.json, "json", false, "Emit structured listing or final download JSON on stdout")
	flags.StringVar(&o.rawCache, "raw-cache", "", "Empty or marked aos-cx-docs-dldr raw-v2 cache directory; unrecognised caches are rejected")
	flags.StringVar(&o.transport, "transport", "compatible", "Browser-free native transport: compatible (default) or http")
	flags.Float64Var(&o.delay, "delay", 0.1, "Minimum per-origin request-start gap, seconds")
	flags.Float64Var(&o.timeout, "timeout", 150, "Overall deadline for a retrieval including robots, redirects, attempts, body consumption and backoff, seconds")
	flags.Float64Var(&o.attemptTimeout, "attempt-timeout", 45, "Deadline for each retrieval attempt, capped by the remaining overall timeout, seconds")
	flags.IntVar(&o.retries, "retries", 2, "Retries after transient failures (0..10)")
	flags.IntVar(&o.workers, "workers", 4, "Parallel workers for catalogue mappings and planned topic prefetch")
	flags.Float64Var(&o.maxMB, "max-resource-mb", 256, "Maximum decoded response size, MiB")
	flags.Float64Var(&o.maxArchiveMB, "max-archive-mb", 1024, "Maximum aggregate source bytes for one HTML guide, MiB")
	flags.StringVar(&o.portal, "portal-url", model.PortalURL, "Override the portal URL for local fixtures")
	if err := flags.MarkHidden("portal-url"); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := flags.MarkHidden("chrome-path"); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := flags.MarkHidden("qpdf-path"); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	err := cmd.Execute()
	if closeErr := closePromptDriver(prompts); err == nil && closeErr != nil {
		err = fmt.Errorf("close interactive prompts: %w", closeErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		fmt.Fprintln(stderr, errPresentation.warning("Cancelled; verified raw cache entries are retained."))
		return 130
	}
	if err != nil {
		fmt.Fprintln(stderr, errPresentation.failure("Error: "+err.Error()))
		return 1
	}
	return code
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.writer.Write(p)
	if cancelled := w.ctx.Err(); cancelled != nil {
		return n, cancelled
	}
	return n, err
}

func (o options) validate() (fetch.Config, error) {
	return o.validateFor(false)
}

func (o options) validateFor(interactive bool) (fetch.Config, error) {
	if o.all && len(o.guides) > 0 {
		return fetch.Config{}, errors.New("--all and --guides are mutually exclusive")
	}
	if o.list && (len(o.guides) > 0 || o.all || o.destination != "") {
		return fetch.Config{}, errors.New("--list cannot be combined with guide downloads or --destination")
	}
	if o.list && o.zip {
		return fetch.Config{}, errors.New("--list cannot be combined with --zip")
	}
	if !o.list && !interactive && (len(o.guides) == 0 && !o.all || o.destination == "" || o.platform == "" || o.version == "") {
		return fetch.Config{}, errors.New("non-interactive downloads require --platform, --version, (--guides ID ... or --all), and --destination")
	}
	if o.list && (o.platform == "") != (o.version == "") {
		return fetch.Config{}, errors.New("provide both --platform and --version for per-guide availability")
	}
	if o.transport != "http" && o.transport != "compatible" {
		return fetch.Config{}, errors.New("supported retrieval transports are http and compatible; neither uses a browser")
	}
	if o.workers < 0 || o.workers > 64 {
		return fetch.Config{}, errors.New("--workers must be between 1 and 64")
	}
	if _, err := fetch.NormalizeURL(o.portal); err != nil {
		return fetch.Config{}, err
	}
	if math.IsNaN(o.delay) || math.IsNaN(o.timeout) || math.IsNaN(o.attemptTimeout) ||
		math.IsNaN(o.maxMB) || math.IsNaN(o.maxArchiveMB) ||
		math.IsInf(o.delay, 0) || math.IsInf(o.timeout, 0) || math.IsInf(o.attemptTimeout, 0) ||
		math.IsInf(o.maxMB, 0) || math.IsInf(o.maxArchiveMB, 0) ||
		o.delay < 0 || o.delay > 300 || o.timeout <= 0 || o.timeout > 86400 ||
		o.attemptTimeout <= 0 || o.attemptTimeout > 86400 || o.maxMB <= 0 || o.maxMB > 16384 {
		return fetch.Config{}, errors.New("invalid network limits: use finite delay 0..300s, timeout >0..86400s, attempt-timeout >0..86400s, response MiB >0..16384")
	}
	if o.attemptTimeout > o.timeout {
		return fetch.Config{}, errors.New("--attempt-timeout must not exceed the overall --timeout")
	}
	if o.maxArchiveMB <= 0 || o.maxArchiveMB > 65536 {
		return fetch.Config{}, errors.New("invalid archive limit: --max-archive-mb must be finite and greater than 0 and no more than 65536")
	}
	config := fetch.Config{Delay: time.Duration(o.delay * float64(time.Second)),
		Timeout:        time.Duration(o.timeout * float64(time.Second)),
		AttemptTimeout: time.Duration(o.attemptTimeout * float64(time.Second)),
		Retries:        o.retries, MaxBytes: int64(o.maxMB * (1 << 20))}
	return config, config.Validate()
}

func (o options) workerCount() int {
	if o.workers == 0 {
		return 4
	}
	return o.workers
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func printListing(out io.Writer, listing Listing, o options) error {
	return printListingStyled(out, listing, o, newHumanPresentation(out))
}

func printListingStyled(out io.Writer, listing Listing, o options, presentation humanPresentation) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n%s %s\n%s %s\n%s %s\n",
		presentation.heading("Catalogue retrieved:"), presentation.plain(listing.Catalog.FetchedAt),
		presentation.heading("Source:"), presentation.plain(listing.Catalog.SourceURL),
		presentation.heading("Platforms:"), presentation.plain(strings.Join(listing.Catalog.Platforms, ", ")),
		presentation.heading("Versions:"), presentation.plain(strings.Join(listing.Catalog.Versions, ", ")))
	if len(listing.Catalog.Guides) > 0 {
		fmt.Fprintln(&b, presentation.heading("Guide ID  Document  Availability / format"))
	}
	documents := make(map[string]model.Document)
	for _, d := range listing.Documents {
		documents[d.ID] = d
	}
	for _, guide := range listing.Catalog.Guides {
		status := "mapping inventory loaded"
		d, present := documents[guide.ID]
		switch {
		case guide.Error != "":
			status = "catalogue error (see diagnostics)"
		case o.platform != "" && present:
			sourceStatus, sourceFound := listing.SourceAvailability[d.ID]
			pdfStatus, pdfFound := listing.PDFAvailability[d.ID]
			status = "[" + availabilityLabel(d, sourceStatus, pdfStatus) + "]"
			switch {
			case sourceFound && sourceStatus.Checked && sourceStatus.Available && d.Kind == "pdf":
				status += "; mapped PDF source verified by body-free HEAD; full bytes verified on download"
			case sourceFound && sourceStatus.Checked && sourceStatus.Available:
				status += "; mapped HTML source verified"
			case sourceFound && sourceStatus.Checked:
				status += "; mapped source unavailable"
			default:
				status += "; mapped source availability unknown (see diagnostics)"
			}
			switch {
			case d.Kind == "pdf":
				status += "; optional PDF: not applicable"
			case pdfFound && pdfStatus.Checked && pdfStatus.Available:
				status += "; native PDF availability verified by source and body-free response"
			case pdfFound && pdfStatus.Checked:
				status += "; native PDF checked unavailable"
			default:
				status += "; native PDF availability unknown (see diagnostics)"
			}
			if d.RouteOrigin != "" {
				status += "; route origin: " + d.RouteOrigin
			}
		case o.platform != "" && guide.Mappings[o.version][o.platform] != "":
			status = "catalogue route error (see diagnostics)"
		case o.platform != "":
			status = "not mapped for this platform/version"
		}
		styledStatus := presentation.success(status)
		if guide.Error != "" || strings.Contains(status, "error") ||
			strings.Contains(status, "unavailable") || strings.Contains(status, "unknown") {
			styledStatus = presentation.warning(status)
		} else if strings.HasPrefix(status, "not mapped") {
			styledStatus = presentation.muted(status)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n",
			presentation.plain(guide.ID), presentation.plain(guide.Title), styledStatus)
		if present {
			fmt.Fprintf(&b, "  %s\n", presentation.muted(d.URL))
		}
	}
	if !listing.Complete {
		if len(listing.Catalog.Warnings) > 0 {
			fmt.Fprintln(&b, presentation.failure("INCOMPLETE catalogue; the full mapped guide set is not known."))
		} else {
			fmt.Fprintln(&b, presentation.failure("INCOMPLETE listing; mapped source or optional PDF availability could not be established."))
		}
	}
	for i, warning := range listing.Catalog.Warnings {
		if i == 5 {
			fmt.Fprintf(&b, "%d additional diagnostics; use --json for full details.\n", len(listing.Catalog.Warnings)-i)
			break
		}
		fmt.Fprintln(&b, presentation.warning("Warning: "+warning))
	}
	for i, warning := range listing.SourceWarnings {
		if i == 5 {
			fmt.Fprintf(&b, "%d additional source availability diagnostics; use --json for full details.\n", len(listing.SourceWarnings)-i)
			break
		}
		fmt.Fprintln(&b, presentation.warning("Source availability warning: "+warning))
	}
	for i, warning := range listing.PDFWarnings {
		if i == 5 {
			fmt.Fprintf(&b, "%d additional PDF availability diagnostics; use --json for full details.\n", len(listing.PDFWarnings)-i)
			break
		}
		fmt.Fprintln(&b, presentation.warning("Optional PDF availability warning: "+warning))
	}
	_, err := io.WriteString(out, b.String())
	return err
}
