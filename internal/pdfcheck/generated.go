package pdfcheck

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

type GeneratedExpectations struct {
	MaxPages           int
	RequireLinks       bool
	RequireAnnotations bool
	RequireOutline     bool
	RequireImage       bool
}

type GeneratedSummary struct {
	PageCount         int
	LetterMediaBoxes  int
	FontObjects       int
	ImageObjects      int
	AnnotationObjects int
	OutlineObjects    int
}

var (
	pageObject       = regexp.MustCompile(`/Type[\x00\t\n\f\r ]+/Page\b`)
	mediaBox         = regexp.MustCompile(`/MediaBox[\x00\t\n\f\r ]*\[[\x00\t\n\f\r ]*([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))[\x00\t\n\f\r ]+([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))[\x00\t\n\f\r ]+([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))[\x00\t\n\f\r ]+([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))[\x00\t\n\f\r ]*\]`)
	fontObject       = regexp.MustCompile(`/Font\b`)
	imageObject      = regexp.MustCompile(`/Subtype[\x00\t\n\f\r ]+/Image\b`)
	annotationObject = regexp.MustCompile(`/Annots\b`)
	outlineObject    = regexp.MustCompile(`/Outlines\b`)
)

// InspectGenerated adds bounded Chrome-output checks to Validate. It verifies
// that each emitted page declares a Letter portrait/landscape media box and
// that expected document resources are represented in the PDF object graph.
func InspectGenerated(reader io.ReaderAt, size int64, expected GeneratedExpectations) (GeneratedSummary, error) {
	var summary GeneratedSummary
	if expected.MaxPages < 1 {
		return summary, errors.New("generated PDF page limit must be positive")
	}
	if err := Validate(reader, size, "application/pdf"); err != nil {
		return summary, err
	}
	patterns := []struct {
		re    *regexp.Regexp
		count *int
	}{
		{pageObject, &summary.PageCount},
		{fontObject, &summary.FontObjects},
		{imageObject, &summary.ImageObjects},
		{annotationObject, &summary.AnnotationObjects},
		{outlineObject, &summary.OutlineObjects},
	}
	const chunkSize = 1 << 20
	const overlapSize = 512
	buffer := make([]byte, chunkSize)
	var offset int64
	var overlap []byte
	for offset < size {
		count := int64(len(buffer))
		if count > size-offset {
			count = size - offset
		}
		n, err := reader.ReadAt(buffer[:count], offset)
		if err != nil && err != io.EOF {
			return summary, err
		}
		window := make([]byte, len(overlap)+n)
		copy(window, overlap)
		copy(window[len(overlap):], buffer[:n])
		for _, pattern := range patterns {
			for _, match := range pattern.re.FindAllIndex(window, -1) {
				if match[1] > len(overlap) {
					(*pattern.count)++
				}
			}
		}
		for _, match := range mediaBox.FindAllSubmatchIndex(window, -1) {
			if match[1] <= len(overlap) {
				continue
			}
			values := make([]float64, 4)
			for index := range values {
				value, parseErr := strconv.ParseFloat(string(window[match[2+index*2]:match[3+index*2]]), 64)
				if parseErr != nil {
					return summary, fmt.Errorf("parse generated PDF media box: %w", parseErr)
				}
				values[index] = value
			}
			width, height := values[2]-values[0], values[3]-values[1]
			if !nearZero(values[0]) || !nearZero(values[1]) ||
				!((near(width, 612) && near(height, 792)) || (near(width, 792) && near(height, 612))) {
				return summary, fmt.Errorf("generated PDF page has non-Letter media box %v", values)
			}
			summary.LetterMediaBoxes++
		}
		if len(window) > overlapSize {
			overlap = bytes.Clone(window[len(window)-overlapSize:])
		} else {
			overlap = bytes.Clone(window)
		}
		offset += int64(n)
		if n == 0 {
			break
		}
	}
	switch {
	case summary.PageCount < 1:
		return summary, errors.New("generated PDF contains no page objects")
	case summary.PageCount > expected.MaxPages:
		return summary, fmt.Errorf("generated PDF has %d pages, exceeding limit %d", summary.PageCount, expected.MaxPages)
	case summary.LetterMediaBoxes != summary.PageCount:
		return summary, fmt.Errorf("generated PDF has %d page objects but %d Letter media boxes", summary.PageCount, summary.LetterMediaBoxes)
	case summary.FontObjects < 1:
		return summary, errors.New("generated PDF contains no font resources")
	case expected.RequireImage && summary.ImageObjects < 1:
		return summary, errors.New("generated PDF contains no image objects for an illustrated guide")
	case (expected.RequireLinks || expected.RequireAnnotations) && summary.AnnotationObjects < 1:
		return summary, errors.New("generated PDF contains no link annotations")
	case expected.RequireOutline && summary.OutlineObjects < 1:
		return summary, errors.New("generated PDF contains no document outline")
	}
	return summary, nil
}

func nearZero(value float64) bool {
	return value > -0.01 && value < 0.01
}

func near(value, expected float64) bool {
	return value > expected-0.1 && value < expected+0.1
}
