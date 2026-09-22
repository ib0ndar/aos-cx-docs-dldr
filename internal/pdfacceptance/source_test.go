//go:build darwin

package pdfacceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSourceEvidenceUsesImmediateBrowserParsedPagesAndDistinctCommandRows(t *testing.T) {
	root := t.TempDir()
	pages := filepath.Join(root, "pages")
	if err := os.Mkdir(pages, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `<html><body>
<div class="screen"><span>long command prefix first ending</span>
<span>long command prefix second ending</span>
<span>long command prefix first ending</span></div>
<table><thead><tr><th>Header Value</th></tr></thead></table>
<pre>preformatted row
second row</pre><p>ordinary prose</p></body></html>`
	if err := os.WriteFile(filepath.Join(pages, "b.HTML"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(pages, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pages, "nested", "ignored.html"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := LoadSourceEvidence(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.CommandRows) != 4 {
		t.Fatalf("command rows are not distinct normalized values: %+v", evidence.CommandRows)
	}
	if _, ok := evidence.TableHeaders["Header Value"]; !ok {
		t.Fatalf("missing thead evidence: %+v", evidence.TableHeaders)
	}
	for _, value := range []string{"preformatted row", "second row", "ordinary prose"} {
		if evidence.TextCounts[value] != 1 {
			t.Fatalf("source count %q=%d", value, evidence.TextCounts[value])
		}
	}
	if evidence.TextCounts["ignored"] != 0 {
		t.Fatal("nested pages directory was traversed")
	}
}

func TestLoadSourceEvidenceExplicitMissingRootFails(t *testing.T) {
	_, err := LoadSourceEvidence(filepath.Join(t.TempDir(), "missing"))
	if err == nil || !strings.Contains(err.Error(), "source guide root is unavailable") {
		t.Fatalf("explicit missing source root was silently accepted: %v", err)
	}
}

func TestLoadSourceEvidenceRejectsSymlinkPage(t *testing.T) {
	root := t.TempDir()
	pages := filepath.Join(root, "pages")
	if err := os.Mkdir(pages, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.html")
	if err := os.WriteFile(target, []byte("<p>source</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(pages, "link.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSourceEvidence(root); err == nil || !strings.Contains(err.Error(), "nonsymlink") {
		t.Fatalf("symlink source page was accepted: %v", err)
	}
}

func TestOrdinaryPreRowsRetainDistinctCommandEvidence(t *testing.T) {
	root := t.TempDir()
	pages := filepath.Join(root, "pages")
	if err := os.Mkdir(pages, 0o700); err != nil {
		t.Fatal(err)
	}
	visible := "a sufficiently long command output prefix"
	body := `<pre>` + visible + ` first
` + visible + ` second
` + visible + ` first</pre><pre><code>nested row</code></pre>`
	if err := os.WriteFile(filepath.Join(pages, "topic.html"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := LoadSourceEvidence(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.CommandRows) != 3 {
		t.Fatalf("ordinary pre/code rows were not deduplicated correctly: %+v", evidence.CommandRows)
	}
	if evidence.TextCounts["nested row"] != 1 {
		t.Fatalf("nested pre/code was processed more than once: %+v", evidence.TextCounts)
	}
	pdfPages := []AcceptancePage{
		{Lines: []AcceptanceLine{acceptanceLine(1, 0, visible, [4]float64{0, 0, 100, 10}, 8)}},
		{Lines: []AcceptanceLine{acceptanceLine(2, 0, visible, [4]float64{0, 0, 100, 10}, 8)}},
	}
	if findings := adjacentBoundaryDuplicates(pdfPages, evidence); len(findings) != 0 {
		t.Fatalf("two distinct ordinary pre rows did not explain clipped prefix: %+v", findings)
	}
}

func TestOrdinaryPreSingleOrDuplicateRowDoesNotExplainPrefix(t *testing.T) {
	visible := "a sufficiently long command output prefix"
	for _, body := range []string{
		`<pre>` + visible + ` first</pre>`,
		`<pre>` + visible + ` first
` + visible + ` first</pre>`,
	} {
		root := t.TempDir()
		pages := filepath.Join(root, "pages")
		if err := os.Mkdir(pages, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pages, "topic.html"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		evidence, err := LoadSourceEvidence(root)
		if err != nil {
			t.Fatal(err)
		}
		pdfPages := []AcceptancePage{
			{Lines: []AcceptanceLine{acceptanceLine(1, 0, visible, [4]float64{0, 0, 100, 10}, 8)}},
			{Lines: []AcceptanceLine{acceptanceLine(2, 0, visible, [4]float64{0, 0, 100, 10}, 8)}},
		}
		findings := adjacentBoundaryDuplicates(pdfPages, evidence)
		if len(findings) != 1 {
			t.Fatalf("one distinct ordinary pre row incorrectly explained prefix: rows=%+v findings=%+v", evidence.CommandRows, findings)
		}
	}
}
