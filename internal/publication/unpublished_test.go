package publication

import (
	"context"
	"net/http"
	"testing"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/model"
)

// unpublishedFixture answers one URL with HTTP 404, as the publisher answers
// a topic it never deployed.
type unpublishedFixture struct {
	fixture
	missing string
}

func (f unpublishedFixture) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	if raw == f.missing {
		return model.Resource{URL: raw, Status: http.StatusNotFound, Headers: http.Header{}, Body: http.NoBody}, nil
	}
	return f.fixture.Get(ctx, raw, refresh)
}

func TestAssembleLinksUnpublishedTopicsOnline(t *testing.T) {
	root := "https://docs.example.test/guide/"
	home, gone := root+"Content/home.htm", root+"Content/gone.htm"
	plan := model.DocumentPlan{
		Document: model.Document{ID: "guide", Title: "Modern Guide", Platform: "6300", Version: "10.16", URL: home, Kind: "flare"},
		Topics:   []model.Topic{{URL: home, Title: "Introduction"}, {URL: gone, Title: "Use case 3"}},
		TOC: []model.TocEntry{{Title: "Introduction", URL: home, Children: []model.TocEntry{
			{Title: "Use case 3", URL: gone},
		}}},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "flare", RootURL: root, Entries: 2, UniqueTopics: 2},
	}
	fetcher := unpublishedFixture{fixture: fixture{
		home: {"text/html", []byte(`<main id="mc-main-content"><h1>Introduction</h1><p><a href="gone.htm">Use case 3</a></p></main>`)},
	}, missing: gone}
	output := t.TempDir()
	result, err := archive.HTML(context.Background(), plan, fetcher, output, false, 1<<20, 8<<20)
	if err != nil || result.Status != "degraded" || len(result.UnpublishedTopics) != 1 {
		t.Fatalf("archive: result=%+v err=%v", result, err)
	}
	record, _ := assembleGuide(t, output, plan, result)
	entries := record.Outline.Entries
	if len(entries) != 1 || len(entries[0].Children) != 1 || entries[0].Children[0].Title != "Use case 3" ||
		entries[0].Children[0].Target.URI != gone || entries[0].Target.Name == "" {
		t.Fatalf("the outline entry of the unpublished topic does not lead to the publisher's page: %+v", record.Outline)
	}

}
