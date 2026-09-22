//go:build chromepdftests && darwin

package pdfgen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"aos-cx-docs-dldr/internal/pdfacceptance"
)

func TestPinnedGeneratedPDFAcceptanceWithMuPDFAndQPDF(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	qpdf := os.Getenv(QPDFPathEnvironment)
	mutool := os.Getenv("AOSCX_DOCS_MUTOOL_PATH")
	mutoolLicense := os.Getenv("AOSCX_DOCS_MUTOOL_LICENSE_PATH")
	if chrome == "" || qpdf == "" || mutool == "" || mutoolLicense == "" {
		t.Skip("pinned Chrome, qpdf, mutool and mutool license paths are required")
	}
	output, plan, archived := rendererFixture(t)
	config := DefaultConfig(chrome)
	config.QPDFPath = qpdf
	record, err := Generate(context.Background(), output, plan, &archived, config)
	if err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(output, record.Path)
	mutoolManifest := filepath.Join(t.TempDir(), "mutool-identity.json")
	if _, err := pdfacceptance.GenerateMuPDFManifest(
		context.Background(), mutool, mutoolLicense, mutoolManifest,
	); err != nil {
		t.Fatal(err)
	}
	tool, err := pdfacceptance.LoadMuPDFTool(context.Background(), mutoolManifest)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Manifest.MutoolPath != mutool {
		t.Fatalf("explicit mutool differs from manifest: %q != %q", mutool, tool.Manifest.MutoolPath)
	}
	layout, err := tool.ExtractLayout(context.Background(), pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := pdfacceptance.LoadSourceEvidence(output)
	if err != nil {
		t.Fatal(err)
	}
	outline, err := AuditOutline(context.Background(), qpdf, pdfPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]pdfacceptance.OutlineEntry, len(outline.Entries))
	for index, entry := range outline.Entries {
		entries[index] = pdfacceptance.OutlineEntry{
			Depth: entry.Depth, PhysicalPage: entry.PhysicalPage, Title: entry.Title,
		}
	}
	report, err := pdfacceptance.AnalyzeAcceptance(layout, source, entries, pdfacceptance.AcceptanceOptions{
		Title: plan.Document.Title, MaxCoverFontPoints: 22.1, MaxLargeRepeatPages: 2,
		ExpectedOutlineRoots: []string{"Cover", "Contents", "Commands"},
		RequirePageFooters:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		body, _ := json.MarshalIndent(report.Violations, "", "  ")
		t.Fatalf("generated PDF failed native acceptance:\n%s", body)
	}
	samples, err := pdfacceptance.BuildSamples(
		context.Background(), tool, layout, pdfPath,
		filepath.Join(t.TempDir(), "samples"), []int{2}, []string{"Spans and repeated headers"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples.PhysicalPages) < 2 || samples.Montage == "" {
		t.Fatalf("unexpected sample record: %+v", samples)
	}
	if export := os.Getenv("AOSCX_PDF_ACCEPTANCE_EXPORT"); export != "" {
		exportAcceptanceFixture(t, output, pdfPath, export)
	}
}

func exportAcceptanceFixture(t *testing.T, guideRoot, pdfPath, destination string) {
	t.Helper()
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acceptance export destination exists or is unreadable: %s: %v", destination, err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(destination)
		}
	}()
	for _, relative := range []string{
		filepath.Base(pdfPath),
		"pages/topic-a.html",
		"pages/topic-b.html",
	} {
		source := pdfPath
		targetRelative := relative
		if relative != filepath.Base(pdfPath) {
			source = filepath.Join(guideRoot, relative)
		}
		target := filepath.Join(destination, targetRelative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		input, err := os.Open(source)
		if err != nil {
			t.Fatal(err)
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = input.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(output, input)
		closeErr := errors.Join(output.Sync(), output.Close(), input.Close())
		if err := errors.Join(copyErr, closeErr); err != nil {
			t.Fatal(err)
		}
	}
	success = true
}
