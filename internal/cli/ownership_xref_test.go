package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"aos-cx-docs-dldr/internal/testutil"
)

func TestExecutableCompactXRefPublisherPDF(t *testing.T) {
	binary(t)
	body := testutil.XRefStreamPDF("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixtureCatalog(w, r) {
			return
		}
		if r.URL.Path != "/pdf/first.pdf" {
			t.Errorf("unexpected fixture request: %s", r.URL)
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(body)
	}))
	defer server.Close()
	code, output, stderr := execute(t, []string{"--transport", "compatible", "--platform", "6300", "--release", "10.10",
		"--guides", "first", "--destination", t.TempDir(), "--raw-cache", t.TempDir(), "--delay", "0",
		"--retries", "0", "--portal-url", server.URL + "/portal/aoscx.html", "--json"})
	if code != 0 {
		t.Fatalf("compact xref executable archive failed: %d %s", code, stderr)
	}
	var result DownloadOutput
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(result.Library, "first", result.Manifest.PDFGuides["first"].PDF.Path))
	if err != nil || !bytes.Equal(saved, body) || result.Manifest.Status != "complete" {
		t.Fatalf("compact PDF was not preserved: %v", err)
	}
}

func TestExecutableRefusesOwnershipLossDuringDownload(t *testing.T) {
	binary(t)
	base := t.TempDir()
	lockPath := filepath.Join(base, "6300", ".10.10.lock")
	journalPath := filepath.Join(base, "6300", ".10.10.transaction.json")
	foreignLock := []byte(`{"pid":987,"host":"fixture","token":"peer"}`)
	foreignJournal := []byte(`{"owner":"peer","keep":true}`)
	var replace atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixtureCatalog(w, r) {
			return
		}
		if r.URL.Path != "/pdf/first.pdf" {
			t.Errorf("unexpected fixture request: %s", r.URL)
			w.WriteHeader(404)
			return
		}
		if replace.Load() {
			if err := os.WriteFile(lockPath, foreignLock, 0o600); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(journalPath, foreignJournal, 0o600); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(testutil.PDF("owned source"))
	}))
	defer server.Close()
	args := []string{"--transport", "compatible", "--platform", "6300", "--release", "10.10", "--guides", "first",
		"--destination", base, "--raw-cache", t.TempDir(), "--portal-url", server.URL + "/portal/aoscx.html",
		"--delay", "0", "--retries", "0", "--json"}
	code, output, stderr := execute(t, args)
	if code != 0 {
		t.Fatalf("initial library failed: %d %s", code, stderr)
	}
	var initial DownloadOutput
	if err := json.Unmarshal(output, &initial); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(initial.Library, "manifest.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	replace.Store(true)
	code, _, stderr = execute(t, append(args, "--refresh"))
	if code != 1 || !strings.Contains(stderr, "ownership") {
		t.Fatalf("ownership-lost publication was not refused: %d %s", code, stderr)
	}
	after, _ := os.ReadFile(manifestPath)
	lock, _ := os.ReadFile(lockPath)
	journal, _ := os.ReadFile(journalPath)
	if !bytes.Equal(before, after) || !bytes.Equal(lock, foreignLock) || !bytes.Equal(journal, foreignJournal) {
		t.Fatal("old library or foreign transaction files were changed")
	}
	staged, _ := filepath.Glob(filepath.Join(base, "6300", ".staging", "*", "first", "*.pdf"))
	if len(staged) != 1 {
		t.Fatal("unpublished PDF staging was not retained")
	}
}
