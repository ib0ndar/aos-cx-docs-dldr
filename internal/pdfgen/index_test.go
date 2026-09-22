package pdfgen

import (
	"bytes"
	"os"
	"testing"
)

func TestGeneratedPDFIndexAddsOneEscapedCompanionLink(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`<!doctype html><html><body><main class="archive-content"><p>Source</p></main></body></html>`)
	if err := os.WriteFile(dir+"/index.html", body, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	updated, original, err := generatedPDFIndex(root, "6300 - Guide.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, body) ||
		bytes.Count(updated, []byte("archive-generated-pdf")) != 1 ||
		!bytes.Contains(updated, []byte("6300%20-%20Guide.pdf")) {
		t.Fatalf("generated PDF link is invalid: %s", updated)
	}
	if err := os.WriteFile(dir+"/index.html", updated, 0o600); err != nil {
		t.Fatal(err)
	}
	again, _, err := generatedPDFIndex(root, "6300 - Guide.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(again, []byte("archive-generated-pdf")) != 1 {
		t.Fatalf("generated PDF link was duplicated: %s", again)
	}
}
