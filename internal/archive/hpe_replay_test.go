package archive

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
)

type overlayFetcher struct {
	fixtures *archiveFixture
	fallback model.Fetcher
	calls    *[]string
}

func (f overlayFetcher) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, raw)
	}
	if _, ok := f.fixtures.resources[raw]; ok {
		return f.fixtures.Get(ctx, raw, refresh)
	}
	return f.fallback.Get(ctx, raw, refresh)
}

func TestM3DRetainedHPEInventoryReplayWithTopicFixtures(t *testing.T) {
	cachePath := os.Getenv("AOSCX_M3D_HPE_REPLAY_CACHE")
	if cachePath == "" {
		t.Skip("set AOSCX_M3D_HPE_REPLAY_CACHE to replay retained HPE inventory inputs")
	}
	store, err := cache.Open(cachePath, offlineTransport{}, 16<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	document := model.Document{ID: "jobscheduler", Title: "Job Scheduler Guide", Platform: "6300",
		Version: "10.18.xxxx", Kind: "hpe",
		URL: "https://support.hpe.com/hpesc/public/docDisplay?mask=sh-rs&docId=sd00007885en_us"}
	plan, err := source.LoadPlan(context.Background(), store, document)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := &archiveFixture{resources: map[string]model.Resource{}, calls: map[string]int{}}
	for index, topic := range plan.Topics {
		if index == 0 {
			continue
		}
		parsed, err := url.Parse(topic.FetchURL)
		if err != nil {
			t.Fatal(err)
		}
		page := parsed.Query().Get("page")
		body := []byte(`<main role="main" class="ditasrc"><article><h1 id="topic">` + topic.Title +
			`</h1><table><tr><th>Parameter</th><td>Value</td></tr></table><pre>switch# show job
  exact spacing</pre></article></main>`)
		fixtures.resources[topic.FetchURL] = model.Resource{URL: topic.FetchURL, Status: 200,
			Headers: http.Header{"Content-Type": {"multiPage;charset=UTF-8"}, "Doc-Id": {"sd00007885en_us"}, "Doc-Page-Name": {page}},
			Body:    io.NopCloser(bytes.NewReader(body))}
	}
	fixtures.resources[hpeGraphikRegular] = fixtureResource(hpeGraphikRegular, "font/woff2", []byte("wOF2regular"))
	fixtures.resources[hpeGraphikBold] = fixtureResource(hpeGraphikBold, "font/woff2", []byte("wOF2bold"))
	logo := "https://support.hpe.com/hpesc/public/api/document/sd00007885en_us/GUID-71F08EEB-4AAF-413C-A50F-397F0381CAA2-high.png?v=1"
	fixtures.resources[logo] = fixtureResource(logo, "image/png", renderPNG)
	fixtures.resources["https://support.hpe.com/resource3/doc-resources/css/hpesc-doc.css"] =
		fixtureResource("https://support.hpe.com/resource3/doc-resources/css/hpesc-doc.css", "text/css",
			[]byte(`main.ditasrc{font-family:"HPE Graphik"}table{border-collapse:collapse}`))
	var calls []string
	result, err := HTML(context.Background(), plan, overlayFetcher{fixtures: fixtures, fallback: store, calls: &calls},
		t.TempDir(), false, 16<<20, 64<<20)
	if err != nil || result.Status != "complete" || len(result.HTML.Topics) != 25 ||
		len(result.HTML.Assets) != 4 || result.HTML.Inventory.UniqueTopics != 25 {
		t.Fatalf("retained HPE inventory replay failed: %+v %v calls=%v", result, err, calls)
	}
	t.Logf("retained HPE plan + fixture topics: topics=%d assets=%d links=%d",
		len(result.HTML.Topics), len(result.HTML.Assets), result.HTML.Integrity.CheckedLinks)
}
