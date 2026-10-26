package archive

import (
	"context"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func TestEmptyPublisherTopicIsArchivedAsServed(t *testing.T) {
	first := ordinaryGuideRoot + "Content/chapter/first.htm"
	empty := ordinaryGuideRoot + "GUID-D5A73C64.html"
	oxygenPage := func(content string) []byte {
		return []byte(`<html><head><title>***</title><script src="app.js"></script></head><body>
<div class=" wh_topic_content body "><main role="main">` + content + `</main></div></body></html>`)
	}
	archiveWith := func(t *testing.T, emptyContent string) (model.ArchiveResult, error) {
		t.Helper()
		plan := ordinaryStylePlan("static", first, empty)
		plan.TOC[1].Title = "***"
		fetcher := &archiveFixture{resources: map[string]model.Resource{
			first: fixtureResource(first, "text/html", oxygenPage(`<article class="- topic/topic topic"><h1>First</h1><p>Text</p></article>`)),
			empty: fixtureResource(empty, "text/html", oxygenPage(emptyContent)),
		}, calls: map[string]int{}}
		return HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 8<<20)
	}

	// The 10.17 Event Log Message Reference Guide lists a topic titled "***"
	// whose page holds an empty DITA topic.
	result, err := archiveWith(t, `<article class="- topic/topic concept/concept topic concept"> <h1 class="- topic/title title"></h1>
<div class="- topic/body concept/conbody body"> <p class="- topic/p p"></p> </div></article>`)
	if err != nil || result.Status != "complete" || len(result.HTML.Topics) != 2 ||
		!slices.ContainsFunc(result.Warnings, func(warning string) bool {
			return strings.Contains(warning, empty) && strings.Contains(warning, "has no content")
		}) {
		t.Fatalf("empty publisher topic was not archived as served: result=%+v err=%v", result, err)
	}

	// An empty page that is not a topic, such as a shell, still fails and
	// names the topic.
	_, err = archiveWith(t, `<div class="loading"><p></p></div>`)
	if err == nil || !strings.Contains(err.Error(), empty) {
		t.Fatalf("an empty page without a topic was accepted or not named: %v", err)
	}
}
