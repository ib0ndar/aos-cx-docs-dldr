package archive

import (
	"os"
	"path/filepath"
	"testing"

	"aos-cx-docs-dldr/internal/testutil"
)

func TestXRefObjectUsesPDFLexicalBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, separator string
		valid           bool
	}{
		{"dictionary-delimiter", "", true},
		{"space", " ", true},
		{"newline", "\n", true},
		{"carriage-return", "\r", true},
		{"tab", "\t", true},
		{"form-feed", "\f", true},
		{"null-whitespace", "\x00", true},
		{"comment-delimiter", "% fixture comment\n", true},
		{"identifier-prefix", "ect", false},
		{"letter-suffix", "X", false},
		{"digit-suffix", "0", false},
		{"underscore-suffix", "_", false},
		{"regular-plus", "+", false},
		{"regular-semicolon", ";", false},
		{"non-PDF-whitespace", "\v", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.bin")
			if err := os.WriteFile(path, testutil.XRefStreamPDF(tc.separator), 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			err = ValidatePublisherPDF(f, "application/pdf")
			if (err == nil) != tc.valid {
				t.Fatalf("PDF lexical boundary valid=%v, got error=%v", tc.valid, err)
			}
		})
	}
}
