package pdfcheck

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func generatedFixture(pages int, media string, image, links bool) []byte {
	var body strings.Builder
	body.WriteString("%PDF-1.7\n")
	body.WriteString("1 0 obj<</Type /Catalog /Pages 2 0 R /Outlines 9 0 R>>endobj\n")
	for index := 0; index < pages; index++ {
		fmt.Fprintf(&body, "%d 0 obj<</Type /Page /MediaBox %s /Resources<</Font<</F1 8 0 R>>>>", index+2, media)
		if links {
			body.WriteString(" /Annots[10 0 R]")
		}
		body.WriteString(">>endobj\n")
	}
	body.WriteString("8 0 obj<</Type /Font>>endobj\n")
	if image {
		body.WriteString("11 0 obj<</Type /XObject /Subtype /Image>>endobj\n")
	}
	offset := body.Len()
	body.WriteString("xref\n0 1\n0000000000 65535 f \n")
	fmt.Fprintf(&body, "trailer<</Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", offset)
	return []byte(body.String())
}

func TestInspectGeneratedChecksPagesMediaFontsLinksAndImages(t *testing.T) {
	body := generatedFixture(2, "[0 0 612 792]", true, true)
	summary, err := InspectGenerated(bytes.NewReader(body), int64(len(body)), GeneratedExpectations{
		MaxPages: 10, RequireLinks: true, RequireImage: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.PageCount != 2 || summary.LetterMediaBoxes != 2 ||
		summary.FontObjects < 2 || summary.ImageObjects != 1 ||
		summary.AnnotationObjects != 2 || summary.OutlineObjects != 1 {
		t.Fatalf("unexpected generated PDF summary: %+v", summary)
	}
}

func TestInspectGeneratedRejectsBoundsAndMissingStructure(t *testing.T) {
	tests := []struct {
		name     string
		body     []byte
		expected GeneratedExpectations
	}{
		{"page-limit", generatedFixture(2, "[0 0 612 792]", false, false), GeneratedExpectations{MaxPages: 1}},
		{"media", generatedFixture(1, "[0 0 595 842]", false, false), GeneratedExpectations{MaxPages: 2}},
		{"image", generatedFixture(1, "[0 0 612 792]", false, false), GeneratedExpectations{MaxPages: 2, RequireImage: true}},
		{"links", generatedFixture(1, "[0 0 612 792]", false, false), GeneratedExpectations{MaxPages: 2, RequireLinks: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := InspectGenerated(bytes.NewReader(test.body), int64(len(test.body)), test.expected); err == nil {
				t.Fatal("invalid generated PDF was accepted")
			}
		})
	}
}
