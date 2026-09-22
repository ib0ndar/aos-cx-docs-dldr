//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"aos-cx-docs-dldr/internal/pdfacceptance"
	"aos-cx-docs-dldr/internal/pdfcheck"
	"aos-cx-docs-dldr/internal/pdfgen"
	"github.com/spf13/pflag"
)

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Type() string   { return "string" }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type intList []int

func (values *intList) String() string {
	result := make([]string, len(*values))
	for index, value := range *values {
		result[index] = strconv.Itoa(value)
	}
	return strings.Join(result, ",")
}
func (values *intList) Type() string { return "int" }
func (values *intList) Set(value string) error {
	number, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	*values = append(*values, number)
	return nil
}

type cliOptions struct {
	expectedOutlineRoots   stringList
	forbiddenOutlineTitles stringList
	forbiddenRunningText   stringList
	headingAnchors         stringList
	maxCoverFontPoints     float64
	maxLargeRepeatPages    int
	mutoolIdentityManifest string
	mutoolLicensePath      string
	mutoolPath             string
	outlineDirectChildren  stringList
	pdf                    string
	qpdfPath               string
	report                 string
	requirePageFooters     bool
	sampleAnchors          stringList
	sampleDir              string
	samplePages            intList
	sourceGuideRoot        string
	tempRoot               string
	title                  string
	writeIdentityManifest  string
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	return runWithAnalyze(ctx, arguments, stdout, stderr, analyze)
}

func runWithAnalyze(
	ctx context.Context,
	arguments []string,
	stdout, stderr io.Writer,
	analyzeFn func(context.Context, cliOptions) (pdfacceptance.AcceptanceReport, error),
) int {
	options, err := parseCLI(arguments, stdout, stderr)
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	if options.writeIdentityManifest != "" {
		manifest, err := pdfacceptance.GenerateMuPDFManifest(
			ctx, options.mutoolPath, options.mutoolLicensePath, options.writeIdentityManifest,
		)
		if err != nil {
			fmt.Fprintf(stderr, "generate mutool identity manifest: %v\n", err)
			return 2
		}
		data, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		data = append(data, '\n')
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		return 0
	}
	report, err := analyzeFn(ctx, options)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	data = append(data, '\n')
	if err := writeAtomicReport(options.report, data); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if report.Passed {
		return 0
	}
	return 1
}

func parseCLI(arguments []string, stdout, stderr io.Writer) (cliOptions, error) {
	var options cliOptions
	flags := pflag.NewFlagSet("aoscx-pdf-acceptance", pflag.ContinueOnError)
	output := stderr
	for _, argument := range arguments {
		if argument == "-h" || argument == "--help" {
			output = stdout
			break
		}
	}
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintln(output, "usage: aoscx-pdf-acceptance PDF --title TITLE --report PATH --mutool-path PATH --mutool-identity-manifest PATH [OPTIONS]")
	}
	flags.Var(&options.forbiddenRunningText, "forbidden-running-text", "forbidden repeated text")
	flags.StringVar(&options.sourceGuideRoot, "source-guide-root", "", "archived HTML guide root")
	flags.StringVar(&options.report, "report", "", "acceptance JSON report")
	flags.StringVar(&options.sampleDir, "sample-dir", "", "new sample image directory")
	flags.Var(&options.samplePages, "sample-page", "physical page to sample")
	flags.Var(&options.sampleAnchors, "sample-anchor", "text anchor to sample")
	flags.Var(&options.headingAnchors, "heading-anchor", "heading text to audit")
	flags.Var(&options.expectedOutlineRoots, "expected-outline-root", "required outline root prefix")
	flags.Var(&options.forbiddenOutlineTitles, "forbidden-outline-title", "forbidden outline title")
	flags.Var(&options.outlineDirectChildren, "outline-direct-children", "TITLE=COUNT direct-child assertion")
	flags.BoolVar(&options.requirePageFooters, "require-page-footers", false, "require exact physical-page footers")
	flags.Float64Var(&options.maxCoverFontPoints, "max-cover-font-points", 22.1, "maximum canonical cover title size")
	flags.IntVar(&options.maxLargeRepeatPages, "max-large-repeat-pages", 2, "maximum unexplained large-text page repeats")
	flags.StringVar(&options.title, "title", "", "canonical guide title")
	flags.StringVar(&options.mutoolPath, "mutool-path", "", "explicit approved mutool executable")
	flags.StringVar(&options.mutoolIdentityManifest, "mutool-identity-manifest", "", "explicit mutool closure identity manifest")
	flags.StringVar(&options.mutoolLicensePath, "mutool-license-path", "", "explicit MuPDF COPYING evidence")
	flags.StringVar(&options.writeIdentityManifest, "write-mutool-identity-manifest", "", "create a new local mutool identity manifest")
	flags.StringVar(&options.qpdfPath, "qpdf-path", "", "explicit pinned qpdf sidecar executable")
	flags.StringVar(&options.tempRoot, "temp-root", "", "owned temporary root for qpdf audit")
	if err := flags.Parse(arguments); err != nil {
		return options, err
	}
	if options.writeIdentityManifest != "" {
		if options.mutoolPath == "" || options.mutoolLicensePath == "" || flags.NArg() != 0 {
			return options, errors.New("--write-mutool-identity-manifest requires --mutool-path and --mutool-license-path only")
		}
		return options, nil
	}
	if flags.NArg() != 1 {
		return options, errors.New("exactly one PDF path is required")
	}
	options.pdf = flags.Arg(0)
	if options.title == "" || options.report == "" ||
		options.mutoolPath == "" || options.mutoolIdentityManifest == "" {
		return options, errors.New("PDF analysis requires --title, --report, --mutool-path, and --mutool-identity-manifest")
	}
	if options.maxCoverFontPoints <= 0 || math.IsNaN(options.maxCoverFontPoints) ||
		math.IsInf(options.maxCoverFontPoints, 0) || options.maxLargeRepeatPages < 0 {
		return options, errors.New("acceptance numeric limits are invalid")
	}
	if err := validateCLIOptions(options); err != nil {
		return options, err
	}
	return options, nil
}

func validateCLIOptions(options cliOptions) error {
	if len(options.samplePages)+len(options.sampleAnchors)+5 > pdfacceptance.AcceptanceMaxSamples {
		return fmt.Errorf("sample selection exceeds the %d-page bound", pdfacceptance.AcceptanceMaxSamples)
	}
	counts := []struct {
		label string
		count int
		max   int
	}{
		{"forbidden running text", len(options.forbiddenRunningText), 256},
		{"sample anchors", len(options.sampleAnchors), 27},
		{"heading anchors", len(options.headingAnchors), 256},
		{"expected outline roots", len(options.expectedOutlineRoots), 256},
		{"forbidden outline titles", len(options.forbiddenOutlineTitles), 1024},
		{"outline child counts", len(options.outlineDirectChildren), 1024},
	}
	totalBytes := len(options.title)
	for _, values := range [][]string{
		options.forbiddenRunningText, options.sampleAnchors, options.headingAnchors,
		options.expectedOutlineRoots, options.forbiddenOutlineTitles, options.outlineDirectChildren,
	} {
		for _, value := range values {
			if len(value) > 16<<10 {
				return errors.New("acceptance option value exceeds 16 KiB")
			}
			totalBytes += len(value)
		}
	}
	if len(options.title) > 16<<10 || totalBytes > 4<<20 {
		return errors.New("acceptance option text exceeds bounded size")
	}
	for _, value := range counts {
		if value.count > value.max {
			return fmt.Errorf("%s has %d values, exceeding %d", value.label, value.count, value.max)
		}
	}
	return nil
}

func analyze(ctx context.Context, options cliOptions) (pdfacceptance.AcceptanceReport, error) {
	tool, err := pdfacceptance.LoadMuPDFTool(ctx, options.mutoolIdentityManifest)
	if err != nil {
		return pdfacceptance.AcceptanceReport{}, err
	}
	explicitMutool, err := filepath.Abs(options.mutoolPath)
	if err != nil {
		return pdfacceptance.AcceptanceReport{}, err
	}
	if explicitMutool != tool.Manifest.MutoolPath {
		return pdfacceptance.AcceptanceReport{}, errors.New("--mutool-path does not match the supplied identity manifest")
	}
	pdfPath, info, err := openValidatedPDF(options.pdf)
	if err != nil {
		return pdfacceptance.AcceptanceReport{}, err
	}
	file, err := os.Open(pdfPath)
	if err != nil {
		return pdfacceptance.AcceptanceReport{}, err
	}
	validateErr := pdfcheck.Validate(file, info.Size(), "application/pdf")
	closeErr := file.Close()
	if err := errors.Join(validateErr, closeErr); err != nil {
		return pdfacceptance.AcceptanceReport{}, err
	}
	source, err := pdfacceptance.LoadSourceEvidence(options.sourceGuideRoot)
	if err != nil {
		return pdfacceptance.AcceptanceReport{}, err
	}
	document, err := tool.ExtractLayout(ctx, pdfPath)
	if err != nil {
		return pdfacceptance.AcceptanceReport{}, err
	}
	outlineAudit, err := pdfgen.AuditOutline(ctx, options.qpdfPath, pdfPath, options.tempRoot)
	if err != nil {
		return pdfacceptance.AcceptanceReport{}, err
	}
	outline := make([]pdfacceptance.OutlineEntry, len(outlineAudit.Entries))
	for index, entry := range outlineAudit.Entries {
		outline[index] = pdfacceptance.OutlineEntry{
			Depth: entry.Depth, PhysicalPage: entry.PhysicalPage, Title: entry.Title,
		}
	}
	report, err := pdfacceptance.AnalyzeAcceptance(document, source, outline, pdfacceptance.AcceptanceOptions{
		ExpectedOutlineRoots:   options.expectedOutlineRoots,
		ForbiddenOutlineTitles: options.forbiddenOutlineTitles,
		ForbiddenRunningText:   options.forbiddenRunningText,
		HeadingAnchors:         options.headingAnchors,
		MaxCoverFontPoints:     options.maxCoverFontPoints,
		MaxLargeRepeatPages:    options.maxLargeRepeatPages,
		OutlineDirectChildren:  options.outlineDirectChildren,
		RequirePageFooters:     options.requirePageFooters,
		Title:                  options.title,
	})
	if err != nil {
		return pdfacceptance.AcceptanceReport{}, err
	}
	report.PDF = pdfPath
	if options.sourceGuideRoot != "" {
		root, err := filepath.Abs(options.sourceGuideRoot)
		if err != nil {
			return pdfacceptance.AcceptanceReport{}, err
		}
		report.SourceGuideRoot = &root
	}
	dependencies := make([]pdfacceptance.ToolFile, len(tool.Manifest.Dependencies))
	for index, dependency := range tool.Manifest.Dependencies {
		dependencies[index] = pdfacceptance.ToolFile{Path: dependency.Path, SHA256: dependency.SHA256}
	}
	report.Tool = pdfacceptance.AcceptanceToolRecord{
		LicensePath: tool.Manifest.LicensePath, LicenseSHA256: tool.Manifest.LicenseSHA256,
		MuPDFDependencyClosure: dependencies,
		MuPDFExecutableSHA256:  tool.Manifest.MutoolSHA256,
		MuPDFIdentityManifest:  tool.ManifestPath, MuPDFIdentitySHA256: tool.ManifestSHA256,
		MuPDFPath: tool.Manifest.MutoolPath, MuPDFVersion: tool.Manifest.MutoolVersion,
		QPDFBundleSHA256:     outlineAudit.BundleSHA256,
		QPDFExecutableSHA256: outlineAudit.ExecutableSHA256,
		QPDFPath:             outlineAudit.ExecutablePath, QPDFVersion: outlineAudit.Version,
	}
	if options.sampleDir != "" {
		samplePages := append([]int(nil), options.samplePages...)
		for _, heading := range report.Headings {
			if page, ok := heading["physical_page"].(int); ok {
				samplePages = append(samplePages, page)
			}
		}
		samples, err := pdfacceptance.BuildSamples(
			ctx, tool, document, pdfPath, options.sampleDir, samplePages, options.sampleAnchors,
		)
		if err != nil {
			return pdfacceptance.AcceptanceReport{}, err
		}
		samples.Montage = filepath.Join(options.sampleDir, "montage.png")
		report.Samples = &samples
	}
	current, err := os.Lstat(pdfPath)
	if err != nil || !os.SameFile(info, current) {
		return pdfacceptance.AcceptanceReport{}, errors.New("acceptance PDF identity changed during analysis")
	}
	return report, nil
}

func openValidatedPDF(raw string) (string, os.FileInfo, error) {
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() < 32 || info.Size() > pdfgen.QPDFOutlineValidationMaxBytes {
		return "", nil, errors.New("acceptance input must be a bounded nonsymlink regular PDF")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", nil, err
	}
	resolvedInfo, err := os.Lstat(resolved)
	if err != nil || !os.SameFile(info, resolvedInfo) {
		return "", nil, errors.New("acceptance input identity changed while resolving its path")
	}
	return resolved, resolvedInfo, nil
}

func writeAtomicReport(path string, data []byte) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("report destination must not be a symbolic link: %s", absolute)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(parent, ".aoscx-pdf-report-")
	if err != nil {
		return err
	}
	temp := file.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temp)
		}
	}()
	if err := os.Chmod(temp, 0o600); err != nil {
		_ = file.Close()
		return err
	}
	_, writeErr := io.Copy(file, bytes.NewReader(data))
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(temp, absolute); err != nil {
		return err
	}
	remove = false
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
