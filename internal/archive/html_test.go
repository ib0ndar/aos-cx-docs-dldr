package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
	"github.com/tdewolff/parse/v2/css"
	"golang.org/x/net/html"
)

const (
	testRoot = "https://docs.example.test/guide/"
	testHome = testRoot + "Content/home.htm"
)

var tinyPNG = []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 0}

type archiveFixture struct {
	mu        sync.Mutex
	resources map[string]model.Resource
	calls     map[string]int
	failures  map[string]error
}

func (f *archiveFixture) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	if err := ctx.Err(); err != nil {
		return model.Resource{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[raw]++
	if err := f.failures[raw]; err != nil {
		return model.Resource{}, err
	}
	resource, ok := f.resources[raw]
	if !ok {
		return model.Resource{}, errors.New("fixture missing " + raw)
	}
	body, err := io.ReadAll(resource.Body)
	if err != nil {
		return model.Resource{}, err
	}
	resource.Body = io.NopCloser(bytes.NewReader(body))
	f.resources[raw] = model.Resource{URL: resource.URL, Status: resource.Status, Headers: resource.Headers.Clone(), Body: io.NopCloser(bytes.NewReader(body))}
	return resource, nil
}

func fixtureResource(raw, contentType string, body []byte) model.Resource {
	return model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(body))}
}

func flarePlan(topics ...model.Topic) model.DocumentPlan {
	doc := model.Document{ID: "job", Title: "Job Scheduler Guide", Platform: "6300", Version: "10.16", URL: testHome, Kind: "flare"}
	if len(topics) == 0 {
		topics = []model.Topic{{URL: testHome, Title: "Home"}}
	}
	toc := make([]model.TocEntry, 0, len(topics))
	for _, topic := range topics {
		toc = append(toc, model.TocEntry{Title: topic.Title, URL: topic.URL})
	}
	return model.DocumentPlan{Document: doc, Topics: topics, TOC: toc,
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "flare", RootURL: testRoot, Entries: len(toc), UniqueTopics: len(topics)}}
}

func TestFlareHelpSystemFallbackArchivesCrossPageFixture(t *testing.T) {
	cover := testRoot + "Content/contents.htm"
	second := testRoot + "Content/second.htm"
	helpSystem := testRoot + "Data/HelpSystem.xml"
	module := testRoot + "Data/Tocs/Guide.js"
	chunk := testRoot + "Data/Tocs/Guide_Chunk0.js"
	resources := map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<html data-mc-help-system-file-name="index.xml" data-mc-path-to-help-system="../" data-mc-target-type="WebHelp2"><body><ul data-mc-toc="True"></ul><a href="contents.htm">Table of Contents</a><main id="mc-main-content"><h1>Home</h1><a href="second.htm#part">Second</a></main></body></html>`)),
		cover: fixtureResource(cover, "text/html", []byte(
			`<html><body><main id="mc-main-content"><h1>Cover and notices</h1><a href="home.htm">Home</a></main></body></html>`)),
		second: fixtureResource(second, "text/html", []byte(
			`<html><body><main id="mc-main-content"><h1 id="part">Second</h1><a href="home.htm">Home</a></main></body></html>`)),
		helpSystem: fixtureResource(helpSystem, "application/xml", []byte(
			`<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"><CatapultSkin SkinType="WebHelp2"/></WebHelpSystem>`)),
		module: fixtureResource(module, "application/javascript", []byte(
			`define({numchunks:1,prefix:'Guide_Chunk',tree:{n:[{i:0,c:0},{i:1,c:0}]}});`)),
		chunk: fixtureResource(chunk, "application/javascript", []byte(
			`define({'/Content/home.htm':{i:[0],t:['Home'],b:['']},'/Content/second.htm':{i:[1],t:['Second'],b:['']}});`)),
	}

	fetcher := &archiveFixture{resources: resources, calls: map[string]int{}}
	document := model.Document{
		ID: "guide", Title: "Guide", Platform: "10000", Version: "10.16",
		Kind: "flare", URL: testHome,
	}
	plan, err := source.LoadPlan(context.Background(), fetcher, document)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Topics) != 3 || plan.Inventory == nil || !plan.Inventory.Complete ||
		plan.Inventory.TOCURL != module {
		t.Fatalf("fallback produced an incomplete plan: %+v", plan)
	}
	result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("cross-page fallback archive failed: result=%+v err=%v", result, err)
	}
	if len(result.HTML.Topics) != 3 || result.HTML.Integrity.CheckedLinks < 3 {
		t.Fatalf("cross-page fallback archive lost topics or links: %+v", result.HTML)
	}
}

func TestFlareUnavailableOptionalDetailedTOCIsNotSupplementaryTopic(t *testing.T) {
	missing := testRoot + "Content/fir-int.htm"
	plan := flarePlan()
	plan.UnavailableNavigation = []model.UnavailableNavigation{{
		URL: missing, HTTPStatus: http.StatusNotFound, Role: "optional-detailed-toc",
	}}
	plan.Warnings = []string{"Publisher detailed TOC shortcut is unavailable"}
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<html><body><main id="mc-main-content"><h1>Home</h1><a href="fir-int.htm">Table of Contents</a></main></body></html>`)),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" || len(result.HTML.Topics) != 1 ||
		fetcher.calls[missing] != 0 || !slices.Contains(result.Warnings, plan.Warnings[0]) {
		t.Fatalf("known unavailable navigation was crawled as content: result=%+v calls=%v err=%v",
			result, fetcher.calls, err)
	}
	page, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(page, []byte(`href="`+missing+`"`)) ||
		!bytes.Contains(page, []byte(`class="archive-online"`)) {
		t.Fatalf("unavailable navigation link was not retained online: %s", page)
	}
}

func TestHTMLArchivePreservesContentAndClosesDependencies(t *testing.T) {
	second := testRoot + "Content/chapter/second.htm"
	extra1 := testRoot + "Content/extra.htm?mode=one"
	extra2 := testRoot + "Content/extra.htm?mode=two"
	css := testRoot + "Resources/styles/main.css"
	nested := testRoot + "Resources/styles/nested.css"
	image := testRoot + "Resources/images/photo.png"
	icon := testRoot + "Resources/styles/icon.png"
	font := testRoot + "Resources/styles/font.woff2"
	vector := testRoot + "Resources/images/diagram.svg"
	resources := map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html; charset=utf-8", []byte(`<html><head><meta name="dc.date" content="2026-06-01">
<link rel="stylesheet" href="../Resources/styles/main.css"><link rel="stylesheet" data-mc-generated="true" href="../Skins/Default/Stylesheets/Nav.css"></head>
<body><header>Publisher shell</header><div id="mc-main-content"><h1 id="intro">Home</h1>
<table><caption>Ports</caption><tr><th rowspan="2">Name</th><td colspan="2">up</td></tr></table><p class="a">Styled</p>
<pre><code>interface 1/1/1
    description  two spaces
	no shutdown
</code></pre><div class="MCDropDownBody" hidden style="display:none;background:url('../Resources/images/photo.png')"><p>Expanded text</p></div>
<a href="chapter/second.htm#target">Second</a><a href="extra.htm?mode=one#one">One</a><a href="extra.htm?mode=two#two">Two</a>
<img data-original="../Resources/images/photo.png" src="data:image/png;base64,AAAA">
<img alt="Diagram" src="../Resources/images/diagram.svg"><form>Feedback</form><script>track()</script></div>
<footer>Copyright 2026 Publisher</footer></body></html>`)),
		second: fixtureResource(second, "text/html", []byte(`<main id="mc-main-content"><h1 id="target">Second</h1></main>`)),
		extra1: fixtureResource(extra1, "text/html", []byte(`<main id="mc-main-content"><h1 id="one">Extra one</h1></main>`)),
		extra2: fixtureResource(extra2, "text/html", []byte(`<main id="mc-main-content"><h1 id="two">Extra two</h1></main>`)),
		css: fixtureResource(css, "text/css", []byte(`@namespace MadCap url(http://www.madcapsoftware.com/Schemas/MadCap.xsd);
@import "nested.css"; @font-face{src:url("font.woff2")} .a{background:image-set(url("icon.png") 1x)} .old{behavior:url(PIE.htc)}`)),
		nested: fixtureResource(nested, "text/css", []byte(`@import "main.css"; .b{background:url("icon.png")}`)),
		image:  fixtureResource(image, "image/png", tinyPNG),
		icon:   fixtureResource(icon, "image/png", append(append([]byte{}, tinyPNG...), 'i')),
		font:   fixtureResource(font, "font/woff2", append([]byte("wOF2"), make([]byte, 8)...)),
		vector: fixtureResource(vector, "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><image href="../images/photo.png"/><script>bad()</script><circle id="dot" r="2"/></svg>`)),
	}
	fetcher := &archiveFixture{resources: resources, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(
		model.Topic{URL: testHome, Title: "Home"},
		model.Topic{URL: second, Title: "Second"},
	), fetcher, output, false, 1<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("archive failed: result=%+v err=%v", result, err)
	}
	noticeCounts := map[model.NoticeKind]int{}
	for _, notice := range result.Notices {
		noticeCounts[notice.Kind]++
	}
	if noticeCounts[model.NoticeNavigationStyleOmitted] != 1 ||
		noticeCounts[model.NoticeCSSCleanup] != 1 || len(result.Warnings) != 0 {
		t.Fatalf("expected policy diagnostics were not typed as notices: notices=%+v warnings=%v", result.Notices, result.Warnings)
	}
	if len(result.HTML.Topics) != 4 || len(result.HTML.Assets) != 6 {
		t.Fatalf("unexpected closure counts: topics=%d assets=%d", len(result.HTML.Topics), len(result.HTML.Assets))
	}
	homeBytes, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(homeBytes))
	if err != nil {
		t.Fatal(err)
	}
	if findNode(doc, func(n *html.Node) bool { return n.Data == "table" }) == nil ||
		findNode(doc, func(n *html.Node) bool { return n.Data == "form" || n.Data == "script" }) != nil ||
		!strings.Contains(string(homeBytes), "description  two spaces") ||
		!strings.Contains(string(homeBytes), "Copyright 2026 Publisher") ||
		!strings.Contains(string(homeBytes), "data-archive-expanded") ||
		!strings.Contains(string(homeBytes), `<svg`) {
		t.Fatalf("substantive HTML/SVG was not faithfully preserved: %s", homeBytes)
	}
	for _, url := range []string{testHome, second, extra1, extra2} {
		if fetcher.calls[url] != 2 {
			t.Fatalf("two-pass topic read missing for %s: %d", url, fetcher.calls[url])
		}
	}
	for _, url := range []string{css, nested, image, icon, font, vector} {
		if fetcher.calls[url] != 1 {
			t.Fatalf("asset was not deduplicated for %s: %d", url, fetcher.calls[url])
		}
	}
	if fetcher.calls[testRoot+"Skins/Default/Stylesheets/Nav.css"] != 0 {
		t.Fatal("generated Flare navigation stylesheet was fetched")
	}
	data, _ := os.ReadFile(filepath.Join(output, "manifest.json"))
	var disk model.HTMLArchive
	if json.Unmarshal(data, &disk) != nil || disk.Integrity.CheckedLinks == 0 || len(disk.Errors) != 0 {
		t.Fatalf("invalid persisted archive manifest: %s", data)
	}
}

func TestHTMLArchiveMissingAssetIsIncompleteAndDoesNotEraseTopics(t *testing.T) {
	second := testRoot + "Content/second.htm"
	missing := testRoot + "Resources/missing.png"
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(`<main id="mc-main-content"><h1>Home</h1><img src="../Resources/missing.png"></main>`)),
		second:   fixtureResource(second, "text/html", []byte(`<main id="mc-main-content"><h1>Second survives</h1></main>`)),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), flarePlan(
		model.Topic{URL: testHome, Title: "Home"}, model.Topic{URL: second, Title: "Second"},
	), fetcher, t.TempDir(), false, 1<<20, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "incomplete" || len(result.HTML.Topics) != 2 || len(result.HTML.Errors) == 0 ||
		fetcher.calls[missing] != 1 {
		t.Fatalf("missing asset not represented as incomplete: %+v calls=%v", result, fetcher.calls)
	}
}

func TestArchiveProgressCountsValidatedTopicsOnceAndDynamicAssetsOnce(t *testing.T) {
	second := testRoot + "Content/second.htm"
	image := testRoot + "Resources/image.png"
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><img src="../Resources/image.png"></main>`)),
		second: fixtureResource(second, "text/html", []byte(
			`<main id="mc-main-content"><h1>Second</h1><img src="../Resources/image.png"></main>`)),
		image: fixtureResource(image, "image/png", tinyPNG),
	}, calls: map[string]int{}}
	var events []Progress
	result, err := HTMLWithWorkersObserved(context.Background(), flarePlan(
		model.Topic{URL: testHome, Title: "Home"},
		model.Topic{URL: testHome, Title: "Duplicate home"},
		model.Topic{URL: second, Title: "Second"},
	), fetcher, t.TempDir(), false, 1<<20, 4<<20, 2, func(progress Progress) {
		events = append(events, progress)
	})
	if err != nil || result.Status != "complete" {
		t.Fatalf("observed archive failed: result=%+v err=%v", result, err)
	}
	last := events[len(events)-1]
	if last.PlannedTopics != 2 || last.TopicsValidated != 2 ||
		last.PagesEmitted != 2 || last.PagesTotal != 2 ||
		last.FilesHashed != 10 || last.FilesTotal != 10 ||
		last.ReferenceFilesChecked != 4 || last.ReferenceFilesTotal != 4 ||
		last.AssetsDiscovered != 1 || last.AssetsCompleted != 1 ||
		last.Stage != ProgressFinal || !last.Final {
		t.Fatalf("progress counted duplicate topics/assets: events=%+v", events)
	}
	wantStages := []ProgressStage{
		ProgressTopics, ProgressBuilding, ProgressHashing,
		ProgressReferences, ProgressManifest, ProgressFinal,
	}
	if got := progressStages(events); !slices.Equal(got, wantStages) {
		t.Fatalf("unexpected progress stage sequence: got=%v want=%v", got, wantStages)
	}
	assertMonotonicProgress(t, events)
	finals := 0
	for _, event := range events {
		if event.SchemaVersion != ProgressSchemaVersion {
			t.Fatalf("progress schema=%d, want %d", event.SchemaVersion, ProgressSchemaVersion)
		}
		if event.Final {
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("archive emitted %d final progress snapshots: %+v", finals, events)
	}
	if fetcher.calls[testHome] != 2 || fetcher.calls[second] != 2 || fetcher.calls[image] != 1 {
		t.Fatalf("observer changed two-pass/dedup behavior: %+v", fetcher.calls)
	}
}

func TestArchiveProgressNoAssetsAndSupplementaryPageTotal(t *testing.T) {
	extra := testRoot + "Content/extra.htm"
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><a href="extra.htm">Extra</a></main>`)),
		extra: fixtureResource(extra, "text/html", []byte(
			`<main id="mc-main-content"><h1>Extra</h1></main>`)),
	}, calls: map[string]int{}}
	var events []Progress
	result, err := HTMLWithWorkersObserved(
		context.Background(), flarePlan(), fetcher, t.TempDir(),
		false, 1<<20, 4<<20, 1, func(progress Progress) {
			events = append(events, progress)
		},
	)
	if err != nil || result.Status != "complete" {
		t.Fatalf("supplementary archive failed: result=%+v err=%v", result, err)
	}
	last := events[len(events)-1]
	if last.PlannedTopics != 1 || last.TopicsValidated != 1 ||
		last.PagesTotal != 2 || last.PagesEmitted != 2 ||
		last.AssetsDiscovered != 0 || last.AssetsCompleted != 0 ||
		last.FilesTotal != 9 || last.FilesHashed != 9 ||
		last.ReferenceFilesTotal != 4 || last.ReferenceFilesChecked != 4 {
		t.Fatalf("supplementary/no-asset progress is not truthful: %+v", last)
	}
	assertMonotonicProgress(t, events)
}

func TestArchiveProgressTracksGrowingCSSAndSVGAssetsDuringBuilding(t *testing.T) {
	style := testRoot + "Resources/style.css"
	vector := testRoot + "Resources/vector.svg"
	image := testRoot + "Resources/image.png"
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<html><head><link rel="stylesheet" href="../Resources/style.css"></head><body><main id="mc-main-content"><h1>Home</h1><img src="../Resources/vector.svg"></main></body></html>`)),
		style: fixtureResource(style, "text/css", []byte(
			`.hero{background-image:url("image.png")}`)),
		vector: fixtureResource(vector, "image/svg+xml", []byte(
			`<svg xmlns="http://www.w3.org/2000/svg"><image href="image.png"/></svg>`)),
		image: fixtureResource(image, "image/png", tinyPNG),
	}, calls: map[string]int{}}
	var events []Progress
	result, err := HTMLWithWorkersObserved(
		context.Background(), flarePlan(), fetcher, t.TempDir(),
		false, 1<<20, 4<<20, 1, func(progress Progress) {
			events = append(events, progress)
		},
	)
	if err != nil || result.Status != "complete" {
		t.Fatalf("dynamic dependency archive failed: result=%+v err=%v", result, err)
	}
	discovered := 0
	for _, event := range events {
		if event.AssetsDiscovered > discovered {
			if event.Stage != ProgressBuilding {
				t.Fatalf("asset inventory grew outside building stage: %+v", event)
			}
			discovered = event.AssetsDiscovered
		}
	}
	if discovered != 3 || events[len(events)-1].AssetsCompleted != 3 {
		t.Fatalf("dynamic dependency counts are wrong: events=%+v", events)
	}
	assertMonotonicProgress(t, events)
}

func TestArchiveProgressFailuresDoNotCompleteFailedStages(t *testing.T) {
	t.Run("missing asset remains dynamic metadata", func(t *testing.T) {
		fetcher := &archiveFixture{resources: map[string]model.Resource{
			testHome: fixtureResource(testHome, "text/html", []byte(
				`<main id="mc-main-content"><h1>Home</h1><img src="../Resources/missing.png"></main>`)),
		}, calls: map[string]int{}}
		var events []Progress
		result, err := HTMLWithWorkersObserved(
			context.Background(), flarePlan(), fetcher, t.TempDir(),
			false, 1<<20, 4<<20, 1, func(progress Progress) {
				events = append(events, progress)
			},
		)
		if err != nil || result.Status != "incomplete" {
			t.Fatalf("missing asset result=%+v err=%v", result, err)
		}

		last := events[len(events)-1]
		if last.AssetsDiscovered != 1 || last.AssetsCompleted != 1 ||
			last.PagesEmitted != 1 || last.PagesTotal != 1 {
			t.Fatalf("missing asset corrupted page/dynamic counts: %+v", last)
		}
	})

	t.Run("rewrite error keeps page completion separate", func(t *testing.T) {
		fetcher := &archiveFixture{resources: map[string]model.Resource{
			testHome: fixtureResource(testHome, "text/html", []byte(
				`<main id="mc-main-content"><h1>Home</h1><p style="background:url(blob:publisher)">Body</p></main>`)),
		}, calls: map[string]int{}}
		var events []Progress
		result, err := HTMLWithWorkersObserved(
			context.Background(), flarePlan(), fetcher, t.TempDir(),
			false, 1<<20, 4<<20, 1, func(progress Progress) {
				events = append(events, progress)
			},
		)
		if err != nil || result.Status != "incomplete" {
			t.Fatalf("rewrite error result=%+v err=%v", result, err)
		}
		last := events[len(events)-1]
		if last.PagesEmitted != 1 || last.PagesTotal != 1 {
			t.Fatalf("rewrite diagnostic was confused with page emission: %+v", last)
		}
	})

	t.Run("page write failure", func(t *testing.T) {
		output := t.TempDir()
		fetcher := singlePageArchiveFixture()
		var events []Progress
		injected := false
		_, err := HTMLWithWorkersObserved(
			context.Background(), flarePlan(), fetcher, output,
			false, 1<<20, 4<<20, 1, func(progress Progress) {
				events = append(events, progress)
				if progress.Stage == ProgressBuilding && progress.PagesEmitted == 0 && !injected {
					injected = true
					name := fileName(testHome, "Home", ".html")
					if mkdirErr := os.Mkdir(filepath.Join(output, "pages", name), 0o700); mkdirErr != nil {
						t.Fatal(mkdirErr)
					}
				}
			},
		)
		if err == nil || !injected {
			t.Fatalf("page write failure was not surfaced: err=%v injected=%v", err, injected)
		}
		for _, event := range events {
			if event.Stage == ProgressBuilding && event.PagesTotal > 0 &&
				event.PagesEmitted >= event.PagesTotal {
				t.Fatalf("failed page write falsely reached 100%%: %+v", event)
			}
		}
	})

	t.Run("hash failure", func(t *testing.T) {
		output := t.TempDir()
		fetcher := singlePageArchiveFixture()
		var events []Progress
		injected := false
		result, err := HTMLWithWorkersObserved(
			context.Background(), flarePlan(), fetcher, output,
			false, 1<<20, 4<<20, 1, func(progress Progress) {
				events = append(events, progress)
				if progress.Stage == ProgressHashing && progress.FilesHashed == 0 && !injected {
					injected = true
					entries, readErr := os.ReadDir(filepath.Join(output, "pages"))
					if readErr != nil || len(entries) != 1 {
						t.Fatalf("locate emitted page: entries=%v err=%v", entries, readErr)
					}
					if removeErr := os.Remove(filepath.Join(output, "pages", entries[0].Name())); removeErr != nil {
						t.Fatal(removeErr)
					}
				}
			},
		)
		if err != nil || result.Status != "incomplete" || !injected {
			t.Fatalf("hash failure result=%+v err=%v injected=%v", result, err, injected)
		}
		assertStageDidNotReachTotal(t, events, ProgressHashing)
	})

	t.Run("reference failure", func(t *testing.T) {
		output := t.TempDir()
		fetcher := singlePageArchiveFixture()
		var events []Progress
		injected := false
		result, err := HTMLWithWorkersObserved(
			context.Background(), flarePlan(), fetcher, output,
			false, 1<<20, 4<<20, 1, func(progress Progress) {
				events = append(events, progress)
				if progress.Stage == ProgressReferences && progress.ReferenceFilesChecked == 0 && !injected {
					injected = true
					if writeErr := os.WriteFile(
						filepath.Join(output, "index.html"),
						[]byte(`<html><body><img src="missing.png"></body></html>`),
						0o600,
					); writeErr != nil {
						t.Fatal(writeErr)
					}
				}
			},
		)
		if err != nil || result.Status != "incomplete" || !injected {
			t.Fatalf("reference failure result=%+v err=%v injected=%v", result, err, injected)
		}
		assertStageDidNotReachTotal(t, events, ProgressReferences)
	})

	t.Run("manifest failure", func(t *testing.T) {
		output := t.TempDir()
		fetcher := singlePageArchiveFixture()
		var events []Progress
		injected := false
		_, err := HTMLWithWorkersObserved(
			context.Background(), flarePlan(), fetcher, output,
			false, 1<<20, 4<<20, 1, func(progress Progress) {
				events = append(events, progress)
				if progress.Stage == ProgressManifest && !injected {
					injected = true
					if mkdirErr := os.Mkdir(filepath.Join(output, "manifest.json"), 0o700); mkdirErr != nil {
						t.Fatal(mkdirErr)
					}
				}
			},
		)
		if err == nil || !injected {
			t.Fatalf("manifest failure was not surfaced: err=%v injected=%v", err, injected)
		}
		if slices.Contains(progressStages(events), ProgressFinal) {
			t.Fatalf("manifest failure emitted final progress: %+v", events)
		}
	})
}

func TestMissingStandaloneRasterPublishesDegradedPlaceholder(t *testing.T) {
	missing := testRoot + "Resources/missing.png"
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><figure><img src="../Resources/missing.png" alt="Topology" width="640" height="480"><figcaption>Network topology</figcaption></figure></main>`)),
		missing: {URL: missing, Status: http.StatusNotFound, Headers: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))},
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "degraded" || result.HTML == nil ||
		result.HTML.Status != "degraded" || len(result.Errors) != 0 ||
		len(result.MissingResources) != 1 || len(result.HTML.MissingResources) != 1 {
		t.Fatalf("degraded raster result=%+v err=%v", result, err)
	}
	record := result.MissingResources[0]
	if record.RequestedURL != missing || record.HTTPStatus != http.StatusNotFound ||
		record.FailureClass != "http-404" || record.Alt != "Topology" ||
		record.Caption != "Network topology" || record.DeclaredWidth != 640 ||
		record.DeclaredHeight != 480 || record.GeneratedPageSHA256 == "" {
		t.Fatalf("missing-resource provenance=%+v", record)
	}
	page, readErr := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if readErr != nil || !bytes.Contains(page, []byte(`class="archive-image-placeholder"`)) ||
		!bytes.Contains(page, []byte(`role="img"`)) ||
		!bytes.Contains(page, []byte(`aria-label="Topology. Image unavailable - http-404`)) ||
		bytes.Contains(page, []byte("<img")) {
		t.Fatalf("invalid degraded placeholder page=%s err=%v", page, readErr)
	}
}

func TestEligibleRasterFailuresBecomeDegradedPlaceholders(t *testing.T) {
	cases := []struct {
		name      string
		resource  model.Resource
		failure   error
		wantClass string
	}{
		{name: "gone", resource: model.Resource{Status: http.StatusGone}, wantClass: "http-410"},
		{name: "timeout", failure: context.DeadlineExceeded, wantClass: "timeout"},
		{name: "unexpected eof", failure: io.ErrUnexpectedEOF, wantClass: "incomplete-body"},
		{name: "retry exhausted", failure: &fetch.RetrievalError{
			Stage: fetch.StageBody, ApplicationRetries: 2, Cause: errors.New("connection reset"),
		}, wantClass: "retry-exhausted"},
		{name: "invalid raster", resource: fixtureResource("", "image/png", []byte("truncated")), wantClass: "invalid-raster"},
		{name: "mime mismatch", resource: fixtureResource("", "text/plain", tinyPNG), wantClass: "mime-mismatch"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			missing := testRoot + "Resources/missing.png"
			resource := test.resource
			resource.URL = missing
			if resource.Body == nil {
				resource.Body = io.NopCloser(bytes.NewReader(nil))
			}
			fetcher := &archiveFixture{
				resources: map[string]model.Resource{
					testHome: fixtureResource(testHome, "text/html", []byte(
						`<main id="mc-main-content"><h1>Home</h1><img src="../Resources/missing.png"></main>`)),
					missing: resource,
				},
				failures: map[string]error{missing: test.failure},
				calls:    map[string]int{},
			}
			result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
			if err != nil || result.Status != "degraded" || len(result.MissingResources) != 1 ||
				result.MissingResources[0].FailureClass != test.wantClass || len(result.Errors) != 0 {
				t.Fatalf("%s result=%+v err=%v", test.name, result, err)
			}
		})
	}
}

func TestPictureUsesWorkingRasterFallbackWithoutDegradation(t *testing.T) {
	missing := testRoot + "Resources/missing.png"
	working := testRoot + "Resources/working.png"
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><picture><source srcset="../Resources/missing.png"><img src="../Resources/working.png" alt="Topology"></picture></main>`)),
		missing: {URL: missing, Status: http.StatusNotFound, Headers: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))},
		working: fixtureResource(working, "image/png", tinyPNG),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" || len(result.MissingResources) != 0 ||
		result.HTML == nil || len(result.HTML.Assets) != 1 {
		t.Fatalf("working picture fallback result=%+v err=%v", result, err)
	}
	page, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil || bytes.Contains(page, []byte("<source")) ||
		!bytes.Contains(page, []byte(`src="../assets/`)) {
		t.Fatalf("picture fallback was not normalized offline: %s err=%v", page, err)
	}
}

func TestPictureAllFailedCandidatesProduceOnePlaceholder(t *testing.T) {
	first := testRoot + "Resources/first.png"
	second := testRoot + "Resources/second.png"
	notFound := func(raw string) model.Resource {
		return model.Resource{
			URL: raw, Status: http.StatusNotFound, Headers: http.Header{},
			Body: io.NopCloser(bytes.NewReader(nil)),
		}
	}
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><picture><source srcset="../Resources/first.png"><img src="../Resources/second.png" alt="Topology"></picture></main>`)),
		first: notFound(first), second: notFound(second),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "degraded" || len(result.MissingResources) != 2 ||
		model.ImagePlaceholderCount(result.MissingResources) != 1 {
		t.Fatalf("failed picture candidates result=%+v err=%v", result, err)
	}
	page, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil || bytes.Count(page, []byte(`class="archive-image-placeholder"`)) != 1 ||
		bytes.Contains(page, []byte("<picture")) || bytes.Contains(page, []byte("<source")) ||
		bytes.Contains(page, []byte("<img")) {
		t.Fatalf("picture was not atomically replaced: %s err=%v", page, err)
	}
}

func TestRepeatedMissingRasterFetchIsDeduplicatedPerSlotProvenanceRetained(t *testing.T) {
	missing := testRoot + "Resources/missing.png"
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><img src="../Resources/missing.png"><img src="../Resources/missing.png"></main>`)),
		missing: {URL: missing, Status: http.StatusNotFound, Headers: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))},
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
	if err != nil || result.Status != "degraded" || fetcher.calls[missing] != 1 ||
		len(result.MissingResources) != 2 || model.ImagePlaceholderCount(result.MissingResources) != 2 ||
		len(result.Warnings) != 1 {
		t.Fatalf("repeated missing raster result=%+v calls=%v err=%v", result, fetcher.calls, err)
	}
}

func TestImagePlaceholderEscapesAccessibleTextAndCapsTinyGeometry(t *testing.T) {
	node := imagePlaceholderNode(
		"image-placeholder-0123456789abcdef",
		rasterFailure{
			requested: "https://docs.example.test/a?<unsafe>&x=1",
			class:     "http-404", message: `missing "image" <unsafe>`,
		},
		`Port "<1>" & uplink`, "", "", 16, 16,
	)
	var rendered bytes.Buffer
	if err := html.Render(&rendered, node); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	if !strings.Contains(body, `role="img"`) ||
		!strings.Contains(body, `archive-image-placeholder-small`) ||
		!strings.Contains(body, `width:16px;height:16px`) ||
		!strings.Contains(body, `Port &#34;&lt;1&gt;&#34; &amp; uplink`) ||
		strings.Contains(body, `<unsafe>`) {
		t.Fatalf("unsafe or oversized placeholder markup: %s", body)
	}
}

func TestMissingRasterAlsoRequiredByCSSRemainsIncomplete(t *testing.T) {
	missing := testRoot + "Resources/missing.png"
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><img src="../Resources/missing.png"><p style="background:url('../Resources/missing.png')">Required background</p></main>`)),
		missing: {URL: missing, Status: http.StatusNotFound, Headers: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))},
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
	if err != nil || result.Status != "incomplete" || len(result.Errors) == 0 {
		t.Fatalf("CSS-required raster was incorrectly recovered: result=%+v err=%v", result, err)
	}
}

func TestAmbiguousDeclaredImageTypeRemainsIncomplete(t *testing.T) {
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><img type="image/svg+xml" src="../Resources/missing.png"></main>`)),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
	if err != nil || result.Status != "incomplete" || len(result.MissingResources) != 0 ||
		len(result.Errors) == 0 {
		t.Fatalf("ambiguous image classification was recovered: result=%+v err=%v", result, err)
	}
}

func TestArchiveProgressCancellationStopsEveryOperationStage(t *testing.T) {
	for _, stage := range []ProgressStage{
		ProgressTopics, ProgressBuilding, ProgressHashing,
		ProgressReferences, ProgressManifest,
	} {
		t.Run(string(stage), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []Progress
			_, err := HTMLWithWorkersObserved(
				ctx, flarePlan(), singlePageArchiveFixture(), t.TempDir(),
				false, 1<<20, 4<<20, 1, func(progress Progress) {
					events = append(events, progress)
					if progress.Stage == stage {
						cancel()
					}
				},
			)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel in %s returned %v", stage, err)
			}
			stages := progressStages(events)
			if slices.Contains(stages, ProgressFinal) {
				t.Fatalf("cancel in %s emitted final stage: %v", stage, stages)
			}
		})
	}
}

func singlePageArchiveFixture() *archiveFixture {
	return &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(
			`<main id="mc-main-content"><h1>Home</h1><p>Body</p></main>`)),
	}, calls: map[string]int{}}
}

func progressStages(events []Progress) []ProgressStage {
	var stages []ProgressStage
	for _, event := range events {
		if len(stages) == 0 || stages[len(stages)-1] != event.Stage {
			stages = append(stages, event.Stage)
		}
	}
	return stages
}

func assertMonotonicProgress(t *testing.T, events []Progress) {
	t.Helper()
	var last Progress
	for index, event := range events {
		if index > 0 {
			if event.TopicsValidated < last.TopicsValidated ||
				event.PagesEmitted < last.PagesEmitted ||
				event.FilesHashed < last.FilesHashed ||
				event.ReferenceFilesChecked < last.ReferenceFilesChecked ||
				event.AssetsDiscovered < last.AssetsDiscovered ||
				event.AssetsCompleted < last.AssetsCompleted {
				t.Fatalf("progress regressed at %d: before=%+v after=%+v", index, last, event)
			}
		}
		last = event
	}
}

func assertStageDidNotReachTotal(t *testing.T, events []Progress, stage ProgressStage) {
	t.Helper()
	for _, event := range events {
		if event.Stage != stage {
			continue
		}
		completed, total := 0, 0
		switch stage {
		case ProgressHashing:
			completed, total = event.FilesHashed, event.FilesTotal
		case ProgressReferences:
			completed, total = event.ReferenceFilesChecked, event.ReferenceFilesTotal
		}
		if total > 0 && completed >= total {
			t.Fatalf("failed %s stage falsely reached 100%%: %+v", stage, event)
		}
	}
}

func TestHTMLArchiveRejectsSoft404AndUnsupportedSourceFamily(t *testing.T) {
	for _, body := range []string{
		`<html><title>Sign in</title><body><form><input type="password"></form></body></html>`,
		`<html><body><main>Loading...</main><script>renderDocument()</script></body></html>`,
		`<html><body><main id="mc-main-content"><h1>Access denied</h1><p>You do not have permission to access this document.</p></main></body></html>`,
		`<html><body><main id="mc-main-content"><h1>Access denied</h1><p>You do not have permission to access this resource.</p><p>Contact your administrator.</p></main></body></html>`,
		`<html><body><main id="mc-main-content"><h1>Page not found</h1><p>The requested page could not be found.</p></main></body></html>`,
		`<html><body><main id="mc-main-content"><h1>Page not found</h1><p>The requested page could not be found.</p><p>Check the address and try again.</p></main></body></html>`,
		`<html><body><main id="mc-main-content"><h1>Access denied</h1><table><tr><td>Publisher logo</td></tr></table></main></body></html>`,
		`<html><body><main id="mc-main-content"><h1>Page not found</h1><pre>decorative wrapper</pre></main></body></html>`,
		`<html><body><main id="mc-main-content"><h1>Error 500</h1></main></body></html>`,
		`<html><body><main id="mc-main-content"></main></body></html>`,
		`{"pages":[]}`,
	} {
		fetcher := &archiveFixture{resources: map[string]model.Resource{
			testHome: fixtureResource(testHome, "text/html", []byte(body)),
		}, calls: map[string]int{}}
		if result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20); err == nil || result.Status != "failed" {
			t.Fatalf("soft error page accepted: result=%+v err=%v", result, err)
		}
	}
	plan := flarePlan()
	plan.Document.Kind = "hpe"
	if _, err := HTML(context.Background(), plan, &archiveFixture{}, t.TempDir(), false, 1<<20, 2<<20); err == nil {
		t.Fatal("unimplemented HPE output was not gated")
	}
}

func TestShortDocumentationKeywordsAreNotShells(t *testing.T) {
	for _, test := range []struct {
		name string
		plan model.DocumentPlan
		body string
	}{
		{
			name: "flare-short-login-command",
			plan: flarePlan(model.Topic{URL: testHome, Title: "login"}),
			body: `<main id="mc-main-content"><h1>login</h1>` +
				`<p>This command starts a local command-line session.</p></main>`,
		},
		{
			name: "flare-short-error-status",
			plan: flarePlan(model.Topic{URL: testHome, Title: "Error"}),
			body: `<main id="mc-main-content"><h1>Error</h1>` +
				`<p>This status indicates an unsuccessful command.</p></main>`,
		},
		{
			name: "flare-quoted-error-examples",
			plan: flarePlan(model.Topic{URL: testHome, Title: "Access denied"}),
			body: `<main id="mc-main-content"><h1>Access denied</h1>` +
				`<p>This command documents authentication diagnostics.</p>` +
				`<blockquote>You do not have permission to access this resource.</blockquote>` +
				`<table><tr><td>The requested page could not be found.</td></tr></table>` +
				`<pre>request failed` + "\n" + `renderDocument()</pre></main>`,
		},
		{
			name: "static-loading-example",
			plan: model.DocumentPlan{
				Document: model.Document{
					ID: "static", Title: "Static", Platform: "6300", Version: "10.18",
					URL: testHome, Kind: "static",
				},
				Topics: []model.Topic{{URL: testHome, Title: "Loading messages"}},
				TOC:    []model.TocEntry{{Title: "Loading messages", URL: testHome}},
				Inventory: &model.InventoryEvidence{
					Complete: true, Kind: "static", RootURL: testRoot, Entries: 1, UniqueTopics: 1,
				},
			},
			body: `<main class="wh_topic_content"><h1>Loading and access denied messages</h1>` +
				`<p>The switch can report loading while a command runs.</p>` +
				`<pre>resource not found` + "\n" + `access denied` + "\n" + `renderDocument()</pre></main>`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fetcher := &archiveFixture{resources: map[string]model.Resource{
				testHome: fixtureResource(testHome, "text/html", []byte(test.body)),
			}, calls: map[string]int{}}
			result, err := HTML(context.Background(), test.plan, fetcher, t.TempDir(), false, 1<<20, 2<<20)
			if err != nil || result.Status != "complete" {
				t.Fatalf("short documentation was classified as a shell: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestHTMLArchiveCancellationWinsBeforePublicationMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := HTML(ctx, flarePlan(), &archiveFixture{}, t.TempDir(), false, 1<<20, 2<<20)
	if !errors.Is(err, context.Canceled) || result.Status != "failed" || result.HTML != nil {
		t.Fatalf("cancellation did not win: result=%+v err=%v", result, err)
	}
}

func TestCSSParserRewritesNestedFunctionsAndRejectsMalformedInput(t *testing.T) {
	var seen []string
	out, diagnostics, err := rewriteCSS([]byte(`@charset "UTF-8";@namespace x url(http://vocabulary);
@import "base.css";.tab-bar .menu-icon{background:url("runtime.png")}.x{background:image-set(url("a.png") 1x,"b.png" 2x);behavior:url("PIE.htc");--image:url(c.png)}`), false,
		func(reference string, kind dependencyKind) (string, error) {
			seen = append(seen, reference)
			return "local/" + reference, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "@charset") || !strings.Contains(text, "@namespace x url(http://vocabulary)") ||
		!strings.Contains(text, `url("local/a.png")`) || !strings.Contains(text, `"local/b.png"`) ||
		!strings.Contains(text, `--image:url("local/c.png")`) || strings.Contains(text, "behavior") ||
		len(diagnostics.Notices) != 2 || len(diagnostics.Warnings) != 0 ||
		len(seen) != 4 || slices.Contains(seen, "runtime.png") {
		t.Fatalf("unexpected CSS rewrite: %q diagnostics=%+v seen=%v", text, diagnostics, seen)
	}
	if _, _, err := rewriteCSS([]byte(`.x{background:url("unterminated)`), false, func(string, dependencyKind) (string, error) {
		return "", nil
	}); err == nil {
		t.Fatal("malformed CSS accepted")
	}
	_, badURLDiagnostics, err := rewriteCSS([]byte(`.x{background:url(foo bar);color:red}`), false,
		func(string, dependencyKind) (string, error) { return "", nil })
	if err != nil || len(badURLDiagnostics.Warnings) != 1 || len(badURLDiagnostics.Notices) != 0 {
		t.Fatalf("recoverable bad publisher URL did not remain actionable: diagnostics=%+v err=%v", badURLDiagnostics, err)
	}
	filteredSeen := 0
	filtered, _, err := rewriteCSSFiltered([]byte(`p.zIconNote{background:url("missing.png")}p.present{background:url("kept.png")}`), false,
		func(reference string, kind dependencyKind) (string, error) {
			filteredSeen++
			return reference, nil
		}, func(tokens []css.Token) bool {
			return selectorGroupRelevant(tokens, map[string]bool{"present": true}, nil)
		})
	if err != nil || filteredSeen != 1 || strings.Contains(string(filtered), "zIconNote") || !strings.Contains(string(filtered), "present") {
		t.Fatalf("selector-aware CSS filtering failed: %q seen=%d err=%v", filtered, filteredSeen, err)
	}
}

func TestHTMLArchiveBookmarkAndUnsupportedContentClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		status    string
		wantWarn  string
		wantError string
	}{
		{"source-missing", `<main id="mc-main-content"><h1>Home</h1><a href="#missing">Broken at publisher</a></main>`,
			"complete", "Publisher bookmark #missing", ""},
		{"lost-locally", `<main id="mc-main-content"><h1>Home</h1><a href="#inside">Target</a><form id="inside">Removed widget</form></main>`,
			"incomplete", "", "was lost during archival"},
		{"unsupported-embed", `<main id="mc-main-content"><h1>Home</h1><iframe src="https://embed.example.test/diagram"></iframe></main>`,
			"incomplete", "", "unsupported substantive <iframe>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &archiveFixture{resources: map[string]model.Resource{
				testHome: fixtureResource(testHome, "text/html", []byte(tc.body)),
			}, calls: map[string]int{}}
			result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
			if err != nil || result.Status != tc.status {
				t.Fatalf("status=%s err=%v result=%+v", result.Status, err, result)
			}
			if tc.wantWarn != "" && !slices.ContainsFunc(result.Warnings, func(value string) bool { return strings.Contains(value, tc.wantWarn) }) {
				t.Fatalf("missing warning %q: %v", tc.wantWarn, result.Warnings)
			}
			if tc.wantWarn != "" && slices.ContainsFunc(result.Notices, func(value model.Notice) bool {
				return strings.Contains(value.Message, tc.wantWarn)
			}) {
				t.Fatalf("actionable warning %q was also classified as a notice: %+v", tc.wantWarn, result.Notices)
			}
			if tc.wantError != "" && !slices.ContainsFunc(result.Errors, func(value string) bool { return strings.Contains(value, tc.wantError) }) {
				t.Fatalf("missing error %q: %v", tc.wantError, result.Errors)
			}
		})
	}
}

func TestHTMLArchiveCrossPageBookmarkClassification(t *testing.T) {
	second := testRoot + "Content/second.htm"
	for _, tc := range []struct {
		name      string
		target    string
		status    string
		wantWarn  string
		wantError string
	}{
		{
			name: "source-missing-percent-escaped",
			target: `<main id="mc-main-content"><h1>Second</h1>` +
				`<figure><figcaption>Publisher figure remains intact</figcaption></figure></main>`,
			status: "complete", wantWarn: "Publisher bookmark #missing value",
		},
		{
			name: "source-present-lost-locally",
			target: `<main id="mc-main-content"><h1>Second</h1>` +
				`<form id="missing value">Removed publisher widget</form></main>`,
			status: "incomplete", wantError: "was lost during archival",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := t.TempDir()
			source := `<main id="mc-main-content"><h1>Home</h1><figure>` +
				`<a href="` + second + `#missing%20value">Figure reference</a></figure></main>`
			fetcher := &archiveFixture{resources: map[string]model.Resource{
				testHome: fixtureResource(testHome, "text/html", []byte(source)),
				second:   fixtureResource(second, "text/html", []byte(tc.target)),
			}, calls: map[string]int{}}
			result, err := HTML(context.Background(), flarePlan(
				model.Topic{URL: testHome, Title: "Home"},
				model.Topic{URL: second, Title: "Second"},
			), fetcher, output, false, 1<<20, 2<<20)
			if err != nil || result.Status != tc.status {
				t.Fatalf("status=%s err=%v result=%+v", result.Status, err, result)
			}
			if tc.wantWarn != "" && !slices.ContainsFunc(result.Warnings, func(value string) bool {
				return strings.Contains(value, tc.wantWarn)
			}) {
				t.Fatalf("missing warning %q: %v", tc.wantWarn, result.Warnings)
			}
			if tc.wantWarn != "" && slices.ContainsFunc(result.Notices, func(value model.Notice) bool {
				return strings.Contains(value.Message, tc.wantWarn)
			}) {
				t.Fatalf("actionable warning %q was also classified as a notice: %+v", tc.wantWarn, result.Notices)
			}
			if tc.wantError != "" && !slices.ContainsFunc(result.Errors, func(value string) bool {
				return strings.Contains(value, tc.wantError)
			}) {
				t.Fatalf("missing error %q: %v", tc.wantError, result.Errors)
			}
			home, readErr := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
			if readErr != nil || !strings.Contains(string(home), "#missing%20value") {
				t.Fatalf("cross-page fragment was not preserved: %q err=%v", home, readErr)
			}
		})
	}
}

func TestHTMLArchiveRedirectBaseAndRawHashes(t *testing.T) {
	actual := "https://cdn.example.test/redirected/Content/home.htm"
	image := "https://cdn.example.test/redirected/shared/photo.png"
	raw := []byte(`<html><head><base href="../shared/"></head><body><main id="mc-main-content"><h1>Caf&#233;</h1><img src="photo.png"></main></body></html>`)
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(actual, "text/html; charset=utf-8", raw),
		image:    fixtureResource(image, "image/png", tinyPNG),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("redirected archive failed: %+v %v", result, err)
	}
	if result.HTML.Topics[0].FinalURL != actual || result.HTML.Topics[0].SourceSHA256 != sourceHash(raw) ||
		result.HTML.Assets[0].SourceSHA256 != sourceHash(tinyPNG) || result.HTML.Assets[0].SHA256 != sourceHash(tinyPNG) {
		t.Fatalf("source/final identity or raw-byte hashes changed: %+v", result.HTML)
	}
}

func TestHTMLArchiveRejectsUnsafeOutputBeforeFetching(t *testing.T) {
	for _, entry := range []string{"pages", "assets", "index.html", "manifest.json"} {
		t.Run(entry, func(t *testing.T) {
			output := t.TempDir()
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(output, entry)); err != nil {
				t.Skip(err)
			}
			fetcher := &archiveFixture{resources: map[string]model.Resource{}, calls: map[string]int{}}
			if _, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20); err == nil {
				t.Fatal("unsafe output symlink accepted")
			}
			if len(fetcher.calls) != 0 {
				t.Fatalf("fetched before output safety validation: %v", fetcher.calls)
			}
		})
	}
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(output, "pages"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetcher := &archiveFixture{resources: map[string]model.Resource{}, calls: map[string]int{}}
	if _, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20); err == nil {
		t.Fatal("unexpected output file type accepted")
	}
	if len(fetcher.calls) != 0 {
		t.Fatal("fetched before output type validation")
	}
}

func TestSVGFragmentFailureAndMalformedCSSAreIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name, html, assetURL, contentType, asset string
	}{
		{"svg-fragment", `<main id="mc-main-content"><h1>Home</h1><img src="../Resources/broken.svg"></main>`,
			testRoot + "Resources/broken.svg", "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg"><use href="#missing"/></svg>`},
		{"css-syntax", `<html><head><link rel="stylesheet" href="../Resources/broken.css"></head><body><main id="mc-main-content"><h1>Home</h1></main></body></html>`,
			testRoot + "Resources/broken.css", "text/css", `.x{background:url("unterminated)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &archiveFixture{resources: map[string]model.Resource{
				testHome:    fixtureResource(testHome, "text/html", []byte(tc.html)),
				tc.assetURL: fixtureResource(tc.assetURL, tc.contentType, []byte(tc.asset)),
			}, calls: map[string]int{}}
			result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
			if err != nil || result.Status != "incomplete" || len(result.Errors) == 0 {
				t.Fatalf("required dependency failure hidden: %+v %v", result, err)
			}
		})
	}
}

func TestExternalTOCNavigationIsRetainedWithoutRetrieval(t *testing.T) {
	external := "https://community.example.test/support/knowledge-base"
	plan := flarePlan()
	plan.TOC = append(plan.TOC, model.TocEntry{Title: "Knowledge Base", URL: external})
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(`<main id="mc-main-content"><h1>Home</h1></main>`)),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" || len(result.HTML.Topics) != 1 || fetcher.calls[external] != 0 {
		t.Fatalf("external TOC entry was treated as a topic: result=%+v calls=%v err=%v", result, fetcher.calls, err)
	}
	toc, _ := os.ReadFile(filepath.Join(output, "toc.html"))
	if !bytes.Contains(toc, []byte(external)) || !bytes.Contains(toc, []byte("archive-online")) {
		t.Fatalf("external navigation was not retained: %s", toc)
	}
}

func TestPublisherReleaseMismatchIsWarnedWithoutRelabeling(t *testing.T) {
	body := `<html><body><div class="Variables.Doc_Title">AOS-CX 10.15 Job Scheduler Guide</div>
<main id="mc-main-content"><h1>Source title</h1></main></body></html>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(body)),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" ||
		!slices.ContainsFunc(result.Warnings, func(value string) bool {
			return strings.Contains(value, "does not match selected release 10.16")
		}) ||
		slices.ContainsFunc(result.Notices, func(value model.Notice) bool {
			return strings.Contains(value.Message, "does not match selected release 10.16")
		}) {
		t.Fatalf("publisher mismatch not retained as warning: %+v %v", result, err)
	}
	page, _ := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if !bytes.Contains(page, []byte("Source title")) || bytes.Contains(page, []byte("AOS-CX 10.15")) {
		t.Fatalf("source content was relabeled or shell leaked: %s", page)
	}
}

func TestDuplicatePublisherIDRemainsAnActionableWarning(t *testing.T) {
	body := `<main id="mc-main-content"><h1 id="duplicate">Home</h1><p id="duplicate">Text</p></main>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(body)),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), flarePlan(), fetcher, t.TempDir(), false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" ||
		!slices.ContainsFunc(result.Warnings, func(value string) bool {
			return strings.Contains(value, "removed duplicate publisher ID #duplicate")
		}) ||
		slices.ContainsFunc(result.Notices, func(value model.Notice) bool {
			return strings.Contains(value.Message, "duplicate publisher ID")
		}) {
		t.Fatalf("duplicate publisher ID severity changed: result=%+v err=%v", result, err)
	}
}

func TestSrcsetParserPreservesDataURLCommaAndDescriptors(t *testing.T) {
	candidates, err := parseSrcset(`data:image/png;base64,AAAA 1x, image%20two.png 2x, plain.png`)
	if err != nil || len(candidates) != 3 ||
		candidates[0][0] != "data:image/png;base64,AAAA" || candidates[0][1] != "1x" ||
		candidates[1][0] != "image%20two.png" || candidates[1][1] != "2x" ||
		len(candidates[2]) != 1 || candidates[2][0] != "plain.png" {
		t.Fatalf("srcset parse failed: %#v %v", candidates, err)
	}
}
