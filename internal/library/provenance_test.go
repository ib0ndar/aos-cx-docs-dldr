package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/pdfcheck"
	"aos-cx-docs-dldr/internal/pdfgen"
	"aos-cx-docs-dldr/internal/storage"
)

// upgradePolicy returns the production version policy and asserts that it
// accepts exactly one version: the current one. This application does not
// upgrade libraries produced by earlier versions.
func upgradePolicy(t *testing.T) nativeVersionPolicy {
	t.Helper()
	policy, err := defaultNativeVersionPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.output != model.Version || len(policy.accepted) != 1 || !policy.accepted[model.Version] {
		t.Fatalf("production version policy drifted: %+v", policy)
	}
	return policy
}

func publishUpgradeFixture(t *testing.T, kind string) (string, string, Manifest) {
	t.Helper()
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "html":
		acceptHTML(t, run, "html")
	case "pdf":
		acceptPDF(t, run, "pdf", "publisher")
	case "mixed":
		acceptHTMLWithDiagnostics(t, run, "html", []model.Notice{{
			Kind: model.NoticeCSSCleanup, Message: "preserved notice",
		}}, stylesheetRecoveryFixture())
		acceptPDF(t, run, "pdf", "publisher")
	case "degraded":
		acceptDegradedHTML(t, run, "html", "missing.png")
	case "incomplete":
		acceptHTML(t, run, "html")
		output, err := run.GuideOutput("failed")
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Accept(model.ArchiveResult{
			Document: model.Document{
				ID: "failed", Title: "Failed", Platform: "6300", Version: "10.10",
				Kind: "pdf", URL: "https://example.test/failed.pdf",
			},
			OutputDir: output,
			Status:    "failed",
			Format:    "pdf",
			Errors:    []string{"preserved failure"},
			Warnings:  []string{"preserved warning"},
		}); err != nil {
			t.Fatal(err)
		}
	case "empty":
	case "generated-pdf":
		acceptHTML(t, run, "html")
		addGeneratedPDFCompanion(t, run, "html")
	default:
		t.Fatalf("unknown fixture kind %q", kind)
	}
	manifest, err := run.Publish(kind == "empty")
	if err != nil {
		t.Fatal(err)
	}
	target := run.Target
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	rewritePublishedApplicationVersion(t, target, "0.6")
	if err := json.Unmarshal(readBytes(t, filepath.Join(target, "manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	return base, target, manifest
}

func addGeneratedPDFCompanion(t *testing.T, run *Run, id string) {
	t.Helper()
	result := run.guides[id]
	body := generatedPDFBytes()
	name, err := storage.PDFFilename(result.Document.Platform, result.Document.Version, result.Document.Title)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run.Stage, id, name), body, 0o640); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	summary, err := pdfcheck.InspectGenerated(bytes.NewReader(body), int64(len(body)), pdfcheck.GeneratedExpectations{
		MaxPages:       1,
		RequireOutline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := &model.GeneratedPDFRecord{
		Path:                              name,
		Status:                            "complete",
		CreatedAt:                         time.Now().UTC().Format(time.RFC3339Nano),
		Renderer:                          pdfgen.RendererName,
		RendererVersion:                   pdfgen.ChromeVersion,
		RendererRevision:                  pdfgen.ChromeRevision,
		ExecutableSHA256:                  pdfgen.ChromeExecutableSHA256,
		Postprocessor:                     pdfgen.QPDFPostprocessorName,
		PostprocessorVersion:              pdfgen.QPDFVersion,
		PostprocessorExecutableSHA256:     pdfgen.QPDFExecutableSHA256,
		PostprocessorBundleSHA256:         pdfgen.QPDFRuntimeBundleSHA256,
		OutlinePlanSHA256:                 strings.Repeat("1", 64),
		FooterPlanSHA256:                  strings.Repeat("2", 64),
		FooterOverlayInputSHA256:          strings.Repeat("3", 64),
		FooterOverlayRenderer:             pdfgen.RendererName,
		FooterOverlayRendererVersion:      pdfgen.ChromeVersion,
		FooterOverlayExecutableSHA256:     pdfgen.ChromeExecutableSHA256,
		FooterEntries:                     summary.PageCount,
		SourceManifestSHA256:              strings.Repeat("4", 64),
		InputSHA256:                       strings.Repeat("5", 64),
		SHA256:                            hex.EncodeToString(sum[:]),
		Size:                              int64(len(body)),
		DurationMilliseconds:              1,
		PeakRSSBytes:                      1,
		PostprocessDurationMilliseconds:   1,
		PostprocessPeakRSSBytes:           1,
		FooterOverlayDurationMilliseconds: 1,
		FooterOverlayPeakRSSBytes:         1,
		Provenance:                        pdfgen.GeneratedProvenance,
		Settings:                          pointer(pdfgen.ExpectedSettings()),
		Validation: &model.GeneratedPDFValidation{
			PageCount:                  summary.PageCount,
			LetterMediaBoxes:           summary.LetterMediaBoxes,
			FontObjects:                summary.FontObjects,
			ImageObjects:               summary.ImageObjects,
			AnnotationObjects:          summary.AnnotationObjects,
			OutlineObjects:             summary.OutlineObjects,
			TOCEntries:                 0,
			TOCLinksChecked:            0,
			LocalRequests:              1,
			CanonicalCoverTitles:       1,
			OutlineEntries:             3,
			OutlineMaxDepth:            1,
			FontsLoaded:                true,
			ScriptExecutionDisabled:    true,
			ResourceClosureValidated:   true,
			CoverTypographyValidated:   true,
			HeadingTypographyValidated: true,
			HeadingPaginationValidated: true,
			FooterOverlayValidated:     true,
			OutlineValidated:           true,
			StructuralValidated:        true,
		},
	}
	result.HTML.GeneratedPDF = record
	run.guides[id] = result
	if err := storage.WriteJSON(run.root, filepath.ToSlash(filepath.Join(run.stage, id, "manifest.json")), result.HTML); err != nil {
		t.Fatal(err)
	}
}

func generatedPDFBytes() []byte {
	var body strings.Builder
	body.WriteString("%PDF-1.7\n")
	body.WriteString("1 0 obj<</Type /Catalog /Pages 2 0 R /Outlines 9 0 R>>endobj\n")
	body.WriteString("2 0 obj<</Type /Page /MediaBox [0 0 612 792] /Resources<</Font<</F1 8 0 R>>>>>>endobj\n")
	body.WriteString("8 0 obj<</Type /Font>>endobj\n")
	offset := body.Len()
	body.WriteString("xref\n0 1\n0000000000 65535 f \n")
	fmt.Fprintf(&body, "trailer<</Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", offset)
	return []byte(body.String())
}

func pointer[T any](value T) *T {
	return &value
}

// futureApplicationVersion returns a two-component version strictly newer than
// the current application, so "future state is rejected" fixtures stay
// meaningful across version bumps.
func futureApplicationVersion(t *testing.T) string {
	t.Helper()
	current, err := parseApplicationVersion(model.Version)
	if err != nil {
		t.Fatal(err)
	}
	future := fmt.Sprintf("%d.%d", current.major+1, 0)
	if future == model.Version {
		t.Fatalf("future-version fixture %q is actually current", future)
	}
	return future
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(body, '\n')
}

func TestUpgradeRejectsUnreadableCentralStateBeforeLock(t *testing.T) {
	base, target, _ := publishUpgradeFixture(t, "pdf")
	manifest := filepath.Join(target, "manifest.json")
	info, err := os.Stat(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifest, 0); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.Chmod(manifest, info.Mode().Perm()) })
	if run, err := openContextWithPolicy(context.Background(), base, "6300", "10.10", upgradePolicy(t), nil); err == nil {
		run.Close()
		t.Fatal("unreadable migration source was accepted")
	}
	current, err := os.Stat(manifest)
	if err != nil || current.Mode().Perm() != 0 {
		t.Fatalf("unreadable source permissions changed: mode=%v err=%v", current.Mode(), err)
	}
	if _, err := os.Lstat(filepath.Join(base, "6300", ".10.10.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable source created a lock: %v", err)
	}
}

func TestUpgradePreflightRejectsCorruptFutureForeignAndUnsafeStateReadOnly(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"future-version", func(t *testing.T, target string) {
			rewritePublishedApplicationVersion(t, target, futureApplicationVersion(t))
		}},
		{"corrupt-guide", func(t *testing.T, target string) {
			if err := os.WriteFile(filepath.Join(target, "pdf", "6300 - 10.10 - pdf Guide.pdf"), []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unexpected-file", func(t *testing.T, target string) {
			if err := os.WriteFile(filepath.Join(target, "foreign.bin"), []byte("foreign"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, target string) {
			name := filepath.Join(target, "pdf", "6300 - 10.10 - pdf Guide.pdf")
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), name); err != nil {
				t.Skip(err)
			}
		}},
		{"fifo", func(t *testing.T, target string) {
			if err := syscall.Mkfifo(filepath.Join(target, "foreign-fifo"), 0o600); err != nil {
				t.Skip(err)
			}
		}},
		{"finder-wrong-location", func(t *testing.T, target string) {
			if err := os.MkdirAll(filepath.Join(target, "pdf", "nested"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "pdf", "nested", ".DS_Store"), []byte("finder"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, target, _ := publishUpgradeFixture(t, "pdf")
			test.mutate(t, target)
			before := cutoverSnapshot(t, base)
			if run, err := openContextWithPolicy(context.Background(), base, "6300", "10.10", upgradePolicy(t), nil); err == nil {
				run.Close()
				t.Fatal("unsafe migration source was accepted")
			}
			if after := cutoverSnapshot(t, base); !bytes.Equal(before, after) {
				t.Fatal("rejected migration source changed before lock")
			}
		})
	}
}

func TestUpgradePreflightRejectsMalformedOrContradictoryProvenanceReadOnly(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "malformed-manifest-provenance",
			mutate: func(t *testing.T, target string) {
				var manifest Manifest
				if err := json.Unmarshal(readBytes(t, filepath.Join(target, "manifest.json")), &manifest); err != nil {
					t.Fatal(err)
				}
				manifest.UpgradedFromVersion = "garbage"
				if err := os.WriteFile(filepath.Join(target, "manifest.json"), mustJSON(t, manifest), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "manifest-history-provenance-conflict",
			mutate: func(t *testing.T, target string) {
				var manifest Manifest
				if err := json.Unmarshal(readBytes(t, filepath.Join(target, "manifest.json")), &manifest); err != nil {
					t.Fatal(err)
				}
				manifest.UpgradedFromVersion = "0.5"
				if err := os.WriteFile(filepath.Join(target, "manifest.json"), mustJSON(t, manifest), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "manifest-history-version-conflict",
			mutate: func(t *testing.T, target string) {
				var history []History
				if err := json.Unmarshal(readBytes(t, filepath.Join(target, "history.json")), &history); err != nil {
					t.Fatal(err)
				}
				history[len(history)-1].ApplicationVersion = "0.5"
				if err := os.WriteFile(filepath.Join(target, "history.json"), mustJSON(t, history), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, target, _ := publishUpgradeFixture(t, "pdf")
			test.mutate(t, target)
			before := cutoverSnapshot(t, base)
			if run, err := Open(base, "6300", "10.10"); err == nil {
				run.Close()
				t.Fatal("invalid migration provenance was accepted")
			}
			if after := cutoverSnapshot(t, base); !bytes.Equal(before, after) {
				t.Fatal("invalid migration provenance caused managed mutation")
			}
		})
	}
}

func TestJournalDecodingRejectsCrossSchemaAndUpgradeJournals(t *testing.T) {
	legacyWithUpgradeField := []byte(`{
		"native_schema_version":1,
		"run_id":"20260916T120000.000000000Z-test",
		"previous_run":"",
		"stage":"6300/.staging/10.10-20260916T120000.000000000Z-test",
		"previous_snapshot":"",
		"operation":"application-upgrade"
	}`)
	if _, err := decodeJournal(legacyWithUpgradeField); err == nil {
		t.Fatal("schema-1 journal accepted an upgrade-only field")
	}

	// Schema 2 was only ever written by the retired upgrade engine. It is now
	// rejected outright rather than validated.
	upgradeJournal := []byte(`{
		"application":"aos-cx-docs-dldr",
		"native_schema_version":2,
		"operation":"application-upgrade",
		"run_id":"20260916T120000.000000000Z-test",
		"previous_run":"20260916T110000.000000000Z-test",
		"stage":"6300/.staging/stage",
		"previous_snapshot":"6300/.snapshots/snapshot",
		"from_application_version":"0.6",
		"to_application_version":"9.9",
		"snapshot_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"stage_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}`)
	if _, err := decodeJournal(upgradeJournal); err == nil {
		t.Fatal("upgrade journal schema was accepted")
	}
	if _, err := decodeJournal([]byte(`{"native_schema_version":3}`)); err == nil {
		t.Fatal("unknown journal schema was accepted")
	}
}
