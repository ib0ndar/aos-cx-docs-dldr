package testutil

import (
	"bytes"
	"fmt"
)

// PDF is an original synthetic one-page fixture, not copied publisher content.
func PDF(label string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%%PDF-1.4\n%% %s\n", label)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
	}
	offsets := []int{0}
	for i, object := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 4\n0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

// TitledPDF is PDF with a document information dictionary whose Title is the
// PDF string title, written as it appears in the file: "(...)" or "<...>".
// With update, the information dictionary and the trailer naming it are an
// incremental update whose trailer points back to the original section.
func TitledPDF(label, title string, update bool) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%%PDF-1.4\n%% %s\n", label)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
	}
	if !update {
		objects = append(objects, "<</Producer(fixture)/Title"+title+"/Author(fixture)>>")
	}
	offsets := []int{0}
	for i, object := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	if !update {
		fmt.Fprintf(&b, "trailer\n<</Size 5/Root 1 0 R/Info 4 0 R/ID [<01><01>]>>\nstartxref\n%d\n%%%%EOF\n", xref)
		return b.Bytes()
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	info := b.Len()
	fmt.Fprintf(&b, "4 0 obj\n<</Title%s>>\nendobj\n", title)
	second := b.Len()
	fmt.Fprintf(&b, "xref\n4 1\n%010d 00000 n \ntrailer\n<< /Size 5 /Root 1 0 R /Info 4 0 R /Prev %d >>\nstartxref\n%d\n%%%%EOF\n",
		info, xref, second)
	return b.Bytes()
}
