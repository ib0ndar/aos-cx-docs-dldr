package pdfcheck

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"regexp"
	"strconv"
)

var header = regexp.MustCompile(`^%PDF-(?:1\.[0-7]|2\.0)[\r\n\t ]`)
var htmlBody = regexp.MustCompile(`(?is)^%PDF-[^\r\n]*[\r\n]+\s*<(?:!doctype|html|body)\b`)
var ending = regexp.MustCompile(`startxref\s+([0-9]+)\s+%%EOF[\r\n\t ]*$`)
var xrefTable = regexp.MustCompile(`^xref[\r\n\t ]+[0-9]+\s+[0-9]+[\r\n\t ]`)
var xrefObject = regexp.MustCompile(`^[0-9]+[\x00\t\n\f\r ]+[0-9]+[\x00\t\n\f\r ]+obj[\x00\t\n\f\r ()<>\[\]{}/%]`)
var xrefType = regexp.MustCompile(`/Type\s*/XRef\b`)

// Validate checks bounded original-PDF byte structure without extracting or
// modifying content. It is shared by source planning and PDF publication.
func Validate(reader io.ReaderAt, size int64, contentType string) error {
	mediaType := ""
	if contentType != "" {
		var err error
		mediaType, _, err = mime.ParseMediaType(contentType)
		if err != nil {
			return fmt.Errorf("invalid PDF content type: %w", err)
		}
	}
	if mediaType != "" && mediaType != "application/pdf" && mediaType != "application/octet-stream" && mediaType != "binary/octet-stream" {
		return fmt.Errorf("expected original PDF, received content type %q", mediaType)
	}
	if size < 32 {
		return errors.New("PDF is too short to contain a complete document")
	}
	head := make([]byte, min(size, 1024))
	if _, err := reader.ReadAt(head, 0); err != nil {
		return err
	}
	if !header.Match(head) {
		return errors.New("response does not begin with a supported PDF header")
	}
	if htmlBody.Match(head) {
		return errors.New("PDF-labelled response contains an HTML error page")
	}
	tail := make([]byte, min(size, 64<<10))
	if _, err := reader.ReadAt(tail, size-int64(len(tail))); err != nil {
		return err
	}
	match := ending.FindSubmatch(tail)
	if match == nil {
		return errors.New("PDF has no complete final startxref/EOF marker")
	}
	offset, err := strconv.ParseInt(string(match[1]), 10, 64)
	if err != nil || offset < int64(len("%PDF-1.0")) || offset >= size {
		return errors.New("PDF cross-reference offset is outside the document")
	}
	xref := make([]byte, min(size-offset, 4096))
	if _, err := reader.ReadAt(xref, offset); err != nil {
		return err
	}
	xref = bytes.TrimLeft(xref, "\r\n\t ")
	if !xrefTable.Match(xref) && !(xrefObject.Match(xref) && xrefType.Match(xref)) {
		return errors.New("PDF startxref does not identify an xref table or stream")
	}
	return nil
}
