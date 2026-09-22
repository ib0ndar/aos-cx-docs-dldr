package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionSearchIndexesHTMLTextAndKeepsPDFTitleOnly(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptHTML(t, run, "routing")
	acceptPDF(t, run, "pdf", "original")
	if _, err := run.Publish(false); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(base, "6300", "10.10", "search-index.js"))
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(string(body), "window.AOSCX_SEARCH = "), ";\n")
	var records []searchEntry
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		t.Fatal(err)
	}
	var htmlText, pdfText string
	for _, record := range records {
		if record.Guide == "routing Guide" && record.Title == "Home" {
			htmlText = record.Text
		}
		if record.Guide == "pdf Guide" {
			pdfText = record.Text
		}
	}
	if !strings.Contains(htmlText, "Offline text") {
		t.Fatalf("HTML body text missing from version search: %q", htmlText)
	}
	if pdfText != "" {
		t.Fatalf("PDF search record contains non-title text: %q", pdfText)
	}
}

func TestVersionSearchScriptContract(t *testing.T) {
	script := string(searchJS)
	for _, expected := range []string{
		`results.replaceChildren();`,
		`status.textContent = "";`,
		`if (!terms.length) return;`,
		`[entry.guide, entry.title, entry.text].join(" ")`,
		`terms.every(term => haystack.includes(term))`,
		`if (count > 50) continue;`,
		`"Showing the first 50 matching results."`,
		`value.normalize("NFKC").toLocaleLowerCase()`,
	} {
		if !strings.Contains(script, expected) {
			t.Errorf("version search script missing %q", expected)
		}
	}
	if strings.Contains(script, `entry.title.toLocaleLowerCase()`) {
		t.Fatal("version search remains title-only")
	}
}
