//go:build chromepdftests

package pdfgen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/publication"
	"aos-cx-docs-dldr/internal/storage"
)

func TestPinnedChromeGeneratesValidatedCompanion(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	qpdf := os.Getenv(QPDFPathEnvironment)
	if qpdf == "" {
		t.Skip(QPDFPathEnvironment + " is not set")
	}
	output, plan, archived := rendererFixture(t)
	config := DefaultConfig(chrome)
	config.QPDFPath = qpdf
	record, err := Generate(context.Background(), output, plan, &archived, config)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "complete" || record.Validation == nil ||
		record.Validation.PageCount < 1 || record.Validation.ExternalRequests != 0 ||
		record.Validation.ScriptElements != 2 || record.Validation.ImageObjects < 1 ||
		record.Settings == nil || record.Settings.SchemaVersion != 5 ||
		record.Settings.GenerateDocumentOutline ||
		record.Validation.CanonicalCoverTitles != 1 ||
		record.Validation.PublisherChromeElements != 0 ||
		record.Validation.FixedOrStickyElements != 0 ||
		record.PostprocessorVersion != QPDFVersion ||
		record.PostprocessorExecutableSHA256 != QPDFExecutableSHA256 ||
		record.PostprocessorBundleSHA256 != QPDFRuntimeBundleSHA256 ||
		!record.Validation.OutlineValidated || record.Validation.OutlineEntries < 4 ||
		!record.Validation.FontsLoaded || !record.Validation.ScriptExecutionDisabled ||
		!record.Validation.ResourceClosureValidated || !record.Validation.CoverTypographyValidated ||
		!record.Validation.HeadingTypographyValidated || !record.Validation.HeadingPaginationValidated ||
		!record.Validation.FooterOverlayValidated || record.FooterEntries != record.Validation.PageCount ||
		record.FooterPlanSHA256 == "" || record.FooterOverlayInputSHA256 == "" {
		t.Fatalf("unexpected generated PDF record: record=%+v validation=%+v", record, record.Validation)
	}
	pdfPath := filepath.Join(output, record.Path)
	if _, err := os.Stat(pdfPath); err != nil {
		t.Fatal(err)
	}
	assertGeneratedOutline(t, qpdf, output, pdfPath, plan, archived)
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".aos-cx-docs-dldr-") {
			t.Fatalf("transient renderer input was retained: %s", entry.Name())
		}
	}
}

func TestPinnedChromeRendersVerifiedDegradedPlaceholder(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	qpdf := os.Getenv(QPDFPathEnvironment)
	if chrome == "" || qpdf == "" {
		t.Skip("pinned Chrome and qpdf paths are required")
	}
	output, plan, archived := rendererFixture(t)
	topic := &archived.HTML.Topics[0]
	topicPath := filepath.Join(output, filepath.FromSlash(topic.Path))
	body, err := os.ReadFile(topicPath)
	if err != nil {
		t.Fatal(err)
	}
	placeholderID := "image-placeholder-integration"
	body = bytes.Replace(body, []byte(`<img src="../assets/image.png">`), []byte(
		`<img src="../assets/image.png"><span id="`+placeholderID+`" class="archive-image-placeholder" role="img" aria-label="Topology. Image unavailable - http-404">Image unavailable - http-404</span>`,
	), 1)
	if err := os.WriteFile(topicPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	topic.SHA256 = hex.EncodeToString(hash[:])
	topic.Size = int64(len(body))
	missing := model.MissingResource{
		RequestedURL:      "https://example.test/assets/missing.png",
		FinalURL:          "https://example.test/assets/missing.png",
		ReferringTopicURL: topic.URL, GeneratedPagePath: topic.Path,
		GeneratedPageSHA256: topic.SHA256, FailureClass: "http-404",
		HTTPStatus: 404, RetrievalStage: "headers", Message: "publisher resource returned HTTP 404",
		ElementType: "img", Alt: "Topology", ExpectedRasterType: "png",
		PlaceholderID: placeholderID, ObservedAt: "2026-09-16T00:00:00Z",
	}
	archived.Status = "degraded"
	archived.MissingResources = []model.MissingResource{missing}
	archived.HTML.Status = "degraded"
	archived.HTML.MissingResources = []model.MissingResource{missing}
	data, err := json.MarshalIndent(archived.HTML, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(chrome)
	config.QPDFPath = qpdf
	record, err := Generate(context.Background(), output, plan, &archived, config)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "complete" || record.InputStatus != "degraded" ||
		record.ImagePlaceholders != 1 || record.Validation == nil ||
		record.Validation.ImagePlaceholders != 1 ||
		record.Validation.ImagePlaceholderOverflows != 0 ||
		record.Validation.FailedImages != 0 || record.Validation.ExternalRequests != 0 {
		t.Fatalf("unexpected degraded generated PDF record: %+v", record)
	}
}

func TestPinnedSidecarsExecutableRelativeConversion(t *testing.T) {
	if os.Getenv("AOSCX_WP11_PACKAGE_SMOKE") != "1" {
		t.Skip("AOSCX_WP11_PACKAGE_SMOKE is not set")
	}
	if os.Getenv(ChromePathEnvironment) != "" || os.Getenv(QPDFPathEnvironment) != "" {
		t.Fatal("package smoke test must not use sidecar environment overrides")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(executable)
	chrome, err := ResolveSidecar("")
	if err != nil {
		t.Fatal(err)
	}
	qpdf, err := ResolveQPDFSidecar("")
	if err != nil {
		t.Fatal(err)
	}
	if chrome.Path != filepath.Join(root, "sidecars", "chrome-headless-shell-mac-arm64", "chrome-headless-shell") {
		t.Fatalf("Chrome was not discovered relative to the moved package: %+v", chrome)
	}
	if qpdf.Path != filepath.Join(root, "sidecars", "qpdf-mac-arm64", "bin", "qpdf") {
		t.Fatalf("qpdf was not discovered relative to the moved package: %+v", qpdf)
	}
	output, plan, archived := rendererFixture(t)
	record, err := Generate(context.Background(), output, plan, &archived, DefaultConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "complete" || record.Validation == nil ||
		!record.Validation.OutlineValidated || !record.Validation.StructuralValidated ||
		record.ExecutableSHA256 != ChromeExecutableSHA256 ||
		record.PostprocessorExecutableSHA256 != QPDFExecutableSHA256 ||
		record.PostprocessorBundleSHA256 != QPDFRuntimeBundleSHA256 {
		t.Fatalf("unexpected package-smoke record: %+v", record)
	}
	assertGeneratedOutline(t, qpdf.Path, output, filepath.Join(output, record.Path), plan, archived)
	if sentinel := os.Getenv("AOSCX_WP13_PACKAGE_SMOKE_SENTINEL"); sentinel != "" {
		if err := os.WriteFile(sentinel, []byte("complete\n"), 0o600); err != nil {
			t.Fatalf("write package smoke sentinel: %v", err)
		}
	}
}

func TestPinnedChromeAppRelativeDiscovery(t *testing.T) {
	root := os.Getenv("AOSCX_WP11_PACKAGE_ROOT")
	if root == "" {
		t.Skip("AOSCX_WP11_PACKAGE_ROOT is not set")
	}
	sidecar, err := resolveSidecar(SidecarOptions{
		ExecutablePath: filepath.Join(root, model.ExecutableName),
		LookupEnv:      func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if sidecar.ExecutableSHA256 != ChromeExecutableSHA256 ||
		sidecar.Path != filepath.Join(root, "sidecars", "chrome-headless-shell-mac-arm64", "chrome-headless-shell") {
		t.Fatalf("unexpected app-relative sidecar: %+v", sidecar)
	}
}

func TestPinnedQPDFAppRelativeDiscovery(t *testing.T) {
	root := os.Getenv("AOSCX_WP11_PACKAGE_ROOT")
	if root == "" {
		t.Skip("AOSCX_WP11_PACKAGE_ROOT is not set")
	}
	sidecar, err := resolveQPDFSidecar(QPDFSidecarOptions{
		ExecutablePath: filepath.Join(root, model.ExecutableName),
		LookupEnv:      func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if sidecar.ExecutableSHA256 != QPDFExecutableSHA256 ||
		sidecar.BundleSHA256 != QPDFRuntimeBundleSHA256 ||
		sidecar.Path != filepath.Join(root, "sidecars", "qpdf-mac-arm64", "bin", "qpdf") {
		t.Fatalf("unexpected app-relative qpdf sidecar: %+v", sidecar)
	}
}

func TestPinnedQPDFRejectsChangedBundleAndExplicitPathWins(t *testing.T) {
	qpdf := os.Getenv(QPDFPathEnvironment)
	if qpdf == "" {
		t.Skip(QPDFPathEnvironment + " is not set")
	}
	copyBundle := func(t *testing.T) string {
		t.Helper()
		sourceRoot := filepath.Clean(filepath.Join(filepath.Dir(qpdf), ".."))
		targetRoot := filepath.Join(t.TempDir(), "qpdf-mac-arm64")
		for _, relative := range []string{
			"bin/qpdf", "lib/libqpdf.30.dylib", "lib/libjpeg.8.dylib", "lib/libcrypto.3.dylib",
		} {
			body, err := os.ReadFile(filepath.Join(sourceRoot, relative))
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(targetRoot, relative)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0o600)
			if relative == "bin/qpdf" {
				mode = 0o700
			}
			if err := os.WriteFile(target, body, mode); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Join(targetRoot, "bin", "qpdf")
	}
	t.Run("changed library", func(t *testing.T) {
		copied := copyBundle(t)
		library := filepath.Clean(filepath.Join(filepath.Dir(copied), "..", "lib", "libcrypto.3.dylib"))
		file, err := os.OpenFile(library, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.Write([]byte{0})
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("mutate copied qpdf library: write=%v close=%v", writeErr, closeErr)
		}
		if _, err := reverifyQPDFSidecar(copied); err == nil ||
			!strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("changed qpdf library was accepted: %v", err)
		}
	})
	t.Run("symlink executable", func(t *testing.T) {
		copied := copyBundle(t)
		if err := os.Remove(copied); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(qpdf, copied); err != nil {
			t.Fatal(err)
		}
		if _, err := reverifyQPDFSidecar(copied); err == nil ||
			!strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("symlinked qpdf executable was accepted: %v", err)
		}
	})
	t.Run("explicit path precedence", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing-qpdf")
		_, err := resolveQPDFSidecar(QPDFSidecarOptions{
			ExplicitPath:   missing,
			ExecutablePath: filepath.Join(t.TempDir(), model.ExecutableName),
			LookupEnv: func(name string) string {
				if name == QPDFPathEnvironment {
					return qpdf
				}
				return ""
			},
		})
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("qpdf environment overrode explicit path: %v", err)
		}
	})
}

func TestPinnedQPDFRSSLimitLeavesNoPartialPDF(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	qpdf := os.Getenv(QPDFPathEnvironment)
	if chrome == "" || qpdf == "" {
		t.Skip("pinned Chrome and qpdf sidecars are required")
	}
	output, plan, archived := rendererFixture(t)
	config := DefaultConfig(chrome)
	config.QPDFPath = qpdf
	config.MaxQPDFRSSBytes = 1
	record, err := Generate(context.Background(), output, plan, &archived, config)
	if err == nil || record.Status != "failed" || !strings.Contains(err.Error(), "qpdf process") {
		t.Fatalf("qpdf RSS limit was not enforced: record=%+v err=%v", record, err)
	}
	entries, readErr := os.ReadDir(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".aos-cx-docs-dldr-") {
			t.Fatalf("failed qpdf retained a private partial: %s", entry.Name())
		}
	}
}

func TestPinnedChromeRefusesExistingDestination(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	qpdf := os.Getenv(QPDFPathEnvironment)
	if qpdf == "" {
		t.Skip(QPDFPathEnvironment + " is not set")
	}
	output, plan, archived := rendererFixture(t)
	name, err := storage.PDFFilename(plan.Document.Platform, plan.Document.Version, plan.Document.Title)
	if err != nil {
		t.Fatal(err)
	}
	existing := []byte("prior verified companion")
	if err := os.WriteFile(filepath.Join(output, name), existing, 0o600); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(chrome)
	config.QPDFPath = qpdf
	record, err := Generate(context.Background(), output, plan, &archived, config)
	if err == nil || !strings.Contains(err.Error(), "destination already exists") || record.Status != "failed" {
		t.Fatalf("existing generated PDF was not rejected: record=%+v err=%v", record, err)
	}
	after, err := os.ReadFile(filepath.Join(output, name))
	if err != nil || !bytes.Equal(after, existing) {
		t.Fatalf("existing generated PDF changed: bytes=%q err=%v", after, err)
	}
}

func TestPinnedChromeRevalidatesIdentityBeforeLaunch(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	sidecar, err := ResolveSidecar(chrome)
	if err != nil {
		t.Fatal(err)
	}
	sidecar.Revision = "changed-after-preflight"
	root := t.TempDir()
	input := filepath.Join(root, "input.html")
	if err := os.WriteFile(input, []byte("<!doctype html><body>test</body>"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(root, ".output-*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if _, err := renderChrome(context.Background(), sidecar, input, root, "", output, DefaultConfig(chrome)); err == nil ||
		!strings.Contains(err.Error(), "identity changed between preflight and launch") {
		t.Fatalf("changed preflight identity was accepted: %v", err)
	}
}

func TestPinnedChromeLimitsLeaveNoPartialPDF(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	qpdf := os.Getenv(QPDFPathEnvironment)
	if qpdf == "" {
		t.Skip(QPDFPathEnvironment + " is not set")
	}
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"output-limit", func(config *Config) { config.MaxPDFBytes = 31 }},
		{"page-limit", func(config *Config) { config.MaxPages = 1 }},
		{"rss-limit", func(config *Config) { config.MaxRSSBytes = 1 }},
		{"deadline", func(config *Config) { config.Timeout = time.Nanosecond }},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, plan, archived := rendererFixture(t)
			config := DefaultConfig(chrome)
			config.QPDFPath = qpdf
			test.mutate(&config)
			record, err := Generate(context.Background(), output, plan, &archived, config)
			if err == nil || record.Status != "failed" {
				t.Fatalf("renderer limit was not enforced: record=%+v err=%v", record, err)
			}
			if _, err := os.Stat(filepath.Join(output, record.Path)); !os.IsNotExist(err) {
				t.Fatalf("failed renderer published a PDF: %v", err)
			}
			entries, err := os.ReadDir(output)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".aos-cx-docs-dldr-") {
					t.Fatalf("failed renderer retained private partial input: %s", entry.Name())
				}
			}
		})
	}
}

func TestPinnedChromeBlocksExternalRequestAtCDP(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	qpdf := os.Getenv(QPDFPathEnvironment)
	if qpdf == "" {
		t.Skip(QPDFPathEnvironment + " is not set")
	}
	root := t.TempDir()
	input := filepath.Join(root, "input.html")
	if err := os.WriteFile(input, []byte(`<!doctype html><html><body><img src="https://example.invalid/tracker.png"></body></html>`), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(root, ".output-*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	sidecar, err := ResolveSidecar(chrome)
	if err != nil {
		t.Fatal(err)
	}
	_, err = renderChrome(context.Background(), sidecar, input, root, "", output, DefaultConfig(chrome))
	if err == nil || !strings.Contains(err.Error(), "blocked nonlocal or unsafe resources") {
		t.Fatalf("CDP did not report the blocked external request: %v", err)
	}
}

func TestPinnedChromeIgnoresOnlyImagesWithoutResourceAttributes(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	sidecar, err := ResolveSidecar(chrome)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		image     string
		wantError string
	}{
		{name: "publisher image wrapper placeholder", image: `<img>`},
		{name: "empty src remains required", image: `<img src="">`, wantError: `src=""`},
		{name: "empty srcset remains required", image: `<img srcset="">`, wantError: `srcset=""`},
		{name: "missing local source remains required", image: `<img src="missing.png">`, wantError: "missing.png"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			input := filepath.Join(root, "input.html")
			body := `<!doctype html><html><body><main class="print-content"><section class="print-topic" data-source-url="https://publisher.example/topic.htm">` +
				test.image + `</section></main></body></html>`
			if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := os.CreateTemp(root, ".output-*.pdf")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			metrics, err := renderChrome(context.Background(), sidecar, input, root, "", output, DefaultConfig(chrome))
			if test.wantError == "" {
				if err != nil || len(metrics.Audit.FailedImages) != 0 || metrics.Audit.ImageCount != 0 {
					t.Fatalf("inert publisher image wrapper was not ignored: metrics=%+v err=%v", metrics, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) ||
				len(metrics.Audit.FailedImages) != 1 || metrics.Audit.ImageCount != 1 {
				t.Fatalf("declared failed image was hidden: metrics=%+v err=%v", metrics, err)
			}
		})
	}
}

func TestPinnedChromePersistsBoundedFailedImageIdentity(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	qpdf := os.Getenv(QPDFPathEnvironment)
	if chrome == "" || qpdf == "" {
		t.Skip("pinned Chrome and qpdf sidecars are required")
	}
	output, plan, archived := rendererFixture(t)
	name := filepath.Join(output, archived.HTML.Topics[0].Path)
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte("</main>"), []byte(`<img src=""></main>`), 1)
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	archived.HTML.Topics[0].Size = int64(len(body))
	archived.HTML.Topics[0].SHA256 = hex.EncodeToString(hash[:])
	archived.HTML.Topics[0].SourceSize = int64(len(body))
	archived.HTML.Topics[0].SourceSHA256 = hex.EncodeToString(hash[:])
	data, err := json.MarshalIndent(archived.HTML, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(chrome)
	config.QPDFPath = qpdf
	record, err := Generate(context.Background(), output, plan, &archived, config)
	if err == nil || record.Status != "failed" || record.Validation == nil ||
		record.Validation.FailedImages != 1 || len(record.Validation.FailedImageDetails) != 1 ||
		!strings.Contains(record.Validation.FailedImageDetails[0], plan.Topics[0].URL) ||
		!strings.Contains(record.Validation.FailedImageDetails[0], `src=""`) {
		t.Fatalf("failed image identity was not retained: record=%+v err=%v", record, err)
	}
}

func TestPinnedChromeRejectsFixedOrStickyPrintContent(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	sidecar, err := ResolveSidecar(chrome)
	if err != nil {
		t.Fatal(err)
	}
	for _, position := range []string{"fixed", "sticky"} {
		t.Run(position, func(t *testing.T) {
			root := t.TempDir()
			input := filepath.Join(root, "input.html")
			body := `<!doctype html><html><body><main class="print-content"><div style="position:` +
				position + `">publisher runtime chrome</div></main></body></html>`
			if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := os.CreateTemp(root, ".output-*.pdf")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			_, err = renderChrome(context.Background(), sidecar, input, root, "", output, DefaultConfig(chrome))
			if err == nil || !strings.Contains(err.Error(), "fixed or sticky publisher elements") {
				t.Fatalf("%s print content was accepted: %v", position, err)
			}
		})
	}
}

func TestPinnedChromeRejectsExternalResourcesBeforeLaunch(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	qpdf := os.Getenv(QPDFPathEnvironment)
	if qpdf == "" {
		t.Skip(QPDFPathEnvironment + " is not set")
	}
	output, plan, archived := rendererFixture(t)
	name := filepath.Join(output, archived.HTML.Topics[0].Path)
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte("</main>"), []byte(`<img src="https://example.invalid/tracker.png"></main>`), 1)
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	archived.HTML.Topics[0].Size = int64(len(body))
	archived.HTML.Topics[0].SHA256 = hex.EncodeToString(hash[:])
	archived.HTML.Topics[0].SourceSize = int64(len(body))
	archived.HTML.Topics[0].SourceSHA256 = hex.EncodeToString(hash[:])
	data, err := json.MarshalIndent(archived.HTML, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(chrome)
	config.QPDFPath = qpdf
	record, err := Generate(context.Background(), output, plan, &archived, config)
	if err == nil || record.Status != "failed" || !strings.Contains(err.Error(), "remains online") {
		t.Fatalf("external resource was not rejected: record=%+v err=%v", record, err)
	}
}

func rendererFixture(t *testing.T) (string, model.DocumentPlan, model.ArchiveResult) {
	t.Helper()
	output := t.TempDir()
	for _, dir := range []string{"pages", "assets"} {
		if err := os.Mkdir(filepath.Join(output, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	files := map[string][]byte{
		"index.html":       []byte(`<!doctype html><html><body><main class="archive-content"><p>Source</p></main></body></html>`),
		"toc.html":         []byte(`<!doctype html><html><body>TOC</body></html>`),
		"search.json":      []byte("[]\n"),
		"search-index.js":  []byte("window.X=[];\n"),
		"search.js":        []byte(`"use strict";`),
		"assets/image.png": png,
		"assets/image.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><defs><linearGradient id="g"><stop offset="0" stop-color="red"/></linearGradient><symbol id="mark" viewBox="0 0 20 20"><circle cx="10" cy="10" r="8" fill="url(#g)"/></symbol></defs><rect width="20" height="20" fill="url(#g)"/></svg>`),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(output, filepath.FromSlash(name)), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	document := model.Document{ID: "guide", Title: "Renderer Guide", Platform: "6300", Version: "10.16",
		URL: "https://example.test/guide/home.html", Kind: "flare"}
	plan := model.DocumentPlan{Document: document,
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "flare", RootURL: "https://example.test/guide/", Entries: 2, UniqueTopics: 2}}
	archive := &model.HTMLArchive{SchemaVersion: 1, Status: "complete", SourceURL: document.URL,
		Inventory: plan.Inventory, GeneratedAt: "2026-09-15T00:00:00Z", Warnings: []string{}, Errors: []string{}}
	for index := range 2 {
		raw := "https://example.test/guide/topic-" + string(rune('a'+index)) + ".html"
		name := "pages/topic-" + string(rune('a'+index)) + ".html"
		heading := `<h1 id="section" data-aoscx-heading-fixture="command">copy running-config startup-config &lt;REMOTE-URL&gt;</h1>`
		if index == 1 {
			heading = `<h1 id="section" data-aoscx-heading-fixture="extreme">` +
				strings.Repeat("A deliberately extreme command topic title that must wrap naturally without clipping ", 6) +
				`</h1>`
		}
		content := `<main class="archive-content"><div id="mc-main-content">` + heading +
			`<p class="heading-following">First substantive content stays with the heading where feasible.</p>` +
			`<h2>Publisher internal heading</h2><h3>Publisher detail heading</h3><script>window.__aoscxPDFScriptProbe=true</script>` +
			`<p><a href="topic-` + string(rune('a'+index)) + `.html#section">self</a></p>`
		if index == 0 {
			content += `<style>div.topichero{position:fixed;z-index:8;top:30px;height:150px}div.container{margin-top:120px}thead{display:table-header-group}tr{break-inside:avoid-page}.tall{height:11in}pre{white-space:pre-wrap}.token{white-space:pre}</style>` +
				`<div class="topichero"><div class="docname">Renderer Guide Publisher Hero</div></div><div class="container">` +
				`<table><caption>Spans and repeated headers</caption><thead><tr><th rowspan="2">Name</th><th colspan="2">Values</th></tr>` +
				`<tr><th>One</th><th>Two</th></tr></thead><tbody><tr><td rowspan="2">alpha</td><td>A1</td><td>A2</td></tr>` +
				`<tr><td>B1</td><td>B2</td></tr><tr class="tall"><td>tall row</td><td>TOP-TALL-ROW</td><td>BOTTOM-TALL-ROW</td></tr></tbody></table>` +
				"<pre>interface 1/1/1\n    description  two-spaces-after-word\n\ttab-indented\nno shutdown</pre>" +
				`<p class="token">UNBREAKABLE_ABCDEFGHIJKLMNOPQRSTUVWXYZ_0123456789_ABCDEFGHIJKLMNOPQRSTUVWXYZ_0123456789_ABCDEFGHIJKLMNOPQRSTUVWXYZ_0123456789</p>` +
				`<svg viewBox="0 0 240 120"><defs><linearGradient id="inline-gradient"><stop stop-color="#ff8300"/><stop offset="1" stop-color="#7630ea"/></linearGradient>` +
				`<clipPath id="inline-clip"><rect x="20" y="20" width="200" height="80"/></clipPath></defs><rect width="240" height="120" fill="url(#inline-gradient)" clip-path="url(#inline-clip)"/></svg>` +
				`<img src="../assets/image.png"><img src="../assets/image.svg"><svg viewBox="0 0 20 20"><use href="../assets/image.svg#mark"/></svg></div>`
		}
		content += `</div></main>`
		body := []byte("<!doctype html><html><body>" + content + "</body></html>")
		if err := os.WriteFile(filepath.Join(output, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(body)
		record := model.FileRecord{URL: raw, Path: name, Status: "complete", Size: int64(len(body)),
			SourceSize: int64(len(body)), SourceSHA256: hex.EncodeToString(hash[:]), SHA256: hex.EncodeToString(hash[:]),
			SourceBookmarks: []string{"section"}, SourceBookmarksComplete: true}
		record.Supplementary = index == 1
		archive.Topics = append(archive.Topics, record)
		plan.Topics = append(plan.Topics, model.Topic{URL: raw, Title: "Topic"})
	}
	plan.TOC = []model.TocEntry{{
		Title: "Commands", Children: []model.TocEntry{
			{Title: "Topic", URL: plan.Topics[0].URL + "#section"},
			{Title: "Repeated topic", URL: plan.Topics[0].URL + "#section"},
			{Title: "External help", URL: "https://support.example.test/help"},
		},
	}}
	for _, name := range []string{"assets/image.png", "assets/image.svg"} {
		body := files[name]
		hash := sha256.Sum256(body)
		archive.Assets = append(archive.Assets, model.FileRecord{URL: "https://example.test/" + name, Path: name,
			Status: "complete", Size: int64(len(body)), SourceSize: int64(len(body)),
			SourceSHA256: hex.EncodeToString(hash[:]), SHA256: hex.EncodeToString(hash[:])})
	}
	hashOf := func(name string) string {
		sum := sha256.Sum256(files[name])
		return hex.EncodeToString(sum[:])
	}
	archive.Integrity = model.HTMLIntegrity{IndexSHA256: hashOf("index.html"), TOCSHA256: hashOf("toc.html"),
		SearchSHA256: hashOf("search.json"), SearchIndexSHA256: hashOf("search-index.js"), SearchJSSHA256: hashOf("search.js")}
	data, _ := json.MarshalIndent(archive, "", "  ")
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return output, plan, model.ArchiveResult{Document: document, OutputDir: output, Status: "complete", Format: "html", HTML: archive}
}

func assertGeneratedOutline(
	t *testing.T,
	qpdfPath, output, pdfPath string,
	plan model.DocumentPlan,
	archived model.ArchiveResult,
) {
	t.Helper()
	root, err := os.OpenRoot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var transient bytes.Buffer
	assembled, err := publication.Assemble(context.Background(), root, ".", plan, archived, &transient)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := expectedOutlineNodes(assembled.Outline)
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := ResolveQPDFSidecar(qpdfPath)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := inspectWithQPDF(
		context.Background(), sidecar, pdfPath, t.TempDir(), "assert-outline",
		QPDFOutlineProcessMaxRSSBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateQPDFOutline(document, expected); err != nil {
		t.Fatal(err)
	}
	for _, node := range flattenOutline(expected) {
		if strings.Contains(node.Title, "Publisher internal") ||
			strings.Contains(node.Title, "Publisher detail") {
			t.Fatalf("publisher content heading leaked into the explicit outline: %q", node.Title)
		}
	}
}
