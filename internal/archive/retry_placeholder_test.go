package archive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
)

func TestRasterRetrySuccessPreventsPlaceholder(t *testing.T) {
	var imageAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/guide/Content/home.htm":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<main id="mc-main-content"><h1>Home</h1><img src="../Resources/image.png"></main>`)
		case "/guide/Resources/image.png":
			if imageAttempts.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Write(tinyPNG)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	result := archiveWithRetryClient(t, server.URL, 1, 3*time.Second)
	if result.Status != "complete" || len(result.MissingResources) != 0 ||
		result.HTML == nil || len(result.HTML.Assets) != 1 || imageAttempts.Load() != 2 {
		t.Fatalf("successful retry degraded image: result=%+v attempts=%d", result, imageAttempts.Load())
	}
}

func TestExhaustedRasterRetriesProduceOneTypedPlaceholder(t *testing.T) {
	var imageAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/guide/Content/home.htm":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<main id="mc-main-content"><h1>Home</h1><img src="../Resources/image.png"></main>`)
		case "/guide/Resources/image.png":
			imageAttempts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	result := archiveWithRetryClient(t, server.URL, 2, 5*time.Second)
	if result.Status != "degraded" || len(result.MissingResources) != 1 ||
		imageAttempts.Load() != 3 {
		t.Fatalf("exhausted raster result=%+v attempts=%d", result, imageAttempts.Load())
	}
	missing := result.MissingResources[0]
	if missing.FailureClass != "retry-exhausted" ||
		missing.ApplicationAttempts != 3 || missing.ApplicationRetries != 2 {
		t.Fatalf("missing retry provenance=%+v", missing)
	}
}

func archiveWithRetryClient(t *testing.T, server string, retries int, timeout time.Duration) model.ArchiveResult {
	return archiveWithRetryClientForKind(t, server, retries, timeout, "flare")
}

func archiveWithRetryClientForKind(t *testing.T, server string, retries int, timeout time.Duration, kind string) model.ArchiveResult {
	t.Helper()
	client, err := fetch.New(fetch.Config{
		Timeout: timeout, AttemptTimeout: time.Second,
		Retries: retries, MaxBytes: 1 << 20,
	}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	store, err := cache.Open(t.TempDir(), client, 1<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	home := server + "/guide/Content/home.htm"
	plan := model.DocumentPlan{
		Document: model.Document{
			ID: "retry", Title: "Retry Guide", Platform: "6300", Version: "10.16",
			URL: home, Kind: kind,
		},
		Topics: []model.Topic{{URL: home, Title: "Home"}},
		TOC:    []model.TocEntry{{Title: "Home", URL: home}},
		Inventory: &model.InventoryEvidence{
			Complete: true, Kind: kind, RootURL: server + "/guide/",
			Entries: 1, UniqueTopics: 1,
		},
	}
	result, err := HTML(context.Background(), plan, store, t.TempDir(), false, 1<<20, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
