//go:build darwin

package pdfacceptance

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"strings"
	"testing"
)

func TestParseStructuredTextStrictHierarchyAndGeometry(t *testing.T) {
	body := `<?xml version="1.0"?>
<document filename="fixture.pdf"><page id="page1" width="612" height="792">
<block bbox="1 2 30 40"><line bbox="1 2 30 14" text="  Ａ  B ">
<font name="Example" size="12"><char c="A" quad="1 2 3 2 1 4 3 4"/></font>
<font name="Example Bold" size="16"><char c="B" quad="4 2 6 2 4 4 6 4"/></font>
</line></block></page></document>`
	document, err := parseStructuredText(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := AcceptanceLine{
		BBox: [4]float64{1, 2, 30, 14}, Block: 0, MaxFontPoints: 16, Page: 1, Text: "A B",
	}
	if len(document.Pages) != 1 || len(document.Pages[0].Lines) != 1 ||
		!reflect.DeepEqual(document.Pages[0].Lines[0], want) {
		t.Fatalf("unexpected structured text: %+v", document)
	}
}

func TestParseStructuredTextRejectsMaliciousAndMalformedXML(t *testing.T) {
	tests := map[string]string{
		"directive":      `<!DOCTYPE document [<!ENTITY x SYSTEM "file:///etc/passwd">]><document><page width="612" height="792"/></document>`,
		"entity":         `<document><page width="612" height="792"><block><line bbox="0 0 1 1" text="&unknown;"/></block></page></document>`,
		"trailing root":  `<document><page width="612" height="792"/></document><document/>`,
		"nonfinite page": `<document><page width="NaN" height="792"/></document>`,
		"nonfinite bbox": `<document><page width="612" height="792"><block><line bbox="0 0 Inf 1" text="x"/></block></page></document>`,
		"bad hierarchy":  `<document><page width="612" height="792"><line bbox="0 0 1 1" text="x"/></page></document>`,
		"unsupported":    `<document><page width="612" height="792"><unknown/></page></document>`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseStructuredText(strings.NewReader(body)); err == nil {
				t.Fatal("malicious/malformed structured text was accepted")
			}
		})
	}
}

func TestMutoolManifestJSONRejectsDuplicateKeysAndTrailingRoots(t *testing.T) {
	for _, value := range []string{
		`{"mutool_path":"first","mutool_path":"second"}`,
		`{"nested":{"key":1,"key":2}}`,
		`{"valid":true}{"trailing":true}`,
	} {
		if err := rejectDuplicateJSONKeys([]byte(value)); err == nil {
			t.Fatalf("ambiguous manifest JSON was accepted: %s", value)
		}
	}
	if err := rejectDuplicateJSONKeys([]byte(`{"metadata":[{"one":1},{"two":2}]}`)); err != nil {
		t.Fatalf("valid nested JSON rejected: %v", err)
	}
}

func TestSanitizedMuPDFEnvironmentRemovesDyldInjection(t *testing.T) {
	got := sanitizedMuPDFEnvironment([]string{"PATH=/bin", "DYLD_LIBRARY_PATH=/tmp", "DYLD_INSERT_LIBRARIES=x", "HOME=/tmp"})
	want := []string{"PATH=/bin", "HOME=/tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized environment=%q want=%q", got, want)
	}
}

func TestMontageGeometryAndEncodingAreDeterministic(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 918, 1188))
	for y := 0; y < source.Bounds().Dy(); y++ {
		for x := 0; x < source.Bounds().Dx(); x++ {
			source.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}
	thumbnail := labelledThumbnail(source, "physical page 00001")
	if thumbnail.Bounds().Dx() != 420 || thumbnail.Bounds().Dy() != 572 {
		t.Fatalf("thumbnail bounds=%v", thumbnail.Bounds())
	}
	montage := buildMontage([]image.Image{thumbnail, thumbnail, thumbnail, thumbnail})
	if montage.Bounds().Dx() != 1260 || montage.Bounds().Dy() != 1144 {
		t.Fatalf("montage bounds=%v", montage.Bounds())
	}
	var first, second bytes.Buffer
	if err := png.Encode(&first, montage); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&second, montage); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("stdlib montage encoding is not deterministic")
	}
}
