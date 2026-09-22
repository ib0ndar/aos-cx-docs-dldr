package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
	"aos-cx-docs-dldr/internal/testutil"
)

type fixtureFetcher struct {
	body []byte
	mime string
}

func (f fixtureFetcher) Get(ctx context.Context, url string, refresh bool) (model.Resource, error) {
	return model.Resource{URL: url, Status: 200, Headers: http.Header{"Content-Type": {f.mime}},
		Body: io.NopCloser(bytes.NewReader(f.body))}, nil
}

func TestPublisherPDFPreservesBytesAndOneFileLayout(t *testing.T) {
	doc := model.Document{ID: "jobscheduler", Title: "Job Scheduler Guide", Platform: "6300", Version: "10.10", Kind: "pdf", URL: "https://example.test/book.pdf"}
	plan, err := source.PublisherPDFPlan(doc)
	if err != nil {
		t.Fatal(err)
	}
	plan.Notices = []model.Notice{{
		Kind: model.NoticeSourcePolicy, Message: "Validated publisher routing policy",
	}}
	body := testutil.PDF("original \x00\xff bytes")
	dir := t.TempDir()
	result, err := PublisherPDF(context.Background(), plan, fixtureFetcher{body, "application/pdf"}, dir, false, 1<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("%+v %v", result, err)
	}
	expectedName := "6300 - 10.10 - Job Scheduler Guide.pdf"
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != expectedName {
		t.Fatalf("not a one-file guide: %v", entries)
	}
	saved, err := os.ReadFile(filepath.Join(dir, expectedName))
	if err != nil || !bytes.Equal(body, saved) {
		t.Fatal("publisher bytes changed")
	}
	sum := sha256.Sum256(body)
	if result.PDF.SHA256 != hex.EncodeToString(sum[:]) || result.PDF.SourceSHA256 != result.PDF.SHA256 || result.PDF.Size != int64(len(body)) {
		t.Fatalf("incorrect original/source provenance: %+v", result.PDF)
	}
	if result.PDF.URL != doc.URL || result.PDF.Origin != model.PDFOriginDirectMapped ||
		result.PDF.SourceVerified || len(result.PDF.Inputs) != 0 {
		t.Fatalf("direct mapped PDF behavior changed: %+v", result.PDF)
	}
	if len(result.Notices) != 1 || result.Notices[0] != plan.Notices[0] {
		t.Fatalf("publisher PDF result lost plan notices: %+v", result.Notices)
	}
}

func TestInvalidPDFNeverPublishesACompleteFile(t *testing.T) {
	valid := testutil.PDF("valid")
	for _, tc := range []struct {
		name, mime string
		body       []byte
	}{
		{"html", "text/html", valid},
		{"json", "application/json", []byte(`{"error":"denied"}`)},
		{"fake", "application/pdf", []byte("%PDF-1.7\n<html>Access denied</html>\n%%EOF\n")},
		{"truncated", "application/pdf", valid[:len(valid)-10]},
		{"bad_xref", "application/pdf", []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\nstartxref\n9\n%%EOF\n")},
		{"trailing", "application/pdf", append(append([]byte{}, valid...), []byte("incomplete extra data")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			doc := model.Document{ID: "pdf", Title: "Guide", Platform: "6300", Version: "10.10", Kind: "pdf", URL: "https://example.test/pdf.pdf"}
			result, err := PublisherPDF(context.Background(), model.DocumentPlan{Document: doc, PDFURL: doc.URL}, fixtureFetcher{tc.body, tc.mime}, dir, false, 1<<20)
			if err == nil || result.Status == "complete" || len(result.Errors) == 0 {
				t.Fatalf("invalid PDF accepted: %+v %v", result, err)
			}
			files, _ := filepath.Glob(filepath.Join(dir, "*.pdf"))
			if len(files) != 0 {
				t.Fatal("invalid PDF exposed as final file")
			}
		})
	}
}

func TestPDFOnlySelectionAndLimits(t *testing.T) {
	if _, err := source.PublisherPDFPlan(model.Document{Kind: "flare", URL: "https://example.test/a.html"}); err == nil {
		t.Fatal("HTML guide substituted with guessed PDF")
	}
	doc := model.Document{ID: "pdf", Title: "Guide", Platform: "6300", Version: "10.10", Kind: "pdf", URL: "https://example.test/a.pdf"}
	plan := model.DocumentPlan{Document: doc, PDFURL: doc.URL}
	_, err := PublisherPDF(context.Background(), plan, fixtureFetcher{testutil.PDF("too large"), "application/octet-stream"}, t.TempDir(), false, 32)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("byte limit ignored: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := PublisherPDF(ctx, plan, fixtureFetcher{testutil.PDF("cancel"), "application/pdf"}, t.TempDir(), false, 1<<20)
	if err == nil || result.Status != "failed" {
		t.Fatal("cancelled PDF accepted")
	}
}

func TestResolvedPublisherPDFPlanRequiresExactEvidence(t *testing.T) {
	body := testutil.PDF("resolved")
	sum := sha256.Sum256(body)
	selected := "https://example.test/guide/index.html"
	pdfURL := "https://example.test/guide/original.pdf"
	valid := model.DocumentPlan{
		Document: model.Document{
			ID: "guide", Title: "Guide", Platform: "6300", Version: "10.18.xxxx",
			Kind: "pdf", URL: selected,
		},
		PDFURL:      pdfURL,
		PDFVerified: true,
		Inputs: []model.SourceInput{{
			RequestedURL: selected,
			FinalURL:     pdfURL,
			Role:         "front",
			ContentType:  "application/pdf",
			SHA256:       hex.EncodeToString(sum[:]),
			Size:         len(body),
			ObservedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		}},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "pdf", RootURL: pdfURL},
	}
	if err := ValidateResolvedPublisherPDFPlan(valid); err != nil {
		t.Fatalf("valid resolved plan rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*model.DocumentPlan)
	}{
		{"not-verified", func(plan *model.DocumentPlan) { plan.PDFVerified = false }},
		{"missing-url", func(plan *model.DocumentPlan) { plan.PDFURL = "" }},
		{"missing-input", func(plan *model.DocumentPlan) { plan.Inputs = nil }},
		{"wrong-final", func(plan *model.DocumentPlan) { plan.Inputs[0].FinalURL = selected }},
		{"bad-hash", func(plan *model.DocumentPlan) { plan.Inputs[0].SHA256 = strings.Repeat("z", 64) }},
		{"short-size", func(plan *model.DocumentPlan) { plan.Inputs[0].Size = 31 }},
		{"missing-inventory", func(plan *model.DocumentPlan) { plan.Inventory = nil }},
		{"wrong-inventory-kind", func(plan *model.DocumentPlan) { plan.Inventory.Kind = "static" }},
		{"wrong-inventory-root", func(plan *model.DocumentPlan) { plan.Inventory.RootURL = selected }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := valid
			plan.Inputs = append([]model.SourceInput{}, valid.Inputs...)
			inventory := *valid.Inventory
			plan.Inventory = &inventory
			tc.mutate(&plan)
			if err := ValidateResolvedPublisherPDFPlan(plan); err == nil {
				t.Fatalf("inconsistent resolved plan accepted: %+v", plan)
			}
		})
	}
}

func TestResolvedPublisherPDFMustMatchVerifiedBytes(t *testing.T) {
	planned := testutil.PDF("planned")
	sum := sha256.Sum256(planned)
	pdfURL := "https://example.test/guide/original.pdf"
	plan := model.DocumentPlan{
		Document: model.Document{
			ID: "guide", Title: "Guide", Platform: "6300", Version: "10.18.xxxx",
			Kind: "pdf", URL: "https://example.test/guide/index.html",
		},
		PDFURL:      pdfURL,
		PDFVerified: true,
		Inputs: []model.SourceInput{{
			RequestedURL: "https://example.test/guide/index.html",
			FinalURL:     pdfURL,
			Role:         "front",
			ContentType:  "application/pdf",
			SHA256:       hex.EncodeToString(sum[:]),
			Size:         len(planned),
		}},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "pdf", RootURL: pdfURL},
	}
	result, err := PublisherPDF(context.Background(), plan,
		fixtureFetcher{testutil.PDF("changed"), "application/pdf"}, t.TempDir(), false, 1<<20)
	if err == nil || result.Status != "failed" || !strings.Contains(err.Error(), "changed after source verification") {
		t.Fatalf("changed resolved PDF accepted: result=%+v err=%v", result, err)
	}
}
