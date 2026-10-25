package storage

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestPortableNamesAndPDFLabels(t *testing.T) {
	for _, label := range []string{".", "..", "/", ""} {
		if _, err := SafeComponent(label); err == nil {
			t.Errorf("unsafe empty component accepted: %q", label)
		}
	}
	name, err := SafeComponent("CON")
	if err != nil || strings.EqualFold(name, "CON") {
		t.Fatalf("Windows reserved component accepted: %s %v", name, err)
	}
	filename, err := PDFFilename("6300", "10.10", "API / CLI: Guide?")
	if err != nil || filename != "6300 - 10.10 - API - CLI Guide.pdf" {
		t.Fatalf("wrong readable filename: %s %v", filename, err)
	}
	filename, err = PDFFilename("6300", "10.10", strings.Repeat("\u6587", 200))
	if err != nil || len(filename) > 240 || !utf8.ValidString(filename) {
		t.Fatalf("invalid long Unicode filename: %s %v", filename, err)
	}
	for _, path := range []string{".", "../outside", "/absolute", "a\\b", "a/../../b"} {
		if SafePath(path) == nil {
			t.Errorf("unsafe managed path accepted: %s", path)
		}
	}
}

func TestPDFFilenameRemovesFormatCharacters(t *testing.T) {
	title := "Job\u200bScheduler\u00ad Guide \u202efdp.exe\u202c \u2066x\u2069\u200e\ufeff"
	filename, err := PDFFilename("6300", "10.10", title)
	if err != nil || filename != "6300 - 10.10 - JobScheduler Guide fdp.exe x.pdf" {
		t.Fatalf("format characters kept in PDF filename: %q %v", filename, err)
	}
	for _, r := range filename {
		if unicode.In(r, unicode.Cf) || unicode.IsControl(r) {
			t.Fatalf("PDF filename contains %U", r)
		}
	}
	if _, err := PDFFilename("6300", "10.10", "\u202e\u200b"); err == nil {
		t.Fatal("title made only of format characters produced a filename")
	}
	legacy := "6300 - 10.10 - Job\u200bScheduler\u00ad Guide \u202efdp.exe\u202c \u2066x\u2069\u200e\ufeff.pdf"
	for name, accepted := range map[string]bool{filename: true, legacy: true, "6300 - 10.10 - Other.pdf": false} {
		if PDFFilenameAccepted("6300", "10.10", title, name) != accepted {
			t.Errorf("PDFFilenameAccepted(%q) = %v", name, !accepted)
		}
	}
	plain, err := PDFFilename("6300", "10.10", "API / CLI: Guide?")
	if err != nil || !PDFFilenameAccepted("6300", "10.10", "API / CLI: Guide?", plain) {
		t.Fatalf("plain title name not accepted: %q %v", plain, err)
	}
}
