package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/model"
)

func TestRetainedHPENavigationPreview(t *testing.T) {
	cachePath := os.Getenv("AOSCX_HPE_PREVIEW_CACHE")
	destination := os.Getenv("AOSCX_HPE_PREVIEW_DESTINATION")
	manifestPath := os.Getenv("AOSCX_HPE_PREVIEW_MANIFEST")
	if cachePath == "" || destination == "" || manifestPath == "" {
		t.Skip("navigation preview requires an explicit copied cache, source manifest and fresh destination")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("navigation preview destination must not already exist")
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var original library.Manifest
	if err := json.Unmarshal(body, &original); err != nil {
		t.Fatal(err)
	}
	if original.Status != "complete" || len(original.HTMLGuides) != 1 {
		t.Fatal("preview requires one complete archived HTML guide")
	}
	var previous model.ArchiveResult
	for _, guide := range original.HTMLGuides {
		previous = guide
	}
	if previous.Document.Kind != "hpe" || previous.HTML == nil {
		t.Fatal("preview requires an HPE HTML guide")
	}
	store, err := cache.Open(cachePath, replayOfflineTransport{}, 16<<20, func(message string) {
		t.Log(message)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	doc := previous.Document
	catalog := model.Catalog{
		Platforms: []string{doc.Platform}, Versions: []string{doc.Version},
		SourceURL: original.CatalogueURL, FetchedAt: original.CatalogueFetchedAt,
		Guides: []model.Guide{{ID: doc.ID, Title: doc.Title,
			Mappings: map[string]map[string]string{doc.Version: {doc.Platform: doc.URL}}}},
	}
	options := options{platform: doc.Platform, version: doc.Version, guides: []string{doc.ID},
		destination: destination, maxMB: 16, maxArchiveMB: 64, json: true}
	var out, stderr bytes.Buffer
	code, err := downloadGuides(context.Background(), options, catalog, store, &out, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("offline navigation preview failed: code=%d err=%v stderr=%s", code, err, &stderr)
	}
	var result DownloadOutput
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	guide := result.Manifest.HTMLGuides[doc.ID]
	if result.Manifest.Status != "complete" || guide.HTML == nil ||
		len(guide.HTML.Topics) != len(previous.HTML.Topics) ||
		len(guide.HTML.Assets) != len(previous.HTML.Assets) {
		t.Fatal("preview changed the archived inventory")
	}
	for index, record := range guide.HTML.Topics {
		if record.URL != previous.HTML.Topics[index].URL ||
			record.SourceSHA256 != previous.HTML.Topics[index].SourceSHA256 {
			t.Fatalf("preview changed topic source identity: %s", record.URL)
		}
	}
	t.Logf("OFFLINE PREVIEW ONLY: retained selection/source date=%s, library=%s, topics=%d, assets=%d",
		catalog.FetchedAt, result.Library, len(guide.HTML.Topics), len(guide.HTML.Assets))
}
