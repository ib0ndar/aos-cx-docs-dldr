package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/testutil"
)

// stallingPDFServer serves the first and second publisher PDFs. The first
// PDF's body stops after a few bytes for the first stalls GET requests, as the
// publisher's does when it stops answering for minutes.
func stallingPDFServer(t *testing.T, stalls int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixtureCatalog(w, r) {
			return
		}
		if r.URL.Path != "/pdf/first.pdf" && r.URL.Path != "/pdf/second.pdf" {
			t.Errorf("unexpected fixture request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		body := testutil.PDF(r.URL.Path)
		if r.Method == http.MethodGet && r.URL.Path == "/pdf/first.pdf" && gets.Add(1) <= stalls {
			w.WriteHeader(http.StatusOK)
			w.Write(body[:16])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server, &gets
}

func stalledGuideArgs(server *httptest.Server, root string) []string {
	return []string{
		"--transport", "http", "--platform", "6300", "--release", "10.10", "--guides", "first", "second",
		"--destination", filepath.Join(root, "library"), "--raw-cache", filepath.Join(root, "raw-v2"),
		"--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0", "--retries", "0",
		"--timeout", "5", "--attempt-timeout", "0.2", "--workers", "1", "--json",
	}
}

func shortStalledGuideWaits(t *testing.T) {
	saved := stalledGuideWaits
	stalledGuideWaits = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}
	t.Cleanup(func() { stalledGuideWaits = saved })
}

func TestStalledGuideIsTriedAgainAfterTheOtherGuides(t *testing.T) {
	shortStalledGuideWaits(t)
	server, gets := stallingPDFServer(t, 2)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), stalledGuideArgs(server, t.TempDir()), &stdout, &stderr, newNativeClient)
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	manifest := output.Manifest
	if manifest.Status != "complete" || len(manifest.PDFGuides) != 2 || len(manifest.Errors) != 0 || gets.Load() != 3 {
		t.Fatalf("stalled guide did not publish after two later tries: gets=%d manifest=%+v", gets.Load(), manifest)
	}
	order := []string{}
	for _, attempt := range manifest.Attempts {
		order = append(order, attempt.ID)
	}
	if !slices.Equal(order, []string{"second", "first"}) {
		t.Fatalf("attempts = %v, want the stalled guide once, after the other guide", order)
	}
	if !slices.ContainsFunc(manifest.Attempts[1].Warnings, func(message string) bool {
		return strings.Contains(message, "An earlier attempt failed because the publisher stopped answering") &&
			strings.Contains(message, "body_bytes=16")
	}) {
		t.Fatalf("published guide does not record its stalled attempt: %q", manifest.Attempts[1].Warnings)
	}
	if text := stderr.String(); strings.Count(text, "Guide first failed because the publisher stopped answering; "+
		"trying it again after the other guides") != 2 || strings.Contains(text, "Guide first failed:") {
		t.Fatalf("stall diagnostics:\n%s", text)
	}
}

func TestGuideThatKeepsStallingFailsOnce(t *testing.T) {
	shortStalledGuideWaits(t)
	server, gets := stallingPDFServer(t, 1<<20)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), stalledGuideArgs(server, t.TempDir()), &stdout, &stderr, newNativeClient)
	if code != 2 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	manifest := output.Manifest
	if len(manifest.PDFGuides) != 1 || len(manifest.Attempts) != 2 || manifest.Attempts[1].ID != "first" ||
		manifest.Attempts[1].Status != "failed" || gets.Load() != 3 {
		t.Fatalf("guide that kept stalling: gets=%d attempts=%+v", gets.Load(), manifest.Attempts)
	}
	if !strings.Contains(stderr.String(), "Guide first failed: retrieval failed") {
		t.Fatalf("final failure not reported:\n%s", &stderr)
	}
}

func TestCancelWhileWaitingRecordsTheStalledGuide(t *testing.T) {
	saved := stalledGuideWaits
	stalledGuideWaits = []time.Duration{time.Hour}
	t.Cleanup(func() { stalledGuideWaits = saved })
	server, _ := stallingPDFServer(t, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout bytes.Buffer
	stderr := &cancelOnMatch{match: "Waiting ", cancel: cancel}
	code := run(ctx, stalledGuideArgs(server, t.TempDir()), &stdout, stderr, newNativeClient)
	if code != 130 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr.buffer)
	}
	if !strings.Contains(stderr.buffer.String(), "Cancelled; accepted guides and prior complete guides published") {
		t.Fatalf("cancellation while waiting:\n%s", &stderr.buffer)
	}
}

type cancelOnMatch struct {
	buffer bytes.Buffer
	match  string
	cancel context.CancelFunc
}

func (w *cancelOnMatch) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) {
		w.cancel()
	}
	return w.buffer.Write(p)
}

var _ io.Writer = (*cancelOnMatch)(nil)

func TestWaitForThePublisherIsReported(t *testing.T) {
	saved, savedReport := stalledGuideWaits, stalledGuideReport
	stalledGuideWaits, stalledGuideReport = []time.Duration{350 * time.Millisecond}, 100*time.Millisecond
	t.Cleanup(func() { stalledGuideWaits, stalledGuideReport = saved, savedReport })
	server, _ := stallingPDFServer(t, 1)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), stalledGuideArgs(server, t.TempDir()), &stdout, &stderr, newNativeClient); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
	if n := strings.Count(stderr.String(), "for the publisher before trying guide first again"); n < 2 {
		t.Fatalf("wait reported %d times:\n%s", n, &stderr)
	}
}
