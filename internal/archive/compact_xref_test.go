package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/testutil"
)

func TestCompactXRefStreamIsArchivedWithoutChanges(t *testing.T) {
	body := testutil.XRefStreamPDF("")
	streamAt := bytes.Index(body, []byte("\nstream\n")) + len("\nstream\n")
	for object := 1; object <= 4; object++ {
		offset := binary.BigEndian.Uint32(body[streamAt+7*object+1 : streamAt+7*object+5])
		if !bytes.HasPrefix(body[offset:], []byte(fmt.Sprintf("%d 0 obj", object))) {
			t.Fatal("fixture cross-reference entry has an incorrect object offset")
		}
	}
	document := model.Document{ID: "compact", Title: "Compact Guide", Platform: "6300", Version: "10.10",
		Kind: "pdf", URL: "https://example.test/compact.pdf"}
	dir := t.TempDir()
	result, err := PublisherPDF(context.Background(), model.DocumentPlan{Document: document, PDFURL: document.URL},
		fixtureFetcher{body: body, mime: "application/pdf"}, dir, false, 1<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("valid compact xref stream rejected: status=%s err=%v", result.Status, err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, result.PDF.Path))
	if err != nil || !bytes.Equal(saved, body) {
		t.Fatalf("compact publisher bytes changed: %v", err)
	}
}
