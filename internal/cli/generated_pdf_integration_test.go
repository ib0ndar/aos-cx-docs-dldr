//go:build chromepdftests

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
