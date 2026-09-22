package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"aos-cx-docs-dldr/internal/cache"
)

func TestRetainedCatalogueArchive(t *testing.T) {
	cataloguePath := os.Getenv("AOSCX_RETAINED_CATALOGUE")
	cachePath := os.Getenv("AOSCX_RETAINED_CACHE")
	destination := os.Getenv("AOSCX_RETAINED_DESTINATION")
	platform := os.Getenv("AOSCX_RETAINED_PLATFORM")
	version := os.Getenv("AOSCX_RETAINED_VERSION")
	guideID := os.Getenv("AOSCX_RETAINED_GUIDE")
	resultPath := os.Getenv("AOSCX_RETAINED_RESULT")
	if cataloguePath == "" || cachePath == "" || destination == "" ||
		platform == "" || version == "" || guideID == "" || resultPath == "" {
		t.Skip("retained archive requires explicit catalogue/cache, selection, destination and result paths")
	}
	body, err := os.ReadFile(cataloguePath)
	if err != nil {
		t.Fatal(err)
	}
	var listing Listing
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	if !listing.Complete || listing.Catalog.FetchedAt == "" {
		t.Fatal("retained catalogue must include complete mappings and its original retrieval date")
	}
	file, err := os.OpenFile(resultPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	store, err := cache.Open(cachePath, replayOfflineTransport{}, 32<<20, func(message string) {
		t.Log(message)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	options := options{platform: platform, version: version, guides: []string{guideID},
		destination: destination, maxMB: 32, maxArchiveMB: 256, json: true}
	var out, stderr bytes.Buffer
	code, err := downloadGuides(context.Background(), options, listing.Catalog, store, &out, &stderr)
	if _, writeErr := file.Write(out.Bytes()); writeErr != nil {
		t.Fatal(writeErr)
	}
	if syncErr := file.Sync(); syncErr != nil {
		t.Fatal(syncErr)
	}
	if err != nil || code != 0 {
		t.Fatalf("retained archive failed: code=%d err=%v stderr=%s", code, err, &stderr)
	}
	var result DownloadOutput
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	guide, found := result.Manifest.HTMLGuides[guideID]
	if result.Manifest.Status != "complete" || !found || guide.Status != "complete" || guide.HTML == nil {
		t.Fatal("retained archive did not publish a complete HTML guide")
	}
	t.Logf("NETWORK DISABLED: catalogue retrieved %s; library=%s, topics=%d, assets=%d",
		listing.Catalog.FetchedAt, result.Library, len(guide.HTML.Topics), len(guide.HTML.Assets))
}
