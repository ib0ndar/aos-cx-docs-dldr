package archive

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
)

// prefetchingFixture prefetches through Get, as the disk cache does.
type prefetchingFixture struct{ *archiveFixture }

func (f prefetchingFixture) Prefetch(ctx context.Context, raw string, refresh bool) error {
	resource, err := f.Get(ctx, raw, refresh)
	if err != nil {
		return err
	}
	defer resource.Body.Close()
	if resource.Status != http.StatusOK {
		return &fetch.StatusError{URL: resource.URL, Status: resource.Status}
	}
	return nil
}

func TestUnpublishedTopicDegradesTheGuide(t *testing.T) {
	first := ordinaryGuideRoot + "Content/chapter/first.htm"
	unpublished := ordinaryGuideRoot + "Content/chapter/unpublished.htm"
	last := ordinaryGuideRoot + "Content/chapter/last.htm"
	page := func(title, extra string) []byte {
		return []byte(`<html><head></head><body><main><h1>` + title + `</h1><p>Text</p>` + extra + `</main></body></html>`)
	}
	archiveWith := func(t *testing.T, fetcher model.Fetcher) (model.ArchiveResult, string, error) {
		t.Helper()
		plan := ordinaryStylePlan("static", first, unpublished, last)
		plan.TOC[1].Title = "Use case 3"
		output := t.TempDir()
		result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 8<<20)
		return result, output, err
	}
	resources := func(status int) *archiveFixture {
		return &archiveFixture{resources: map[string]model.Resource{
			first:       fixtureResource(first, "text/html", page("First", `<p><a href="unpublished.htm#step">Use case 3, step 1</a></p>`)),
			unpublished: statusResource(unpublished, status),
			last:        fixtureResource(last, "text/html", page("Last", "")),
		}, calls: map[string]int{}}
	}

	for _, test := range []struct {
		name     string
		status   int
		prefetch bool
	}{
		{"not found", http.StatusNotFound, false},
		{"gone", http.StatusGone, false},
		{"not found while prefetching", http.StatusNotFound, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := resources(test.status)
			var fetcher model.Fetcher = fixture
			if test.prefetch {
				fetcher = prefetchingFixture{fixture}
			}
			result, output, err := archiveWith(t, fetcher)
			if err != nil || result.Status != "degraded" || len(result.Errors) != 0 ||
				len(result.HTML.Topics) != 2 || len(result.UnpublishedTopics) != 1 {
				t.Fatalf("unpublished topic did not degrade the guide: result=%+v err=%v", result, err)
			}
			record := result.UnpublishedTopics[0]
			if record.URL != unpublished || record.HTTPStatus != test.status || record.ObservedAt == "" ||
				result.HTML.Status != "degraded" || len(result.HTML.UnpublishedTopics) != 1 {
				t.Fatalf("unpublished topic record: %+v", result.HTML)
			}
			if fixture.calls[unpublished] != 1 {
				t.Fatalf("the unpublished topic was requested %d times", fixture.calls[unpublished])
			}
			toc, err := os.ReadFile(filepath.Join(output, "toc.html"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(toc), `<a class="archive-online" href="`+unpublished+`">Use case 3</a>`) {
				t.Fatalf("the TOC entry does not lead to the publisher's page: %s", toc)
			}
			firstPage, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(result.HTML.Topics[0].Path)))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(firstPage), `href="`+unpublished+`#step"`) {
				t.Fatalf("the link to the unpublished topic does not lead to the publisher's page: %s", firstPage)
			}
		})
	}

	t.Run("server error", func(t *testing.T) {
		result, _, err := archiveWith(t, resources(http.StatusServiceUnavailable))
		if err == nil || result.Status != "failed" {
			t.Fatalf("a topic that failed to download did not fail the guide: result=%+v err=%v", result, err)
		}
	})

	t.Run("nothing published", func(t *testing.T) {
		fixture := resources(http.StatusNotFound)
		fixture.resources[first] = statusResource(first, http.StatusNotFound)
		fixture.resources[last] = statusResource(last, http.StatusGone)
		result, _, err := archiveWith(t, fixture)
		if err == nil || result.Status != "failed" || !strings.Contains(err.Error(), "none of the guide's topics") {
			t.Fatalf("a guide without any published topic was accepted: result=%+v err=%v", result, err)
		}
	})
}

// orderedPrefetcher holds the prefetch of hold until the prefetch of release
// starts, so that results reach the archive in a known order.
type orderedPrefetcher struct {
	*archiveFixture
	hold, release string
	released      chan struct{}
	once          sync.Once
}

func (f *orderedPrefetcher) Prefetch(ctx context.Context, raw string, refresh bool) error {
	if raw == f.release {
		f.once.Do(func() { close(f.released) })
	}
	if raw == f.hold {
		select {
		case <-f.released:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return prefetchingFixture{f.archiveFixture}.Prefetch(ctx, raw, refresh)
}

func TestUnpublishedTopicPrefetchedOutOfOrderIsAttributedToItself(t *testing.T) {
	// The unpublished topic's 404 arrives while the archive waits for the
	// first topic. It must be kept for its own topic, not taken as the first
	// topic's result, which also left the archive waiting forever for it.
	first := ordinaryGuideRoot + "Content/chapter/first.htm"
	unpublished := ordinaryGuideRoot + "Content/chapter/unpublished.htm"
	last := ordinaryGuideRoot + "Content/chapter/last.htm"
	page := func(title string) []byte {
		return []byte(`<html><head></head><body><main><h1>` + title + `</h1><p>Text</p></main></body></html>`)
	}
	fetcher := &orderedPrefetcher{archiveFixture: &archiveFixture{resources: map[string]model.Resource{
		first:       fixtureResource(first, "text/html", page("First")),
		unpublished: statusResource(unpublished, http.StatusNotFound),
		last:        fixtureResource(last, "text/html", page("Last")),
	}, calls: map[string]int{}}, hold: first, release: last, released: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := HTMLWithWorkers(ctx, ordinaryStylePlan("static", first, unpublished, last),
		fetcher, t.TempDir(), false, 1<<20, 8<<20, 2)
	if err != nil || result.Status != "degraded" || len(result.UnpublishedTopics) != 1 ||
		result.UnpublishedTopics[0].URL != unpublished || len(result.HTML.Topics) != 2 ||
		result.HTML.Topics[0].URL != first || result.HTML.Topics[1].URL != last {
		t.Fatalf("out-of-order prefetch result was misattributed: result=%+v err=%v", result, err)
	}
}
