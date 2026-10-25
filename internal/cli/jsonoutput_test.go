package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestJSONOutputEscapesTerminalControlsWithoutChangingValues(t *testing.T) {
	// DEL, C1 CSI/OSC, bidi override/pop/isolates, zero-width space/joiner,
	// soft hyphen, BOM, a supplementary-plane tag character, and the line and
	// paragraph separators that encoding/json already escapes.
	controls := []rune{0x7f, 0x9b, 0x9d, 0x202e, 0x202c, 0x2066, 0x2069, 0x200b, 0x200d, 0xad, 0xfeff, 0xe0041, 0x2028, 0x2029}
	var title strings.Builder
	title.WriteString("Guide \x1b[2J\a")
	for _, char := range controls {
		title.WriteRune(char)
		title.WriteString("x")
	}
	title.WriteString(" 文档 <b>&")
	value := map[string]any{"title": title.String(), "lines": []string{"a b c", "plain"}}
	var output bytes.Buffer
	if err := writeJSON(&output, value); err != nil {
		t.Fatal(err)
	}
	encoded := output.String()
	for _, char := range append([]rune{0x1b, 0x07}, controls...) {
		if strings.ContainsRune(encoded, char) {
			t.Fatalf("JSON output retained raw U+%04X: %q", char, encoded)
		}
	}
	for _, char := range controls {
		escape := fmt.Sprintf(`\u%04x`, char)
		if char > 0xffff {
			high, low := utf16.EncodeRune(char)
			escape = fmt.Sprintf(`\u%04x\u%04x`, high, low)
		}
		if !strings.Contains(encoded, escape) {
			t.Fatalf("JSON output lacks escape %s: %s", escape, encoded)
		}
	}
	htmlEscaped := fmt.Sprintf(`\u%04xb\u%04x\u%04x`, '<', '>', '&')
	if !strings.Contains(encoded, "文档") || !strings.Contains(encoded, htmlEscaped) ||
		!strings.HasPrefix(encoded, "{\n  \"lines\": [\n    \"") || !strings.HasSuffix(encoded, "\n}\n") {
		t.Fatalf("JSON output lost Unicode text, HTML escaping or indentation: %s", encoded)
	}
	var decoded map[string]any
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["title"] != value["title"] {
		t.Fatalf("decoded title changed: got %q want %q", decoded["title"], value["title"])
	}
	lines, _ := decoded["lines"].([]any)
	if len(lines) != 2 || lines[0] != "a b c" || lines[1] != "plain" {
		t.Fatalf("decoded lines changed: %#v", decoded["lines"])
	}
}

func TestJSONOutputLeavesOrdinaryDocumentsByteIdentical(t *testing.T) {
	value := map[string]any{
		"library": "/tmp/AOS-CX", "status": "complete", "titles": []string{"Guide – 文档", "Ü"},
	}
	var output bytes.Buffer
	if err := writeJSON(&output, value); err != nil {
		t.Fatal(err)
	}
	var expected bytes.Buffer
	encoder := json.NewEncoder(&expected)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), expected.Bytes()) {
		t.Fatalf("ordinary JSON output changed:\n%s\nwant:\n%s", &output, &expected)
	}
}
