package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/model"
)

type replayOfflineTransport struct{}

func (replayOfflineTransport) Download(context.Context, model.Request, func(model.Resource) error) (model.Resource, error) {
	return model.Resource{}, errors.New("retained-input CLI replay attempted a network request")
}

// TestM3CRetainedInputCLIPipeline is an explicit uncommitted-evidence gate.
func TestM3CRetainedInputCLIPipeline(t *testing.T) {
	cachePath := os.Getenv("AOSCX_M3C_REPLAY_CACHE")
	destination := os.Getenv("AOSCX_M3C_REPLAY_DESTINATION")
	if cachePath == "" || destination == "" {
		t.Skip("set M3C replay cache and destination to run retained-input CLI evidence")
	}
	store, err := cache.Open(cachePath, replayOfflineTransport{}, 16<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.16/HTML/job_scheduler/Content/home.htm"
	catalog := model.Catalog{Platforms: []string{"6300"}, Versions: []string{"10.16"},
		SourceURL: "retained-live-catalogue", FetchedAt: "2026-09-12T18:09:59Z",
		Guides: []model.Guide{{ID: "jobscheduler", Title: "Job Scheduler Guide",
			Mappings: map[string]map[string]string{"10.16": {"6300": target}}}}}
	options := options{platform: "6300", version: "10.16", guides: []string{"jobscheduler"}, destination: destination,
		maxMB: 16, maxArchiveMB: 64, json: true}
	var out, stderr bytes.Buffer
	code, err := downloadGuides(context.Background(), options, catalog, store, &out, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("retained-input CLI pipeline failed: code=%d err=%v stderr=%s stdout=%s", code, err, &stderr, &out)
	}
	var result DownloadOutput
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	guide := result.Manifest.HTMLGuides["jobscheduler"]
	if result.Manifest.Status != "complete" || guide.HTML == nil || len(guide.HTML.Topics) != 24 || len(guide.HTML.Assets) != 6 {
		t.Fatalf("retained-input CLI result is incomplete: %+v", result.Manifest)
	}
	t.Logf("retained-input CLI library=%s topics=%d assets=%d links=%d",
		result.Library, len(guide.HTML.Topics), len(guide.HTML.Assets), guide.HTML.Integrity.CheckedLinks)
}
