package library

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/model"
)

// acceptUnpublishedHTML accepts a guide whose TOC lists the named topics,
// which the publisher answers with HTTP 404.
func acceptUnpublishedHTML(t *testing.T, r *Run, id string, unpublished ...string) model.ArchiveResult {
	t.Helper()
	home := "https://example.test/guide/Content/home.htm"
	doc := model.Document{
		ID: id, Title: id + " Guide", Platform: "6300", Version: "10.10", Kind: "flare",
		URL: home, RouteOrigin: "publisher-url",
	}
	resources := resourceFetcher{home: {
		URL: home, Status: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html"}},
		Body: io.NopCloser(strings.NewReader(`<main id="mc-main-content"><h1>Home</h1><p>Offline text</p></main>`)),
	}}
	plan := model.DocumentPlan{Document: doc, Topics: []model.Topic{{URL: home, Title: "Home"}},
		TOC: []model.TocEntry{{Title: "Home", URL: home}}}
	for _, name := range unpublished {
		url := "https://example.test/guide/Content/" + name
		resources[url] = model.Resource{URL: url, Status: http.StatusNotFound, Headers: http.Header{}, Body: http.NoBody}
		plan.Topics = append(plan.Topics, model.Topic{URL: url, Title: name})
		plan.TOC = append(plan.TOC, model.TocEntry{Title: name, URL: url})
	}
	plan.Inventory = &model.InventoryEvidence{Complete: true, Kind: "flare", RootURL: "https://example.test/guide/",
		Entries: len(plan.TOC), UniqueTopics: len(plan.Topics)}
	guide, output := guideOutput(t, r, id)
	result, err := archive.HTMLInRoot(context.Background(), plan, resources, guide, output, false, 1<<20, 2<<20, 1, nil)
	if err != nil || result.Status != "degraded" || len(result.UnpublishedTopics) != len(unpublished) {
		t.Fatalf("degraded archive failed: %+v %v", result, err)
	}
	if err := r.Accept(result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestUnpublishedTopicReplacementPrecedence(t *testing.T) {
	base := t.TempDir()
	publish := func(accept func(*Run)) Manifest {
		t.Helper()
		run, err := Open(base, "6300", "10.10")
		if err != nil {
			t.Fatal(err)
		}
		accept(run)
		manifest, err := run.Publish(false)
		if err != nil {
			t.Fatal(err)
		}
		if zipPath, err := run.ExportZIP(context.Background()); err != nil || zipPath == "" {
			t.Fatalf("ZIP export failed: path=%q err=%v", zipPath, err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		return manifest
	}

	first := publish(func(r *Run) { acceptUnpublishedHTML(t, r, "html", "a.htm", "b.htm") })
	if first.Status != "degraded" || len(first.HTMLGuides["html"].UnpublishedTopics) != 2 ||
		first.Guides[0].UnpublishedCount != 2 || first.Guides[0].TopicCount != 1 {
		t.Fatalf("first degraded publication=%+v", first)
	}
	worse := publish(func(r *Run) { acceptUnpublishedHTML(t, r, "html", "a.htm", "b.htm", "c.htm") })
	if len(worse.HTMLGuides["html"].UnpublishedTopics) != 2 || !worse.Attempts[0].RetainedPrevious ||
		len(worse.Attempts[0].UnpublishedTopics) != 3 {
		t.Fatalf("a copy missing more topics replaced the prior one=%+v", worse)
	}
	improved := publish(func(r *Run) { acceptUnpublishedHTML(t, r, "html", "a.htm") })
	if len(improved.HTMLGuides["html"].UnpublishedTopics) != 1 || improved.Attempts[0].RetainedPrevious {
		t.Fatalf("a copy missing fewer topics was not accepted=%+v", improved)
	}
	complete := publish(func(r *Run) { acceptHTML(t, r, "html") })
	if complete.Status != "complete" || complete.HTMLGuides["html"].Status != "complete" {
		t.Fatalf("complete guide did not replace degraded=%+v", complete)
	}
	retained := publish(func(r *Run) { acceptUnpublishedHTML(t, r, "html", "a.htm") })
	if retained.Status != "degraded" || retained.HTMLGuides["html"].Status != "complete" ||
		!retained.Attempts[0].RetainedPrevious {
		t.Fatalf("degraded refresh replaced complete guide=%+v", retained)
	}
}
