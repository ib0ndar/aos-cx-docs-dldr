package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// writeJSON writes value as the indented --json document. encoding/json
// escapes C0 controls, U+2028 and U+2029 but passes DEL, C1 controls (8-bit
// CSI and OSC introducers) and Unicode format characters (bidi overrides and
// isolates, zero-width characters) through raw, so publisher text could drive
// or visually reorder a terminal that displays the output. Those code points
// are rewritten as standard \u escapes; decoded values are unchanged.
func writeJSON(out io.Writer, value any) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	_, err := out.Write(escapeJSONTerminalControls(buffer.Bytes()))
	return err
}

// escapeJSONTerminalControls escapes every DEL, C1 control and Unicode Cc, Cf,
// Zl or Zp code point in encoded JSON. Such code points can occur only inside
// string literals, where a \u escape is equivalent; structural whitespace is
// ASCII and below DEL, so it is never touched. Code points outside the Basic
// Multilingual Plane are written as UTF-16 surrogate pairs, as JSON requires.
func escapeJSONTerminalControls(data []byte) []byte {
	var output []byte
	copied := 0
	for index := 0; index < len(data); {
		if data[index] < utf8.RuneSelf && data[index] != 0x7f {
			index++
			continue
		}
		char, size := utf8.DecodeRune(data[index:])
		if char == utf8.RuneError && size == 1 ||
			!unicode.In(char, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			index += size
			continue
		}
		if output == nil {
			output = make([]byte, 0, len(data)+64)
		}
		output = append(output, data[copied:index]...)
		if char > 0xffff {
			high, low := utf16.EncodeRune(char)
			output = fmt.Appendf(output, `\u%04x\u%04x`, high, low)
		} else {
			output = fmt.Appendf(output, `\u%04x`, char)
		}
		index += size
		copied = index
	}
	if output == nil {
		return data
	}
	return append(output, data[copied:]...)
}
