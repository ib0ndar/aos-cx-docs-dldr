package storage

import (
	"strings"
	"testing"
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
