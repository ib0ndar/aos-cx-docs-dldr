package archive

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/testutil"
)

type finalURLFetcher struct {
	final string
	body  []byte
}

func (f finalURLFetcher) Get(ctx context.Context, url string, refresh bool) (model.Resource, error) {
	return model.Resource{URL: f.final, Status: http.StatusOK, Headers: http.Header{"Content-Type": {"application/pdf"}},
		Body: io.NopCloser(bytes.NewReader(f.body))}, nil
}

func TestDirectMappedPDFFinalURLKeepsProbeScope(t *testing.T) {
	doc := model.Document{ID: "cli", Title: "CLI", Platform: "6300", Version: "10.13", Kind: "pdf",
		URL: "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.13/PDF/cli_6300.pdf"}
	for _, final := range []string{
		"https://arubanetworking.hpe.com/techdocs/private/cli_6300.pdf",
		"https://arubanetworking.hpe.com/techdocs/AOS-CX/10.13/PDF/%2e%2e/%2e%2e/private/cli.pdf",
		"https://other.example/techdocs/AOS-CX/10.13/PDF/cli_6300.pdf",
	} {
		dir := t.TempDir()
		result, err := PublisherPDF(context.Background(), model.DocumentPlan{Document: doc, PDFURL: doc.URL},
			finalURLFetcher{final, testutil.PDF("cached")}, dir, false, 1<<20)
		entries, _ := os.ReadDir(dir)
		if err == nil || result.Status == "complete" || !strings.Contains(err.Error(), "outside the selected document scope") ||
			len(entries) != 0 {
			t.Fatalf("direct PDF accepted final URL %s: %+v %v %v", final, result, entries, err)
		}
	}
}

func TestDirectMappedPDFDownloadRedirectsStayInProbeScope(t *testing.T) {
	var outside atomic.Int32
	body := testutil.PDF("direct")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/AOS-CX/10.13/PDF/moved.pdf":
			http.Redirect(w, r, "/AOS-CX/10.13/PDF/current.pdf", http.StatusFound)
		case "/AOS-CX/10.13/PDF/escape.pdf":
			http.Redirect(w, r, "/private/escape.pdf", http.StatusFound)
		case "/AOS-CX/10.13/PDF/current.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(body)
		default:
			outside.Add(1)
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(body)
		}
	}))
	defer server.Close()
	client, err := fetch.New(fetch.Config{Timeout: 5 * time.Second, MaxBytes: 1 << 20, AllowLoopbackFixtures: true}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := cache.Open(t.TempDir(), client, 1<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	document := func(name string) model.Document {
		return model.Document{ID: name, Title: name, Platform: "6300", Version: "10.13", Kind: "pdf",
			URL: server.URL + "/AOS-CX/10.13/PDF/" + name + ".pdf"}
	}
	moved := document("moved")
	result, err := PublisherPDF(context.Background(), model.DocumentPlan{Document: moved, PDFURL: moved.URL},
		store, t.TempDir(), false, 1<<20)
	if err != nil || result.PDF == nil || result.PDF.FinalURL != server.URL+"/AOS-CX/10.13/PDF/current.pdf" {
		t.Fatalf("in-scope direct PDF redirect failed: %+v %v", result, err)
	}
	escape := document("escape")
	result, err = PublisherPDF(context.Background(), model.DocumentPlan{Document: escape, PDFURL: escape.URL},
		store, t.TempDir(), false, 1<<20)
	if err == nil || result.Status == "complete" || !strings.Contains(err.Error(), "outside the selected document scope") ||
		outside.Load() != 0 {
		t.Fatalf("direct PDF download left its probe scope: outside=%d %+v %v", outside.Load(), result, err)
	}
}
