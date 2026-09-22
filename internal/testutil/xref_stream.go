package testutil

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// XRefStreamPDF has one page and an uncompressed xref stream with real offsets.
// separator permits testing the lexical boundary after the object keyword.
func XRefStreamPDF(separator string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.5\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
	}
	offsets := []uint32{0}
	for i, object := range objects {
		offsets = append(offsets, uint32(b.Len()))
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := b.Len()
	offsets = append(offsets, uint32(xref))
	stream := make([]byte, 5*7)
	binary.BigEndian.PutUint16(stream[5:7], 65535)
	for i := 1; i < len(offsets); i++ {
		stream[7*i] = 1
		binary.BigEndian.PutUint32(stream[7*i+1:7*i+5], offsets[i])
	}
	fmt.Fprintf(&b, "4 0 obj%s<</Type/XRef /Size 5 /Root 1 0 R /W [1 4 2] /Length %d>>\nstream\n", separator, len(stream))
	b.Write(stream)
	fmt.Fprintf(&b, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}
