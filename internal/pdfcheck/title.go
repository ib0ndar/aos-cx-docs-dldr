package pdfcheck

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxXRefSections = 64
	maxTitleBytes   = 4096
)

var (
	infoReference = regexp.MustCompile(`/Info[\x00\t\n\f\r ]+([0-9]{1,10})[\x00\t\n\f\r ]+([0-9]{1,5})[\x00\t\n\f\r ]+R\b`)
	prevReference = regexp.MustCompile(`/Prev[\x00\t\n\f\r ]+([0-9]{1,15})\b`)
	subsection    = regexp.MustCompile(`^([0-9]{1,10})[\t ]+([0-9]{1,10})[\t ]*\r?\n?`)
	xrefEntry     = regexp.MustCompile(`^([0-9]{10}) ([0-9]{5}) ([nf])[\r\n ]`)
	titleKey      = regexp.MustCompile(`/Title[\x00\t\n\f\r ]*([(<])`)
)

// DocumentTitle returns the Title of the document information dictionary that
// the PDF's final trailer names. It reads only what a classic cross-reference
// table locates: a PDF that keeps its trailer in a cross-reference stream, or
// its information dictionary in a compressed object stream, has no readable
// title here.
func DocumentTitle(reader io.ReaderAt, size int64) (string, error) {
	if err := Validate(reader, size, ""); err != nil {
		return "", err
	}
	tail := make([]byte, min(size, 64<<10))
	if _, err := reader.ReadAt(tail, size-int64(len(tail))); err != nil {
		return "", err
	}
	end := ending.FindSubmatchIndex(tail)
	start, err := strconv.ParseInt(string(tail[end[2]:end[3]]), 10, 64)
	if err != nil {
		return "", err
	}
	trailerAt := bytes.LastIndex(tail[:end[0]], []byte("trailer"))
	if trailerAt < 0 {
		return "", errors.New("PDF has no classic trailer to name its document information")
	}
	info := infoReference.FindSubmatch(tail[trailerAt:end[0]])
	if info == nil {
		return "", errors.New("PDF trailer names no document information dictionary")
	}
	number, _ := strconv.Atoi(string(info[1]))
	generation, _ := strconv.Atoi(string(info[2]))
	offset, err := objectOffset(reader, size, start, number, generation)
	if err != nil {
		return "", err
	}
	object := make([]byte, min(size-offset, 64<<10))
	if _, err := reader.ReadAt(object, offset); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	header := fmt.Sprintf("%d %d obj", number, generation)
	if !bytes.HasPrefix(bytes.Join(bytes.Fields(object[:min(len(object), 64)]), []byte(" ")), []byte(header)) {
		return "", fmt.Errorf("PDF cross-reference entry does not locate object %d %d", number, generation)
	}
	if stop := bytes.Index(object, []byte("endobj")); stop >= 0 {
		object = object[:stop]
	}
	key := titleKey.FindSubmatchIndex(object)
	if key == nil {
		return "", errors.New("PDF document information has no string Title")
	}
	return pdfString(object[key[2]:])
}

// objectOffset follows the classic cross-reference sections from start, the
// newest first, to the byte offset of object number with generation.
func objectOffset(reader io.ReaderAt, size, start int64, number, generation int) (int64, error) {
	seen := map[int64]bool{}
	for range maxXRefSections {
		if start < 0 || start >= size || seen[start] {
			return 0, errors.New("PDF cross-reference chain is invalid")
		}
		seen[start] = true
		position := start
		head := make([]byte, min(size-position, 64))
		if _, err := reader.ReadAt(head, position); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if !xrefTable.Match(head) {
			return 0, errors.New("PDF keeps its cross-reference in a stream; its document title is not read")
		}
		position += int64(len("xref"))
		for {
			line := make([]byte, min(size-position, 64))
			if _, err := reader.ReadAt(line, position); err != nil && !errors.Is(err, io.EOF) {
				return 0, err
			}
			trimmed := bytes.TrimLeft(line, "\r\n\t ")
			position += int64(len(line) - len(trimmed))
			if bytes.HasPrefix(trimmed, []byte("trailer")) {
				break
			}
			match := subsection.FindSubmatch(trimmed)
			if match == nil {
				return 0, errors.New("PDF cross-reference section is malformed")
			}
			first, _ := strconv.Atoi(string(match[1]))
			count, _ := strconv.Atoi(string(match[2]))
			position += int64(len(match[0]))
			if number >= first && number < first+count {
				entry := make([]byte, 20)
				if _, err := reader.ReadAt(entry, position+int64(number-first)*20); err != nil {
					return 0, err
				}
				fields := xrefEntry.FindSubmatch(entry)
				if fields == nil {
					return 0, errors.New("PDF cross-reference entry is malformed")
				}
				entryGeneration, _ := strconv.Atoi(string(fields[2]))
				if string(fields[3]) != "n" || entryGeneration != generation {
					return 0, fmt.Errorf("PDF object %d %d is not in use", number, generation)
				}
				return strconv.ParseInt(string(fields[1]), 10, 64)
			}
			position += int64(count) * 20
		}
		trailer := make([]byte, min(size-position, 4096))
		if _, err := reader.ReadAt(trailer, position); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if stop := bytes.Index(trailer, []byte("startxref")); stop >= 0 {
			trailer = trailer[:stop]
		}
		previous := prevReference.FindSubmatch(trailer)
		if previous == nil {
			return 0, fmt.Errorf("PDF cross-reference does not locate object %d %d", number, generation)
		}
		start, _ = strconv.ParseInt(string(previous[1]), 10, 64)
	}
	return 0, errors.New("PDF cross-reference chain is too long")
}

// pdfString decodes the literal or hexadecimal string that b begins with.
func pdfString(b []byte) (string, error) {
	var raw []byte
	switch b[0] {
	case '(':
		depth := 0
		for i := 0; i < len(b); i++ {
			c := b[i]
			switch {
			case c == '\\' && i+1 < len(b):
				i++
				switch e := b[i]; e {
				case 'n':
					raw = append(raw, '\n')
				case 'r':
					raw = append(raw, '\r')
				case 't':
					raw = append(raw, '\t')
				case 'b':
					raw = append(raw, '\b')
				case 'f':
					raw = append(raw, '\f')
				case '\r':
					if i+1 < len(b) && b[i+1] == '\n' {
						i++
					}
				case '\n':
				default:
					if e >= '0' && e <= '7' {
						value := int(e - '0')
						for n := 0; n < 2 && i+1 < len(b) && b[i+1] >= '0' && b[i+1] <= '7'; n++ {
							i++
							value = value*8 + int(b[i]-'0')
						}
						raw = append(raw, byte(value))
					} else {
						raw = append(raw, e)
					}
				}
				continue
			case c == '(':
				depth++
				if depth == 1 {
					continue
				}
			case c == ')':
				depth--
				if depth == 0 {
					return decodeText(raw), nil
				}
			}
			raw = append(raw, c)
			if len(raw) > maxTitleBytes {
				return "", errors.New("PDF title is too long")
			}
		}
		return "", errors.New("PDF title string is not terminated")
	case '<':
		stop := bytes.IndexByte(b, '>')
		if stop < 0 || stop > 2*maxTitleBytes+64 {
			return "", errors.New("PDF title string is not terminated")
		}
		digits := strings.Join(strings.Fields(string(b[1:stop])), "")
		if len(digits)%2 == 1 {
			digits += "0"
		}
		for i := 0; i < len(digits); i += 2 {
			value, err := strconv.ParseUint(digits[i:i+2], 16, 8)
			if err != nil {
				return "", errors.New("PDF title is not a valid hexadecimal string")
			}
			raw = append(raw, byte(value))
		}
		return decodeText(raw), nil
	}
	return "", errors.New("PDF title is not a string")
}

// decodeText reads a PDF text string: UTF-16BE or UTF-8 with a byte order
// mark, otherwise PDFDocEncoding, read here as Latin-1.
func decodeText(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte{0xfe, 0xff}):
		units := make([]uint16, 0, len(raw)/2)
		for i := 2; i+1 < len(raw); i += 2 {
			units = append(units, uint16(raw[i])<<8|uint16(raw[i+1]))
		}
		return string(utf16.Decode(units))
	case bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) && utf8.Valid(raw[3:]):
		return string(raw[3:])
	}
	runes := make([]rune, len(raw))
	for i, c := range raw {
		runes[i] = rune(c)
	}
	return string(runes)
}
