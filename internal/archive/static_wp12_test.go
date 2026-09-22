package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

const staticWP12Root = "https://static.example.test/guide/"

func staticWP12ArchivePlan() model.DocumentPlan {
	home := staticWP12Root + "index.html?release=10.18"
	topic := staticWP12Root + "topics/commands.html?view=full"
	return model.DocumentPlan{
		Document: model.Document{
			ID: "static-wp12", Title: "Static WP12 Guide", Platform: "6300",
			Version: "10.18", Kind: "static", URL: home,
		},
		Topics: []model.Topic{
			{URL: home, Title: "Home"},
			{URL: topic, Title: "Commands"},
		},
		TOC: []model.TocEntry{
			{Title: "Home", URL: home},
			{Title: "Command reference", Children: []model.TocEntry{
				{Title: "Commands", URL: topic + "#commands"},
				{Title: "Commands repeated", URL: topic + "#examples"},
			}},
		},
		Inventory: &model.InventoryEvidence{
			Complete: true, Kind: "static", RootURL: staticWP12Root,
			TOCURL: staticWP12Root + "toc.html", Entries: 4, UniqueTopics: 2,
		},
	}
}

func staticWP12Resources() map[string]model.Resource {
	home := staticWP12Root + "index.html?release=10.18"
	topic := staticWP12Root + "topics/commands.html?view=full"
	style := staticWP12Root + "styles/site.css"
	font := staticWP12Root + "fonts/oxygen.woff2"
	background := staticWP12Root + "images/background.png"
	photo := staticWP12Root + "images/photo.png"
	vector := staticWP12Root + "images/whole.svg"
	resources := map[string]model.Resource{
		home: fixtureResource(staticWP12Root+"published/index.html?release=10.18", "text/html", []byte(
			`<html><head><base href="../content/"><link rel="stylesheet" href="../styles/site.css"></head><body>
<header class="wh_header">Publisher shell</header><main class="wh_topic_content"><h1 id="home">Static guide</h1>
<p><a href="../topics/commands.html?view=full#commands">Commands</a></p>
<figure><img src="../images/photo.png" alt="Switch"><figcaption>Figure 1. Switch</figcaption></figure>
<img src="../images/whole.svg" alt="Whole vector">
<details><summary>More</summary><p>Expanded static details</p></details><script>loadNavigation()</script>
</main></body></html>`)),
		topic: fixtureResource(topic, "application/xhtml+xml", []byte(
			`<html><body><article><h1 id="commands">Commands</h1>
<table><caption>Command values</caption><tr><th rowspan="2">Command</th><th colspan="2">Output</th></tr>
<tr><td id="examples">State</td><td>Meaning</td></tr></table>
<pre><code>switch# show system
  exact  spacing	kept</code></pre>
<a href="../index.html?release=10.18#home">Home</a></article></body></html>`)),
		style: fixtureResource(style, "text/css", []byte(
			`@font-face{font-family:"Oxygen Fixture";src:url("../fonts/oxygen.woff2") format("woff2")}
.wh_topic_content{font-family:"Oxygen Fixture";background-image:url("../images/background.png")}`)),
		font:       fixtureResource(font, "font/woff2", append([]byte("wOF2"), bytes.Repeat([]byte{0}, 12)...)),
		background: fixtureResource(background, "image/png", tinyPNG),
		photo:      fixtureResource(photo, "image/png", tinyPNG),
		vector: fixtureResource(vector, "image/svg+xml", []byte(
			`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 40 20"><defs>
<linearGradient id="paint"><stop stop-color="#fff"/></linearGradient></defs>
<rect id="panel" width="40" height="20" fill="url(#paint)"/></svg>`)),
	}
	return resources
}

func TestStaticAuthoredArchiveCorpusPreservesAssetsSemanticsAndIdentity(t *testing.T) {
	plan := staticWP12ArchivePlan()
	fetcher := &archiveFixture{resources: staticWP12Resources(), calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("static authored corpus failed: result=%+v err=%v", result, err)
	}
	if len(result.HTML.Topics) != 2 || len(result.HTML.Assets) != 5 ||
		result.HTML.Topics[0].URL != plan.Topics[0].URL ||
		result.HTML.Topics[0].FinalURL != staticWP12Root+"published/index.html?release=10.18" ||
		result.HTML.Integrity.CheckedLinks == 0 {
		t.Fatalf("static corpus identity/inventory incomplete: %+v", result.HTML)
	}
	for _, raw := range []string{
		staticWP12Root + "styles/site.css",
		staticWP12Root + "fonts/oxygen.woff2",
		staticWP12Root + "images/background.png",
		staticWP12Root + "images/photo.png",
		staticWP12Root + "images/whole.svg",
	} {
		if fetcher.calls[raw] != 1 {
			t.Fatalf("required static dependency call count changed: %s calls=%v", raw, fetcher.calls)
		}
	}

	homeBytes, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	topicBytes, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[1].Path))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(homeBytes, []byte("Publisher shell")) || bytes.Contains(homeBytes, []byte("<script")) ||
		!bytes.Contains(homeBytes, []byte("<details open")) ||
		!bytes.Contains(homeBytes, []byte("Expanded static details")) ||
		!bytes.Contains(homeBytes, []byte(`linearGradient`)) ||
		!bytes.Contains(homeBytes, []byte(`class="archive-source-static"`)) {
		t.Fatalf("static shell/details/vector handling changed: %s", homeBytes)
	}
	if !bytes.Contains(topicBytes, []byte(`rowspan="2"`)) ||
		!bytes.Contains(topicBytes, []byte(`colspan="2"`)) ||
		!bytes.Contains(topicBytes, []byte("<caption>Command values</caption>")) ||
		!bytes.Contains(topicBytes, []byte("switch# show system\n  exact  spacing\tkept")) {
		t.Fatalf("static table/code fidelity changed: %s", topicBytes)
	}
	doc, err := html.Parse(bytes.NewReader(homeBytes))
	if err != nil {
		t.Fatal(err)
	}
	link := findNode(doc, func(node *html.Node) bool {
		return node.Data == "a" && strings.TrimSpace(nodeText(node)) == "Commands"
	})
	if link == nil || strings.HasPrefix(attr(link, "href"), "http") ||
		!strings.HasSuffix(attr(link, "href"), "#commands") {
		t.Fatalf("meaningful static query/fragment was not localized: %s", homeBytes)
	}
}

func TestStaticRasterDegradesButRequiredDependenciesAndTopicsFailClosed(t *testing.T) {
	plan := staticWP12ArchivePlan()
	for _, test := range []struct {
		name        string
		target      string
		replacement model.Resource
		wantStatus  string
		wantMissing int
	}{
		{
			name: "standalone-raster", target: staticWP12Root + "images/photo.png",
			replacement: model.Resource{
				URL: staticWP12Root + "images/photo.png", Status: http.StatusNotFound,
				Headers: http.Header{}, Body: http.NoBody,
			},
			wantStatus: "degraded", wantMissing: 1,
		},
		{
			name: "stylesheet", target: staticWP12Root + "styles/site.css",
			replacement: model.Resource{
				URL: staticWP12Root + "styles/site.css", Status: http.StatusNotFound,
				Headers: http.Header{}, Body: http.NoBody,
			},
			wantStatus: "incomplete",
		},
		{
			name: "font", target: staticWP12Root + "fonts/oxygen.woff2",
			replacement: model.Resource{
				URL: staticWP12Root + "fonts/oxygen.woff2", Status: http.StatusNotFound,
				Headers: http.Header{}, Body: http.NoBody,
			},
			wantStatus: "incomplete",
		},
		{
			name: "svg", target: staticWP12Root + "images/whole.svg",
			replacement: fixtureResource(staticWP12Root+"images/whole.svg", "image/svg+xml",
				[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><use href="#missing"/></svg>`)),
			wantStatus: "incomplete",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resources := staticWP12Resources()
			resources[test.target] = test.replacement
			result, err := HTML(context.Background(), plan,
				&archiveFixture{resources: resources, calls: map[string]int{}},
				t.TempDir(), false, 1<<20, 4<<20)
			if err != nil || result.Status != test.wantStatus ||
				len(result.MissingResources) != test.wantMissing {
				t.Fatalf("static dependency classification changed: result=%+v err=%v", result, err)
			}
			if test.wantStatus == "incomplete" && len(result.Errors) == 0 {
				t.Fatalf("critical static dependency lacked error: %+v", result)
			}
		})
	}

	t.Run("topic", func(t *testing.T) {
		resources := staticWP12Resources()
		topic := plan.Topics[1].URL
		resources[topic] = model.Resource{
			URL: topic, Status: http.StatusNotFound, Headers: http.Header{}, Body: http.NoBody,
		}
		result, err := HTML(context.Background(), plan,
			&archiveFixture{resources: resources, calls: map[string]int{}},
			t.TempDir(), false, 1<<20, 4<<20)
		if err == nil || result.Status != "failed" || len(result.Errors) == 0 {
			t.Fatalf("missing static topic did not fail closed: result=%+v err=%v", result, err)
		}
	})
}

func TestStaticAuthoredShellCorpusFailsClosed(t *testing.T) {
	home := staticWP12Root + "index.html"
	plan := staticWP12ArchivePlan()
	plan.Document.URL = home
	plan.Topics = []model.Topic{{URL: home, Title: "Home"}}
	plan.TOC = []model.TocEntry{{Title: "Home", URL: home}}
	plan.Inventory.Entries, plan.Inventory.UniqueTopics = 1, 1
	for _, test := range []struct {
		name string
		body string
	}{
		{"authentication", `<main class="wh_topic_content"><h1>Sign in</h1><form><input type="password"></form></main>`},
		{"error", `<main class="wh_topic_content"><h1>Page not found</h1><p>The requested page could not be found.</p></main>`},
		{"loading", `<main class="wh_topic_content"><h1>Loading content</h1></main>`},
		{"javascript", `<main class="wh_topic_content"><h1>Loading</h1><p>Please wait while content loads.</p></main><script>renderDocument()</script>`},
		{"json", `{"pages":[]}`},
		{"empty", `<main class="wh_topic_content"> </main>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fetcher := &archiveFixture{resources: map[string]model.Resource{
				home: fixtureResource(home, "text/html", []byte(test.body)),
			}, calls: map[string]int{}}
			result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 2<<20)
			if err == nil || result.Status != "failed" || len(result.Errors) == 0 {
				t.Fatalf("static publisher shell accepted: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestStaticArchiveRejectsAmbiguousOrOutOfScopeTopicBase(t *testing.T) {
	home := staticWP12Root + "index.html"
	plan := staticWP12ArchivePlan()
	plan.Document.URL = home
	plan.Topics = []model.Topic{{URL: home, Title: "Home"}}
	plan.TOC = []model.TocEntry{{Title: "Home", URL: home}}
	plan.Inventory.Entries, plan.Inventory.UniqueTopics = 1, 1
	for _, test := range []struct {
		name string
		base string
		want string
	}{
		{"multiple", `<base href="./"><base href="topics/">`, "ambiguous"},
		{"empty", `<base href="">`, "empty href"},
		{"foreign", `<base href="https://outside.example.test/guide/">`, "outside"},
		{"traversal", `<base href="../other/">`, "outside"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `<html><head>` + test.base + `</head><body><main class="wh_topic_content"><h1>Home</h1></main></body></html>`
			fetcher := &archiveFixture{resources: map[string]model.Resource{
				home: fixtureResource(home, "text/html", []byte(body)),
			}, calls: map[string]int{}}
			result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 2<<20)
			if err == nil || result.Status != "failed" ||
				!strings.Contains(strings.ToLower(strings.Join(result.Errors, " ")), test.want) {
				t.Fatalf("unsafe static topic base accepted: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestStaticArchiveHonorsCancellationAndResourceLimits(t *testing.T) {
	plan := staticWP12ArchivePlan()
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fetcher := &archiveFixture{resources: staticWP12Resources(), calls: map[string]int{}}
		result, err := HTML(ctx, plan, fetcher, t.TempDir(), false, 1<<20, 4<<20)
		if !errors.Is(err, context.Canceled) || result.Status != "failed" || len(fetcher.calls) != 0 {
			t.Fatalf("cancelled static archive continued: result=%+v calls=%v err=%v", result, fetcher.calls, err)
		}
	})
	t.Run("per-resource", func(t *testing.T) {
		fetcher := &archiveFixture{resources: staticWP12Resources(), calls: map[string]int{}}
		result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 64, 4<<20)
		if err == nil || result.Status != "failed" ||
			!strings.Contains(strings.Join(result.Errors, " "), "byte limit") {
			t.Fatalf("static resource limit ignored: result=%+v err=%v", result, err)
		}
	})
	t.Run("aggregate", func(t *testing.T) {
		fetcher := &archiveFixture{resources: staticWP12Resources(), calls: map[string]int{}}
		result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 1000)
		if err != nil || result.Status != "incomplete" ||
			!strings.Contains(strings.Join(result.Errors, " "), "aggregate source-byte limit") {
			t.Fatalf("static aggregate limit ignored: result=%+v err=%v", result, err)
		}
	})
}

func TestStaticArchiveRetainsOnlyExpectedFetches(t *testing.T) {
	plan := staticWP12ArchivePlan()
	fetcher := &archiveFixture{resources: staticWP12Resources(), calls: map[string]int{}}
	result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("static fetch inventory failed: result=%+v err=%v", result, err)
	}
	for _, forbidden := range []string{
		staticWP12Root + "toc.json",
		staticWP12Root + "runtime.js",
		staticWP12Root + "shortcut.html",
	} {
		if fetcher.calls[forbidden] != 0 {
			t.Fatalf("static archive crawled unplanned resource: %s calls=%v", forbidden, fetcher.calls)
		}
	}
	if !slices.ContainsFunc(result.HTML.Assets, func(record model.FileRecord) bool {
		return record.URL == staticWP12Root+"fonts/oxygen.woff2" && record.ContentType == "font/woff2"
	}) {
		t.Fatalf("static font provenance missing: %+v", result.HTML.Assets)
	}
}

func TestStaticArchiveUsesSharedRetryBudget(t *testing.T) {
	var imageAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/guide/Content/home.htm":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<main class="wh_topic_content"><h1>Home</h1><img src="../Resources/image.png"></main>`)
		case "/guide/Resources/image.png":
			if imageAttempts.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(tinyPNG)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	result := archiveWithRetryClientForKind(t, server.URL, 1, 3*time.Second, "static")
	if result.Status != "complete" || len(result.MissingResources) != 0 || imageAttempts.Load() != 2 {
		t.Fatalf("static shared retry path changed: result=%+v attempts=%d", result, imageAttempts.Load())
	}
}

func TestStaticTopicReadFailurePreservesWrappedCancellation(t *testing.T) {
	plan := staticWP12ArchivePlan()
	topic := plan.Topics[1].URL
	fetcher := &archiveFixture{
		resources: staticWP12Resources(), calls: map[string]int{},
		failures: map[string]error{topic: context.Canceled},
	}
	result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 4<<20)
	if !errors.Is(err, context.Canceled) || result.Status != "failed" {
		t.Fatalf("static topic cancellation identity lost: result=%+v err=%v", result, err)
	}
}
