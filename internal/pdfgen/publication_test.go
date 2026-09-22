package pdfgen

import (
	"os"
	"path/filepath"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
)

func TestExpectedSettingsUseExplicitOutlineSchema(t *testing.T) {
	settings := ExpectedSettings()
	if settings.SchemaVersion != 5 ||
		settings.OutlinePlanSchemaVersion != QPDFOutlinePlanSchemaVersion ||
		settings.FooterPlanSchemaVersion != FooterPlanSchemaVersion ||
		settings.HeadingStyleVersion != HeadingStyleVersion ||
		settings.FooterStyleVersion != FooterStyleVersion ||
		settings.PageNumberFormat != "decimal" ||
		settings.PageNumberPosition != "lower-right" ||
		settings.FirstVisiblePageNumber != 2 || settings.CoverFooterVisible ||
		settings.FooterCategoryPolicy != "first-authoritative-local-occurrence-parent-or-root-self-at-page-top" ||
		settings.GenerateDocumentOutline {
		t.Fatalf("generated PDF settings do not require current pagination and outline behavior: %+v", settings)
	}
}

func TestRollbackGeneratedPDFPublicationRestoresAllSurfaces(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	oldIndex := []byte("<!doctype html><title>HTML only</title>\n")
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte("new index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "guide.pdf"), []byte("new PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldManifest := model.HTMLArchive{
		SchemaVersion: 1,
		Status:        "complete",
		Integrity:     model.HTMLIntegrity{IndexSHA256: "old-index-hash"},
	}
	newManifest := oldManifest
	newManifest.GeneratedPDF = &model.GeneratedPDFRecord{Path: "guide.pdf", Status: "complete"}
	if err := storage.WriteJSON(root, "manifest.json", &newManifest); err != nil {
		t.Fatal(err)
	}

	if err := rollbackGeneratedPDFPublication(root, "guide.pdf", oldIndex, &oldManifest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "guide.pdf")); !os.IsNotExist(err) {
		t.Fatalf("rolled-back PDF still exists: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(directory, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(index) != string(oldIndex) {
		t.Fatalf("restored index = %q, want %q", index, oldIndex)
	}
	var restored model.HTMLArchive
	if err := storage.ReadJSON(root, "manifest.json", &restored); err != nil {
		t.Fatal(err)
	}
	if restored.GeneratedPDF != nil || restored.Integrity.IndexSHA256 != oldManifest.Integrity.IndexSHA256 {
		t.Fatalf("restored manifest = %+v, want %+v", restored, oldManifest)
	}
}
