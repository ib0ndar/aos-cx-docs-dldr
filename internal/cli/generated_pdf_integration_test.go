//go:build chromepdftests

package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/pdfgen"
)

func TestPinnedChromeCLIProducesManifestIndexSearchAndZIPCompanion(t *testing.T) {
	chrome := os.Getenv(pdfgen.ChromePathEnvironment)
	if chrome == "" {
		t.Skip(pdfgen.ChromePathEnvironment + " is not set")
	}
	qpdf := os.Getenv(pdfgen.QPDFPathEnvironment)
	if qpdf == "" {
		t.Skip(pdfgen.QPDFPathEnvironment + " is not set")
	}
	server := generatedPDFServer(t)
	defer server.Close()
	args := append(
		generatedPDFArgs(t, server, filepath.Join(t.TempDir(), "library"), chrome),
		"--qpdf-path", qpdf, "--zip",
	)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, newNativeClient); code != 0 {
		t.Fatalf("generated PDF CLI exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	assertGeneratedPDFPublished(t, output)
	original := output.Manifest.HTMLGuides["job"].HTML.GeneratedPDF.SHA256
	failedArgs := append([]string{}, args...)
	for index := range failedArgs {
		if failedArgs[index] == "--chrome-path" && index+1 < len(failedArgs) {
			failedArgs[index+1] = filepath.Join(t.TempDir(), "missing-chrome")
		}
	}
	failedArgs = append(failedArgs, "--refresh")
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), failedArgs, &stdout, &stderr, newNativeClient); code != 2 {
		t.Fatalf("failed generated PDF refresh exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var failed DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &failed); err != nil {
		t.Fatal(err)
	}
	retained := failed.Manifest.HTMLGuides["job"].HTML.GeneratedPDF
	if failed.Manifest.Status != "incomplete" || retained == nil || retained.Status != "complete" ||
		retained.SHA256 != original || len(failed.Manifest.Attempts) != 1 ||
		!failed.Manifest.Attempts[0].RetainedPrevious ||
		failed.Manifest.Attempts[0].Status != "incomplete" {
		t.Fatalf("failed conversion refresh replaced the prior companion: %+v", failed.Manifest)
	}
	zipBefore, err := os.ReadFile(failed.ZIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(failed.Library, "job", retained.Path), 32); err != nil {
		t.Fatal(err)
	}
	if _, err := library.ExportZIP(context.Background(), failed.Library); err == nil {
		t.Fatal("truncated generated PDF passed library ZIP verification")
	}
	zipAfter, err := os.ReadFile(failed.ZIP)
	if err != nil || !bytes.Equal(zipBefore, zipAfter) {
		t.Fatalf("failed ZIP verification replaced the prior ZIP: err=%v", err)
	}
}

func TestPinnedChromeCLIPublishesDegradedPlaceholderPDFAndZIP(t *testing.T) {
	chrome := os.Getenv(pdfgen.ChromePathEnvironment)
	qpdf := os.Getenv(pdfgen.QPDFPathEnvironment)
	if chrome == "" || qpdf == "" {
		t.Skip("pinned Chrome and qpdf paths are required")
	}
	server := generatedPDFServerWithImageStatus(t, http.StatusNotFound)
	defer server.Close()
	args := append(
		generatedPDFArgs(t, server, filepath.Join(t.TempDir(), "library"), chrome),
		"--qpdf-path", qpdf, "--zip",
	)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, newNativeClient); code != 2 {
		t.Fatalf("degraded PDF CLI exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	guide := output.Manifest.HTMLGuides["job"]
	generated := guide.HTML.GeneratedPDF
	if output.Manifest.Status != "degraded" || guide.Status != "degraded" ||
		len(guide.MissingResources) != 1 || generated == nil ||
		generated.Status != "complete" || generated.InputStatus != "degraded" ||
		generated.ImagePlaceholders != 1 || generated.Validation == nil ||
		generated.Validation.ImagePlaceholders != 1 ||
		generated.Validation.ImagePlaceholderOverflows != 0 ||
		generated.Validation.FailedImages != 0 || generated.Validation.ExternalRequests != 0 {
		t.Fatalf("degraded PDF publication mismatch: %+v", output.Manifest)
	}
	page, err := os.ReadFile(filepath.Join(output.Library, "job", guide.HTML.Topics[0].Path))
	if err != nil || !bytes.Contains(page, []byte(`class="archive-image-placeholder"`)) {
		t.Fatalf("published placeholder missing: %v", err)
	}
	if output.ZIP == "" {
		t.Fatal("degraded ZIP path is empty")
	}
}

func TestPinnedChromeCLIReplacesHTMLWithPDF(t *testing.T) {
	chrome := os.Getenv(pdfgen.ChromePathEnvironment)
	if chrome == "" {
		t.Skip(pdfgen.ChromePathEnvironment + " is not set")
	}
	qpdf := os.Getenv(pdfgen.QPDFPathEnvironment)
	if qpdf == "" {
		t.Skip(pdfgen.QPDFPathEnvironment + " is not set")
	}
	server := generatedPDFServer(t)
	defer server.Close()
	args := append(
		generatedPDFArgs(t, server, filepath.Join(t.TempDir(), "library"), chrome),
		"--qpdf-path", qpdf, "--zip", "--replace-html-with-pdf",
	)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, newNativeClient); code != 0 {
		t.Fatalf("PDF replacement CLI exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	guide := output.Manifest.HTMLGuides["job"]
	generated := guide.HTML.GeneratedPDF
	if output.Manifest.Status != "complete" || !guide.AtReleaseRoot || generated == nil ||
		generated.Status != "complete" || len(output.Manifest.Guides) != 1 || !output.Manifest.Guides[0].AtReleaseRoot {
		t.Fatalf("PDF did not replace the HTML: %+v", output.Manifest)
	}
	entries, err := os.ReadDir(output.Library)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		if entry.IsDir() {
			t.Fatalf("release folder still holds folder %s: %v", entry.Name(), names)
		}
	}
	if !slices.Contains(names, generated.Path) {
		t.Fatalf("release folder holds %v, not the PDF %s", names, generated.Path)
	}
	search, _ := os.ReadFile(filepath.Join(output.Library, "search-index.js"))
	if strings.Contains(string(search), "Offline generated PDF fixture") {
		t.Fatal("search still holds the text of removed HTML")
	}
	reader, err := zip.OpenReader(output.ZIP)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if !containsZIPMember(reader.File, "10.16/"+generated.Path) || containsZIPMember(reader.File, "10.16/job/index.html") {
		t.Fatal("ZIP does not hold exactly the PDF of the converted guide")
	}
	if _, err := library.ExportZIP(context.Background(), output.Library); err != nil {
		t.Fatalf("published library with PDFs in the release folder does not verify: %v", err)
	}
}
