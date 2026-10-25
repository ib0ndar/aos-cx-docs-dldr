package library

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
	for _, expected := range []string{
		`const href = localLink(entry.url);`,
		`if (!href) continue;`,
		`link.href = href;`,
		`.href.startsWith(libraryBase)`,
		`/^[a-z][a-z\d+.-]*:/i.test(value)`,
	} {
		if !strings.Contains(script, expected) {
			t.Errorf("version search script does not confine links: missing %q", expected)
		}
	}
	if strings.Contains(script, `link.href = entry.url`) {
		t.Fatal("version search assigns archived URLs to links unchecked")
	}
}

func TestVersionIndexContentSecurityPolicy(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptHTML(t, run, "routing")
	if _, err := run.Publish(false); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(base, "6300", "10.10", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	policy := regexp.MustCompile(`<meta http-equiv="Content-Security-Policy" content="([^"]+)">`).FindSubmatch(body)
	if policy == nil {
		t.Fatal("version index has no Content-Security-Policy")
	}
	directives := map[string]string{}
	for _, directive := range strings.Split(string(policy[1]), ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(directive), " ")
		directives[name] = value
	}
	for name, value := range map[string]string{
		"default-src": "'none'", "script-src": "'self'", "style-src": "'unsafe-inline'",
		"object-src": "'none'", "base-uri": "'none'", "form-action": "'none'",
	} {
		if directives[name] != value {
			t.Errorf("CSP %s = %q, want %q", name, directives[name], value)
		}
	}
	if len(directives) != 6 {
		t.Errorf("unexpected CSP directives: %v", directives)
	}
	if bytes.Contains(body, []byte("<script>")) || regexp.MustCompile(`\son[a-z]+=`).Match(body) {
		t.Fatal("version index uses inline script that its CSP would block")
	}
}
