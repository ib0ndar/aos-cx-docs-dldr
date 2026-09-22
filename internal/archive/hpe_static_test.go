package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

var renderPNG, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jf1kAAAAASUVORK5CYII=")

const aclPublisherMissingFigure = "GUID-2AE6F40D-168D-44E3-A7E7-C39AFB9BA723__FIG_D65E1_D66E1_D67E1_D68E1_D69E1_D70E1_1E010F97565C4046A33D2804C37DBDFA"

func hpeArchivePlan() model.DocumentPlan {
	host := "https://support.example.test"
	id := "doc-A"
	public := host + "/hpesc/public/docDisplay?docId=" + id
	api := host + "/hpesc/public/api/document/" + id
	topic := public + "&page=GUID-topic.html"
	return model.DocumentPlan{
		Document: model.Document{ID: "hpe", Title: "HPE Guide", Platform: "6300", Version: "10.18.xxxx",
			URL: public + "&mask=sh-rs", Kind: "hpe"},
		Topics: []model.Topic{
			{URL: public, Title: "Front", FetchURL: api + "?ignorePayload=true"},
			{URL: topic, Title: "Topic", FetchURL: api + "?page=GUID-topic.html"},
		},
		TOC: []model.TocEntry{{Title: "Guide", URL: public + "&mask=sh-rs#front",
			Children: []model.TocEntry{{Title: "Topic", URL: topic + "#topic"}}}},
		Styles: []string{host + "/resource3/doc-resources/css/hpesc-doc.css"},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "hpe", RootURL: api,
			TOCURL: api + "?page=content.json", Entries: 2, UniqueTopics: 2},
	}
}

func hpeResources() map[string]model.Resource {
	host := "https://support.example.test"
	api := host + "/hpesc/public/api/document/doc-A"
	public := host + "/hpesc/public/docDisplay?docId=doc-A"
	image := api + "/GUID-image.png?v=1"
	style := host + "/resource3/doc-resources/css/hpesc-doc.css"
	return map[string]model.Resource{
		api + "?ignorePayload=true": {
			URL: api + "?ignorePayload=true", Status: 200,
			Headers: http.Header{"Content-Type": {"multiPage;charset=UTF-8"}, "Doc-Id": {"doc-A"}, "Doc-Page-Name": {"index.html"}},
			Body: io.NopCloser(strings.NewReader(`<main role="main" class="ditasrc"><h1 id="front">Front matter</h1>
<div class="publishedDate">Published: September 2026</div><div class="copyright">Copyright 2026 Publisher</div>
<a href="?page=content.json">TOC data</a>
<a href="GUID-topic.html#topic">Bare topic</a>
<a href="` + public + `&amp;page=GUID-topic.html#` + aclPublisherMissingFigure + `">Publisher missing bookmark</a></main>`)),
		},
		api + "?page=GUID-topic.html": {
			URL: api + "?page=GUID-topic.html", Status: 200,
			Headers: http.Header{"Content-Type": {"MULTIPAGE; CHARSET=\"UTF-8\""}, "Doc-Id": {"doc-A"}, "Doc-Page-Name": {"GUID-topic.html"}},
			Body: io.NopCloser(strings.NewReader(`<html><head><base href="` + api + `/"></head><body><main role="main" class="ditasrc"><article><h1 id="topic">Topic</h1>
<figure><img alt="Diagram" src="GUID-image.png?v=1"><figcaption>Figure 1. Diagram</figcaption></figure>
<table class="table"><caption>Parameters</caption><tr><th rowspan="2">Name</th><th colspan="2">Value</th></tr><tr><td>A</td><td>B</td></tr></table>
<table class="table"><tr><th>Convention</th><th>Usage</th></tr>
<tr><td><code class="ph codeph">example‐text</code></td><td>Short token</td></tr>
<tr><td><code class="ph codeph">this-is-an-intentionally-extreme-inline-code-token-that-must-scroll-inside-its-table-cell-without-expanding-the-page</code></td><td>Long token</td></tr></table>
<p>Outside <code class="ph codeph">example‐text</code></p>
<pre>switch# show job
    preserved  spacing</pre>
<a href="` + public + `#front">Public front</a>
<a href="` + api + `?ignorePayload=true#front">API front</a>
<a href="https://support.example.test/documents/doc-A/html/GUID-topic.html#topic">Legacy API topic</a>
<a href="?page=GUID-topic.html#topic">Query alias</a>
<a href="?docId=doc-B&amp;page=GUID-topic.html">Foreign</a>
<a href="https://evil.example/hpesc/public/docDisplay?docId=doc-A&amp;page=GUID-topic.html">Wrong host</a>
<a href="https://support.example.test/unrelated?docId=doc-A&amp;page=GUID-topic.html">Wrong path</a>
<a href="https://support.example.test/documents/doc-A/html/GUID-topic.html?page=GUID-other.html">Conflicting page</a>
<a href="GUID-unknown.html">Unknown</a></article></main></body></html>`)),
		},
		image: fixtureResource(image, "image/png", renderPNG),
		style: fixtureResource(style, "text/css", []byte(
			`main.ditasrc{color:#111}table.table{margin-top:27px}.codeph{white-space:normal;word-wrap:break-word;word-break:break-all!important}table code{white-space:unset!important}`)),
		hpeGraphikRegular: fixtureResource(hpeGraphikRegular, "font/woff2", append([]byte("wOF2"), bytes.Repeat([]byte{0}, 8)...)),
		hpeGraphikBold:    fixtureResource(hpeGraphikBold, "font/woff2", append([]byte("wOF2"), bytes.Repeat([]byte{1}, 8)...)),
	}
}

func TestHPEArchiveValidatesIdentityAliasesAssetsFontsAndContent(t *testing.T) {
	plan := hpeArchivePlan()
	fetcher := &archiveFixture{resources: hpeResources(), calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 2<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("HPE archive failed: %+v %v", result, err)
	}
	if len(result.HTML.Topics) != 2 || len(result.HTML.Assets) != 4 {
		t.Fatalf("unexpected HPE counts: topics=%d assets=%d", len(result.HTML.Topics), len(result.HTML.Assets))
	}
	if result.HTML.Topics[0].Metadata["publishedDate"] != "Published: September 2026" ||
		!slices.Contains(result.HTML.Topics[0].Copyright, "Copyright 2026 Publisher") {
		t.Fatalf("HPE metadata/copyright missing: %+v", result.HTML.Topics[0])
	}
	topicBytes, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[1].Path))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(topicBytes))
	if err != nil {
		t.Fatal(err)
	}
	content := findNode(doc, func(node *html.Node) bool { return hasClass(node, "archive-content") })
	for _, label := range []string{"Public front", "API front", "Legacy API topic", "Query alias"} {
		link := findNode(content, func(node *html.Node) bool {
			return node.Data == "a" && strings.TrimSpace(nodeText(node)) == label
		})
		if link == nil || strings.HasPrefix(attr(link, "href"), "http") {
			t.Fatalf("known HPE alias was not localized: %s", label)
		}
	}
	frontBytes, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	frontDoc, err := html.Parse(bytes.NewReader(frontBytes))
	if err != nil {
		t.Fatal(err)
	}
	missing := findNode(frontDoc, func(node *html.Node) bool {
		return node.Data == "a" && strings.TrimSpace(nodeText(node)) == "Publisher missing bookmark"
	})
	if missing == nil || strings.HasPrefix(attr(missing, "href"), "http") ||
		!strings.HasSuffix(attr(missing, "href"), "#"+aclPublisherMissingFigure) {
		t.Fatalf("publisher-missing HPE alias/fragment was not preserved: %v", missing)
	}
	if !slices.ContainsFunc(result.Warnings, func(value string) bool {
		return strings.Contains(value, "Publisher bookmark #"+aclPublisherMissingFigure) &&
			strings.Contains(value, "docDisplay?docId=doc-A")
	}) {
		t.Fatalf("source-absent HPE bookmark warning missing: %v", result.Warnings)
	}
	for _, label := range []string{"Foreign", "Wrong host", "Wrong path", "Conflicting page", "Unknown"} {
		link := findNode(content, func(node *html.Node) bool {
			return node.Data == "a" && strings.TrimSpace(nodeText(node)) == label
		})
		if link == nil || !hasClass(link, "archive-online") {
			t.Fatalf("unknown/cross-document HPE link was not retained online: %s", label)
		}
	}
	if findNode(content, func(node *html.Node) bool {
		return node.Data == "th" && attr(node, "rowspan") == "2"
	}) == nil || !bytes.Contains(topicBytes, []byte("preserved  spacing")) ||
		!bytes.Contains(topicBytes, []byte(`font-family:"HPE Graphik"`)) ||
		!bytes.Contains(topicBytes, []byte("padding:77px 12px 1.5rem 28px")) ||
		!bytes.Contains(topicBytes, []byte("caption,body.archive-source-hpe .archive-content figcaption")) ||
		!bytes.Contains(topicBytes, []byte(`class="archive-source-hpe"`)) {
		t.Fatalf("HPE table/code/font/layout fidelity missing: %s", topicBytes)
	}
	if !slices.ContainsFunc(result.Warnings, func(value string) bool {
		return strings.Contains(value, "Unplanned or ambiguous HPE topic alias")
	}) {
		t.Fatalf("unplanned HPE aliases were not diagnosed: %v", result.Warnings)
	}
	if slices.ContainsFunc(result.Notices, func(value model.Notice) bool {
		return strings.Contains(value.Message, "Unplanned or ambiguous HPE topic alias")
	}) {
		t.Fatalf("ambiguous HPE alias was classified as a notice: %+v", result.Notices)
	}
	for _, raw := range []string{
		"https://support.example.test/unrelated?docId=doc-A&page=GUID-topic.html",
		"https://support.example.test/documents/doc-A/html/GUID-topic.html?page=GUID-other.html",
	} {
		if !slices.ContainsFunc(result.Warnings, func(value string) bool { return strings.Contains(value, raw) }) {
			t.Fatalf("invalid HPE route was not diagnosed: %s warnings=%v", raw, result.Warnings)
		}
	}
	for _, raw := range []string{
		"https://support.example.test/hpesc/public/api/document/doc-A?ignorePayload=true",
		"https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html",
		"https://support.example.test/hpesc/public/api/document/doc-A/GUID-image.png?v=1",
		"https://support.example.test/resource3/doc-resources/css/hpesc-doc.css",
		hpeGraphikRegular, hpeGraphikBold,
	} {
		if fetcher.calls[raw] == 0 {
			t.Fatalf("required HPE source was not read: %s calls=%v", raw, fetcher.calls)
		}
	}
	if fetcher.calls["https://support.example.test/hpesc/public/api/document/doc-A?page=content.json"] != 0 ||
		fetcher.calls["https://support.example.test/hpesc/public/api/document/doc-A/GUID-unknown.html"] != 0 {
		t.Fatalf("unplanned HPE reference was fetched: %v", fetcher.calls)
	}
}

func TestHPEStandaloneRaster404PublishesDegradedPlaceholder(t *testing.T) {
	plan := hpeArchivePlan()
	resources := hpeResources()
	image := "https://support.example.test/hpesc/public/api/document/doc-A/GUID-image.png?v=1"
	resources[image] = model.Resource{
		URL: image, Status: http.StatusNotFound, Headers: http.Header{},
		Body: io.NopCloser(bytes.NewReader(nil)),
	}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan,
		&archiveFixture{resources: resources, calls: map[string]int{}},
		output, false, 2<<20, 8<<20)
	if err != nil || result.Status != "degraded" || len(result.MissingResources) != 1 ||
		result.MissingResources[0].ReferringTopicURL != plan.Topics[1].URL ||
		result.MissingResources[0].Caption != "Figure 1. Diagram" {
		t.Fatalf("HPE degraded image result=%+v err=%v", result, err)
	}
}

func TestHPETableInlineCodeStaysAtomicAndContainedWithoutChangingContent(t *testing.T) {
	plan := hpeArchivePlan()
	resources := hpeResources()
	topicURL := plan.Topics[1].FetchURL
	source := []byte(`<main role="main" class="ditasrc"><article><h1 id="topic">Syntax</h1>
<table class="table"><tr><th>Convention</th><th>Usage</th></tr>
<tr><td><p><code class="ph codeph">example‐text</code></p></td><td>Short token</td></tr>
<tr><td><code class="ph codeph">this-is-an-intentionally-extreme-inline-code-token-that-must-scroll-inside-its-table-cell-without-expanding-the-page</code></td><td>Long token</td></tr></table>
<p>Outside <code class="ph codeph">example‐text</code> remains publisher-controlled prose.</p>
<pre class="codeblock"><code>switch# show interface
  exact  spacing</code></pre></article></main>`)
	resources[topicURL] = model.Resource{
		URL: topicURL, Status: 200,
		Headers: http.Header{"Content-Type": {"multiPage;charset=UTF-8"}, "Doc-Id": {"doc-A"}, "Doc-Page-Name": {"GUID-topic.html"}},
		Body:    io.NopCloser(bytes.NewReader(source)),
	}
	styleURL := plan.Styles[0]
	resources[styleURL] = fixtureResource(styleURL, "text/css", []byte(
		`.codeph{white-space:normal;word-wrap:break-word;word-break:break-all!important}table code{white-space:unset!important}`))
	output := t.TempDir()
	result, err := HTML(context.Background(), plan,
		&archiveFixture{resources: resources, calls: map[string]int{}}, output, false, 2<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("HPE code layout fixture failed: %+v %v", result, err)
	}
	topic := result.HTML.Topics[1]
	body, err := os.ReadFile(filepath.Join(output, topic.Path))
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range [][]byte{
		[]byte("example‐text"),
		[]byte("this-is-an-intentionally-extreme-inline-code-token-that-must-scroll-inside-its-table-cell-without-expanding-the-page"),
		[]byte("switch# show interface\n  exact  spacing"),
	} {
		if !bytes.Contains(body, exact) {
			t.Fatalf("generated HTML changed exact source content %q: %s", exact, body)
		}
	}
	rule := `body.archive-source-hpe .archive-content table code.codeph{display:inline-block;max-width:min(60ch,calc(100vw - 3rem));overflow-x:auto;
white-space:nowrap!important;word-break:normal!important;overflow-wrap:normal!important}`
	if !bytes.Contains(body, []byte(rule)) {
		t.Fatalf("generated HPE table-code compensation missing: %s", body)
	}
	for _, forbidden := range []string{
		"body.archive-source-flare .archive-content table code.codeph",
		"body.archive-source-static .archive-content table code.codeph",
		".archive-content>code.codeph",
		"table pre", "table .codeblock",
	} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("HPE table-code rule escaped its intended scope: %q", forbidden)
		}
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	outside := findNode(doc, func(node *html.Node) bool {
		return node.Data == "code" && hasClass(node, "codeph") && node.Parent != nil && node.Parent.Data == "p" &&
			strings.Contains(nodeText(node.Parent), "Outside")
	})
	block := findNode(doc, func(node *html.Node) bool { return node.Data == "pre" && hasClass(node, "codeblock") })
	if outside == nil || block == nil || strings.TrimSpace(nodeText(outside)) != "example‐text" {
		t.Fatalf("negative-scope code content changed: outside=%v block=%q", outside, nodeText(block))
	}
	sourceDigest := sha256.Sum256(source)
	if topic.SourceSHA256 != hex.EncodeToString(sourceDigest[:]) || topic.SHA256 == topic.SourceSHA256 {
		t.Fatalf("source/local hashes do not preserve rewrite distinction: source=%s local=%s", topic.SourceSHA256, topic.SHA256)
	}
}

func TestHPETableInlineCodeCompensationDoesNotTargetFlareOrStatic(t *testing.T) {
	body := []byte(`<table><tr><td><code class="ph codeph">example‐text</code></td></tr></table>
<p><code class="ph codeph">outside‐table</code></p><pre class="codeblock"><code>show interface
  exact  spacing</code></pre>`)
	staticRoot := "https://static.example.test/code/"
	cases := []struct {
		kind     string
		plan     model.DocumentPlan
		resource model.Resource
	}{
		{
			kind: "flare",
			plan: flarePlan(model.Topic{URL: testHome, Title: "Flare"}),
			resource: fixtureResource(testHome, "text/html",
				append([]byte(`<main id="mc-main-content"><h1>Flare</h1>`), append(body, []byte(`</main>`)...)...)),
		},
		{
			kind: "static",
			plan: model.DocumentPlan{
				Document: model.Document{ID: "static", Title: "Static", Platform: "6300", Version: "10.18",
					URL: staticRoot + "index.html", Kind: "static"},
				Topics:    []model.Topic{{URL: staticRoot + "index.html", Title: "Static"}},
				TOC:       []model.TocEntry{{Title: "Static", URL: staticRoot + "index.html"}},
				Inventory: &model.InventoryEvidence{Complete: true, Kind: "static", RootURL: staticRoot, Entries: 1, UniqueTopics: 1},
			},
			resource: fixtureResource(staticRoot+"index.html", "text/html",
				append([]byte(`<main class="wh_topic_content"><h1>Static</h1>`), append(body, []byte(`</main>`)...)...)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			output := t.TempDir()
			url := tc.plan.Topics[0].URL
			result, err := HTML(context.Background(), tc.plan,
				&archiveFixture{resources: map[string]model.Resource{url: tc.resource}, calls: map[string]int{}},
				output, false, 2<<20, 8<<20)
			if err != nil || result.Status != "complete" {
				t.Fatalf("%s negative-scope archive failed: %+v %v", tc.kind, result, err)
			}
			generated, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
			if err != nil {
				t.Fatal(err)
			}
			for _, exact := range [][]byte{
				[]byte(`class="archive-source-` + tc.kind + `"`),
				[]byte("example‐text"),
				[]byte("outside‐table"),
				[]byte("show interface\n  exact  spacing"),
			} {
				if !bytes.Contains(generated, exact) {
					t.Fatalf("%s archive changed negative-scope content %q: %s", tc.kind, exact, generated)
				}
			}
			if bytes.Contains(generated, []byte(`body.archive-source-`+tc.kind+` .archive-content table code.codeph`)) {
				t.Fatalf("HPE table-code compensation targeted %s output", tc.kind)
			}
		})
	}
}

func TestHPESamePageBookmarkClassification(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		status       string
		wantWarning  string
		wantError    string
		preservedRef string
		warningCount int
	}{
		{
			name: "present-and-once-decoded",
			body: `<main role="main" class="ditasrc"><article><h1 id="front">Front</h1>
<p id="space value">Space</p><p id="literal%20value">Literal percent</p>
<a href="#front">Front</a><a href="#space%20value">Space</a>
<a href="#literal%2520value">Literal</a></article></main>`,
			status: "complete", preservedRef: "#literal%2520value",
		},
		{
			name: "source-absent-duplicate",
			body: `<main role="main" class="ditasrc"><article><h1 id="front">Front</h1>
<a href="#missing%2520value">First</a><a href="#missing%2520value">Second</a></article></main>`,
			status: "complete", wantWarning: "Publisher bookmark #missing%20value",
			preservedRef: "#missing%2520value", warningCount: 1,
		},
		{
			name: "source-present-lost",
			body: `<main role="main" class="ditasrc"><article><h1 id="front">Front</h1>
<a href="#inside">Removed target</a><form id="inside"><input></form></article></main>`,
			status: "incomplete", wantError: "source bookmark #inside was lost during archival",
		},
		{
			name: "cross-document-is-not-current-page",
			body: `<main role="main" class="ditasrc"><article><h1 id="front">Front</h1>
<a href="https://support.example.test/hpesc/public/docDisplay?docId=doc-B&amp;page=GUID-other.html#missing">Foreign</a>
</article></main>`,
			status: "complete",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := hpeArchivePlan()
			plan.Topics = plan.Topics[:1]
			plan.TOC = []model.TocEntry{{Title: "Front", URL: plan.Topics[0].URL + "#front"}}
			plan.Inventory.Entries, plan.Inventory.UniqueTopics = 1, 1
			resources := hpeResources()
			front := plan.Topics[0].FetchURL
			resources[front] = model.Resource{
				URL: front, Status: http.StatusOK,
				Headers: http.Header{
					"Content-Type":  {"multiPage;charset=UTF-8"},
					"Doc-Id":        {"doc-A"},
					"Doc-Page-Name": {"index.html"},
				},
				Body: io.NopCloser(strings.NewReader(tc.body)),
			}
			output := t.TempDir()
			result, err := HTML(context.Background(), plan,
				&archiveFixture{resources: resources, calls: map[string]int{}},
				output, false, 2<<20, 8<<20)
			if err != nil || result.Status != tc.status {
				t.Fatalf("status=%s err=%v result=%+v", result.Status, err, result)
			}
			if tc.wantWarning != "" {
				count := 0
				for _, warning := range result.Warnings {
					if strings.Contains(warning, tc.wantWarning) {
						count++
					}
				}
				if count != tc.warningCount {
					t.Fatalf("warning count=%d want=%d warnings=%v", count, tc.warningCount, result.Warnings)
				}
			}
			if tc.wantError != "" && !slices.ContainsFunc(result.Errors, func(value string) bool {
				return strings.Contains(value, tc.wantError)
			}) {
				t.Fatalf("missing bookmark-loss error %q: %v", tc.wantError, result.Errors)
			}
			if tc.preservedRef != "" {
				page, readErr := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
				if readErr != nil || !bytes.Contains(page, []byte(`href="`+tc.preservedRef+`"`)) {
					t.Fatalf("same-page href was not preserved: %q err=%v", page, readErr)
				}
			}
			if tc.name == "cross-document-is-not-current-page" {
				if slices.ContainsFunc(result.Warnings, func(value string) bool {
					return strings.Contains(value, "Publisher bookmark #missing")
				}) {
					t.Fatalf("foreign-document fragment treated as current page: %v", result.Warnings)
				}
			}
		})
	}
}

func TestCachedHPEOperatorTopicReplay(t *testing.T) {
	fixturePath := os.Getenv("AOSCX_HPE_OPERATOR_TOPIC_BODY")
	if fixturePath == "" {
		t.Skip("set AOSCX_HPE_OPERATOR_TOPIC_BODY to the checksum-verified cached topic copy")
	}
	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceHash(body); got != "76ab0d37125a6ad6b11a615282921d801fe12265c209e9daf88b0d5f8ea0e9dd" {
		t.Fatalf("cached topic checksum changed: %s", got)
	}
	const (
		documentID = "sd00007891en_usen_us"
		pageID     = "GUID-9BD91F9E-C51E-4F62-8561-BF83AE5D8C6C.html"
	)
	public := "https://support.hpe.com/hpesc/public/docDisplay?docId=" + documentID
	topicURL := public + "&page=" + pageID
	fetchURL := "https://support.hpe.com/hpesc/public/api/document/" + documentID + "?page=" + pageID
	plan := model.DocumentPlan{
		Document: model.Document{
			ID: "cli", Title: "CLI Guide", Platform: "8360", Version: "10.18.xxxx",
			URL: public, Kind: "hpe",
		},
		Topics: []model.Topic{{URL: topicURL, FetchURL: fetchURL, Title: "Operator context"}},
		TOC:    []model.TocEntry{{Title: "Operator context", URL: topicURL}},
		Inventory: &model.InventoryEvidence{
			Complete: true, Kind: "hpe", RootURL: fetchURL, TOCURL: fetchURL,
			Entries: 1, UniqueTopics: 1,
		},
	}
	resources := map[string]model.Resource{
		fetchURL: {
			URL: fetchURL, Status: 200,
			Headers: http.Header{
				"Content-Type":  {"multiPage"},
				"Doc-Id":        {documentID},
				"Doc-Page-Name": {pageID},
			},
			Body: io.NopCloser(bytes.NewReader(body)),
		},
		hpeGraphikRegular: fixtureResource(hpeGraphikRegular, "font/woff2", []byte("wOF2regular")),
		hpeGraphikBold:    fixtureResource(hpeGraphikBold, "font/woff2", []byte("wOF2bold")),
	}
	fetcher := &archiveFixture{resources: resources, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 2<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("verified short HPE topic was rejected: status=%s errors=%v err=%v", result.Status, result.Errors, err)
	}
	if fetcher.calls[fetchURL] != 2 {
		t.Fatalf("cached topic did not pass both archive reads: calls=%v", fetcher.calls)
	}
	saved, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(saved))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nodeText(doc), "Navigating to the operator context") ||
		len(matchingNodes(doc, func(node *html.Node) bool {
			return node.Type == html.ElementNode && node.Data == "li"
		})) != 2 ||
		len(matchingNodes(doc, func(node *html.Node) bool {
			return node.Type == html.ElementNode && node.Data == "strong"
		})) != 2 {
		t.Fatalf("cached topic content was not preserved: %s", saved)
	}
}

func TestHPEShortOperatorInstructionsAreDocumentContent(t *testing.T) {
	const body = `<main role="main" class="ditasrc"><article><h1 id="topic">Navigating to the operator context (&gt;)</h1>` +
		`<div class="body conbody"><p>To navigate to the operator command context, do one of the following:</p><ul>` +
		`<li>Log in to the switch CLI with an <strong>operator-group</strong> account.</li>` +
		`<li>From manager context, enter the <strong>disable</strong> command.</li></ul></div></article></main>`
	resources := hpeResources()
	key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
	resource := resources[key]
	resource.Body = io.NopCloser(strings.NewReader(body))
	resources[key] = resource
	output := t.TempDir()
	result, err := HTML(context.Background(), hpeArchivePlan(),
		&archiveFixture{resources: resources, calls: map[string]int{}}, output, false, 2<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("short HPE instructions were classified as a login shell: result=%+v err=%v", result, err)
	}
	saved, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[1].Path))
	if err != nil {
		t.Fatal(err)
	}
	savedDoc, err := html.Parse(bytes.NewReader(saved))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(saved, []byte("Log in to the switch CLI")) ||
		len(matchingNodes(savedDoc, func(node *html.Node) bool {
			return node.Type == html.ElementNode && node.Data == "strong"
		})) != 2 ||
		len(matchingNodes(savedDoc, func(node *html.Node) bool {
			return node.Type == html.ElementNode && node.Data == "li"
		})) != 2 {
		t.Fatalf("short HPE instructions were not preserved: %s", saved)
	}
}

func TestHPEShortErrorCommandDescriptionIsDocumentContent(t *testing.T) {
	const body = `<main role="main" class="ditasrc"><article><h1 id="topic">Error</h1>` +
		`<div class="body conbody"><p>This status indicates an unsuccessful command.</p></div></article></main>`
	resources := hpeResources()
	key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
	resource := resources[key]
	resource.Body = io.NopCloser(strings.NewReader(body))
	resources[key] = resource
	result, err := HTML(context.Background(), hpeArchivePlan(),
		&archiveFixture{resources: resources, calls: map[string]int{}}, t.TempDir(), false, 2<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("short HPE Error command description was classified as a shell: result=%+v err=%v", result, err)
	}
}

func TestHPEArchiveRejectsWrongResponseIdentityAndShape(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]model.Resource)
		want   string
	}{
		{"document", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.Headers.Set("Doc-Id", "doc-B")
			resources[key] = resource
		}, "document mismatch"},
		{"page", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.Headers.Set("Doc-Page-Name", "index.html")
			resources[key] = resource
		}, "page mismatch"},
		{"final-query", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.URL += "&revision=other"
			resources[key] = resource
		}, "query mismatch"},
		{"final-host", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.URL = "https://evil.example/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resources[key] = resource
		}, "query mismatch"},
		{"final-path", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.URL = "https://support.example.test/other/document/doc-A?page=GUID-topic.html"
			resources[key] = resource
		}, "query mismatch"},
		{"shape", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.Body = io.NopCloser(strings.NewReader(`<main><h1>Support shell</h1></main>`))
			resources[key] = resource
		}, "main.ditasrc"},
		{"login", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.Body = io.NopCloser(strings.NewReader(`<main class="ditasrc"><h1>Sign in</h1><form><input type="password"></form></main>`))
			resources[key] = resource
		}, "login"},
		{"access-denied", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.Body = io.NopCloser(strings.NewReader(
				`<main class="ditasrc"><h1>Access denied</h1><p>You do not have permission to access this document.</p></main>`,
			))
			resources[key] = resource
		}, "error"},
		{"not-found", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.Body = io.NopCloser(strings.NewReader(
				`<main class="ditasrc"><h1>Page not found</h1><p>The requested page could not be found.</p></main>`,
			))
			resources[key] = resource
		}, "error"},
		{"javascript-shell", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.Body = io.NopCloser(strings.NewReader(
				`<main class="ditasrc"><h1>Loading</h1><p>Please wait while content loads.</p></main><script>renderDocument()</script>`,
			))
			resources[key] = resource
		}, "JavaScript-only shell"},
		{"json-shell", func(resources map[string]model.Resource) {
			key := "https://support.example.test/hpesc/public/api/document/doc-A?page=GUID-topic.html"
			resource := resources[key]
			resource.Body = io.NopCloser(strings.NewReader(`{"pages":[]}`))
			resources[key] = resource
		}, "JSON placeholder"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resources := hpeResources()
			test.mutate(resources)
			result, err := HTML(context.Background(), hpeArchivePlan(),
				&archiveFixture{resources: resources, calls: map[string]int{}}, t.TempDir(), false, 2<<20, 8<<20)
			if err == nil || result.Status != "failed" || !strings.Contains(strings.Join(result.Errors, " "), test.want) {
				t.Fatalf("invalid HPE response accepted: result=%+v err=%v", result, err)
			}

		})
	}
}

func TestHPEQueryVariantsDoNotCollapseAmbiguousBareAlias(t *testing.T) {
	host := "https://support.example.test"
	public := host + "/hpesc/public/docDisplay?docId=doc-A&page=GUID-home.html"
	api := host + "/hpesc/public/api/document/doc-A?page=GUID-home.html"
	firstPublic := strings.Replace(public, "GUID-home", "GUID-next", 1) + "&revision=one"
	secondPublic := strings.Replace(public, "GUID-home", "GUID-next", 1) + "&revision=two"
	firstAPI := strings.Replace(api, "GUID-home", "GUID-next", 1) + "&revision=one"
	secondAPI := strings.Replace(api, "GUID-home", "GUID-next", 1) + "&revision=two"
	plan := model.DocumentPlan{
		Document: model.Document{ID: "variants", Title: "Variants", Platform: "6300", Version: "10.18", URL: public, Kind: "hpe"},
		Topics: []model.Topic{{URL: public, Title: "Home", FetchURL: api},
			{URL: firstPublic, Title: "First", FetchURL: firstAPI},
			{URL: secondPublic, Title: "Second", FetchURL: secondAPI}},
		TOC: []model.TocEntry{{Title: "Home", URL: public},
			{Title: "First", URL: firstPublic}, {Title: "Second", URL: secondPublic}},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "hpe", RootURL: host, Entries: 3, UniqueTopics: 3},
	}

	body := func(title, page string) model.Resource {
		return model.Resource{URL: page, Status: 200,
			Headers: http.Header{"Content-Type": {"multiPage;charset=UTF-8"}},
			Body:    io.NopCloser(strings.NewReader(`<main class="ditasrc"><h1>` + title + `</h1></main>`))}
	}
	resources := map[string]model.Resource{
		api: body("Home", api), firstAPI: body("First", firstAPI), secondAPI: body("Second", secondAPI),
		hpeGraphikRegular: fixtureResource(hpeGraphikRegular, "font/woff2", append([]byte("wOF2"), bytes.Repeat([]byte{0}, 8)...)),
		hpeGraphikBold:    fixtureResource(hpeGraphikBold, "font/woff2", append([]byte("wOF2"), bytes.Repeat([]byte{1}, 8)...)),
	}
	resources[api] = model.Resource{URL: api, Status: 200, Headers: http.Header{"Content-Type": {"multiPage;charset=UTF-8"}},
		Body: io.NopCloser(strings.NewReader(`<main class="ditasrc"><h1>Home</h1>
<a href="GUID-next.html">Ambiguous</a><a href="?page=GUID-next.html&amp;revision=two">Specific</a></main>`))}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, &archiveFixture{resources: resources, calls: map[string]int{}},
		output, false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("HPE variants failed: %+v %v", result, err)
	}
	homeBytes, _ := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	doc, _ := html.Parse(bytes.NewReader(homeBytes))
	ambiguous := findNode(doc, func(node *html.Node) bool {
		return node.Data == "a" && strings.TrimSpace(nodeText(node)) == "Ambiguous"
	})
	specific := findNode(doc, func(node *html.Node) bool { return node.Data == "a" && strings.TrimSpace(nodeText(node)) == "Specific" })
	if ambiguous == nil || !hasClass(ambiguous, "archive-online") || specific == nil ||
		strings.HasPrefix(attr(specific, "href"), "http") {
		t.Fatalf("HPE query alias ambiguity collapsed: %s", homeBytes)
	}
}

func TestHPEMissingRequiredGraphikFontIsIncomplete(t *testing.T) {
	resources := hpeResources()
	delete(resources, hpeGraphikBold)
	result, err := HTML(context.Background(), hpeArchivePlan(),
		&archiveFixture{resources: resources, calls: map[string]int{}}, t.TempDir(), false, 2<<20, 8<<20)
	if err != nil || result.Status != "incomplete" ||
		!strings.Contains(strings.Join(result.Errors, " "), "HPEGraphik-Bold") {
		t.Fatalf("missing HPE font was not an incomplete archive: %+v %v", result, err)
	}

}

func TestHPERelativeAliasesInheritMeaningfulQueryIdentity(t *testing.T) {
	host := "https://support.example.test"
	public := host + "/hpesc/public/docDisplay?docId=doc-A&page=GUID-home.html&revision=one"
	nextPublic := strings.Replace(public, "GUID-home", "GUID-next", 1)
	api := host + "/hpesc/public/api/document/doc-A?page=GUID-home.html&revision=one"
	nextAPI := strings.Replace(api, "GUID-home", "GUID-next", 1)
	plan := model.DocumentPlan{
		Document:  model.Document{ID: "inherited", Title: "Inherited", Platform: "6300", Version: "10.18", URL: public, Kind: "hpe"},
		Topics:    []model.Topic{{URL: public, Title: "Home", FetchURL: api}, {URL: nextPublic, Title: "Next", FetchURL: nextAPI}},
		TOC:       []model.TocEntry{{Title: "Home", URL: public}, {Title: "Next", URL: nextPublic}},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "hpe", RootURL: host, Entries: 2, UniqueTopics: 2},
	}
	resource := func(raw, content string) model.Resource {
		return model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {"multiPage;charset=UTF-8"}},
			Body: io.NopCloser(strings.NewReader(content))}
	}
	resources := map[string]model.Resource{
		api: resource(api, `<main class="ditasrc"><h1>Home</h1>
<a href="GUID-next.html#next">Bare</a><a href="?page=GUID-next.html#next">Query</a></main>`),
		nextAPI:           resource(nextAPI, `<main class="ditasrc"><h1 id="next">Next</h1></main>`),
		hpeGraphikRegular: fixtureResource(hpeGraphikRegular, "font/woff2", []byte("wOF2regular")),
		hpeGraphikBold:    fixtureResource(hpeGraphikBold, "font/woff2", []byte("wOF2bold")),
	}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, &archiveFixture{resources: resources, calls: map[string]int{}},
		output, false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("inherited aliases failed: %+v %v", result, err)
	}
	body, _ := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	doc, _ := html.Parse(bytes.NewReader(body))
	for _, label := range []string{"Bare", "Query"} {
		link := findNode(doc, func(node *html.Node) bool { return node.Data == "a" && strings.TrimSpace(nodeText(node)) == label })
		if link == nil || strings.HasPrefix(attr(link, "href"), "http") || !strings.HasSuffix(attr(link, "href"), "#next") {
			t.Fatalf("relative alias did not inherit revision identity: %s body=%s", label, body)
		}
	}
}

func TestStaticRejectsHPEMultipageMIME(t *testing.T) {
	raw := "https://static.example.test/guide/index.html"
	plan := model.DocumentPlan{
		Document: model.Document{ID: "static", Title: "Static", Platform: "6300", Version: "10.18", URL: raw, Kind: "static"},
		Topics:   []model.Topic{{URL: raw, Title: "Static"}}, TOC: []model.TocEntry{{Title: "Static", URL: raw}},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "static", RootURL: "https://static.example.test/guide/", Entries: 1, UniqueTopics: 1},
	}
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		raw: fixtureResource(raw, "multiPage;charset=UTF-8", []byte(`<main class="wh_topic_content"><h1>Wrong family</h1></main>`)),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 2<<20)
	if err == nil || result.Status != "failed" ||
		!strings.Contains(strings.ToLower(strings.Join(result.Errors, " ")), "multipage") {
		t.Fatalf("static source accepted HPE-only MIME: %+v %v", result, err)
	}
}

func TestStaticArchiveKeepsSubstantiveContentAndRejectsShell(t *testing.T) {
	root := "https://static.example.test/guide/"
	home := root + "index.html?release=10.18"
	child := root + "topics/child.html?mode=full"
	style := root + "styles/document.css"
	skinNamedContentStyle := root + "Skins/content.css"
	image := root + "images/diagram.png"
	plan := model.DocumentPlan{
		Document:  model.Document{ID: "static", Title: "Static Guide", Platform: "6300", Version: "10.18", URL: home, Kind: "static"},
		Topics:    []model.Topic{{URL: home, Title: "Home"}, {URL: child, Title: "Child"}},
		TOC:       []model.TocEntry{{Title: "Home", URL: home, Children: []model.TocEntry{{Title: "Child", URL: child + "#child"}}}},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "static", RootURL: root, TOCURL: home, Entries: 2, UniqueTopics: 2},
	}

	resources := map[string]model.Resource{
		home: fixtureResource(home, "text/html", []byte(`<html><head><link rel="stylesheet" href="styles/document.css">
<link rel="stylesheet" data-mc-generated="true" href="Skins/content.css"></head><body>
<div class="wh_header">Publisher shell</div><div class="wh_topic_content"><h1>Static home</h1>
<a href="topics/child.html?mode=full#child">Child</a><details><summary>More</summary><p>Expanded content</p></details>
<img src="images/diagram.png"><script>runtime()</script></div></body></html>`)),
		child: fixtureResource(child, "text/html", []byte(`<article><h1 id="child">Child</h1>
<table><caption>Values</caption><tr><th colspan="2">Header</th></tr><tr><td>A</td><td>B</td></tr></table>
<pre>line one
  line two</pre><a href="../outside.html">Online</a></article>`)),
		style:                 fixtureResource(style, "text/css", []byte(`.wh_topic_content{color:#222;background:url("../images/diagram.png")}`)),
		skinNamedContentStyle: fixtureResource(skinNamedContentStyle, "text/css", []byte(`.wh_topic_content{border:1px solid #333}`)),
		image:                 fixtureResource(image, "image/png", tinyPNG),
	}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, &archiveFixture{resources: resources, calls: map[string]int{}},
		output, false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("static archive failed: %+v %v", result, err)
	}
	if len(result.HTML.Assets) != 3 {
		t.Fatalf("static stylesheet under Skins was incorrectly omitted: %+v", result.HTML.Assets)
	}
	homeBytes, _ := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	childBytes, _ := os.ReadFile(filepath.Join(output, result.HTML.Topics[1].Path))
	if bytes.Contains(homeBytes, []byte("Publisher shell")) || bytes.Contains(homeBytes, []byte("<script")) ||
		!bytes.Contains(homeBytes, []byte("Expanded content")) || !bytes.Contains(homeBytes, []byte("<details open")) ||
		!bytes.Contains(childBytes, []byte(`colspan="2"`)) || !bytes.Contains(childBytes, []byte("  line two")) ||
		!bytes.Contains(childBytes, []byte(`class="archive-online"`)) {
		t.Fatalf("static content/shell handling failed: %s\n%s", homeBytes, childBytes)
	}

	shellPlan := plan
	shellPlan.Topics = []model.Topic{{URL: home, Title: "Home"}}
	shellPlan.TOC = []model.TocEntry{{Title: "Home", URL: home}}
	shellFetcher := &archiveFixture{resources: map[string]model.Resource{
		home: fixtureResource(home, "text/html", []byte(`<html><body><div class="wh_header">Only shell</div></body></html>`)),
	}, calls: map[string]int{}}
	failed, err := HTML(context.Background(), shellPlan, shellFetcher, t.TempDir(), false, 1<<20, 2<<20)
	if err == nil || failed.Status != "failed" || !strings.Contains(strings.Join(failed.Errors, " "), "substantive static") {
		t.Fatalf("static publisher shell was accepted: %+v %v", failed, err)
	}
}

func TestStaticRejectsEmptyPreferredContentContainer(t *testing.T) {
	raw := "https://static.example.test/guide/index.html"
	plan := model.DocumentPlan{
		Document: model.Document{ID: "static", Title: "Static", Platform: "6300", Version: "10.18", URL: raw, Kind: "static"},
		Topics:   []model.Topic{{URL: raw, Title: "Static"}},
		TOC:      []model.TocEntry{{Title: "Static", URL: raw}},
		Inventory: &model.InventoryEvidence{
			Complete: true, Kind: "static", RootURL: "https://static.example.test/guide/", Entries: 1, UniqueTopics: 1,
		},
	}
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		raw: fixtureResource(raw, "text/html", []byte(
			`<main><div class="wh_topic_content"> </div><article><h1>Shell fallback text</h1></article></main>`)),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 2<<20)
	if err == nil || result.Status != "failed" ||
		!strings.Contains(strings.Join(result.Errors, " "), "substantive static") {
		t.Fatalf("empty selected static content was accepted: %+v %v", result, err)
	}
}

func TestSVGStylesheetSelectorsAreNotPrunedByHTMLClassInventory(t *testing.T) {
	svg := testRoot + "Resources/diagram.svg"
	style := testRoot + "Resources/svg.css"
	image := testRoot + "Resources/pattern.png"
	plan := flarePlan()
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(`<main id="mc-main-content"><h1>Home</h1>
<img src="../Resources/diagram.svg"></main>`)),
		svg: fixtureResource(svg, "image/svg+xml", []byte(`<?xml-stylesheet type="text/css" href="svg.css"?>
<svg xmlns="http://www.w3.org/2000/svg"><rect class="svg-only-shape" width="10" height="10"/></svg>`)),
		style: fixtureResource(style, "text/css", []byte(`.svg-only-shape{fill:red;background:url("pattern.png")}`)),
		image: fixtureResource(image, "image/png", renderPNG),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("SVG stylesheet archive failed: %+v %v", result, err)
	}

	if fetcher.calls[style] != 1 || fetcher.calls[image] != 1 {
		t.Fatalf("SVG-only stylesheet dependency was pruned: %v", fetcher.calls)
	}
	for _, asset := range result.HTML.Assets {
		if asset.URL == style {
			body, _ := os.ReadFile(filepath.Join(output, asset.Path))
			if !bytes.Contains(body, []byte(".svg-only-shape")) {
				t.Fatalf("SVG-only selector was removed: %s", body)
			}
			return
		}
	}
	t.Fatal("SVG stylesheet missing from manifest")
}

func TestStylesheetCacheUpgradesWhenLaterRequiredBySVG(t *testing.T) {
	style := testRoot + "Resources/shared.css"
	svg := testRoot + "Resources/diagram.svg"
	plan := flarePlan()
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(`<html><head><link rel="stylesheet" href="../Resources/shared.css"></head>
<body><main id="mc-main-content"><h1>Home</h1><p class="html-used">Text</p><img src="../Resources/diagram.svg"></main></body></html>`)),
		style: fixtureResource(style, "text/css", []byte(`.html-used{color:black}.svg-only{fill:red}`)),
		svg: fixtureResource(svg, "image/svg+xml", []byte(`<?xml-stylesheet type="text/css" href="shared.css"?>
<svg xmlns="http://www.w3.org/2000/svg"><rect class="svg-only" width="10" height="10"/></svg>`)),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("mode-aware CSS archive failed: %+v %v", result, err)
	}
	if fetcher.calls[style] != 1 {
		t.Fatalf("shared stylesheet was fetched more than once: %v", fetcher.calls)
	}
	for _, asset := range result.HTML.Assets {
		if asset.URL == style {
			body, _ := os.ReadFile(filepath.Join(output, asset.Path))
			if !bytes.Contains(body, []byte(".html-used")) || !bytes.Contains(body, []byte(".svg-only")) {
				t.Fatalf("SVG upgrade lost stylesheet selectors: %s", body)
			}
			return
		}
	}
	t.Fatal("shared stylesheet missing")
}

func TestStylesheetCacheUpgradesWhenSVGRequestsProcessingStylesheet(t *testing.T) {
	style := testRoot + "Resources/shared.css"
	svg := testRoot + "Resources/diagram.svg"
	plan := flarePlan()
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(`<html><head><link rel="stylesheet" href="../Resources/shared.css"></head>
<body><main id="mc-main-content"><h1>Home</h1><p class="html-used">Text</p></main></body></html>`)),
		style: fixtureResource(style, "text/css", []byte(
			`.html-used{background:url("diagram.svg")}.svg-only{fill:red}`)),
		svg: fixtureResource(svg, "image/svg+xml", []byte(`<?xml-stylesheet type="text/css" href="shared.css"?>
<svg xmlns="http://www.w3.org/2000/svg"><rect class="svg-only" width="10" height="10"/></svg>`)),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("cyclic mode-aware CSS archive failed: %+v %v", result, err)
	}
	if fetcher.calls[style] != 1 || fetcher.calls[svg] != 1 {
		t.Fatalf("cyclic stylesheet dependencies were fetched more than once: %v", fetcher.calls)
	}
	for _, asset := range result.HTML.Assets {
		if asset.URL == style {
			body, _ := os.ReadFile(filepath.Join(output, asset.Path))
			if !bytes.Contains(body, []byte(".html-used")) || !bytes.Contains(body, []byte(".svg-only")) {
				t.Fatalf("SVG request during stylesheet processing lost selectors: %s", body)
			}
			return
		}
	}
	t.Fatal("shared stylesheet missing")
}

func TestM3DLocalRenderArtifacts(t *testing.T) {
	root := os.Getenv("AOSCX_M3D_RENDER_DIR")
	if root == "" {
		t.Skip("set AOSCX_M3D_RENDER_DIR for persistent local render fixtures")
	}
	if !strings.HasPrefix(filepath.Base(root), "render-") {
		t.Fatal("render artifact directory must use the owned render-* name")
	}
	hpeOutput := filepath.Join(root, "hpe")
	staticOutput := filepath.Join(root, "static")
	for _, directory := range []string{hpeOutput, staticOutput} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hpe := hpeResources()
	regular, regularErr := os.ReadFile("/System/Library/Fonts/Geneva.ttf")
	bold, boldErr := os.ReadFile("/System/Library/Fonts/SFNSMono.ttf")
	if regularErr != nil || boldErr != nil {
		t.Skip("valid local font fixtures unavailable")
	}
	hpe[hpeGraphikRegular] = fixtureResource(hpeGraphikRegular, "font/ttf", regular)
	hpe[hpeGraphikBold] = fixtureResource(hpeGraphikBold, "font/ttf", bold)
	hpeResult, err := HTML(context.Background(), hpeArchivePlan(),
		&archiveFixture{resources: hpe, calls: map[string]int{}}, hpeOutput, false, 2<<20, 8<<20)
	if err != nil || hpeResult.Status != "complete" {
		t.Fatalf("persistent HPE render fixture failed: %+v %v", hpeResult, err)
	}
	staticRoot := "https://static.example.test/render/"
	staticPlan := model.DocumentPlan{
		Document: model.Document{ID: "static-render", Title: "Static Render", Platform: "6300", Version: "10.18",
			URL: staticRoot + "index.html", Kind: "static"},
		Topics:    []model.Topic{{URL: staticRoot + "index.html", Title: "Static"}},
		TOC:       []model.TocEntry{{Title: "Static", URL: staticRoot + "index.html#static"}},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "static", RootURL: staticRoot, Entries: 1, UniqueTopics: 1},
	}
	staticFetcher := &archiveFixture{resources: map[string]model.Resource{
		staticRoot + "index.html": fixtureResource(staticRoot+"index.html", "text/html", []byte(
			`<html><body><div class="wh_header">Shell</div><main class="wh_topic_content"><h1 id="static">Static</h1>
<table><caption>Configuration</caption><tr><th>Command</th><th>Meaning</th></tr><tr><td><code>show job</code></td><td>Status</td></tr></table>
<pre>switch# show job
  preserved spacing</pre></main></body></html>`)),
	}, calls: map[string]int{}}
	staticResult, err := HTML(context.Background(), staticPlan, staticFetcher, staticOutput, false, 2<<20, 8<<20)
	if err != nil || staticResult.Status != "complete" {
		t.Fatalf("persistent static render fixture failed: %+v %v", staticResult, err)
	}
	t.Logf("render fixtures: hpe=%s static=%s", hpeOutput, staticOutput)
}
