package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/pdfgen"
)

func generatedPDFServer(t *testing.T) *httptest.Server {
	return generatedPDFServerWithImageStatus(t, http.StatusOK)
}

func generatedPDFServerWithImageStatus(t *testing.T, imageStatus int) *httptest.Server {
	t.Helper()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			io.WriteString(w, "User-agent: *\nAllow: /\n")
		case "/portal/aoscx.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<select id="platform"><option>6300</option></select><select id="ver"><option>10.16</option></select>
<div id="menu1"><table><tr><td id="job" onclick="openFile('HTML','job','aoscx')">Job Scheduler Guide</td></tr></table></div>`)
		case "/portal/json/aoscx/job.json":
			io.WriteString(w, `{"10.16":{"6300":"`+server.URL+`/guide/Content/home.htm"}}`)
		case "/guide/Content/home.htm":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html data-mc-path-to-help-system="../"><head></head><body>
<ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul><main id="mc-main-content"><h1 id="home">Home</h1>
<p>Offline generated PDF fixture.</p><a href="second.htm#second">Second</a><img src="../Resources/icon.png"></main></body></html>`)
		case "/guide/Data/Tocs/Guide.js":
			io.WriteString(w, `define({numchunks:1,prefix:'Chunk',tree:{n:[{i:0,c:0},{i:1,c:0}]}});`)
		case "/guide/Data/Tocs/Chunk0.js":
			io.WriteString(w, `define({'/Content/home.htm':{i:[0],t:['Home'],b:['']},'/Content/second.htm':{i:[1],t:['Second'],b:['second']}});`)
		case "/guide/Content/second.htm":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<main id="mc-main-content"><h1 id="second">Second</h1><pre>show  exact
  spacing</pre><table><tr><th>State</th><td>ready</td></tr></table></main>`)
		case "/guide/Resources/icon.png":
			if imageStatus != http.StatusOK {
				w.WriteHeader(imageStatus)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Write(png)
		default:
			t.Errorf("unexpected generated PDF fixture request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return server
}

func generatedPDFArgs(t *testing.T, server *httptest.Server, base, chrome string) []string {
	t.Helper()
	return []string{
		"--transport", "http", "--platform", "6300", "--version", "10.16",
		"--guides", "job", "--destination", base, "--raw-cache", filepath.Join(t.TempDir(), "cache"),
		"--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0", "--retries", "0",
		"--workers", "1", "--json", "--convert-html-to-pdf", "--chrome-path", chrome,
	}
}

func TestGeneratedPDFFailurePublishesCompleteHTMLWithDurableDiagnostics(t *testing.T) {
	server := generatedPDFServer(t)
	defer server.Close()
	base := filepath.Join(t.TempDir(), "library")
	args := generatedPDFArgs(t, server, base, filepath.Join(t.TempDir(), "missing-chrome"))
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, newNativeClient); code != 2 {
		t.Fatalf("generated PDF failure exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	if !strings.Contains(stderr.String(), "HTML complete; companion PDF failed") ||
		strings.Contains(stderr.String(), "Guide job failed") {
		t.Fatalf("generated PDF diagnostics relabelled complete HTML: %s", &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	guide := output.Manifest.HTMLGuides["job"]
	if output.Manifest.Status != "incomplete" || guide.Status != "complete" ||
		guide.HTML == nil || guide.HTML.Status != "complete" ||
		guide.HTML.GeneratedPDF == nil || guide.HTML.GeneratedPDF.Status != "failed" ||
		len(output.Manifest.Attempts) != 1 || output.Manifest.Attempts[0].Status != "incomplete" {
		t.Fatalf("generated PDF failure changed HTML completeness or lost diagnostics: %+v", output.Manifest)
	}
	index, err := os.ReadFile(filepath.Join(output.Library, "job", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(index, []byte("archive-generated-pdf")) {
		t.Fatal("failed generated PDF was linked from the guide index")
	}
	if _, err := os.Stat(filepath.Join(output.Library, "job", guide.HTML.GeneratedPDF.Path)); !os.IsNotExist(err) {
		t.Fatalf("failed generated PDF left a published artifact: %v", err)
	}
}

func assertGeneratedPDFPublished(t *testing.T, output DownloadOutput) {
	t.Helper()
	guide := output.Manifest.HTMLGuides["job"]
	generated := guide.HTML.GeneratedPDF
	if output.Manifest.Status != "complete" || guide.Status != "complete" ||
		generated == nil || generated.Status != "complete" ||
		generated.RendererVersion != pdfgen.ChromeVersion ||
		generated.ExecutableSHA256 != pdfgen.ChromeExecutableSHA256 ||
		generated.Validation == nil || !generated.Validation.StructuralValidated {
		t.Fatalf("generated PDF publication is incomplete: %+v", output.Manifest)
	}
	body, err := os.ReadFile(filepath.Join(output.Library, "job", generated.Path))
	if err != nil || len(body) != int(generated.Size) {
		t.Fatalf("generated PDF artifact mismatch: size=%d err=%v", len(body), err)
	}
	index, _ := os.ReadFile(filepath.Join(output.Library, "index.html"))
	guideIndex, _ := os.ReadFile(filepath.Join(output.Library, "job", "index.html"))
	if !bytes.Contains(index, []byte("generated PDF")) ||
		!bytes.Contains(guideIndex, []byte("archive-generated-pdf")) {
		t.Fatal("generated PDF links are missing from version or guide index")
	}
	if output.ZIP != "" {
		reader, err := zip.OpenReader(output.ZIP)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		member := filepath.ToSlash(filepath.Join("10.16", "job", generated.Path))
		if !containsZIPMember(reader.File, member) {
			t.Fatalf("generated PDF is missing from ZIP: %s", member)
		}
	}
	search, _ := os.ReadFile(filepath.Join(output.Library, "search-index.js"))
	if !strings.Contains(string(search), "Offline generated PDF fixture") {
		t.Fatal("HTML full-text search was replaced by PDF title-only semantics")
	}
}

func containsZIPMember(files []*zip.File, suffix string) bool {
	for _, file := range files {
		if strings.HasSuffix(file.Name, suffix) {
			return true
		}
	}
	return false
}

func TestGeneratedPDFFlagsAreMutuallyExclusive(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--convert-html-to-pdf", "--no-convert-html-to-pdf",
	}, &stdout, &stderr, func(string, fetch.Config, func(string)) (*fetch.Client, error) {
		t.Fatal("mutually exclusive flags reached transport construction")
		return nil, nil
	})
	if code != 1 || !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("invalid generated PDF flags were accepted: code=%d stderr=%s", code, &stderr)
	}
}
