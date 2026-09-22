//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/pdfacceptance"
)

func TestCLIReportAndExitContractThroughAnalysisSeam(t *testing.T) {
	for _, test := range []struct {
		name   string
		passed bool
		exit   int
	}{
		{name: "pass", passed: true, exit: 0},
		{name: "violations", passed: false, exit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			reportPath := filepath.Join(t.TempDir(), "report.json")
			report := pdfacceptance.AcceptanceReport{
				CanonicalTitleMatches: []pdfacceptance.AcceptanceLine{},
				Headings:              []map[string]any{},
				MaxCoverFontPoints:    22.1,
				Outline: pdfacceptance.OutlineRecord{
					DirectChildren: map[string]int{}, EntryCount: 0,
					MaxDepth: 0, RootTitles: []string{},
				},
				PageFooters:         []pdfacceptance.FooterRecord{},
				Passed:              test.passed,
				PDF:                 "/owned/input.pdf",
				PhysicalPages:       2,
				Samples:             nil,
				SourceCommandValues: 0,
				SourceGuideRoot:     nil,
				SourceTextValues:    0,
				SourceTheadValues:   0,
				Title:               "Guide",
				Tool: pdfacceptance.AcceptanceToolRecord{
					MuPDFDependencyClosure: []pdfacceptance.ToolFile{},
				},
				Violations: map[string]any{
					"outline": []map[string]any{},
				},
			}
			analyze := func(context.Context, cliOptions) (pdfacceptance.AcceptanceReport, error) {
				return report, nil
			}
			var stdout, stderr bytes.Buffer
			exit := runWithAnalyze(context.Background(), []string{
				"/owned/input.pdf", "--title", "Guide", "--report", reportPath,
				"--mutool-path", "/owned/mutool",
				"--mutool-identity-manifest", "/owned/mutool.json",
			}, &stdout, &stderr, analyze)
			if exit != test.exit || stderr.Len() != 0 {
				t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
			}
			written, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(written, stdout.Bytes()) {
				t.Fatal("stdout and atomic report JSON differ")
			}
			if test.passed {
				const golden = `{
  "canonical_title_matches": [],
  "headings": [],
  "max_cover_font_points": 22.1,
  "outline": {
    "direct_children": {},
    "entry_count": 0,
    "max_depth": 0,
    "root_titles": []
  },
  "page_footers": [],
  "passed": true,
  "pdf": "/owned/input.pdf",
  "physical_pages": 2,
  "samples": null,
  "source_command_values": 0,
  "source_guide_root": null,
  "source_text_values": 0,
  "source_thead_values": 0,
  "title": "Guide",
  "tool": {
    "license_path": "",
    "license_sha256": "",
    "mupdf_dependency_closure": [],
    "mupdf_executable_sha256": "",
    "mupdf_identity_manifest": "",
    "mupdf_identity_manifest_sha256": "",
    "mupdf_path": "",
    "mupdf_version": "",
    "qpdf_bundle_sha256": "",
    "qpdf_executable_sha256": "",
    "qpdf_path": "",
    "qpdf_version": ""
  },
  "violations": {
    "outline": []
  }
}
`
				if string(written) != golden {
					t.Fatalf("CLI JSON changed from golden:\n%s", written)
				}
			}
			var decoded map[string]any
			if err := json.Unmarshal(written, &decoded); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{
				"canonical_title_matches", "headings", "max_cover_font_points",
				"outline", "page_footers", "passed", "pdf", "physical_pages",
				"samples", "source_command_values", "source_guide_root",
				"source_text_values", "source_thead_values", "title", "violations",
			} {
				if _, ok := decoded[key]; !ok {
					t.Fatalf("compatibility report key %q is missing from %s", key, written)
				}
			}
			if _, ok := decoded["tool"]; !ok {
				t.Fatal("additive tool identity is missing")
			}
		})
	}
}

func TestCLIErrorsExitTwoWithoutSuccessShapedReport(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if exit := run(context.Background(), []string{"--help"}, &stdout, &stderr); exit != 0 ||
			stderr.Len() != 0 || !strings.Contains(stdout.String(), "usage: aoscx-pdf-acceptance") {
			t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
		}
	})
	t.Run("usage", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if exit := run(context.Background(), nil, &stdout, &stderr); exit != 2 ||
			stdout.Len() != 0 || !strings.Contains(stderr.String(), "exactly one PDF") {
			t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
		}
	})
	t.Run("analysis error", func(t *testing.T) {
		report := filepath.Join(t.TempDir(), "report.json")
		analyze := func(context.Context, cliOptions) (pdfacceptance.AcceptanceReport, error) {
			return pdfacceptance.AcceptanceReport{}, errors.New("bounded parser failure")
		}
		var stdout, stderr bytes.Buffer
		exit := runWithAnalyze(context.Background(), []string{
			"input.pdf", "--title", "Guide", "--report", report,
			"--mutool-path", "mutool", "--mutool-identity-manifest", "identity.json",
		}, &stdout, &stderr, analyze)
		if exit != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "bounded parser failure") {
			t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
		}
		if _, err := os.Lstat(report); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("operational failure wrote success-shaped report: %v", err)
		}
	})
}

func TestWriteAtomicReportReplacesRegularFileAndRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	report := filepath.Join(root, "report.json")
	if err := os.WriteFile(report, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomicReport(report, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(report); err != nil || string(body) != "new\n" {
		t.Fatalf("atomic report=%q err=%v", body, err)
	}
	if err := os.Remove(report); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), report); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomicReport(report, []byte("unsafe\n")); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("report symlink was accepted: %v", err)
	}
}
