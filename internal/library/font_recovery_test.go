package library

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/model"
)

// acceptUnavailableFontHTML archives a guide whose stylesheet declares a font
// the publisher no longer serves, plus any missing standalone images.
func acceptUnavailableFontHTML(t *testing.T, r *Run, id string, missingNames ...string) model.ArchiveResult {
	t.Helper()
	sourceURL := "https://example.test/guide/Content/home.htm"
	styleURL := "https://example.test/guide/Resources/fonts.css"
	fontURL := "https://example.test/guide/Resources/Metric-Light.woff2"
	doc := model.Document{
		ID: id, Title: id + " Guide", Platform: "6300", Version: "10.10", Kind: "flare",
		URL: sourceURL, RouteOrigin: "publisher-url",
	}
	var images strings.Builder
	resources := resourceFetcher{}
	for _, name := range missingNames {
		images.WriteString(`<img alt="Missing" src="../Resources/` + name + `">`)
		url := "https://example.test/guide/Resources/" + name
		resources[url] = model.Resource{
			URL: url, Status: http.StatusNotFound, Headers: http.Header{},
			Body: io.NopCloser(bytes.NewReader(nil)),
		}
	}
	resources[sourceURL] = model.Resource{
		URL: sourceURL, Status: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html"}},
		Body: io.NopCloser(strings.NewReader(`<html><head><link rel="stylesheet" href="../Resources/fonts.css"></head><body>` +
			`<main id="mc-main-content"><h1>Home</h1>` + images.String() + `</main></body></html>`)),
	}
	resources[styleURL] = model.Resource{
		URL: styleURL, Status: http.StatusOK, Headers: http.Header{"Content-Type": {"text/css"}},
		Body: io.NopCloser(strings.NewReader(
			`@font-face{font-family:'Metric';src:url(Metric-Light.woff2) format('woff2')}h1{font-family:Metric,Arial,sans-serif}`)),
	}
	resources[fontURL] = model.Resource{
		URL: fontURL, Status: http.StatusNotFound, Headers: http.Header{},
		Body: io.NopCloser(bytes.NewReader(nil)),
	}
	plan := model.DocumentPlan{
		Document: doc, Topics: []model.Topic{{URL: sourceURL, Title: "Home"}},
		TOC: []model.TocEntry{{Title: "Home", URL: sourceURL}},
		Inventory: &model.InventoryEvidence{
			Complete: true, Kind: "flare", RootURL: "https://example.test/guide/",
			Entries: 1, UniqueTopics: 1,
		},
	}
	want := "complete"
	if len(missingNames) > 0 {
		want = "degraded"
	}
	guide, output := guideOutput(t, r, id)
	result, err := archive.HTMLInRoot(context.Background(), plan, resources, guide, output, false, 1<<20, 2<<20, 1, nil)
	if err != nil || result.Status != want || len(result.MissingResources) != len(missingNames) ||
		len(result.StylesheetRecoveries) != 1 || result.StylesheetRecoveries[0].StylesheetURL != styleURL ||
		!result.StylesheetRecoveries[0].FontFaceRemoved {
		t.Fatalf("unavailable-font archive failed: %+v %v", result, err)
	}
	if err := r.Accept(result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestUnavailableFontRecoveryPublishesVerifiesAndReopens(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	accepted := acceptUnavailableFontHTML(t, run, "html")
	published, err := run.Publish(false)
	if err != nil || published.Status != "complete" || published.HTMLGuides["html"].Status != "complete" ||
		!reflect.DeepEqual(published.HTMLGuides["html"].StylesheetRecoveries, accepted.StylesheetRecoveries) ||
		!reflect.DeepEqual(published.Attempts[0].StylesheetRecoveries, accepted.StylesheetRecoveries) {
		t.Fatalf("font recovery publication=%+v err=%v", published, err)
	}
	if zipPath, zipErr := run.ExportZIP(context.Background()); zipErr != nil || zipPath == "" {
		t.Fatalf("font recovery ZIP export failed: path=%q err=%v", zipPath, zipErr)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening verifies every published guide against its manifest.
	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(run.previous.HTMLGuides["html"].StylesheetRecoveries, accepted.StylesheetRecoveries) {
		t.Fatalf("reopen lost font recovery: %+v", run.previous.HTMLGuides["html"])
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnavailableFontRecoveryKeepsDegradedRefreshPrecedence(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptDegradedHTML(t, run, "html", "a.png")
	if first, err := run.Publish(false); err != nil || first.Status != "degraded" {
		t.Fatalf("first degraded publication=%+v err=%v", first, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	// A font recovery is not a missing resource: the same placeholder set
	// still replaces the prior degraded guide.
	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptUnavailableFontHTML(t, run, "html", "a.png")
	same, err := run.Publish(false)
	if err != nil || same.Status != "degraded" || same.Attempts[0].RetainedPrevious ||
		len(same.HTMLGuides["html"].MissingResources) != 1 ||
		len(same.HTMLGuides["html"].StylesheetRecoveries) != 1 {
		t.Fatalf("degraded refresh with font recovery=%+v err=%v", same, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptUnavailableFontHTML(t, run, "html", "a.png", "b.png")
	worse, err := run.Publish(false)
	if err != nil || !worse.Attempts[0].RetainedPrevious ||
		len(worse.HTMLGuides["html"].MissingResources) != 1 {
		t.Fatalf("larger missing set replaced prior degraded guide=%+v err=%v", worse, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptUnavailableFontHTML(t, run, "html")
	complete, err := run.Publish(false)
	if err != nil || complete.Status != "complete" || complete.HTMLGuides["html"].Status != "complete" {
		t.Fatalf("complete guide with font recovery did not replace degraded=%+v err=%v", complete, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptUnavailableFontHTML(t, run, "html", "a.png")
	retained, err := run.Publish(false)
	if err != nil || retained.HTMLGuides["html"].Status != "complete" || !retained.Attempts[0].RetainedPrevious {
		t.Fatalf("degraded refresh replaced complete guide=%+v err=%v", retained, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}
