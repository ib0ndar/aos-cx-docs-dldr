//go:build chromepdftests

package pdfgen

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/pdfcheck"
	"aos-cx-docs-dldr/internal/publication"
	"aos-cx-docs-dldr/internal/storage"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
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
		record.Validation.ScriptElements != 0 || record.Validation.ImageObjects < 1 ||
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

// Publication strips script from print input, so renderer-level script
// disabling is proven directly: a script that reaches Chrome must stay inert.
func TestPinnedChromeKeepsScriptThatReachesRendererInert(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	sidecar, err := ResolveSidecar(chrome)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	input := filepath.Join(root, "input.html")
	body := `<!doctype html><html><body><main class="print-content"><section class="print-topic">` +
		`<p>Script probe</p><script>window.__aoscxPDFScriptProbe=true</script></section></main></body></html>`
	if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(root, ".output-*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	metrics, err := renderChrome(context.Background(), sidecar, input, root, "", output, DefaultConfig(chrome))
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Audit.ScriptElements != 1 || metrics.Audit.ScriptProbe {
		t.Fatalf("script reached the renderer but was not kept inert: audit=%+v", metrics.Audit)
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

// printContentWidth is the width of a printed page's content area in CSS
// pixels, as Chrome lays it out: Letter paper less the side margins.
func printContentWidth() int64 {
	settings := ExpectedSettings()
	return int64((8.5 - settings.MarginLeftInches - settings.MarginRightInches) * 96)
}

// layOutPrintDocument adds publisher content to the renderer fixture's second
// topic, assembles the print document and opens it in the pinned Chrome with
// screen media, as the renderer prints it, at the content-area width, where
// the area's left edge is x=0. Chrome clips each printed page to that area.
// check then inspects the layout.
func layOutPrintDocument(t *testing.T, content string, check chromedp.Action) {
	t.Helper()
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	sidecar, err := ResolveSidecar(chrome)
	if err != nil {
		t.Fatal(err)
	}
	output, plan, archived := rendererFixture(t)
	topic := &archived.HTML.Topics[1]
	topicPath := filepath.Join(output, filepath.FromSlash(topic.Path))
	body, err := os.ReadFile(topicPath)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte(`</div></main>`), []byte(content+`</div></main>`), 1)
	if err := os.WriteFile(topicPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	topic.SHA256 = hex.EncodeToString(hash[:])
	topic.Size = int64(len(body))
	data, err := json.MarshalIndent(archived.HTML, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var document bytes.Buffer
	if _, err := publication.Assemble(context.Background(), root, ".", plan, archived, &document); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(output, "print.html")
	if err := os.WriteFile(input, document.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	tempRoot := t.TempDir()
	if err := createChromeLaunchDirectories(tempRoot); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var logs bytes.Buffer
	allocatorCtx, allocatorCancel := chromedp.NewExecAllocator(ctx, chromeAllocatorOptions(sidecar.Path, tempRoot, &logs)...)
	defer allocatorCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocatorCtx)
	defer browserCancel()
	if err := chromedp.Run(browserCtx,
		emulation.SetScriptExecutionDisabled(true),
		emulation.SetEmulatedMedia().WithMedia("screen"),
		emulation.SetDeviceMetricsOverride(printContentWidth(), 1000, 1, false),
		chromedp.Navigate((&url.URL{Scheme: "file", Path: input}).String()),
		check,
	); err != nil {
		t.Fatalf("lay out print document: %v\n%s", err, logs.String())
	}
}

// A list marker that hangs left of the print area loses its left part: a
// bullet prints as a half disc and step 10 as ".0.". The publisher's DITA
// stylesheet indents lists by only 1em; the print stylesheet's gutter must
// hold the rest of the marker.
func TestPinnedChromeKeepsListMarkersInsidePrintArea(t *testing.T) {
	lists := `<style>body{margin:0}ul.ul,ol.ol{padding-left:0;margin-left:1em}li.li{padding-left:.25em}</style>` +
		`<ul class="ul"><li class="li">Broadcast address</li><li class="li">Multicast address</li></ul>` +
		`<ol class="ol">` + strings.Repeat(`<li class="li">Step</li>`, 12) + `</ol>`
	markers := 0
	var clipped []string
	layOutPrintDocument(t, lists, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := dom.GetDocument().WithDepth(-1).Do(ctx)
		if err != nil {
			return err
		}
		var visit func(*cdp.Node) error
		visit = func(node *cdp.Node) error {
			if node.LocalName == "li" && slices.Contains(strings.Fields(node.AttributeValue("class")), "li") {
				for _, pseudo := range node.PseudoElements {
					if pseudo.PseudoType != cdp.PseudoTypeMarker {
						continue
					}
					box, err := dom.GetBoxModel().WithBackendNodeID(pseudo.BackendNodeID).Do(ctx)
					if err != nil {
						return err
					}
					markers++
					left := min(box.Border[0], box.Border[2], box.Border[4], box.Border[6])
					if left < 0 {
						clipped = append(clipped, fmt.Sprintf("marker %d at x=%.2f", markers, left))
					}
				}
			}
			for _, child := range node.Children {
				if err := visit(child); err != nil {
					return err
				}
			}
			return nil
		}
		return visit(tree)
	}))
	if markers != 14 || len(clipped) != 0 {
		t.Fatalf("list markers must lie inside the %d px print area: checked %d of 14, clipped %v",
			printContentWidth(), markers, clipped)
	}
}

// The publisher's Oxygen stylesheet caps code blocks at 600px and makes them
// scroll, and keeps captions on one line. Print cannot scroll, so a capped
// block loses every line past the cap; and a caption wider than the page
// makes Chrome shrink the whole document to fit it.
func TestPinnedChromePrintsCappedCodeBlocksAndWideCaptionsWhole(t *testing.T) {
	var code strings.Builder
	for line := range 120 {
		fmt.Fprintf(&code, "interface 1/1/%d\n", line+1)
	}
	content := `<style>pre{max-height:600px;overflow:auto}caption{white-space:nowrap}</style>` +
		`<pre class="pre codeblock">` + code.String() + `</pre>` +
		`<table class="wide-caption"><caption>` + strings.Repeat("Configuration task list for PIM and IGMP on an SVI. ", 8) +
		`</caption><tr><td>cell</td></tr></table>`
	var layout struct {
		HiddenCode   float64 `json:"hidden_code"`
		CaptionRight float64 `json:"caption_right"`
	}
	layOutPrintDocument(t, content, chromedp.Evaluate(`(() => {
		const code = document.querySelector(".print-content pre.codeblock");
		const caption = document.createRange();
		caption.selectNodeContents(document.querySelector(".print-content table.wide-caption caption"));
		return {hidden_code: code.scrollHeight - code.clientHeight, caption_right: caption.getBoundingClientRect().right};
	})()`, &layout))
	if layout.HiddenCode > 1 {
		t.Fatalf("code block hides %.0f px of its lines", layout.HiddenCode)
	}
	if width := float64(printContentWidth()); layout.CaptionRight > width {
		t.Fatalf("caption text ends at x=%.0f px, past the %.0f px print area", layout.CaptionRight, width)
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

// Publisher stylesheets carry rules for a scrolling browser window that cost
// content or pagination in print, often with more specificity than the
// print stylesheet's defaults. Each rule here comes from a reviewed guide;
// the print rules must win in Chrome's own cascade.
func TestPinnedChromePrintRulesOutrankPublisherRules(t *testing.T) {
	content := `<style>.screen{white-space:pre}div.screen>p{white-space:pre}p.CLI{white-space:normal;hyphens:auto}` +
		`.wh_topic_content{margin-bottom:1.875rem}h1{page-break-before:avoid}ol>li>ol>li{list-style-type:lower-alpha}` +
		`.fixture-font{font-family:'Open Sans'}p.CLI_Indented{white-space:pre}.inner-wrap{min-height:94.5vh}</style>` +
		`<div class="wh_topic_content"><h1 class="fixture-h1">Heading</h1><h4>Minor heading</h4><p class="subheading">Command History</p>` +
		`<div class="screen"><p>` + strings.Repeat("show arp vrf default ", 30) + `</p></div>` +
		`<p class="CLI">ip dhcp-snooping   preferred   default</p>` +
		`<div class="note"><div class="note__title">NOTE</div><p>Note text.</p></div>` +
		`<table><caption>Table 1. Caption</caption><tr><td>cell</td></tr></table>` +
		`<pre class="fixture-pre">line 1
line 2</pre><div class="p"><strong>Usage</strong><ul><li>item</li></ul></div>` +
		`<p class="CLI_Indented fixture-cli">` + strings.Repeat("ip igmp snooping static-group ", 30) + `</p>` +
		`<table class="fixture-parameters"><tr><td><p class="CLI_Indented">` + strings.Repeat("vlan-id ", 90) + `</p></td><td>` +
		strings.Repeat("x", 300) + `</td></tr></table><p class="fixture-float" style="float:left;width:900px">floated</p>` +
		`<table class="fixture-head"><thead><tr><th>Header</th></tr></thead><tbody><tr><td>row</td></tr></tbody></table>` +
		`<p class="Figure_Title">Figure 1 Topology</p><p class="fixture-image"><img alt="Topology"></p>` +
		`<div class="screen fixture-config"><p class="subheading">interface 1/1/1</p></div>` +
		`<span id="fixture-anchor"></span><h3 class="fixture-h3">Section after an anchor</h3>` +
		`<table class="fixture-command"><thead><tr><th>Platforms</th></tr></thead><tbody><tr><td>All</td></tr></tbody></table>` +
		`<p class="Subheading">Procedure</p>` + "<div class=\"screen fixture-gaps\">\n<p>one</p>\n<p>two</p>\n</div>" +
		`<p class="Figure_Title fixture-figure-image"><img alt="Inline figure"></p>` +
		`<p class="fixture-lead">The following:</p><ul><li>item</li></ul>` +
		`<h4 class="fixture-h4b">Examples</h4><p>&nbsp;</p><p class="inner-wrap fixture-inner">Card text</p>` +
		`<table class="fixture-tall"><tbody><tr><td>` + strings.Repeat("<p>value</p>", 6) + `</td></tr><tr><td>short</td></tr></tbody></table>` +
		`<table class="fixture-wide"><colgroup>` + strings.Repeat(`<col style="width:86px"><col style="width:820px">`, 6) +
		`<col style="width:86px"></colgroup><tr>` + strings.Repeat("<th>8360</th>", 13) + `</tr>` +
		`<tr><td><p class="fixture-command-name">show active-gateway</p></td>` + strings.Repeat("<td>Yes</td>", 12) + `</tr></table>` +
		`<p class="fixture-codelead">Showing IP routes:</p><pre>show ip route</pre>` +
		"<div class=\"screen fixture-listbox\">\n<ul>\n<li>a</li>\n<li>b</li>\n</ul>\n</div>" +
		`<p class="fixture-label"><b>Procedure</b></p><div class="note">Note text.</div>` +
		`<p class="fixture-extrabold" style="font-family:'Open Sans Extrabold'">All Switch Series</p>` +
		`<div class="p fixture-runlead">Configure the switch:<pre>vsx</pre></div>` +
		`<ul class="fixture-listcode"><li>Run:</li></ul><pre>show vsx status</pre>` +
		`<p class="fixture-clilead">Enter:</p><p class="CLI">show vsx brief</p>` +
		`<p class="fixture-figlead">For example:</p><figure class="fig"><img alt="Example"></figure>` +
		`<p class="fixture-wraplead">Using secure prompt:</p><div class="p"><pre>copy</pre></div>` +
		`<p class="fixture-quotelead">Mroutes on last hop router:</p><blockquote><div class="screen"><p>show</p></div></blockquote>` +
		`<ul><li class="fixture-itemlead">Example curl command:</li><div class="screen"><p>curl</p></div>` +
		`<li class="fixture-itemclilead">Run:</li><p><span class="CLI">show vsx</span></p>` +
		`<li class="fixture-itemclass">Enter:</li><p class="CLI">show vsx</p>` +
		`<li class="fixture-pair">Redistributing the router to BGP.<div class="screen"><p>redistribute ospf</p></div></li></ul>` +
		`<p class="CLI fixture-clilist">vlan 2</p><ul><li>interface vlan 2</li></ul>` +
		`<p class="fixture-blanklead">Configure the switch:</p><p>&nbsp;</p><div class="screen"><p>vsx</p></div>` +
		`<ol><li class="step"><span class="cmd">Run the command:</span><div class="itemgroup fixture-itemgroup"><pre>show</pre></div></li></ol>` +
		`<div class="Indent fixture-indent"><ul><li>Configure:</li></ul></div><div class="screen"><p>pim</p></div>` +
		`<section class="subheading fixture-examples"><p>Examples</p><p>Example text.</p></section>` +
		`<ul class="fixture-listblank"><li>Configure:</li></ul><p>&nbsp;</p><div class="screen"><p>vsx</p></div>` +
		"<pre class=\"fixture-tail\"><code>" + strings.Repeat("show vsx status\n", 12) + "      </code></pre>" +
		`<p class="fixture-chain">Example:</p><p>Configure the switch.</p><pre>vsx</pre>` +
		`<p class="fixture-tablelead">The tasks are as follows:</p><table><tr><td>task</td></tr></table>` +
		`<p class="fixture-imagelead">Use the following deployment:</p><p class="fixture-image-block"><img alt="Deployment"><img alt="Detail"></p>` +
		`<p class="CLI fixture-clirun">ntp enable</p><p class="CLI">no ntp enable</p>` +
		"<pre class=\"fixture-head\">show vsx\n\n\nstatus\n" + strings.Repeat("show vsx status\n", 12) + "</pre>" +
		"<div class=\"screen fixture-edges\">\n<p>first</p>\n<p>second</p>\n<p>" + strings.Repeat("line\n", 10) + "</p>\n<p>last</p>\n</div>" +
		`<p class="fixture-font">Open Sans text</p><p class="subheading fixture-tail">&nbsp;</p></div>`
	var styles map[string]string
	var fonts []string
	layOutPrintDocument(t, content, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := chromedp.Evaluate(`(() => {
			const style = (selector, property) => {
				const element = document.querySelector(selector);
				return element ? getComputedStyle(element)[property] : "missing";
			};
			const screen = document.querySelector(".print-content div.screen");
			const right = element => {
				const range = document.createRange();
				range.selectNodeContents(element);
				return Math.max(range.getBoundingClientRect().right, element.getBoundingClientRect().right);
			};
			const widest = Math.max(...[".fixture-cli", "table.fixture-parameters", ".fixture-float", "table.fixture-wide"].map(
				selector => right(document.querySelector(".print-content " + selector))));
			return {
				screen_hidden: String(screen.scrollWidth - screen.clientWidth),
				cli_white_space: style(".print-content p.CLI", "whiteSpace"),
				cli_hyphens: style(".print-content p.CLI", "hyphens"),
				note_break_inside: style(".print-content div.note", "breakInside"),
				note_title_break_after: style(".print-content .note__title", "breakAfter"),
				topic_margin_bottom: style(".print-content .wh_topic_content", "marginBottom"),
				h1_break_before: style(".print-content h1.fixture-h1", "breakBefore"),
				h4_break_after: style(".print-content h4", "breakAfter"),
				subheading_break_after: style(".print-content p.subheading", "breakAfter"),
				caption_break_after: style(".print-content caption", "breakAfter"),
				toc_nested_marker: style(".print-toc ol ol li", "listStyleType"),
				screen_break_inside: style(".print-content div.screen", "breakInside"),
				pre_widows: style(".print-content pre.fixture-pre", "widows"),
				pre_orphans: style(".print-content pre.fixture-pre", "orphans"),
				label_list_break_before: style(".print-content div.p > ul", "breakBefore"),
				flare_overflow: String(Math.max(0, Math.ceil(widest - innerWidth))),
				thead_break_inside: style(".print-content table.fixture-head thead", "breakInside"),
				figure_title_break_after: style(".print-content p.Figure_Title", "breakAfter"),
				figure_image_break_inside: style(".print-content p.fixture-image", "breakInside"),
				screen_first_line_break_before: style(".print-content div.fixture-gaps > p", "breakBefore"),
				config_break_inside: style(".print-content div.fixture-config > p.subheading", "breakInside"),
				anchored_heading_break_before: style(".print-content h3.fixture-h3", "breakBefore"),
				short_code_break_inside: style(".print-content pre.fixture-pre", "breakInside"),
				content_padding_right: style(".print-content", "paddingRight"),
				command_row_break_inside: style(".print-content table.fixture-command tbody tr", "breakInside"),
				capital_subheading_break_after: style(".print-content p.Subheading", "breakAfter"),
				screen_gap_display: style(".print-content div.fixture-gaps > .print-screen-gap", "display"),
				screen_gap_break_before: style(".print-content div.fixture-gaps > .print-screen-gap:not(:first-child)", "breakBefore"),
				figure_with_image_break_after: style(".print-content p.fixture-figure-image", "breakAfter"),
				figure_with_image_break_inside: style(".print-content p.fixture-figure-image", "breakInside"),
				lead_break_after: style(".print-content p.fixture-lead", "breakAfter"),
				screen_gap_break_inside: style(".print-content div.fixture-gaps > .print-screen-gap", "breakInside"),
				heading_blank_break_after: style(".print-content h4.fixture-h4b + p", "breakAfter"),
				topic_tail_break_after: style(".print-content p.fixture-tail", "breakAfter"),
				tall_row_break_inside: style(".print-content table.fixture-tall tr:first-child", "breakInside"),
				short_row_break_inside: style(".print-content table.fixture-tall tr:last-child", "breakInside"),
				screen_box_break_before: style(".print-content div.fixture-gaps > :first-child", "breakBefore"),
				thead_break_after: style(".print-content table.fixture-head thead", "breakAfter"),
				inner_wrap_min_height: style(".print-content p.fixture-inner", "minHeight"),
				cell_min_width: String(Number.parseFloat(style(".print-content table.fixture-tall td", "minWidth")) > 0),
				wide_table_cell_min_width: style(".print-content table.fixture-wide td", "minWidth"),
				code_lead_break_after: style(".print-content p.fixture-codelead", "breakAfter"),
				list_box_line_break_before: style(".print-content div.fixture-listbox > ul > li", "breakBefore"),
				label_break_after: style(".print-content p.fixture-label", "breakAfter"),
				wide_table_word_lines: (() => {
					const text = document.querySelector(".print-content p.fixture-command-name").firstChild;
					const start = text.data.indexOf("gateway");
					const range = document.createRange();
					range.setStart(text, start);
					range.setEnd(text, start + "gateway".length);
					return String(range.getClientRects().length);
				})(),
				leadin_display: style(".print-content div.fixture-runlead > .print-leadin", "display"),
				leadin_break_after: style(".print-content div.fixture-runlead > .print-leadin", "breakAfter"),
				list_code_break_after: style(".print-content ul.fixture-listcode", "breakAfter"),
				cli_lead_break_after: style(".print-content p.fixture-clilead", "breakAfter"),
				figure_lead_break_after: style(".print-content p.fixture-figlead", "breakAfter"),
				wrapped_code_lead_break_after: style(".print-content p.fixture-wraplead", "breakAfter"),
				quoted_code_lead_break_after: style(".print-content p.fixture-quotelead", "breakAfter"),
				item_code_lead_break_after: style(".print-content li.fixture-itemlead", "breakAfter"),
				item_cli_lead_break_after: style(".print-content li.fixture-itemclilead", "breakAfter"),
				item_cli_class_lead_break_after: style(".print-content li.fixture-itemclass", "breakAfter"),
				lead_pair_break_inside: style(".print-content li.fixture-pair", "breakInside"),
				cli_before_list_break_after: style(".print-content p.fixture-clilist", "breakAfter"),
				blank_lead_break_after: style(".print-content p.fixture-blanklead", "breakAfter"),
				step_code_break_before: style(".print-content div.fixture-itemgroup", "breakBefore"),
				indented_lead_break_after: style(".print-content div.fixture-indent", "breakAfter"),
				section_label_break_after: style(".print-content section.fixture-examples > p:first-child", "breakAfter"),
				list_blank_code_break_after: style(".print-content ul.fixture-listblank", "breakAfter"),
				code_tail_widows: style(".print-content pre.fixture-tail", "widows"),
				code_head_orphans: style(".print-content pre.fixture-head", "orphans"),
				chain_break_after: style(".print-content p.fixture-chain", "breakAfter"),
				table_lead_break_after: style(".print-content p.fixture-tablelead", "breakAfter"),
				image_lead_break_after: style(".print-content p.fixture-imagelead", "breakAfter"),
				image_block_orphans: style(".print-content p.fixture-image-block", "orphans"),
				chain_break_inside: style(".print-content p.fixture-chain", "breakInside"),
				cli_run_break_after: style(".print-content p.fixture-clirun", "breakAfter"),
				screen_edge_break_before: style(".print-content div.fixture-edges > p:first-of-type", "breakBefore"),
				screen_edge_break_inside: style(".print-content div.fixture-edges > p:first-of-type", "breakInside"),
				screen_middle_break_before: style(".print-content div.fixture-edges > p:nth-of-type(3)", "breakBefore"),
				item_code_lead_break_inside: style(".print-content li.fixture-itemlead", "breakInside"),
				code_lead_break_inside: style(".print-content p.fixture-codelead", "breakInside"),
				list_lead_break_inside: style(".print-content p.fixture-lead", "breakInside"),
				run_lead_break_inside: style(".print-content div.fixture-runlead > .print-leadin", "breakInside"),
				cli_lead_break_inside: style(".print-content p.fixture-clilead", "breakInside"),
				step_command_display: style(".print-content li.step > span.cmd", "display"),
				step_command_break_inside: style(".print-content li.step > span.cmd", "breakInside"),
			};
		})()`, &styles).Do(ctx); err != nil {
			return err
		}
		tree, err := dom.GetDocument().Do(ctx)
		if err != nil {
			return err
		}
		if err := css.Enable().Do(ctx); err != nil {
			return err
		}
		for _, selector := range []string{".print-content p.fixture-font", ".print-content p.fixture-extrabold"} {
			node, err := dom.QuerySelector(tree.NodeID, selector).Do(ctx)
			if err != nil {
				return err
			}
			used, err := css.GetPlatformFontsForNode(node).Do(ctx)
			if err != nil {
				return err
			}
			for _, font := range used {
				fonts = append(fonts, font.FamilyName)
			}
		}
		return nil
	}))
	want := map[string]string{
		"screen_hidden": "0", "cli_white_space": "pre-wrap", "cli_hyphens": "manual",
		"note_break_inside": "avoid-page", "note_title_break_after": "avoid-page", "topic_margin_bottom": "0px",
		"h1_break_before": "auto", "h4_break_after": "avoid-page", "subheading_break_after": "avoid-page",
		"caption_break_after": "avoid-page", "toc_nested_marker": "none",
		"screen_break_inside": "avoid-page", "pre_widows": "3", "pre_orphans": "3",
		"label_list_break_before": "avoid-page", "flare_overflow": "0", "thead_break_inside": "avoid-page",
		"figure_title_break_after": "avoid-page", "figure_image_break_inside": "avoid-page",
		"screen_first_line_break_before": "avoid-page", "config_break_inside": "auto",
		"anchored_heading_break_before": "avoid-page", "short_code_break_inside": "avoid-page",
		"content_padding_right": "1px", "command_row_break_inside": "auto", "capital_subheading_break_after": "avoid-page",
		"screen_gap_display": "block", "screen_gap_break_before": "avoid-page", "figure_with_image_break_after": "auto",
		"figure_with_image_break_inside": "avoid-page", "lead_break_after": "avoid-page",
		"screen_gap_break_inside": "avoid-page", "heading_blank_break_after": "avoid-page", "topic_tail_break_after": "auto",
		"tall_row_break_inside": "auto", "short_row_break_inside": "avoid-page",
		"screen_box_break_before": "auto", "thead_break_after": "avoid-page", "inner_wrap_min_height": "0px",
		"cell_min_width": "true", "wide_table_cell_min_width": "0px", "code_lead_break_after": "avoid-page",
		"list_box_line_break_before": "avoid-page", "label_break_after": "avoid-page",
		"wide_table_word_lines": "1", "leadin_display": "block", "leadin_break_after": "avoid-page",
		"list_code_break_after": "avoid-page", "cli_lead_break_after": "avoid-page", "figure_lead_break_after": "avoid-page",
		"wrapped_code_lead_break_after": "avoid-page", "quoted_code_lead_break_after": "avoid-page",
		"item_code_lead_break_after": "avoid-page", "item_cli_lead_break_after": "avoid-page",
		"item_cli_class_lead_break_after": "avoid-page", "lead_pair_break_inside": "avoid-page",
		"cli_before_list_break_after": "auto", "blank_lead_break_after": "avoid-page", "step_code_break_before": "avoid-page",
		"indented_lead_break_after": "avoid-page", "section_label_break_after": "avoid-page",
		"list_blank_code_break_after": "avoid-page", "code_tail_widows": "4", "code_lead_break_inside": "avoid-page",
		"code_head_orphans": "5", "chain_break_after": "avoid-page", "table_lead_break_after": "avoid-page",
		"image_lead_break_after": "avoid-page", "image_block_orphans": "1", "chain_break_inside": "avoid-page",
		"cli_run_break_after": "avoid-page", "screen_edge_break_before": "avoid-page", "screen_edge_break_inside": "avoid-page",
		"screen_middle_break_before": "avoid-page", "item_code_lead_break_inside": "avoid-page",
		"list_lead_break_inside": "avoid-page", "run_lead_break_inside": "avoid-page", "cli_lead_break_inside": "avoid-page",
		"step_command_display": "block", "step_command_break_inside": "avoid-page",
	}
	for key, value := range want {
		if styles[key] != value {
			t.Errorf("%s = %q, want %q", key, styles[key], value)
		}
	}
	if len(fonts) == 0 || slices.ContainsFunc(fonts, func(family string) bool { return strings.Contains(family, "Times") }) {
		t.Errorf("Open Sans and Open Sans Extrabold text rendered with %v, want installed sans-serif faces", fonts)
	}
}

// The renderer prints with the publisher stylesheets' screen rules, as the
// HTML archive shows a guide, not with the rules a publisher wrote for its
// own PDF output. A rule that makes an element four pages tall shows which
// media the page was printed for.
func TestPinnedChromeRendersWithScreenMedia(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	sidecar, err := ResolveSidecar(chrome)
	if err != nil {
		t.Fatal(err)
	}
	for media, tall := range map[string]bool{"screen": true, "print": false} {
		t.Run(media, func(t *testing.T) {
			root := t.TempDir()
			input := filepath.Join(root, "input.html")
			body := `<!doctype html><html><head><style>@page{size:Letter;margin:.72in .68in}@media ` + media +
				`{.probe{height:4000px}}</style></head><body><main class="print-content"><div class="probe">publisher text</div></main></body></html>`
			if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := os.CreateTemp(root, ".output-*.pdf")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			if _, err := renderChrome(context.Background(), sidecar, input, root, "", output, DefaultConfig(chrome)); err != nil {
				t.Fatal(err)
			}
			info, err := output.Stat()
			if err != nil {
				t.Fatal(err)
			}
			summary, err := pdfcheck.InspectGenerated(output, info.Size(), pdfcheck.GeneratedExpectations{MaxPages: 100})
			if err != nil {
				t.Fatal(err)
			}
			if got := summary.PageCount > 1; got != tall {
				t.Fatalf("@media %s rule applied=%v, want %v (%d pages)", media, got, tall, summary.PageCount)
			}
		})
	}
}

// Chrome shrinks every page of a document to fit its widest element, so one
// table whose words cannot fit the page would print the whole guide small.
// The renderer scales such a table down on its own instead.
func TestPinnedChromeFitsWideTablesInsteadOfShrinkingTheDocument(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	sidecar, err := ResolveSidecar(chrome)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	input := filepath.Join(root, "input.html")
	body := `<!doctype html><html><head><style>@page{size:Letter;margin:.72in .68in}</style></head><body>` +
		`<main class="print-content"><p>Publisher text</p><table><tr>` + strings.Repeat("<td>Intermediate_Router1</td>", 12) +
		`</tr></table><table><tr><td>cell</td></tr></table></main></body></html>`
	if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(root, ".output-*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if _, err := renderChrome(context.Background(), sidecar, input, root, "", output, DefaultConfig(chrome)); err != nil {
		t.Fatal(err)
	}
	printed, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if scale := pdfContentScale(t, printed); scale != 3.125 {
		t.Fatalf("document printed at %.2f %% of its size, want 100 %%", scale/3.125*100)
	}
}

// pdfContentScale returns the first scale that a Chrome-printed page's
// content sets after flipping the page: 3.125 for a document printed at
// full size, less when Chrome shrank it to fit its widest element.
func pdfContentScale(t *testing.T, body []byte) float64 {
	t.Helper()
	scale := regexp.MustCompile(`(?m)^([\d.]+) 0 0 [\d.]+ [-\d.]+ [-\d.]+ cm$`)
	for _, stream := range regexp.MustCompile(`(?s)>>\s*stream\r?\n(.*?)\r?\nendstream`).FindAllSubmatch(body, -1) {
		reader, err := zlib.NewReader(bytes.NewReader(stream[1]))
		if err != nil {
			continue
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			continue
		}
		for _, match := range scale.FindAllSubmatch(content, -1) {
			if value, err := strconv.ParseFloat(string(match[1]), 64); err == nil && value > 1 {
				return value
			}
		}
	}
	t.Fatal("printed PDF sets no content scale")
	return 0
}

// A parent topic whose page is only its title ends with that heading, and
// the renderer refuses a print document whose headings may end a page. The
// rule that frees the element ending a topic from keep-with-next must leave
// headings alone.
func TestPinnedChromeGeneratesTopicsThatEndWithAHeading(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	qpdf := os.Getenv(QPDFPathEnvironment)
	if chrome == "" || qpdf == "" {
		t.Skip("pinned Chrome and qpdf paths are required")
	}
	output, plan, archived := rendererFixture(t)
	topic := &archived.HTML.Topics[1]
	topicPath := filepath.Join(output, filepath.FromSlash(topic.Path))
	body, err := os.ReadFile(topicPath)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte(`</div></main>`), []byte(`<h2>Trailing heading</h2></div></main>`), 1)
	if err := os.WriteFile(topicPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	topic.SHA256 = hex.EncodeToString(hash[:])
	topic.Size = int64(len(body))
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
	if record.Status != "complete" || record.Validation == nil || !record.Validation.HeadingPaginationValidated {
		t.Fatalf("topic ending with a heading was not generated: %+v", record)
	}
}
