package json5

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// The upstream byte scanner needs JSON5 Unicode whitespace represented as
// scanner whitespace. Preserve byte offsets and never normalize string values.
type unicodeReader struct {
	reader      *bufio.Reader
	pending     []byte
	quote       rune
	escaped     bool
	line, block bool
	previous    rune
	offset      int64
}

func (r *unicodeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		ch, size, err := r.reader.ReadRune()
		if err != nil {
			return 0, err
		}
		r.offset += int64(size)
		if ch == utf8.RuneError && size == 1 {
			return 0, &SyntaxError{"invalid UTF-8 input", r.offset}
		}
		out := []byte(string(ch))
		resetPrevious := false
		switch {
		case r.quote != 0:
			if r.escaped {
				r.escaped = false
			} else if ch == '\\' {
				r.escaped = true
			} else if ch == r.quote {
				r.quote = 0
			} else if ch == '\u2028' || ch == '\u2029' {
				return 0, &SyntaxError{"unescaped line terminator in string", r.offset}
			}
		case r.block:
			if r.previous == '*' && ch == '/' {
				r.block = false
				resetPrevious = true
			}
		case r.line:
			if ch == '\r' || ch == '\n' || ch == '\u2028' || ch == '\u2029' {
				r.line = false
				out = bytes.Repeat([]byte{' '}, size)
				out[0] = '\n'
				resetPrevious = true
			}
		default:
			if r.previous == '/' && ch == '*' {
				r.block = true
			} else if r.previous == '/' && ch == '/' {
				r.line = true
			} else if ch == '"' || ch == '\'' {
				r.quote = ch
			} else if ch == '\uFEFF' || unicode.Is(unicode.Zs, ch) || ch == '\u2028' || ch == '\u2029' {
				out = bytes.Repeat([]byte{' '}, size)
			}
		}
		if resetPrevious {
			r.previous = 0
		} else {
			r.previous = ch
		}
		r.pending = out
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func normalizedReader(reader io.Reader) io.Reader {
	return &unicodeReader{reader: bufio.NewReader(reader)}
}

func normalizeInput(data []byte) ([]byte, error) {
	return io.ReadAll(normalizedReader(bytes.NewReader(data)))
}

func identifierStart(r rune) bool {
	return r == '$' || r == '_' || unicode.IsLetter(r) || unicode.Is(unicode.Nl, r)
}

func identifierPart(r rune) bool {
	return identifierStart(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) ||
		unicode.Is(unicode.Nd, r) || unicode.Is(unicode.Pc, r) || r == '\u200C' || r == '\u200D'
}

func decodeKey(raw []byte) ([]byte, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if raw[0] == '"' || raw[0] == '\'' {
		return unquoteBytes(raw)
	}
	var b strings.Builder
	first := true
	for len(raw) > 0 {
		var r rune
		var size int
		if raw[0] == '\\' {
			r = getu4(raw)
			if r < 0 {
				return nil, false
			}
			size = 6
			if utf16.IsSurrogate(r) {
				if len(raw) < 12 {
					return nil, false
				}
				next := getu4(raw[6:])
				r = utf16.DecodeRune(r, next)
				if r == unicode.ReplacementChar {
					return nil, false
				}
				size = 12
			}
		} else {
			r, size = utf8.DecodeRune(raw)
		}
		if (first && !identifierStart(r)) || (!first && !identifierPart(r)) {
			return nil, false
		}
		b.WriteRune(r)
		raw = raw[size:]
		first = false
	}
	return []byte(b.String()), true
}
