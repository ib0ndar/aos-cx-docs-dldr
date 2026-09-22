package library

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
	"aos-cx-docs-dldr/internal/testutil"
)

type pdfFetcher []byte

func (p pdfFetcher) Get(ctx context.Context, url string, refresh bool) (model.Resource, error) {
	return model.Resource{URL: url, Status: 200, Headers: http.Header{"Content-Type": {"application/pdf"}}, Body: io.NopCloser(bytes.NewReader(p))}, nil
}

type htmlFetcher []byte

func (h htmlFetcher) Get(ctx context.Context, url string, refresh bool) (model.Resource, error) {
	return model.Resource{URL: url, Status: 200, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body: io.NopCloser(bytes.NewReader(h))}, nil
}

type resourceFetcher map[string]model.Resource

func (f resourceFetcher) Get(ctx context.Context, url string, refresh bool) (model.Resource, error) {
	resource, ok := f[url]
	if !ok {
		return model.Resource{}, fmt.Errorf("missing fixture %s", url)
	}
	body, err := io.ReadAll(resource.Body)
	if err != nil {
		return model.Resource{}, err
	}
	resource.Body.Close()
	f[url] = model.Resource{URL: resource.URL, Status: resource.Status, Headers: resource.Headers.Clone(),
		Body: io.NopCloser(bytes.NewReader(body))}
	resource.Body = io.NopCloser(bytes.NewReader(body))
	return resource, nil
}

func acceptPDF(t *testing.T, r *Run, id, label string) model.ArchiveResult {
	t.Helper()
	doc := model.Document{
		ID: id, Title: id + " Guide", Platform: "6300", Version: "10.10", Kind: "pdf",
		URL: "https://example.test/" + id + ".pdf", RouteOrigin: "publisher-url",
	}
	output, err := r.GuideOutput(id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := archive.PublisherPDF(context.Background(), model.DocumentPlan{Document: doc, PDFURL: doc.URL}, pdfFetcher(testutil.PDF(label)), output, false, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(result); err != nil {
		t.Fatal(err)
	}
	return result
}

func publishPDF(t *testing.T, base, label string) (string, Manifest) {
	t.Helper()
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptPDF(t, r, "job", label)
	m, err := r.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return r.Target, m
}

func acceptHTML(t *testing.T, r *Run, id string) model.ArchiveResult {
	return acceptHTMLWithNotices(t, r, id, nil)
}

func acceptHTMLWithNotices(t *testing.T, r *Run, id string, notices []model.Notice) model.ArchiveResult {
	return acceptHTMLWithDiagnostics(t, r, id, notices, nil)
}

func acceptHTMLWithDiagnostics(t *testing.T, r *Run, id string, notices []model.Notice, recoveries []model.StylesheetRecovery) model.ArchiveResult {
	t.Helper()
	url := "https://example.test/guide/Content/home.htm"
	doc := model.Document{
		ID: id, Title: id + " Guide", Platform: "6300", Version: "10.10", Kind: "flare",
		URL: url, RouteOrigin: "publisher-url",
	}
	plan := model.DocumentPlan{Document: doc, Topics: []model.Topic{{URL: url, Title: "Home"}},
		TOC:       []model.TocEntry{{Title: "Home", URL: url}},
		Notices:   append([]model.Notice{}, notices...),
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "flare", RootURL: "https://example.test/guide/", Entries: 1, UniqueTopics: 1}}
	output, err := r.GuideOutput(id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := archive.HTML(context.Background(), plan,
		htmlFetcher(`<main id="mc-main-content"><h1 id="home">Home</h1><p>Offline text</p></main>`),
		output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("HTML archive failed: %+v %v", result, err)
	}
	result.StylesheetRecoveries = cloneStylesheetRecoveries(recoveries)
	result.HTML.StylesheetRecoveries = cloneStylesheetRecoveries(recoveries)
	if err := storage.WriteJSON(r.root, path.Join(r.stage, ".work", id, "manifest.json"), result.HTML); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(result); err != nil {
		t.Fatal(err)
	}
	return result
}

func acceptDegradedHTML(t *testing.T, r *Run, id string, missingNames ...string) model.ArchiveResult {
	t.Helper()
	sourceURL := "https://example.test/guide/Content/home.htm"
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
		Body: io.NopCloser(strings.NewReader(`<main id="mc-main-content"><h1>Home</h1>` + images.String() + `</main>`)),
	}
	plan := model.DocumentPlan{
		Document: doc, Topics: []model.Topic{{URL: sourceURL, Title: "Home"}},
		TOC: []model.TocEntry{{Title: "Home", URL: sourceURL}},
		Inventory: &model.InventoryEvidence{
			Complete: true, Kind: "flare", RootURL: "https://example.test/guide/",
			Entries: 1, UniqueTopics: 1,
		},
	}
	output, err := r.GuideOutput(id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := archive.HTML(context.Background(), plan, resources, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "degraded" || len(result.MissingResources) != len(missingNames) {
		t.Fatalf("degraded archive failed: %+v %v", result, err)
	}
	if err := r.Accept(result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDegradedGuideReplacementPrecedence(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptDegradedHTML(t, run, "html", "a.png", "b.png")
	first, err := run.Publish(false)
	if err != nil || first.Status != "degraded" ||
		len(first.HTMLGuides["html"].MissingResources) != 2 {
		t.Fatalf("first degraded publication=%+v err=%v", first, err)
	}
	if zipPath, zipErr := run.ExportZIP(context.Background()); zipErr != nil || zipPath == "" {
		t.Fatalf("degraded ZIP export failed: path=%q err=%v", zipPath, zipErr)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptDegradedHTML(t, run, "html", "a.png", "b.png", "c.png")
	worse, err := run.Publish(false)
	if err != nil || worse.Status != "degraded" ||
		len(worse.HTMLGuides["html"].MissingResources) != 2 ||
		len(worse.Attempts) != 1 || !worse.Attempts[0].RetainedPrevious {
		t.Fatalf("worse degraded candidate replaced prior=%+v err=%v", worse, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptDegradedHTML(t, run, "html", "a.png")
	improved, err := run.Publish(false)
	if err != nil || improved.Status != "degraded" ||
		len(improved.HTMLGuides["html"].MissingResources) != 1 ||
		improved.Attempts[0].RetainedPrevious {
		t.Fatalf("improved degraded candidate not accepted=%+v err=%v", improved, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptHTML(t, run, "html")
	complete, err := run.Publish(false)
	if err != nil || complete.Status != "complete" || complete.HTMLGuides["html"].Status != "complete" {
		t.Fatalf("complete guide did not upgrade degraded=%+v err=%v", complete, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptDegradedHTML(t, run, "html", "a.png")
	retained, err := run.Publish(false)
	if err != nil || retained.Status != "degraded" ||
		retained.HTMLGuides["html"].Status != "complete" ||
		!retained.Attempts[0].RetainedPrevious {
		t.Fatalf("degraded refresh replaced complete guide=%+v err=%v", retained, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVersionStatusPrecedence(t *testing.T) {
	guides := []Guide{{Status: "degraded"}}
	if status := versionStatus(nil, nil, guides); status != "degraded" {
		t.Fatalf("degraded visible guide status=%q", status)
	}
	attempts := []Attempt{{Status: "degraded"}, {Status: "incomplete"}}
	if status := versionStatus(nil, attempts, guides); status != "incomplete" {
		t.Fatalf("incomplete attempt did not outrank degraded: %q", status)
	}
	if status := versionStatus([]string{"fatal"}, []Attempt{{Status: "complete"}}, nil); status != "incomplete" {
		t.Fatalf("fatal publication error status=%q", status)
	}
	if status := versionStatus(nil, []Attempt{{Status: "complete"}}, []Guide{{Status: "complete"}}); status != "complete" {
		t.Fatalf("complete status=%q", status)
	}
}

func TestSkippedUnavailableMetadataPreservesPriorCompleteGuide(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptHTML(t, run, "html")
	first, err := run.Publish(false)
	if err != nil || first.Status != "complete" {
		t.Fatalf("initial publication=%+v err=%v", first, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	run, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	run.SetSkippedUnavailable([]model.SkippedUnavailableGuide{{
		ID: "html", Title: "html Guide", Kind: "flare",
		MappedSourceURL: "https://example.test/guide/Content/home.htm",
		ProbeURL:        "https://example.test/guide/Content/home.htm",
		FinalURL:        "https://example.test/guide/Content/home.htm",
		Checked:         true, Reason: "mapped source HTTP 404", RetainedPrevious: true,
	}})
	second, err := run.Publish(false)
	if err != nil || second.Status != "complete" || len(second.Guides) != 1 ||
		second.Guides[0].ID != "html" || len(second.Attempts) != 0 ||
		len(second.SkippedUnavailable) != 1 ||
		!second.SkippedUnavailable[0].RetainedPrevious {
		t.Fatalf("skipped prior guide publication=%+v err=%v", second, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublisherNoticesPersistThroughManifestHistoryAndReopen(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	notices := []model.Notice{
		{Kind: model.NoticeExternalLinkRetained, Message: "External navigation retained, not downloaded: https://example.test/support"},
		{Kind: model.NoticeCSSCleanup, Message: "Removed 12 publisher navigation/runtime or provably unused CSS rules"},
	}
	recoveries := []model.StylesheetRecovery{{
		BrokenURL: "https://example.test/broken.css", HTTPStatus: 404,
		BrokenDeclarations: []model.BrokenStylesheetReference{
			{URL: "https://example.test/broken.css", HTTPStatus: 404},
			{URL: "https://example.test/nested/broken.css", HTTPStatus: 410},
		},
		ReplacementURL:          "https://example.test/guide/Content/Resources/TableStyles/Table.css",
		ReplacementFinalURL:     "https://example.test/guide/Content/Resources/TableStyles/Table.css",
		ReplacementSourceSHA256: strings.Repeat("a", 64), ReplacementSourceSize: 123,
		TableStyleFamilies: []string{"TableStyle-Table"}, AffectedTopics: []string{"https://example.test/guide/Content/home.htm"},
	}}
	acceptHTMLWithDiagnostics(t, run, "html", notices, recoveries)
	target := run.Target
	manifest, err := run.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest.HTMLGuides["html"].Notices, notices) ||
		!reflect.DeepEqual(manifest.HTMLGuides["html"].HTML.Notices, notices) ||
		!reflect.DeepEqual(manifest.Attempts[0].Notices, notices) ||
		!reflect.DeepEqual(manifest.HTMLGuides["html"].StylesheetRecoveries, recoveries) ||
		!reflect.DeepEqual(manifest.HTMLGuides["html"].HTML.StylesheetRecoveries, recoveries) ||
		!reflect.DeepEqual(manifest.Attempts[0].StylesheetRecoveries, recoveries) {
		t.Fatalf("central manifest lost publisher notices: %+v", manifest)
	}
	var history []History
	historyBody, err := os.ReadFile(filepath.Join(target, "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(historyBody, &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !reflect.DeepEqual(history[0].Attempts[0].Notices, notices) ||
		!reflect.DeepEqual(history[0].Attempts[0].StylesheetRecoveries, recoveries) {
		t.Fatalf("history lost publisher notices: %+v", history)
	}
	reopened, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.previous.HTMLGuides["html"].Notices, notices) ||
		!reflect.DeepEqual(reopened.previous.HTMLGuides["html"].StylesheetRecoveries, recoveries) {
		t.Fatalf("reopen lost publisher notices: %+v", reopened.previous)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOlderVersionLibraryIsRejectedReadOnlyOnOpen(t *testing.T) {
	base, target, _ := publishUpgradeFixture(t, "html")
	before := cutoverSnapshot(t, base)
	rewritePublishedApplicationVersion(t, target, "0.5")
	afterRewrite := cutoverSnapshot(t, base)

	reopened, err := Open(base, "6300", "10.10")
	if reopened != nil {
		reopened.Close()
	}
	if err == nil {
		t.Fatal("an older-version library was opened instead of rejected")
	}
	if !strings.Contains(err.Error(), "the existing state is preserved") ||
		!strings.Contains(err.Error(), "only version "+model.Version+" libraries") {
		t.Fatalf("rejection is not actionable: %v", err)
	}
	if after := cutoverSnapshot(t, base); !bytes.Equal(afterRewrite, after) {
		t.Fatalf("rejected library was modified:\nbefore=%s\nafter=%s", afterRewrite, after)
	}
	if bytes.Equal(before, afterRewrite) {
		t.Fatal("fixture rewrite did not change the library")
	}
}

func rewritePublishedApplicationVersion(t *testing.T, target, version string) {
	t.Helper()
	manifestPath := filepath.Join(target, "manifest.json")
	var manifest Manifest
	body, err := os.ReadFile(manifestPath)
	if err != nil || json.Unmarshal(body, &manifest) != nil {
		t.Fatal(err)
	}
	manifest.ApplicationVersion = version
	manifest.UpgradedFromVersion = ""
	body, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(target, "history.json")
	var history []History
	body, err = os.ReadFile(historyPath)
	if err != nil || json.Unmarshal(body, &history) != nil {
		t.Fatal(err)
	}
	for index := range history {
		history[index].ApplicationVersion = version
		history[index].UpgradedFromVersion = ""
	}
	body, err = json.MarshalIndent(history, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyPath, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaOneManifestWithoutNoticesStillLoads(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptHTML(t, run, "html")
	if _, err := run.Publish(false); err != nil {
		t.Fatal(err)
	}
	target := run.Target
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", filepath.Join("html", "manifest.json")} {
		full := filepath.Join(target, name)
		body, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal(body, &value); err != nil {
			t.Fatal(err)
		}
		removeNoticeFields(value)
		body, err = json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, '\n')
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatalf("schema-1 manifest without additive notices was rejected: %v", err)
	}
	if notices := reopened.previous.HTMLGuides["html"].Notices; len(notices) != 0 {
		t.Fatalf("absent notices did not decode empty: %+v", notices)
	}
	if recoveries := reopened.previous.HTMLGuides["html"].StylesheetRecoveries; len(recoveries) != 0 {
		t.Fatalf("absent stylesheet recoveries did not decode empty: %+v", recoveries)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func removeNoticeFields(value any) {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, "notices")
		delete(typed, "stylesheet_recoveries")
		for _, child := range typed {
			removeNoticeFields(child)
		}
	case []any:
		for _, child := range typed {
			removeNoticeFields(child)
		}
	}
}

func TestMixedHTMLAndPDFLibraryPublicationAndVerification(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}

	acceptPDF(t, run, "pdf", "original")
	htmlResult := acceptHTML(t, run, "html")
	manifest, err := run.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if len(manifest.PDFGuides) != 1 || len(manifest.HTMLGuides) != 1 || len(manifest.Guides) != 2 ||
		manifest.Guides[0].AssetCount+manifest.Guides[1].AssetCount != 0 ||
		manifest.Guides[0].TopicCount+manifest.Guides[1].TopicCount != 1 ||
		manifest.Guides[0].RouteOrigin != "publisher-url" || manifest.Guides[1].RouteOrigin != "publisher-url" {
		t.Fatalf("mixed central manifest is inconsistent: %+v", manifest)
	}
	index, _ := os.ReadFile(filepath.Join(base, "6300", "10.10", "index.html"))
	search, _ := os.ReadFile(filepath.Join(base, "6300", "10.10", "search-index.js"))
	if !bytes.Contains(index, []byte("html/index.html")) || !bytes.Contains(index, []byte(".pdf")) ||
		!bytes.Contains(search, []byte("Offline text")) {
		t.Fatalf("mixed guide index/search missing links: %s\n%s", index, search)
	}
	reopened, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.previous.HTMLGuides["html"].HTML.Integrity != htmlResult.HTML.Integrity {
		t.Fatal("reopened HTML integrity metadata changed")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(base, "6300", "10.10", "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var tampered Manifest
	if err := json.Unmarshal(data, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.Guides[0].RouteOrigin = "tampered"
	data, err = json.MarshalIndent(tampered, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(base, "6300", "10.10"); err == nil {
		reopened.Close()
		t.Fatal("inconsistent central route provenance was accepted")
	}
}

func TestGeneratedLibraryIndexUsesFormatAwareCopy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		populate  func(*testing.T, *Run)
		cancelled bool
		want      []string
		notWant   []string
	}{
		{
			name: "html-only",
			populate: func(t *testing.T, run *Run) {
				acceptHTML(t, run, "html")
			},
			want: []string{
				"Downloaded HTML guides are available for offline browsing.",
				"Search downloaded HTML guides, topics, and text",
				`placeholder="Guide, topic, or archived HTML text"`,
				`data-empty-message="No matching downloaded HTML guide, topic, or text."`,
				"(native HTML library).",
			},
			notWant: []string{"Search downloaded PDF titles", "PDF contents are not extracted"},
		},
		{
			name: "pdf-only",
			populate: func(t *testing.T, run *Run) {
				acceptPDF(t, run, "pdf", "original")
			},
			want: []string{
				"Original publisher PDFs are saved without conversion.",
				"Search downloaded PDF titles",
				`placeholder="Guide title"`,
				`data-empty-message="No matching downloaded PDF titles."`,
				"(native PDF library). PDF contents are not extracted or indexed.",
			},
			notWant: []string{"Search downloaded HTML topic titles"},
		},
		{
			name: "mixed",
			populate: func(t *testing.T, run *Run) {
				acceptHTML(t, run, "html")
				acceptPDF(t, run, "pdf", "original")
			},
			want: []string{
				"Downloaded HTML guides and original publisher PDFs are available offline.",
				"Search downloaded guides, topics, and HTML text",
				`placeholder="Guide, topic, or archived HTML text"`,
				`data-empty-message="No matching downloaded guide, topic, or HTML text."`,
				"(native mixed-format library). PDF contents are not extracted or indexed.",
			},
			notWant: []string{"Search downloaded PDF titles"},
		},
		{
			name:      "empty-incomplete",
			populate:  func(*testing.T, *Run) {},
			cancelled: true,
			want: []string{
				"Library status: <strong>incomplete</strong>.",
				"No complete guides are currently available in this library.",
				"Search downloaded content",
				`placeholder="No downloaded content"`,
				`data-empty-message="No downloaded content is available to search."`,
				"(native library).",
			},
			notWant: []string{"Search downloaded PDF titles", "PDF contents are not extracted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			run, err := Open(base, "6300", "10.10")
			if err != nil {
				t.Fatal(err)
			}
			tc.populate(t, run)
			if _, err := run.Publish(tc.cancelled); err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(base, "6300", "10.10", "index.html"))
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range tc.want {
				if !bytes.Contains(body, []byte(expected)) {
					t.Errorf("generated index missing %q:\n%s", expected, body)
				}
			}
			for _, unexpected := range tc.notWant {
				if bytes.Contains(body, []byte(unexpected)) {
					t.Errorf("generated index contains inapplicable copy %q:\n%s", unexpected, body)
				}
			}
		})
	}
}

func acceptSourceHTML(t *testing.T, run *Run, kind string) model.ArchiveResult {
	t.Helper()
	var plan model.DocumentPlan
	var fetcher model.Fetcher
	switch kind {
	case "hpe":
		public := "https://support.example.test/hpesc/public/docDisplay?docId=doc-A"
		api := "https://support.example.test/hpesc/public/api/document/doc-A?ignorePayload=true"
		plan = model.DocumentPlan{
			Document:  model.Document{ID: "hpe", Title: "HPE", Platform: "6300", Version: "10.10", Kind: "hpe", URL: public},
			Topics:    []model.Topic{{URL: public, Title: "HPE", FetchURL: api}},
			TOC:       []model.TocEntry{{Title: "HPE", URL: public}},
			Inventory: &model.InventoryEvidence{Complete: true, Kind: "hpe", RootURL: api, Entries: 1, UniqueTopics: 1},
		}
		fetcher = resourceFetcher{
			api: {URL: api, Status: 200, Headers: http.Header{"Content-Type": {"multiPage;charset=UTF-8"}},
				Body: io.NopCloser(strings.NewReader(`<main class="ditasrc"><h1>HPE</h1></main>`))},
			"https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Regular-Web.woff2": {
				URL: "https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Regular-Web.woff2", Status: 200,
				Headers: http.Header{"Content-Type": {"font/woff2"}}, Body: io.NopCloser(bytes.NewReader([]byte("wOF2regular")))},
			"https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Bold-Web.woff2": {
				URL: "https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Bold-Web.woff2", Status: 200,
				Headers: http.Header{"Content-Type": {"font/woff2"}}, Body: io.NopCloser(bytes.NewReader([]byte("wOF2bold")))},
		}
	case "static":
		public := "https://static.example.test/guide/index.html"
		plan = model.DocumentPlan{
			Document:  model.Document{ID: "static", Title: "Static", Platform: "6300", Version: "10.10", Kind: "static", URL: public},
			Topics:    []model.Topic{{URL: public, Title: "Static"}},
			TOC:       []model.TocEntry{{Title: "Static", URL: public}},
			Inventory: &model.InventoryEvidence{Complete: true, Kind: "static", RootURL: "https://static.example.test/guide/", Entries: 1, UniqueTopics: 1},
		}
		fetcher = htmlFetcher(`<main><h1>Static</h1></main>`)
	default:
		t.Fatal("unsupported fixture kind", kind)
	}
	output, err := run.GuideOutput(plan.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := archive.HTML(context.Background(), plan, fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("%s archive failed: %+v %v", kind, result, err)
	}
	if err := run.Accept(result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPDFAndAllHTMLKindsShareTransactionalLibrary(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}

	acceptPDF(t, run, "pdf", "original")
	acceptHTML(t, run, "html")
	acceptSourceHTML(t, run, "hpe")
	acceptSourceHTML(t, run, "static")
	manifest, err := run.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "complete" || len(manifest.Guides) != 4 ||
		len(manifest.PDFGuides) != 1 || len(manifest.HTMLGuides) != 3 {
		t.Fatalf("cross-format library incomplete: %+v", manifest)
	}
	reopened, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.previous.Guides) != 4 {
		t.Fatalf("cross-format library did not verify on reopen: %+v", reopened.previous)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHTMLSourceHistoryIncludesPlanInputs(t *testing.T) {
	old := model.ArchiveResult{Format: "html", HTML: &model.HTMLArchive{Inputs: []model.SourceInput{{
		RequestedURL: "https://example.test/toc.json", Role: "toc", SHA256: strings.Repeat("a", 64),
	}}}}
	current := model.ArchiveResult{Format: "html", HTML: &model.HTMLArchive{Inputs: []model.SourceInput{{
		RequestedURL: "https://example.test/toc.json", Role: "toc", SHA256: strings.Repeat("b", 64),
	}}}}
	if reflect.DeepEqual(sourceDigests(old), sourceDigests(current)) {
		t.Fatal("source-plan change was omitted from HTML history identity")
	}
}

func TestCancelledMixedBatchPublishesAcceptedPDFAndHPE(t *testing.T) {
	run, err := Open(t.TempDir(), "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptPDF(t, run, "pdf", "original")
	acceptSourceHTML(t, run, "hpe")
	manifest, err := run.Publish(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "incomplete" || len(manifest.PDFGuides) != 1 || len(manifest.HTMLGuides) != 1 ||
		manifest.PDFGuides["pdf"].Status != "complete" || manifest.HTMLGuides["hpe"].Status != "complete" {
		t.Fatalf("cancelled mixed batch lost accepted guides: %+v", manifest)
	}
}

func TestFailedHTMLUpdateRetainsPreviousCompleteGuide(t *testing.T) {
	base := t.TempDir()
	first, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	original := acceptHTML(t, first, "html")
	if _, err := first.Publish(false); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	output, err := second.GuideOutput("html")
	if err != nil {
		t.Fatal(err)
	}
	failed := model.ArchiveResult{Document: original.Document, OutputDir: output, Status: "incomplete", Format: "html",
		Errors: []string{"required image unavailable"}}
	if err := second.Accept(failed); err != nil {
		t.Fatal(err)
	}
	manifest, err := second.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	retained := manifest.HTMLGuides["html"]
	if manifest.Status != "incomplete" || !manifest.Attempts[0].RetainedPrevious ||
		retained.HTML.Topics[0].SHA256 != original.HTML.Topics[0].SHA256 {
		t.Fatalf("failed HTML update replaced complete guide: %+v", manifest)
	}
}

func TestNativePDFLibraryLayoutHistoryAndPreviousRetention(t *testing.T) {
	base := t.TempDir()
	target, first := publishPDF(t, base, "original")
	name := "6300 - 10.10 - job Guide.pdf"
	files, _ := os.ReadDir(filepath.Join(target, "job"))
	if len(files) != 1 || files[0].Name() != name {
		t.Fatalf("publisher guide not minimal: %v", files)
	}
	index, _ := os.ReadFile(filepath.Join(target, "index.html"))
	search, _ := os.ReadFile(filepath.Join(target, "search-index.js"))
	if !bytes.Contains(index, []byte("job/6300%20-%2010.10%20-%20job%20Guide.pdf")) ||
		!bytes.Contains(search, []byte("job/6300%20-%2010.10%20-%20job%20Guide.pdf")) {
		t.Fatal("index/search do not link directly to PDF")
	}
	_, second := publishPDF(t, base, "changed")
	if first.PDFGuides["job"].PDF.SourceSHA256 == second.PDFGuides["job"].PDF.SourceSHA256 {
		t.Fatal("source change lost")
	}
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.history) != 2 || len(r.history[1].Changes["job"].Changed) != 1 {
		t.Fatalf("bad source history: %+v", r.history)
	}
	snapshot := filepath.Join(base, "6300", filepath.FromSlash(r.history[1].PreviousSnapshot), "job", name)
	old, _ := os.ReadFile(snapshot)
	if !bytes.Equal(old, testutil.PDF("original")) {
		t.Fatal("previous snapshot changed")
	}
	doc := second.PDFGuides["job"].Document
	output, err := r.GuideOutput("job")
	if err != nil {
		t.Fatal(err)
	}
	result := model.ArchiveResult{Document: doc, OutputDir: output, Status: "failed", Format: "pdf", Errors: []string{"publisher unavailable"}}
	if err := r.Accept(result); err != nil {
		t.Fatal(err)
	}
	m, err := r.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if m.Status != "incomplete" || !m.Attempts[0].RetainedPrevious || m.PDFGuides["job"].PDF.SHA256 != second.PDFGuides["job"].PDF.SHA256 {
		t.Fatalf("failed update replaced previous guide: %+v", m)
	}
	work, _ := filepath.Glob(filepath.Join(base, "6300", ".incomplete", "*", "unfinished", "job", "attempt.json"))
	if len(work) != 1 {
		t.Fatal("failed working diagnostics not retained")
	}
}

func TestUserRootShortcutAlwaysAppendsPlatformVersion(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "cloud")
	if err := os.Mkdir(actual, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "shortcut")
	if err := os.Symlink(actual, alias); err != nil {
		t.Skip(err)
	}
	base := filepath.Join(alias, "6300", "10.10")
	target, _ := publishPDF(t, base, "body")
	resolved, err := filepath.EvalSymlinks(actual)
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(resolved, "6300", "10.10", "6300", "10.10")
	if target != expected {
		t.Fatalf("base suffix incorrectly omitted: %s", target)
	}
}

func TestLocksAndUnrecognizedDestinationsAreNotOverwritten(t *testing.T) {
	base := t.TempDir()
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(base, "6300", "10.10"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("lock ignored: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "6300", "10.10")
	os.MkdirAll(target, 0o700)
	os.WriteFile(filepath.Join(target, "my-notes.txt"), []byte("keep"), 0o600)
	if _, err := Open(base, "6300", "10.10"); err == nil {
		t.Fatal("unrecognized destination overwritten")
	}
	notes, _ := os.ReadFile(filepath.Join(target, "my-notes.txt"))
	if string(notes) != "keep" {
		t.Fatal("user data changed")
	}
	if _, err := os.Stat(filepath.Join(base, "6300", ".10.10.lock")); !os.IsNotExist(err) {
		t.Fatal("lock leaked after failed open")
	}
}

func TestManagedSymlinksAreRefused(t *testing.T) {
	for _, name := range []string{"6300", "6300/10.10", "6300/.staging", "6300/.snapshots", "6300/.incomplete"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			outside := t.TempDir()
			link := filepath.Join(base, filepath.FromSlash(name))
			os.MkdirAll(filepath.Dir(link), 0o700)
			if err := os.Symlink(outside, link); err != nil {
				t.Skip(err)
			}
			if _, err := Open(base, "6300", "10.10"); err == nil {
				t.Fatal("managed symlink accepted")
			}
			files, _ := os.ReadDir(outside)
			if len(files) != 0 {
				t.Fatal("symlink target changed")
			}
		})
	}
}

func TestFailedReplacementRollsBackAndRecoversJournal(t *testing.T) {
	for _, failRollback := range []bool{false, true} {
		t.Run(fmt.Sprint(failRollback), func(t *testing.T) {
			base := t.TempDir()
			target, previous := publishPDF(t, base, "original")
			r, err := Open(base, "6300", "10.10")
			if err != nil {
				t.Fatal(err)
			}
			acceptPDF(t, r, "job", "replacement")
			r.rename = func(from, to string) error {
				if from == r.stage || (failRollback && strings.Contains(from, ".snapshots/")) {
					return errors.New("injected rename failure")
				}
				return r.root.Rename(from, to)
			}
			if _, err := r.Publish(false); err == nil {
				t.Fatal("injected publication failure hidden")
			}
			r.Close()
			recovered, err := Open(base, "6300", "10.10")
			if err != nil {
				t.Fatal(err)
			}
			if recovered.previous.RunID != previous.RunID {
				t.Fatal("previous complete library not restored")
			}
			data, _ := os.ReadFile(filepath.Join(target, "job", "6300 - 10.10 - job Guide.pdf"))
			if !bytes.Equal(data, testutil.PDF("original")) {
				t.Fatal("rollback lost original bytes")
			}
			if _, err := os.Stat(filepath.Join(base, "6300", ".10.10.transaction.json")); !os.IsNotExist(err) {
				t.Fatal("recovered journal not cleared")
			}
			if failRollback {
				entries, _ := filepath.Glob(filepath.Join(base, "6300", ".snapshots", "10.10", "*", "manifest.json"))
				if len(entries) == 0 {
					t.Fatal("recovery moved/deleted source snapshot instead of preserving it")
				}
			}
			recovered.Close()
		})
	}
}

func TestRecoveryRejectsCorruptSnapshot(t *testing.T) {
	base := t.TempDir()
	publishPDF(t, base, "original")
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}

	acceptPDF(t, r, "job", "changed")
	r.rename = func(from, to string) error {
		if from == r.stage || strings.Contains(from, ".snapshots/") {
			return errors.New("stop")
		}
		return r.root.Rename(from, to)
	}
	r.Publish(false)
	var tx journal
	if err := storage.ReadJSON(r.root, r.journal, &tx); err != nil {
		t.Fatal(err)
	}
	pdf := path.Join(tx.PreviousSnapshot, "job", "6300 - 10.10 - job Guide.pdf")
	if err := storage.Atomic(r.root, pdf, []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := Open(base, "6300", "10.10"); err == nil {
		t.Fatal("corrupt recovery source exposed")
	}
	if _, err := os.Stat(r.Target); !os.IsNotExist(err) {
		t.Fatal("invalid snapshot exposed")
	}
}

func TestCancellationPublishesOnlyAcceptedPDFs(t *testing.T) {
	base := t.TempDir()
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptPDF(t, r, "first", "done")
	work, err := r.GuideOutput("unfinished")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, ".download.part"), []byte("partial"), 0o600)
	m, err := r.Publish(true)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if m.Status != "incomplete" || len(m.PDFGuides) != 1 || m.PDFGuides["first"].Status != "complete" {
		t.Fatalf("cancelled publication wrong: %+v", m)
	}
	saved, _ := filepath.Glob(filepath.Join(base, "6300", ".incomplete", "*", "unfinished", "unfinished", ".download.part"))
	if len(saved) != 1 {
		t.Fatal("unfinished bytes not retained")
	}
}
