package library

import (
	"context"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/pdfgen"
	"aos-cx-docs-dldr/internal/storage"
)

// archiveHTMLWithPDF archives an HTML guide in its work directory with a
// generated-PDF record of the given status, without accepting it.
func archiveHTMLWithPDF(t *testing.T, r *Run, id, status string) model.ArchiveResult {
	t.Helper()
	result := archiveHTMLGuide(t, r, id, nil, nil)
	name, err := storage.PDFFilename(result.Document.Platform, result.Document.Version, result.Document.Title)
	if err != nil {
		t.Fatal(err)
	}
	if status == "complete" {
		body := generatedPDFBytes()
		if err := os.WriteFile(filepath.Join(result.OutputDir, name), body, 0o640); err != nil {
			t.Fatal(err)
		}
		result.HTML.GeneratedPDF = generatedPDFRecord(t, name, body)
	} else {
		result.HTML.GeneratedPDF = &model.GeneratedPDFRecord{
			Path: name, Status: "failed", Error: "renderer stopped", Renderer: pdfgen.RendererName,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
	}
	if err := storage.WriteJSON(r.root, path.Join(r.stage, ".work", id, "manifest.json"), result.HTML); err != nil {
		t.Fatal(err)
	}
	return result
}

func directoryNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestReplaceHTMLWithPDFKeepsOnlyTheVerifiedPDF(t *testing.T) {
	base := t.TempDir()
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	r.ReplaceHTMLWithPDF = true
	converted := archiveHTMLWithPDF(t, r, "acls", "complete")
	if err := r.Accept(converted); err != nil {
		t.Fatal(err)
	}
	// A guide without a PDF keeps its HTML in the same run.
	acceptHTML(t, r, "vsx")
	manifest, err := r.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	pdfName := converted.HTML.GeneratedPDF.Path
	result := manifest.HTMLGuides["acls"]
	if manifest.Status != "complete" || !result.HTMLRemoved || manifest.HTMLGuides["vsx"].HTMLRemoved ||
		len(manifest.Guides) != 2 || !manifest.Guides[0].HTMLRemoved || manifest.Guides[1].HTMLRemoved ||
		result.HTML == nil || len(result.HTML.Topics) != 1 || result.HTML.GeneratedPDF.Status != "complete" {
		t.Fatalf("replaced HTML was not recorded: %+v", manifest)
	}
	if names := directoryNames(t, filepath.Join(r.Target, "acls")); !slices.Equal(names, []string{pdfName}) {
		t.Fatalf("guide whose HTML was replaced holds %v, want only %s", names, pdfName)
	}
	if _, err := os.Stat(filepath.Join(r.Target, "vsx", "index.html")); err != nil {
		t.Fatalf("guide without a PDF lost its HTML: %v", err)
	}

	index, err := os.ReadFile(filepath.Join(r.Target, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	pdfLink := (&url.URL{Path: path.Join("acls", pdfName)}).EscapedPath()
	if !strings.Contains(string(index), `href="`+pdfLink+`"`) || strings.Contains(string(index), `href="acls/index.html"`) ||
		!strings.Contains(string(index), "Downloaded HTML guides and PDFs are available offline.") {
		t.Fatalf("release index does not link the PDF in place of the HTML:\n%s", index)
	}
	search, err := os.ReadFile(filepath.Join(r.Target, "search-index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(search), `"url":"`+pdfLink+`"`) || strings.Contains(string(search), "acls/pages/") ||
		!strings.Contains(string(search), "vsx/pages/") {
		t.Fatalf("search does not treat the replaced guide as title-only:\n%s", search)
	}

	reopened, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatalf("library with a replaced guide does not reopen: %v", err)
	}
	for name, mutate := range map[string]func(*model.ArchiveResult){
		"no PDF record": func(result *model.ArchiveResult) { result.HTML.GeneratedPDF = nil },
		"failed PDF": func(result *model.ArchiveResult) {
			failed := *result.HTML.GeneratedPDF
			failed.Status = "failed"
			result.HTML.GeneratedPDF = &failed
		},
		"HTML still claimed": func(result *model.ArchiveResult) { result.HTMLRemoved = false },
	} {
		mutated := cloneArchiveResult(result)
		mutate(&mutated)
		if err := reopened.verifyGuide(path.Join(reopened.target, "acls"), mutated); err == nil {
			t.Fatalf("%s: inconsistent replaced guide was accepted", name)
		}
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	zipPath, err := ExportZIP(context.Background(), r.Target)
	if err != nil {
		t.Fatalf("library with a replaced guide does not export: %v", err)
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(r.Target, "acls", "index.html"), []byte("<p>stray</p>"), 0o640); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(base, "6300", "10.10"); err == nil {
		reopened.Close()
		t.Fatal("a stray file beside the PDF of a replaced guide was accepted")
	} else if !strings.Contains(err.Error(), "unexpected/missing files in HTML guide") {
		t.Fatalf("a stray file beside the PDF of a replaced guide was rejected for another reason: %v", err)
	}
}

func TestReplaceHTMLWithPDFOnlyLibraryIndex(t *testing.T) {
	base := t.TempDir()
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	r.ReplaceHTMLWithPDF = true
	if err := r.Accept(archiveHTMLWithPDF(t, r, "acls", "complete")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(false); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(r.Target, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "PDFs generated from downloaded HTML guides are saved offline.") ||
		!strings.Contains(string(index), "Search downloaded PDF titles") {
		t.Fatalf("PDF-only library index describes HTML guides:\n%s", index)
	}
}

func TestReplaceHTMLWithPDFKeepsHTMLWhenThePDFFailed(t *testing.T) {
	base := t.TempDir()
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	r.ReplaceHTMLWithPDF = true
	if err := r.Accept(archiveHTMLWithPDF(t, r, "acls", "failed")); err != nil {
		t.Fatal(err)
	}
	manifest, err := r.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if manifest.HTMLGuides["acls"].HTMLRemoved || manifest.Guides[0].HTMLRemoved ||
		manifest.Attempts[0].Status != "incomplete" {
		t.Fatalf("failed PDF removed the HTML or was not reported: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(r.Target, "acls", "index.html")); err != nil {
		t.Fatalf("failed PDF removed the HTML: %v", err)
	}
}

func TestReplacedGuideIsRetainedWhenALaterPDFFails(t *testing.T) {
	base := t.TempDir()
	first, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	first.ReplaceHTMLWithPDF = true
	if err := first.Accept(archiveHTMLWithPDF(t, first, "acls", "complete")); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Publish(false); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	second.ReplaceHTMLWithPDF = true
	if err := second.Accept(archiveHTMLWithPDF(t, second, "acls", "failed")); err != nil {
		t.Fatal(err)
	}
	manifest, err := second.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if !manifest.HTMLGuides["acls"].HTMLRemoved || !manifest.Attempts[0].RetainedPrevious {
		t.Fatalf("failed PDF displaced the earlier PDF: %+v", manifest)
	}
	if names := directoryNames(t, filepath.Join(second.Target, "acls")); len(names) != 1 ||
		!strings.HasSuffix(names[0], ".pdf") {
		t.Fatalf("retained guide holds %v, want only its PDF", names)
	}
}
