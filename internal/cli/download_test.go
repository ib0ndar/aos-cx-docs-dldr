package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/testutil"
	"github.com/charmbracelet/x/ansi"
)

const pdfPortal = `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.10</option><option>10.16</option></select><div id="menu1"><table><tr>
<td id="first" onclick="openFile('PDF','first','aoscx')">First Guide</td>
<td id="second" onclick="openFile('PDF','second','aoscx')">Second Guide</td>
<td id="html" onclick="openFile('HTML','html','aoscx')">HTML Guide</td></tr></table></div></html>`

func binary(t *testing.T) string {
	t.Helper()
	value := os.Getenv("AOSCX_TEST_BINARY")
	if value == "" {
		t.Skip("actual executable coverage requires AOSCX_TEST_BINARY")
	}
	return value
}

func execute(t *testing.T, args []string) (int, []byte, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary(t), args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return code, stdout.Bytes(), stderr.String()
}

func fixtureCatalog(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/robots.txt":
		w.WriteHeader(404)
	case "/portal/aoscx.html":
		io.WriteString(w, pdfPortal)
	case "/portal/json/aoscx/first.json", "/portal/json/aoscx/second.json":
		id := strings.TrimSuffix(filepath.Base(r.URL.Path), ".json")
		json.NewEncoder(w).Encode(map[string]any{"10.10": map[string]string{"6300": "http://" + r.Host + "/pdf/" + id + ".pdf"}})
	case "/portal/json/aoscx/html.json":
		json.NewEncoder(w).Encode(map[string]any{"10.10": map[string]string{"6300": "http://" + r.Host + "/guide.html"}})
	default:
		return false
	}
	return true
}

func TestExecutablePDFPublicationResumeRefreshAndFailedUpdate(t *testing.T) {
	binary(t)
	var mode, gets, conditional atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixtureCatalog(w, r) {
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/pdf/") {
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(404)
			return
		}
		gets.Add(1)
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("ETag", `"v1"`)
		if mode.Load() == 1 && r.Header.Get("If-None-Match") == `"v1"` {
			conditional.Add(1)
			w.WriteHeader(304)
			return
		}
		if mode.Load() == 2 {
			io.WriteString(w, "%PDF-1.7\ntruncated")
			return
		}
		w.Write(testutil.PDF(filepath.Base(r.URL.Path)))
	}))
	defer server.Close()
	base, cache := filepath.Join(t.TempDir(), "new", "library"), t.TempDir()
	args := []string{"--transport", "compatible", "--platform", "6300", "--version", "10.10", "--guides", "first", "first", "second",
		"--destination", base, "--raw-cache", cache, "--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0", "--retries", "0", "--json", "--zip",
		"--convert-html-to-pdf", "--chrome-path", filepath.Join(t.TempDir(), "must-not-be-used")}
	code, output, stderr := execute(t, args)
	if code != 0 {
		t.Fatalf("initial archive exit=%d output=%s stderr=%s", code, output, stderr)
	}
	var result DownloadOutput
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if result.Library != filepath.Join(base, "6300", "10.10") || result.Manifest.NativeSchemaVersion != 1 ||
		result.ZIP != filepath.Join(base, "6300", "10.10.zip") || result.Manifest.Status != "complete" ||
		len(result.Manifest.PDFGuides) != 2 || len(result.Manifest.Attempts) != 2 {
		t.Fatalf("incorrect published library: %+v", result)
	}
	for id, guide := range result.Manifest.PDFGuides {
		files, _ := os.ReadDir(filepath.Join(result.Library, id))
		if len(files) != 1 || files[0].Name() != guide.PDF.Path {
			t.Fatal("publisher guide has companion files")
		}
		body, _ := os.ReadFile(filepath.Join(result.Library, id, guide.PDF.Path))
		if !bytes.Equal(body, testutil.PDF(id+".pdf")) || guide.PDF.SourceSHA256 != guide.PDF.SHA256 {
			t.Fatal("original PDF bytes/provenance changed")
		}
	}
	code, _, stderr = execute(t, args)
	if code != 0 || gets.Load() != 2 {
		t.Fatalf("resume fetched PDFs or failed: %d gets=%d %s", code, gets.Load(), stderr)
	}
	mode.Store(1)
	refresh := append(append([]string{}, args...), "--refresh")
	code, _, stderr = execute(t, refresh)
	if code != 0 || conditional.Load() != 2 {
		t.Fatalf("conditional PDF refresh failed: %d count=%d %s", code, conditional.Load(), stderr)
	}
	mode.Store(2)
	code, output, stderr = execute(t, refresh)
	if code != 2 {
		t.Fatalf("invalid refresh did not exit incomplete: %d %s %s", code, output, stderr)
	}
	var failed DownloadOutput
	json.Unmarshal(output, &failed)
	if failed.Manifest.Status != "incomplete" || !failed.Manifest.Attempts[0].RetainedPrevious ||
		failed.ZIP != result.ZIP ||
		failed.Manifest.PDFGuides["first"].PDF.SHA256 != result.Manifest.PDFGuides["first"].PDF.SHA256 {
		t.Fatal("failed update destroyed previous complete guide")
	}
	if _, err := os.Stat(filepath.Join(base, "6300", ".10.10.lock")); !os.IsNotExist(err) {
		t.Fatal("native lock leaked")
	}
}

func TestFinderMetadataAllowsCLIResumeAndRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixtureCatalog(w, r) {
			return
		}
		if r.URL.Path != "/pdf/first.pdf" {
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(testutil.PDF("first.pdf"))
	}))
	defer server.Close()
	base, cache := filepath.Join(t.TempDir(), "library"), t.TempDir()
	args := []string{
		"--transport", "http",
		"--platform", "6300",
		"--version", "10.10",
		"--guides", "first",
		"--destination", base,
		"--raw-cache", cache,
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0",
		"--retries", "0",
		"--json",
	}
	runOnce := func(extra ...string) DownloadOutput {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), append(append([]string{}, args...), extra...), &stdout, &stderr, newNativeClient); code != 0 {
			t.Fatalf("CLI exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
		}
		var output DownloadOutput
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatal(err)
		}
		return output
	}
	initial := runOnce()
	finderMetadata := filepath.Join(initial.Library, ".DS_Store")
	if err := os.WriteFile(finderMetadata, []byte("Finder metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := runOnce()
	if resumed.Manifest.Status != "complete" {
		t.Fatalf("Finder metadata blocked cached resume: %+v", resumed.Manifest)
	}
	if _, err := os.Stat(finderMetadata); !os.IsNotExist(err) {
		t.Fatalf("normal resume publication retained Finder metadata: %v", err)
	}
	if err := os.WriteFile(finderMetadata, []byte("Finder metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	refreshed := runOnce("--refresh")
	if refreshed.Manifest.Status != "complete" {
		t.Fatalf("Finder metadata blocked refresh: %+v", refreshed.Manifest)
	}
	if _, err := os.Stat(finderMetadata); !os.IsNotExist(err) {
		t.Fatalf("normal refresh publication retained Finder metadata: %v", err)
	}
}

func TestExecutableUnsupportedSelectionCreatesNoLibrary(t *testing.T) {
	binary(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fixtureCatalog(w, r) {
			t.Errorf("unexpected guide request: %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	for _, selection := range [][]string{
		{"--guides", "missing"},
	} {
		base := filepath.Join(t.TempDir(), "not-created")
		args := []string{"--platform", "6300", "--version", "10.10", "--destination", base, "--raw-cache", t.TempDir(),
			"--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0", "--retries", "0"}
		args = append(args, selection...)
		code, _, stderr := execute(t, args)
		if code != 1 {
			t.Fatalf("unsupported selection succeeded: %v code=%d %s", selection, code, stderr)
		}

		if _, err := os.Stat(base); !os.IsNotExist(err) {
			t.Fatal("invalid selection created output library")
		}
	}
}

func TestExecutableStaticHTMLPublication(t *testing.T) {
	binary(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixtureCatalog(w, r) {
			return
		}
		switch r.URL.Path {
		case "/guide.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><head><link rel="stylesheet" href="/static.css"></head><body>
	<div class="wh_header">Shell</div><nav class="toc"><ul><li><a href="/guide.html">Home</a>
	<ul><li><a href="/topic.html?mode=full#topic">Topic</a></li></ul></li></ul></nav>
	<main class="wh_topic_content"><h1>Home</h1><a href="/topic.html?mode=full#topic">Topic</a>
	<img src="/image.png"></main></body></html>`)
		case "/topic.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<article><h1 id="topic">Topic</h1><table><tr><th>Key</th><td>Value</td></tr></table></article>`)
		case "/static.css":
			w.Header().Set("Content-Type", "text/css")
			io.WriteString(w, `.wh_topic_content{background:url("/image.png")}`)
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(tinyPNGFixture)
		default:
			t.Errorf("unexpected static request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	base := t.TempDir()
	args := []string{"--platform", "6300", "--version", "10.10", "--guides", "html",
		"--destination", base, "--raw-cache", t.TempDir(), "--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--json"}
	code, output, stderr := execute(t, args)
	if code != 0 {
		t.Fatalf("static executable failed: code=%d stdout=%s stderr=%s", code, output, stderr)
	}
	var result DownloadOutput
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	guide := result.Manifest.HTMLGuides["html"]
	if result.Manifest.Status != "complete" || guide.Document.Kind != "static" ||
		len(guide.HTML.Topics) != 2 || len(guide.HTML.Assets) != 2 ||
		guide.HTML.Integrity.CheckedLinks == 0 {
		t.Fatalf("static executable output incomplete: %+v", result)
	}
}

func TestCLIInitiallyStaticRoutePublishesVerifiedPDF(t *testing.T) {
	body := testutil.PDF("resolved static route")
	sum := sha256.Sum256(body)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">Resolved PDF Guide</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/guide.json":
			json.NewEncoder(w).Encode(map[string]any{
				"10.18.xxxx": map[string]string{"6300": server.URL + "/guide/index.html"},
			})
		case "/guide/index.html":
			http.Redirect(w, r, "/guide/original.pdf", http.StatusFound)
		case "/guide/original.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(body)
		default:
			t.Errorf("unexpected resolved-PDF request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "6300", "--version", "10.18.xxxx",
		"--guides", "guide", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "cache"),
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--workers", "1", "--json",
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, newNativeClient); code != 0 {
		t.Fatalf("resolved static PDF exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("stdout is not isolated JSON: %v\n%s", err, &stdout)
	}
	guide := output.Manifest.PDFGuides["guide"]
	pdfURL := server.URL + "/guide/original.pdf"
	selectedURL := server.URL + "/guide/index.html"
	if output.Manifest.Status != "complete" || len(output.Manifest.HTMLGuides) != 0 ||
		guide.Status != "complete" || guide.Format != "pdf" || guide.Document.Kind != "pdf" ||
		guide.Document.URL != selectedURL || guide.Document.RouteOrigin != "publisher-url" ||
		guide.PDF == nil || guide.PDF.URL != pdfURL || guide.PDF.FinalURL != pdfURL ||
		!guide.PDF.SourceVerified || guide.PDF.SourceSHA256 != hex.EncodeToString(sum[:]) ||
		guide.PDF.SourceSHA256 != guide.PDF.SHA256 || guide.PDF.Size != int64(len(body)) {
		t.Fatalf("resolved static PDF provenance incomplete: %+v", output.Manifest)
	}
	if len(guide.PDF.Inputs) != 1 || guide.PDF.Inputs[0].RequestedURL != selectedURL ||
		guide.PDF.Inputs[0].FinalURL != pdfURL ||
		guide.PDF.Inputs[0].SHA256 != guide.PDF.SourceSHA256 ||
		guide.PDF.Inputs[0].Size != len(body) {
		t.Fatalf("resolved source evidence missing: %+v", guide.PDF.Inputs)
	}
	entries, err := os.ReadDir(filepath.Join(output.Library, "guide"))
	if err != nil || len(entries) != 1 || entries[0].Name() != guide.PDF.Path {
		t.Fatalf("resolved publisher PDF is not a one-file guide: %v err=%v", entries, err)
	}
	saved, err := os.ReadFile(filepath.Join(output.Library, "guide", guide.PDF.Path))
	if err != nil || !bytes.Equal(saved, body) {
		t.Fatal("resolved publisher PDF bytes changed")
	}
	if len(output.Manifest.Attempts) != 1 || output.Manifest.Attempts[0].Format != "pdf" ||
		output.Manifest.Attempts[0].Status != "complete" {
		t.Fatalf("resolved PDF attempt/history semantics changed: %+v", output.Manifest.Attempts)
	}
}

func TestCLIInvalidResolvedPDFAndHTMLFailureStayIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
	}{
		{"invalid-pdf", "application/pdf", "%PDF-1.7\ntruncated"},
		{"ordinary-html-failure", "text/html", `<html><main><h1>Guide</h1></main></html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var guessedPDF atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/robots.txt":
					w.WriteHeader(http.StatusNotFound)
				case "/portal/aoscx.html":
					io.WriteString(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">Broken HTML Guide</td>
</tr></table></div></html>`)
				case "/portal/json/aoscx/guide.json":
					json.NewEncoder(w).Encode(map[string]any{
						"10.18.xxxx": map[string]string{"6300": server.URL + "/guide/index.html"},
					})
				case "/guide/index.html":
					w.Header().Set("Content-Type", tc.contentType)
					io.WriteString(w, tc.body)
				default:
					if strings.HasSuffix(strings.ToLower(r.URL.Path), ".pdf") {
						guessedPDF.Add(1)
					}
					t.Errorf("unexpected failure-fixture request: %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			root := t.TempDir()
			args := []string{
				"--transport", "http", "--platform", "6300", "--version", "10.18.xxxx",
				"--guides", "guide", "--destination", filepath.Join(root, "library"),
				"--raw-cache", filepath.Join(root, "cache"),
				"--portal-url", server.URL + "/portal/aoscx.html",
				"--delay", "0", "--retries", "0", "--workers", "1", "--json",
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr, newNativeClient); code != 2 {
				t.Fatalf("invalid source exit=%d, want 2; stdout=%s stderr=%s", code, &stdout, &stderr)
			}
			var output DownloadOutput
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("incomplete stdout is not JSON: %v\n%s", err, &stdout)
			}
			if output.Manifest.Status != "incomplete" || len(output.Manifest.PDFGuides) != 0 ||
				len(output.Manifest.HTMLGuides) != 0 || len(output.Manifest.Attempts) != 1 ||
				output.Manifest.Attempts[0].Status != "failed" ||
				output.Manifest.Attempts[0].Format != "html" || guessedPDF.Load() != 0 {
				t.Fatalf("invalid HTML source became a guessed PDF: %+v guesses=%d", output.Manifest, guessedPDF.Load())
			}
			errors := strings.Join(output.Manifest.Attempts[0].Errors, " ")
			if tc.name == "invalid-pdf" && !strings.Contains(errors, "expected complete original PDF bytes") {
				t.Fatalf("invalid PDF failure lost byte-validation detail: %s", errors)
			}
			if tc.name == "ordinary-html-failure" && strings.Contains(strings.ToLower(errors), "pdf") {
				t.Fatalf("ordinary HTML failure triggered PDF behavior: %s", errors)
			}
		})
	}
}

func TestExecutableCancellationPublishesAcceptedPDFs(t *testing.T) {
	binary(t)
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt delivery unavailable on Windows")
	}

	active := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixtureCatalog(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		if r.URL.Path == "/pdf/first.pdf" {
			w.Write(testutil.PDF("first.pdf"))
			return
		}
		if r.URL.Path == "/pdf/second.pdf" {
			active <- struct{}{}
			<-r.Context().Done()
			return
		}
		t.Errorf("unexpected request: %s", r.URL)
	}))
	defer server.Close()
	base := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary(t), "--transport", "compatible", "--platform", "6300", "--version", "10.10",
		"--all", "--destination", base, "--raw-cache", t.TempDir(), "--delay", "0", "--retries", "0",
		"--portal-url", server.URL+"/portal/aoscx.html")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-active:
	case <-ctx.Done():
		command.Wait()
		t.Fatal("second PDF did not start")
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		command.Process.Kill()
		command.Wait()
		t.Fatal(err)
	}
	err := command.Wait()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 130 {
		t.Fatalf("cancellation exit=%v stderr=%s", err, &stderr)
	}
	data, err := os.ReadFile(filepath.Join(base, "6300", "10.10", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest library.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "incomplete" || len(manifest.PDFGuides) != 1 || manifest.PDFGuides["first"].Status != "complete" {
		t.Fatalf("cancelled batch publication invalid: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(base, "6300", ".10.10.lock")); !os.IsNotExist(err) {
		t.Fatal("lock not released")
	}
	retained, _ := filepath.Glob(filepath.Join(base, "6300", ".incomplete", "*", "unfinished", "second", "attempt.json"))
	if len(retained) != 1 {
		t.Fatal("cancelled guide diagnostics not retained")
	}
}

func TestAttemptBudgetExhaustionPublishesAcceptedGuideAndReturnsNonzero(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fixtureCatalog(w, r) {
			return
		}
		if r.URL.Path == "/pdf/first.pdf" {
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(testutil.PDF("first.pdf"))
			return
		}
		if r.URL.Path == "/pdf/second.pdf" && r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "application/pdf")
			return
		}
		t.Fatalf("budget allowed unexpected request: %s", r.URL)
	}))
	defer server.Close()
	budget, err := fetch.NewAttemptBudget(8, 1)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	args := []string{"--platform", "6300", "--version", "10.10", "--guides", "first", "second",
		"--destination", base, "--raw-cache", t.TempDir(), "--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--json"}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr,
		func(_ string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
			config.Attempts = budget
			return fetch.New(config, notify)
		})
	if code != 2 {
		t.Fatalf("budget exhaustion returned code %d: stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.Manifest.Status != "incomplete" || len(output.Manifest.PDFGuides) != 1 ||
		output.Manifest.PDFGuides["first"].Status != "complete" ||
		!strings.Contains(strings.Join(output.Manifest.Errors, " "), "attempt budget exhausted") ||
		hits.Load() != 8 || budget.Stats().AttemptedTransmissions != 8 ||
		budget.Stats().ObservedHeaderWrites != 8 {
		t.Fatalf("budget exhaustion publication invalid: hits=%d stats=%+v output=%+v", hits.Load(), budget.Stats(), output)
	}
}

func TestIncompleteResultWithoutReturnedErrorPrintsBoundedReasons(t *testing.T) {
	var stderr bytes.Buffer
	presentation := newHumanPresentationWith(&stderr, true)
	progress := newProgressReporterWithTerminal(&stderr, progressTerminal{
		enabled: true,
		width:   func() int { return 80 },
	}, presentation)
	progress.HTML(1, 1, "Guide", archive.Progress{
		TopicsValidated: 105, PlannedTopics: 105,
		AssetsDiscovered: 4, AssetsCompleted: 4,
	})
	result := model.ArchiveResult{
		Status: "incomplete",
		Notices: []model.Notice{
			{Kind: model.NoticeExternalLinkRetained, Message: "External navigation retained, not downloaded: https://publisher.example.test/support"},
			{Kind: model.NoticeCSSCleanup, Message: "Removed 12 publisher navigation/runtime or provably unused CSS rules"},
		},
		Errors: []string{
			"missing anchor one", "missing anchor two", "missing anchor three",
			"error four", "error five", "error six", "error seven",
		},
		Warnings: []string{
			"publisher omitted anchor one", "publisher omitted anchor two",
			"publisher omitted anchor three", "publisher warning four",
		},
	}
	reportGuideDiagnostics(progress, &stderr, presentation, "acl", result, nil)
	progress.GuideResult(1, 1, "Guide", result.Status, 105, 4, 0)
	manifest := library.Manifest{
		Status: "incomplete",
		Errors: []string{
			"Guide: missing anchor one",
			"Unfinished work retained at /owned/.incomplete/attempt",
		},
	}
	reportIncompleteManifest(progress, &stderr, presentation, "/owned/8320/10.18.xxxx", manifest)
	progress.Finish("incomplete", "Published incomplete library")

	text := stderr.String()
	plain := ansi.Strip(text)
	for _, expected := range []string{
		"Guide acl archive error: missing anchor one",
		"Guide acl archive error: error five",
		"Guide acl: 2 more archive errors",
		"Guide acl: 2 publisher notices (1 external link retained; 1 CSS cleanup); see published manifest.",
		"Guide acl publisher/source warning: publisher omitted anchor one",
		"Guide acl: 1 more publisher/source warnings",
		"Full diagnostics: /owned/8320/10.18.xxxx/manifest.json",
		"Unfinished work retained at /owned/.incomplete/attempt",
		"Published incomplete library",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("incomplete diagnostics omitted %q:\n%s", expected, plain)
		}
	}
	if strings.Contains(plain, "Guide acl archive error: error six") ||
		strings.Contains(plain, "publisher/source warning: publisher warning four") ||
		strings.Contains(plain, "publisher.example.test/support") ||
		strings.Contains(plain, "Removed 12 publisher") {
		t.Fatalf("bounded diagnostics printed overflow details:\n%s", plain)
	}
	if !strings.Contains(text, presentation.failure("Guide acl archive error: missing anchor one")) ||
		!strings.Contains(text, presentation.notice("Guide acl: 2 publisher notices (1 external link retained; 1 CSS cleanup); see published manifest.")) ||
		!strings.Contains(text, presentation.warning("Guide acl publisher/source warning: publisher omitted anchor one")) ||
		!strings.Contains(text, presentation.warning("Published incomplete library (elapsed 0s)")) {
		t.Fatalf("incomplete errors/warnings/final status did not use semantic colors:\n%q", text)
	}
	if strings.Contains(plain, "cache downloaded") {
		t.Fatal("per-event diagnostics repeated detailed cache counters")
	}
}

func TestCompleteGuidePrintsOneTypedPublisherNoticeSummary(t *testing.T) {
	notices := make([]model.Notice, 0, 28)
	for i := 0; i < 2; i++ {
		notices = append(notices, model.Notice{
			Kind:    model.NoticeExternalLinkRetained,
			Message: fmt.Sprintf("External navigation retained, not downloaded: https://external.example.test/%d", i),
		})
	}
	for i := 0; i < 12; i++ {
		notices = append(notices, model.Notice{
			Kind:    model.NoticeNavigationStyleOmitted,
			Message: fmt.Sprintf("Publisher navigation/toolbar skin stylesheet omitted: skin-%d.css", i),
		})
	}
	for i := 0; i < 14; i++ {
		notices = append(notices, model.Notice{
			Kind:    model.NoticeCSSCleanup,
			Message: fmt.Sprintf("Removed %d publisher CSS rules", i+1),
		})
	}

	var stderr bytes.Buffer
	presentation := newHumanPresentationWith(&stderr, true)
	progress := newProgressReporterWithTerminal(&stderr, progressTerminal{
		enabled: true,
		width:   func() int { return 96 },
	}, presentation)
	progress.HTML(1, 1, "Access Control Lists Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion,
		Stage:         archive.ProgressBuilding,
		PagesEmitted:  8,
		PagesTotal:    28,
	})
	reportGuideDiagnostics(progress, &stderr, presentation, "acls", model.ArchiveResult{
		Status:  "complete",
		Notices: notices,
		Warnings: []string{
			"Publisher advertised out-of-guide table stylesheet broken.css returned HTTP 404; used unique source-verified same-guide stylesheet replacement.css for TableStyle-Parameter-Description.",
		},
		StylesheetRecoveries: []model.StylesheetRecovery{{
			BrokenURL: "https://publisher.example.test/broken.css", HTTPStatus: 404,
			ReplacementURL:          "https://publisher.example.test/guide/replacement.css",
			ReplacementFinalURL:     "https://publisher.example.test/guide/replacement.css",
			ReplacementSourceSHA256: strings.Repeat("a", 64), ReplacementSourceSize: 128,
			TableStyleFamilies: []string{"TableStyle-Parameter-Description"},
			AffectedTopics:     []string{"https://publisher.example.test/guide/topic.htm"},
		}},
	}, nil)

	want := "Guide acls: 28 publisher notices (2 external links retained; 12 navigation styles omitted; 14 CSS cleanup); see published manifest."
	text := stderr.String()
	plain := ansi.Strip(text)
	if strings.Count(plain, want) != 1 || !strings.Contains(text, presentation.notice(want)) {
		t.Fatalf("typed notice summary missing or duplicated:\n%q", text)
	}
	for _, hidden := range []string{"external.example.test", "skin-0.css", "Removed 1 publisher CSS rules"} {
		if strings.Contains(plain, hidden) {
			t.Fatalf("individual publisher notice %q leaked to console:\n%s", hidden, plain)
		}
	}
	if strings.Contains(plain, "publisher/source warning") {
		if strings.Count(plain, "Publisher advertised out-of-guide table stylesheet") != 1 {
			t.Fatalf("typed recovery warning was hidden or duplicated:\n%s", plain)
		}
	} else {
		t.Fatalf("actionable recovery warning was not printed separately:\n%s", plain)
	}
	structured := model.ArchiveResult{
		Status: "complete", Notices: notices,
		StylesheetRecoveries: []model.StylesheetRecovery{{
			BrokenURL: "broken", HTTPStatus: 404, ReplacementURL: "replacement",
		}},
	}
	encoded, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.ArchiveResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded.Notices, notices) {
		t.Fatalf("structured manifest lost ACL-style notice detail: got=%+v want=%+v", decoded.Notices, notices)
	}
	if !reflect.DeepEqual(decoded.StylesheetRecoveries, structured.StylesheetRecoveries) {
		t.Fatalf("structured manifest lost stylesheet recovery detail: got=%+v want=%+v",
			decoded.StylesheetRecoveries, structured.StylesheetRecoveries)
	}
}

func TestPublisherNoticeSummariesArePlainAndPerGuide(t *testing.T) {
	var stderr bytes.Buffer
	presentation := newHumanPresentationWith(&stderr, false)
	for _, guideID := range []string{"first", "second"} {
		reportGuideDiagnostics(nil, &stderr, presentation, guideID, model.ArchiveResult{
			Status: "complete",
			Notices: []model.Notice{{
				Kind:    model.NoticeSourcePolicy,
				Message: "publisher-controlled detail \x1b[31m must stay in the manifest",
			}},
		}, nil)
	}
	text := stderr.String()
	if strings.Contains(text, "\x1b") ||
		strings.Count(text, "publisher notice (1 source policy)") != 2 ||
		strings.Contains(text, "publisher-controlled detail") {
		t.Fatalf("redirected per-guide notice summaries were noisy or unsafe:\n%q", text)
	}
}

func TestDegradedHTMLPublishesImagePlaceholderThroughCLI(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>8320</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="acl" onclick="openFile('HTML','acl','aoscx')">ACL Guide</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/acl.json":
			json.NewEncoder(w).Encode(map[string]any{
				"10.18.xxxx": map[string]string{"8320": server.URL + "/book/index.html"},
			})
		case "/book/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body><nav class="wh_publication_toc"><ul>
<li><a href="topic.html">Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>Home</h1></main></body></html>`)
		case "/book/topic.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<article><h1>Topic</h1><img src="missing.png"></article>`)
		case "/book/missing.png":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected incomplete fixture request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "8320", "--version", "10.18.xxxx",
		"--guides", "acl", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "cache"),
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--workers", "1", "--json",
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, newNativeClient)
	if code != 2 {
		t.Fatalf("incomplete HTML exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var result DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("incomplete JSON was invalid: %v\n%s", err, &stdout)
	}
	if result.Manifest.Status != "degraded" || len(result.Manifest.HTMLGuides) != 1 {
		t.Fatalf("degraded archive status changed: %+v", result.Manifest)
	}
	if len(result.Manifest.Attempts) != 1 ||
		result.Manifest.Attempts[0].Status != "degraded" ||
		len(result.Manifest.Attempts[0].MissingResources) != 1 ||
		!slices.ContainsFunc(result.Manifest.Attempts[0].Warnings, func(message string) bool {
			return strings.Contains(message, "offline placeholder") &&
				strings.Contains(message, "/book/missing.png")
		}) {
		t.Fatalf("full manifest omitted structured retrieval diagnostics: %+v", result.Manifest.Attempts)
	}
	text := stderr.String()
	for _, expected := range []string{
		"Guide acl publisher/source warning:",
		"missing.png",
		"Full diagnostics: " + filepath.Join(result.Library, "manifest.json"),
		"Published degraded library",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("incomplete CLI diagnostics omitted %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "\x1b[") {
		t.Fatal("redirected incomplete diagnostics contained terminal controls")
	}
}

func TestNoninteractiveAllPublishesAvailableSubsetWithSkippedMetadata(t *testing.T) {
	server := promptUnavailableFixtureServer(t)
	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "6000", "--version", "10.17",
		"--all", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "raw-v2"),
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--workers", "1", "--json", "--zip",
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, newNativeClient)
	if code != 0 {
		t.Fatalf("all-available exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.Manifest.Status != "complete" || len(output.Manifest.Guides) != 1 ||
		output.Manifest.Guides[0].ID != "other" ||
		len(output.Manifest.SkippedUnavailable) != 1 ||
		output.Manifest.SkippedUnavailable[0].ID != "webUI" ||
		!output.Manifest.SkippedUnavailable[0].Checked ||
		output.Manifest.SkippedUnavailable[0].Available ||
		len(output.Manifest.Attempts) != 1 || output.Manifest.Attempts[0].ID != "other" {
		t.Fatalf("all-available manifest=%+v", output.Manifest)
	}
	if !strings.Contains(stderr.String(), "Downloading 1 available mapped guides; skipped 1 unavailable") {
		t.Fatalf("all-available summary missing: %s", &stderr)
	}
	reader, err := zip.OpenReader(output.ZIP)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, file := range reader.File {
		if strings.Contains(file.Name, "/webUI/") {
			t.Fatalf("skipped guide appeared in ZIP: %s", file.Name)
		}
	}
	var history []library.History
	body, err := os.ReadFile(filepath.Join(output.Library, "history.json"))
	if err != nil || json.Unmarshal(body, &history) != nil ||
		len(history) != 1 || len(history[0].SkippedUnavailable) != 1 ||
		history[0].SkippedUnavailable[0].ID != "webUI" {
		t.Fatalf("skipped history missing: history=%+v err=%v", history, err)
	}
}

func TestAuthoritativeFlareTopic404FailsGuideAndRetainsPreviousComplete(t *testing.T) {
	var server *httptest.Server
	var missing atomic.Bool
	var topicRequests atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="job" onclick="openFile('HTML','job','aoscx')">Job Scheduler Guide</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/job.json":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"10.16": map[string]string{"6300": server.URL + "/guide/Content/home.htm"},
			})
		case "/guide/Content/home.htm":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html data-mc-path-to-help-system="../"><body>
<ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul>
<main id="mc-main-content"><h1>Home</h1><p>The required topic is declared only by the authoritative TOC.</p></main>
</body></html>`)
		case "/guide/Data/Tocs/Guide.js":
			w.Header().Set("Content-Type", "application/javascript")
			io.WriteString(w, `define({numchunks:1,prefix:'Guide_Chunk',tree:{n:[{i:0,c:0},{i:1,c:0}]}});`)
		case "/guide/Data/Tocs/Guide_Chunk0.js":
			w.Header().Set("Content-Type", "application/javascript")
			io.WriteString(w, `define({
'/Content/home.htm':{i:[0],t:['Home'],b:['']},
'/Content/required.htm':{i:[1],t:['Required topic'],b:['']}
});`)
		case "/guide/Content/required.htm":
			topicRequests.Add(1)
			if missing.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<main id="mc-main-content"><h1>Required topic</h1><p>Required source content.</p></main>`)
		default:
			t.Errorf("unexpected request (no alternate source is permitted): %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "6300", "--version", "10.16",
		"--guides", "job", "--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "cache"),
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--workers", "1", "--json",
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, newNativeClient); code != 0 {
		t.Fatalf("initial complete archive exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var initial DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	initialGuide := initial.Manifest.HTMLGuides["job"]
	requiredIndex := slices.IndexFunc(initialGuide.HTML.Topics, func(topic model.FileRecord) bool {
		return topic.URL == server.URL+"/guide/Content/required.htm"
	})
	if initial.Manifest.Status != "complete" || initialGuide.Status != "complete" || requiredIndex < 0 {
		t.Fatalf("initial authoritative guide was not complete: %+v", initial.Manifest)
	}
	requiredSHA := initialGuide.HTML.Topics[requiredIndex].SHA256

	missing.Store(true)
	stdout.Reset()
	stderr.Reset()
	refreshArgs := append(append([]string{}, args...), "--refresh")
	code := run(context.Background(), refreshArgs, &stdout, &stderr, newNativeClient)
	if code != 2 {
		t.Fatalf("required topic 404 exit=%d, want 2; stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var failed DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &failed); err != nil {
		t.Fatalf("failed-run JSON was invalid: %v\n%s", err, &stdout)
	}
	if failed.Manifest.Status != "incomplete" || len(failed.Manifest.Attempts) != 1 {
		t.Fatalf("failed guide did not publish an incomplete version manifest: %+v", failed.Manifest)
	}
	attempt := failed.Manifest.Attempts[0]
	exactTopic := server.URL + "/guide/Content/required.htm"
	hasExact404 := func(messages []string) bool {
		return slices.ContainsFunc(messages, func(message string) bool {
			return strings.Contains(message, "HTTP 404") &&
				strings.Contains(message, exactTopic) &&
				strings.Contains(message, "stage=headers")
		})
	}
	if attempt.ID != "job" || attempt.Status != "failed" || attempt.Format != "html" ||
		!attempt.RetainedPrevious || !hasExact404(attempt.Errors) {
		t.Fatalf("failed attempt omitted exact 404 diagnostics or retention: %+v", attempt)
	}
	attemptPaths, err := filepath.Glob(filepath.Join(root, "library", "6300", ".incomplete", "*", "unfinished", "job", "attempt.json"))
	if err != nil || len(attemptPaths) != 1 {
		t.Fatalf("retained failed guide result not found: paths=%v err=%v", attemptPaths, err)
	}
	var archiveResult model.ArchiveResult
	attemptBytes, err := os.ReadFile(attemptPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(attemptBytes, &archiveResult); err != nil {
		t.Fatal(err)
	}
	if archiveResult.Status != "failed" || archiveResult.Format != "html" ||
		archiveResult.Document.ID != "job" || archiveResult.HTML != nil ||
		!hasExact404(archiveResult.Errors) {
		t.Fatalf("retained guide result changed failure semantics: %+v", archiveResult)
	}
	retained := failed.Manifest.HTMLGuides["job"]
	retainedIndex := slices.IndexFunc(retained.HTML.Topics, func(topic model.FileRecord) bool {
		return topic.URL == exactTopic
	})
	if retained.Status != "complete" || retainedIndex < 0 ||
		retained.HTML.Topics[retainedIndex].SHA256 != requiredSHA {
		t.Fatalf("required topic failure replaced the prior complete guide: %+v", retained)
	}
	if topicRequests.Load() != 2 {
		t.Fatalf("exact authoritative topic request count=%d, want initial plus one failed refresh", topicRequests.Load())
	}
}
