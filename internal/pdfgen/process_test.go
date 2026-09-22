package pdfgen

import (
	"bytes"
	"strings"
	"testing"
)

func TestBoundedChromeLogTruncatesWithoutShortWrite(t *testing.T) {
	log := &boundedLog{limit: 4}
	if written, err := log.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("bounded log write = %d, %v", written, err)
	}
	if value := log.String(); value != "abcd\n[Chrome diagnostics truncated]" {
		t.Fatalf("unexpected bounded log: %q", value)
	}
}

func TestLimitedFileWriterRejectsOversizedOutput(t *testing.T) {
	var output bytes.Buffer
	writer := &limitedBuffer{buffer: &output, limit: 4}
	if _, err := writer.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("e")); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversized process output was accepted: %v", err)
	}
}
