package library

import (
	"archive/zip"
	"context"
	"errors"
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
	return archiveTitledHTMLWithPDF(t, r, id, id+" Guide", status)
}

func archiveTitledHTMLWithPDF(t *testing.T, r *Run, id, title, status string) model.ArchiveResult {
	t.Helper()
	result := archiveTitledHTMLGuide(t, r, id, title, nil, nil)
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

func openReplacing(t *testing.T, base string) *Run {
	t.Helper()
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	r.ReplaceHTMLWithPDF = true
	return r
}

func publishAndClose(t *testing.T, r *Run) Manifest {
	t.Helper()
	manifest, err := r.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestReplaceHTMLWithPDFKeepsOnlyThePDFsInTheReleaseFolder(t *testing.T) {
	base := t.TempDir()
	r := openReplacing(t, base)
	converted := archiveHTMLWithPDF(t, r, "acls", "complete")
	if err := r.Accept(converted); err != nil {
		t.Fatal(err)
	}
	publisher := acceptPDF(t, r, "cli", "CLI")
	// A guide without a PDF keeps its HTML, in its folder, in the same run.
	acceptHTML(t, r, "vsx")
	manifest := publishAndClose(t, r)

	generatedName, publisherName := converted.HTML.GeneratedPDF.Path, publisher.PDF.Path
	if manifest.Status != "complete" || !manifest.HTMLGuides["acls"].AtReleaseRoot ||
		!manifest.PDFGuides["cli"].AtReleaseRoot || manifest.HTMLGuides["vsx"].AtReleaseRoot ||
		len(manifest.Guides) != 3 || !manifest.Guides[0].AtReleaseRoot || !manifest.Guides[1].AtReleaseRoot ||
		manifest.Guides[2].AtReleaseRoot || len(manifest.HTMLGuides["acls"].HTML.Topics) != 1 {
		t.Fatalf("release-folder PDFs were not recorded: %+v", manifest)
	}
	want := []string{generatedName, publisherName, "history.json", "index.html", "manifest.json",
		"search-index.js", "search.js", "vsx"}
	slices.Sort(want)
	if names := directoryNames(t, r.Target); !slices.Equal(names, want) {
		t.Fatalf("release folder holds %v, want %v", names, want)
	}

	index, err := os.ReadFile(filepath.Join(r.Target, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	generatedLink := (&url.URL{Path: generatedName}).EscapedPath()
	publisherLink := (&url.URL{Path: publisherName}).EscapedPath()
	if !strings.Contains(string(index), `href="`+generatedLink+`"`) ||
		!strings.Contains(string(index), `href="`+publisherLink+`"`) ||
		strings.Contains(string(index), `href="acls/`) || strings.Contains(string(index), `href="cli/`) ||
		!strings.Contains(string(index), "Downloaded HTML guides and PDFs are available offline.") {
		t.Fatalf("release index does not link the PDFs in the release folder:\n%s", index)
	}
	search, err := os.ReadFile(filepath.Join(r.Target, "search-index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(search), `"url":"`+generatedLink+`"`) || strings.Contains(string(search), "acls/") ||
		!strings.Contains(string(search), "vsx/pages/") {
		t.Fatalf("search does not treat the converted guide as title-only:\n%s", search)
	}

	reopened, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatalf("library with PDFs in the release folder does not reopen: %v", err)
	}
	result := manifest.HTMLGuides["acls"]
	for name, mutate := range map[string]func(*model.ArchiveResult){
		"no PDF record": func(result *model.ArchiveResult) { result.HTML.GeneratedPDF = nil },
		"failed PDF": func(result *model.ArchiveResult) {
			failed := *result.HTML.GeneratedPDF
			failed.Status = "failed"
			result.HTML.GeneratedPDF = &failed
		},
		"folder claimed": func(result *model.ArchiveResult) { result.AtReleaseRoot = false },
	} {
		mutated := cloneArchiveResult(result)
		mutate(&mutated)
		if err := reopened.verifyGuide(path.Join(reopened.target, "acls"), mutated); err == nil {
			t.Fatalf("%s: inconsistent release-folder guide was accepted", name)
		}
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	zipPath, err := ExportZIP(context.Background(), r.Target)
	if err != nil {
		t.Fatalf("library with PDFs in the release folder does not export: %v", err)
	}
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	for _, file := range reader.File {
		members = append(members, file.Name)
	}
	reader.Close()
	if !slices.Contains(members, "10.10/"+generatedName) || !slices.Contains(members, "10.10/"+publisherName) ||
		slices.ContainsFunc(members, func(name string) bool { return strings.HasPrefix(name, "10.10/acls/") }) {
		t.Fatalf("ZIP does not hold the PDFs in the release folder: %v", members)
	}

	if err := os.MkdirAll(filepath.Join(r.Target, "acls"), 0o700); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(base, "6300", "10.10"); err == nil {
		reopened.Close()
		t.Fatal("a folder for a guide kept in the release folder was accepted")
	} else if !strings.Contains(err.Error(), "also has a folder") {
		t.Fatalf("a folder for a guide kept in the release folder was rejected for another reason: %v", err)
	}
	if err := os.Remove(filepath.Join(r.Target, "acls")); err != nil {
		t.Fatal(err)
	}
	pdfFile, err := os.OpenFile(filepath.Join(r.Target, generatedName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := pdfFile.WriteString("%")
	if err := errors.Join(writeErr, pdfFile.Close()); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(base, "6300", "10.10"); err == nil {
		reopened.Close()
		t.Fatal("a changed PDF in the release folder was accepted")
	} else if !strings.Contains(err.Error(), "generated PDF integrity mismatch") {
		t.Fatalf("a changed PDF in the release folder was rejected for another reason: %v", err)
	}
}

func TestReplaceHTMLWithPDFLeavesOnlyThePDFs(t *testing.T) {
	base := t.TempDir()
	// An earlier library of the release, whose copy the next run snapshots.
	earlier, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptHTML(t, earlier, "acls")
	publishAndClose(t, earlier)

	r := openReplacing(t, base)
	converted := archiveHTMLWithPDF(t, r, "acls", "complete")
	if err := r.Accept(converted); err != nil {
		t.Fatal(err)
	}
	publisher := acceptPDF(t, r, "cli", "CLI")
	manifest, err := r.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{converted.HTML.GeneratedPDF.Path, publisher.PDF.Path}
	slices.Sort(want)
	if names := directoryNames(t, r.Target); !slices.Equal(names, want) || !r.KeptOnlyPDFs() ||
		manifest.Status != "complete" || !manifest.HTMLGuides["acls"].AtReleaseRoot {
		t.Fatalf("release folder holds %v, want only %v (kept only PDFs %v, %+v)", names, want, r.KeptOnlyPDFs(), manifest)
	}
	if names := directoryNames(t, filepath.Join(base, "6300", ".staging")); len(names) != 0 {
		t.Fatalf("work folders were left behind: %v", names)
	}
	if _, err := os.Lstat(filepath.Join(base, "6300", ".snapshots", "10.10")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshots of the earlier copy were left behind: %v", err)
	}
	zipPath, err := r.ExportZIP(context.Background())
	if err != nil {
		t.Fatalf("release folder of only PDFs does not export: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	for _, file := range reader.File {
		members = append(members, file.Name)
	}
	reader.Close()
	if !slices.Equal(members, []string{"10.10/" + want[0], "10.10/" + want[1]}) {
		t.Fatalf("ZIP holds %v, want only the PDFs", members)
	}

	// The folder keeps no records, so no later run can tell it from foreign
	// files: it is refused, and left as it is.
	if err := Preflight(base, "6300", "10.10"); err == nil || !strings.Contains(err.Error(), "holds only PDFs") {
		t.Fatalf("release folder of only PDFs passed preflight: %v", err)
	}
	if reopened, err := Open(base, "6300", "10.10"); err == nil {
		reopened.Close()
		t.Fatal("release folder of only PDFs was opened")
	} else if !strings.Contains(err.Error(), "holds only PDFs") {
		t.Fatalf("release folder of only PDFs was refused for another reason: %v", err)
	}
	if _, err := ExportZIP(context.Background(), r.Target); err == nil {
		t.Fatal("release folder of only PDFs was exported without its records")
	}
	if names := directoryNames(t, r.Target); !slices.Equal(names, want) {
		t.Fatalf("refused folder changed: %v", names)
	}
}

func TestUnfinishedPDFOnlyRunKeepsTheRecords(t *testing.T) {
	base := t.TempDir()
	r := openReplacing(t, base)
	converted := archiveHTMLWithPDF(t, r, "acls", "complete")
	if err := r.Accept(converted); err != nil {
		t.Fatal(err)
	}
	manifest, err := r.Publish(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{converted.HTML.GeneratedPDF.Path, "history.json", "index.html", "manifest.json",
		"search-index.js", "search.js"}
	slices.Sort(want)
	if names := directoryNames(t, r.Target); !slices.Equal(names, want) || r.KeptOnlyPDFs() ||
		manifest.Status != "incomplete" {
		t.Fatalf("cancelled run left %v, want %v (kept only PDFs %v)", names, want, r.KeptOnlyPDFs())
	}
	index, err := os.ReadFile(filepath.Join(r.Target, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "PDFs generated from downloaded HTML guides are saved offline.") ||
		!strings.Contains(string(index), "Search downloaded PDF titles") {
		t.Fatalf("PDF-only library index describes HTML guides:\n%s", index)
	}
	// A later run finishes it, and then only the PDF is left.
	again := openReplacing(t, base)
	if err := again.Accept(archiveHTMLWithPDF(t, again, "acls", "complete")); err != nil {
		t.Fatal(err)
	}
	publishAndClose(t, again)
	if names := directoryNames(t, again.Target); !slices.Equal(names, []string{converted.HTML.GeneratedPDF.Path}) {
		t.Fatalf("finished run left %v", names)
	}
}

func TestReplaceHTMLWithPDFKeepsHTMLWhenThePDFFailed(t *testing.T) {
	base := t.TempDir()
	r := openReplacing(t, base)
	if err := r.Accept(archiveHTMLWithPDF(t, r, "acls", "failed")); err != nil {
		t.Fatal(err)
	}
	manifest := publishAndClose(t, r)
	if manifest.HTMLGuides["acls"].AtReleaseRoot || manifest.Guides[0].AtReleaseRoot ||
		manifest.Attempts[0].Status != "incomplete" {
		t.Fatalf("failed PDF removed the HTML or was not reported: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(r.Target, "acls", "index.html")); err != nil {
		t.Fatalf("failed PDF removed the HTML: %v", err)
	}
}

func TestReleaseFolderPDFIsRetainedWhenALaterPDFFails(t *testing.T) {
	base := t.TempDir()
	first := openReplacing(t, base)
	converted := archiveHTMLWithPDF(t, first, "acls", "complete")
	if err := first.Accept(converted); err != nil {
		t.Fatal(err)
	}
	// A guide that keeps its HTML keeps the library's records too.
	acceptHTML(t, first, "vsx")
	publishAndClose(t, first)

	second := openReplacing(t, base)
	if err := second.Accept(archiveHTMLWithPDF(t, second, "acls", "failed")); err != nil {
		t.Fatal(err)
	}
	manifest := publishAndClose(t, second)
	if !manifest.HTMLGuides["acls"].AtReleaseRoot || !manifest.Attempts[0].RetainedPrevious {
		t.Fatalf("failed PDF displaced the earlier PDF: %+v", manifest)
	}
	names := directoryNames(t, second.Target)
	if !slices.Contains(names, converted.HTML.GeneratedPDF.Path) || slices.Contains(names, "acls") {
		t.Fatalf("retained guide is not only its PDF in the release folder: %v", names)
	}
}

func TestGuideMovesBetweenReleaseFolderAndItsFolder(t *testing.T) {
	base := t.TempDir()
	first := openReplacing(t, base)
	converted := archiveHTMLWithPDF(t, first, "acls", "complete")
	if err := first.Accept(converted); err != nil {
		t.Fatal(err)
	}
	// A guide that keeps its HTML keeps the library's records too.
	acceptHTML(t, first, "vsx")
	publishAndClose(t, first)

	// Downloaded again without --replace-html-with-pdf, the guide is HTML in
	// its folder again and its PDF leaves the release folder.
	second, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptHTML(t, second, "acls")
	manifest := publishAndClose(t, second)
	names := directoryNames(t, second.Target)
	if manifest.HTMLGuides["acls"].AtReleaseRoot || slices.Contains(names, converted.HTML.GeneratedPDF.Path) ||
		!slices.Contains(names, "acls") {
		t.Fatalf("guide did not return to its folder: %v %+v", names, manifest.Guides)
	}

	third := openReplacing(t, base)
	if err := third.Accept(archiveHTMLWithPDF(t, third, "acls", "complete")); err != nil {
		t.Fatal(err)
	}
	manifest = publishAndClose(t, third)
	names = directoryNames(t, third.Target)
	if !manifest.HTMLGuides["acls"].AtReleaseRoot || !slices.Contains(names, converted.HTML.GeneratedPDF.Path) ||
		slices.Contains(names, "acls") {
		t.Fatalf("guide folder was not removed: %v %+v", names, manifest.Guides)
	}
}

func TestSamePDFNameKeepsTheSecondGuideInItsFolder(t *testing.T) {
	base := t.TempDir()
	r := openReplacing(t, base)
	first := archiveTitledHTMLWithPDF(t, r, "acls", "Shared Guide", "complete")
	if err := r.Accept(first); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(archiveTitledHTMLWithPDF(t, r, "acls2", "Shared Guide", "complete")); err != nil {
		t.Fatal(err)
	}
	manifest := publishAndClose(t, r)
	if !manifest.HTMLGuides["acls"].AtReleaseRoot || manifest.HTMLGuides["acls2"].AtReleaseRoot ||
		!slices.ContainsFunc(manifest.Attempts[1].Warnings, func(warning string) bool {
			return strings.Contains(warning, "already holds a PDF named")
		}) {
		t.Fatalf("PDF name collision was not resolved: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(r.Target, "acls2", first.HTML.GeneratedPDF.Path)); err != nil {
		t.Fatalf("second guide did not keep its PDF in its folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Target, "acls2", "index.html")); err != nil {
		t.Fatalf("second guide did not keep its HTML: %v", err)
	}
	reopened, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatalf("library does not reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
