package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutableRetainedPublisherPDFLocalReplay(t *testing.T) {
	sourcePath := os.Getenv("AOSCX_PDF_REPLAY_SOURCE")
	if sourcePath == "" {
		t.Skip("real PDF byte replay requires an explicit retained source path")
	}
	binary(t)
	body, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(404)
		case "/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>6300</option></select><select id="ver"><option>10.10</option></select><div id="menu1"><table><tr><td id="jobscheduler" onclick="openFile('PDF','jobscheduler','aoscx')">Job Scheduler Guide</td></tr></table></div></html>`)
		case "/json/aoscx/jobscheduler.json":
			json.NewEncoder(w).Encode(map[string]any{"10.10": map[string]string{"6300": "http://" + r.Host + "/guide.pdf"}})
		case "/guide.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(body)
		default:
			t.Errorf("unexpected local replay request: %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	base := t.TempDir()
	code, output, stderr := execute(t, []string{"--transport", "compatible", "--platform", "6300", "--release", "10.10",
		"--guides", "jobscheduler", "--destination", base, "--raw-cache", t.TempDir(), "--portal-url", server.URL + "/aoscx.html",
		"--delay", "0", "--retries", "0", "--max-resource-mb", "8", "--json"})
	if code != 0 {
		t.Fatalf("local retained-PDF replay failed: %d %s", code, stderr)
	}
	var result DownloadOutput
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	pdf := result.Manifest.PDFGuides["jobscheduler"].PDF
	if pdf == nil || pdf.SHA256 != hex.EncodeToString(sum[:]) || pdf.URL != server.URL+"/guide.pdf" {
		t.Fatal("replay provenance or hash wrong")
	}
	saved, err := os.ReadFile(filepath.Join(result.Library, "jobscheduler", pdf.Path))
	if err != nil || !bytes.Equal(saved, body) {
		t.Fatal("retained publisher bytes changed in executable")
	}
	t.Logf("LOCAL FIXTURE ONLY: %d unchanged publisher PDF bytes, SHA256 %s; mapping/URLs deliberately local", len(body), pdf.SHA256)
}
