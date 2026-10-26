package pdfcheck

import (
	"bytes"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/testutil"
)

func TestDocumentTitleReadsTheInformationDictionary(t *testing.T) {
	for _, tc := range []struct {
		name, title, want string
		update            bool
	}{
		{"literal with escapes", `(AOS-CX 10.16.xxxx CoPP Guide \(8100, 8320, 8325\) \101\\)`, `AOS-CX 10.16.xxxx CoPP Guide (8100, 8320, 8325) A\`, false},
		{"balanced parentheses", `(Guide (8325) edition)`, `Guide (8325) edition`, false},
		{"UTF-16 hexadecimal", `<FEFF0043006F005000500020 00470075 00690064 0065>`, `CoPP Guide`, false},
		{"incremental update", `(Updated title)`, `Updated title`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := testutil.TitledPDF("title", tc.title, tc.update)
			got, err := DocumentTitle(bytes.NewReader(body), int64(len(body)))
			if err != nil || got != tc.want {
				t.Fatalf("title = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestDocumentTitleRefusesWhatItCannotLocate(t *testing.T) {
	untitled := testutil.PDF("untitled")
	noTitle := []byte(strings.Replace(string(testutil.TitledPDF("t", "(x)", false)), "/Title(x)", "/Subject(x)", 1))
	stream := []byte("%PDF-1.5\n1 0 obj\n<< /Type /Catalog >>\nendobj\n" +
		"2 0 obj\n<< /Type /XRef /Size 3 /Info 1 0 R /W [1 2 1] /Length 0 >>\nstream\n\nendstream\nendobj\n" +
		"startxref\n44\n%%EOF\n")
	for name, body := range map[string][]byte{
		"no information dictionary": untitled,
		"no title":                  noTitle,
		"cross-reference stream":    stream,
	} {
		if title, err := DocumentTitle(bytes.NewReader(body), int64(len(body))); err == nil {
			t.Errorf("%s: read title %q", name, title)
		}
	}
}
