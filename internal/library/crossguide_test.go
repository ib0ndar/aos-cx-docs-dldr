package library

import (
	stdzip "archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
	"golang.org/x/net/html"
)

const (
	testGraphikRegular = "https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Regular-Web.woff2"
	testGraphikBold    = "https://www.hpe.com/content/dam/hpe/fonts/graphik/HPEGraphik-Bold-Web.woff2"
)

func acceptFixtureHTML(
	t *testing.T,
	run *Run,
	document model.Document,
	topic model.Topic,
	finalURL, body string,
) model.ArchiveResult {
	t.Helper()
	if topic.FetchURL == "" {
		topic.FetchURL = topic.URL
	}
	if finalURL == "" {
		finalURL = topic.FetchURL
	}
	contentType := "text/html; charset=utf-8"
	if document.Kind == "hpe" {
		contentType = "multiPage;charset=UTF-8"
	}
	resources := resourceFetcher{
		topic.FetchURL: {
			URL: finalURL, Status: 200, Headers: http.Header{"Content-Type": {contentType}},
			Body: io.NopCloser(strings.NewReader(body)),
		},
	}
	if document.Kind == "hpe" {
		resources[testGraphikRegular] = model.Resource{
			URL: testGraphikRegular, Status: 200, Headers: http.Header{"Content-Type": {"font/woff2"}},
			Body: io.NopCloser(strings.NewReader("wOF2regular")),
		}
		resources[testGraphikBold] = model.Resource{
			URL: testGraphikBold, Status: 200, Headers: http.Header{"Content-Type": {"font/woff2"}},
			Body: io.NopCloser(strings.NewReader("wOF2bold")),
		}
	}
	root := topic.URL
	if parsed, err := url.Parse(topic.URL); err == nil {
		parsed.RawQuery, parsed.Fragment = "", ""
		parsed.Path = path.Dir(parsed.Path) + "/"
		root = parsed.String()
	}
	plan := model.DocumentPlan{
		Document: document,
		Topics:   []model.Topic{topic},
		TOC:      []model.TocEntry{{Title: topic.Title, URL: topic.URL}},
		Inventory: &model.InventoryEvidence{
			Complete: true, Kind: document.Kind, RootURL: root, Entries: 1, UniqueTopics: 1,
		},
	}
	output, err := run.GuideOutput(document.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := archive.HTML(context.Background(), plan, resources, output, false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("%s fixture archive failed: %+v %v", document.ID, result, err)
	}
	if err := run.Accept(result); err != nil {
		t.Fatal(err)
	}
	return result
}

func hrefByText(t *testing.T, filename, text string) string {
	t.Helper()
	body, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var href string
	walkHTML(document, func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "a" &&
			strings.TrimSpace(nodeTextForTest(node)) == text {
			href = htmlAttribute(node, "href")
		}
	})
	if href == "" {
		t.Fatalf("link %q missing from %s", text, filename)
	}
	return href
}

func nodeTextForTest(node *html.Node) string {
	var text strings.Builder
	walkHTML(node, func(child *html.Node) {
		if child.Type == html.TextNode {
			text.WriteString(child.Data)
		}
	})
	return text.String()
}

func localCrossGuideHref(sourceGuide string, source model.FileRecord, targetGuide string, target model.FileRecord, query, fragment string) string {
	relative, _ := filepath.Rel(
		filepath.FromSlash(path.Dir(path.Join(sourceGuide, source.Path))),
		filepath.FromSlash(path.Join(targetGuide, target.Path)),
	)
	href := (&url.URL{Path: filepath.ToSlash(relative)}).EscapedPath()
	if query != "" {
		href += "?" + query
	}
	if fragment != "" {
		href += "#" + fragment
	}
	return href
}

func TestCrossGuideLocalizationMixedLibraryAliasesAndZIP(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}

	flareURL := "https://flare.example.test/guide/Content/target.htm"
	flare := acceptFixtureHTML(t, run,
		model.Document{ID: "flare", Title: "Flare Guide", Platform: "6300", Version: "10.10", Kind: "flare", URL: flareURL},
		model.Topic{URL: flareURL, Title: "Flare target"}, "",
		`<div id="mc-main-content"><h1 id="flare-mark">Flare target</h1><p>routing fabric body phrase</p></div>`)

	hpePublic := "https://support.example.test/hpesc/public/docDisplay?mask=sh-rs&docId=doc-A&page=topic.html&edition=blue"
	hpeFetch := "https://support.example.test/hpesc/public/api/document/doc-A?page=topic.html&edition=blue"
	hpe := acceptFixtureHTML(t, run,
		model.Document{ID: "hpe", Title: "HPE Guide", Platform: "6300", Version: "10.10", Kind: "hpe", URL: hpePublic},
		model.Topic{URL: hpePublic, FetchURL: hpeFetch, Title: "HPE target"}, hpeFetch,
		`<main class="ditasrc"><h1 id="hpe-mark">HPE target</h1><p>access policy body phrase</p></main>`)

	staticPublic := "https://static.example.test/guide/target.html?edition=blue"
	staticFetch := "https://static.example.test/render/target.html?edition=blue"
	staticFinal := "https://cdn.example.test/final/target.html?edition=blue"
	static := acceptFixtureHTML(t, run,
		model.Document{ID: "static", Title: "Static Guide", Platform: "6300", Version: "10.10", Kind: "static", URL: staticPublic},
		model.Topic{URL: staticPublic, FetchURL: staticFetch, Title: "Static target"}, staticFinal,
		`<main><h1 id="static-mark">Static target</h1><p>oxygen body phrase</p></main>`)

	missingURL := "https://static.example.test/guide/missing.html"
	missing := acceptFixtureHTML(t, run,
		model.Document{ID: "missing", Title: "Missing Bookmark", Platform: "6300", Version: "10.10", Kind: "static", URL: missingURL},
		model.Topic{URL: missingURL, Title: "Missing bookmark"}, "",
		`<main><h1>Missing bookmark</h1></main>`)

	sharedFinal := "https://cdn.example.test/shared/topic.html"
	for _, id := range []string{"ambiguous-a", "ambiguous-b"} {
		public := "https://static.example.test/" + id + "/topic.html"
		acceptFixtureHTML(t, run,
			model.Document{ID: id, Title: id, Platform: "6300", Version: "10.10", Kind: "static", URL: public},
			model.Topic{URL: public, Title: id}, sharedFinal,
			`<main><h1 id="shared">Shared alias</h1></main>`)
	}

	pdf := acceptPDF(t, run, "pdf", "original")
	failedURL := "https://static.example.test/failed/topic.html"
	failedOutput, err := run.GuideOutput("failed")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Accept(model.ArchiveResult{
		Document:  model.Document{ID: "failed", Title: "Failed Guide", Platform: "6300", Version: "10.10", Kind: "static", URL: failedURL},
		OutputDir: failedOutput, Status: "failed", Format: "html", Errors: []string{"fixture failure"},
	}); err != nil {
		t.Fatal(err)
	}
	incompleteURL := "https://static.example.test/incomplete/topic.html"
	incompleteOutput, err := run.GuideOutput("incomplete")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Accept(model.ArchiveResult{
		Document:  model.Document{ID: "incomplete", Title: "Incomplete Guide", Platform: "6300", Version: "10.10", Kind: "static", URL: incompleteURL},
		OutputDir: incompleteOutput, Status: "incomplete", Format: "html", Errors: []string{"fixture incomplete"},
	}); err != nil {
		t.Fatal(err)
	}

	sourceURL := "https://source.example.test/guide/source.html"
	sourceBody := fmt.Sprintf(`<main><h1>Source</h1>
<a href="%s#flare-mark">Flare public</a>
<a href="%s#hpe-mark">HPE public</a>
<a href="%s#hpe-mark">HPE fetch</a>
<a href="%s#static-mark">Static final</a>
<a href="%s#missing-mark">Source absent bookmark</a>
<a href="%s">Ambiguous</a>
<a href="%s">PDF</a>
<a href="%s">Failed</a>
<a href="%s">Incomplete</a>
<a href="https://foreign.example.test/topic.html">Foreign</a>
<a href="https://static.example.test/guide/target.html?edition=red#static-mark">Wrong query</a>
<div class="archive-provenance"><a href="%s#flare-mark">Provenance</a></div>
</main>`, flareURL, hpePublic, hpeFetch, staticFinal, missingURL, sharedFinal,
		pdf.Document.URL, failedURL, incompleteURL, flareURL)
	source := acceptFixtureHTML(t, run,
		model.Document{ID: "source", Title: "Source Guide", Platform: "6300", Version: "10.10", Kind: "static", URL: sourceURL},
		model.Topic{URL: sourceURL, Title: "Source"}, "", sourceBody)

	manifest, err := run.Publish(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	sourcePage := filepath.Join(run.Target, "source", filepath.FromSlash(source.HTML.Topics[0].Path))
	for _, tc := range []struct {
		text   string
		guide  string
		target model.FileRecord
		query  string
		frag   string
	}{
		{"Flare public", "flare", flare.HTML.Topics[0], "", "flare-mark"},
		{"HPE public", "hpe", hpe.HTML.Topics[0], "mask=sh-rs&docId=doc-A&page=topic.html&edition=blue", "hpe-mark"},
		{"HPE fetch", "hpe", hpe.HTML.Topics[0], "page=topic.html&edition=blue", "hpe-mark"},
		{"Static final", "static", static.HTML.Topics[0], "edition=blue", "static-mark"},
		{"Source absent bookmark", "missing", missing.HTML.Topics[0], "", "missing-mark"},
	} {
		want := localCrossGuideHref("source", source.HTML.Topics[0], tc.guide, tc.target, tc.query, tc.frag)
		if got := hrefByText(t, sourcePage, tc.text); got != want {
			t.Errorf("%s href = %q, want %q", tc.text, got, want)
		}
	}
	for _, tc := range []struct {
		text string
		want string
	}{
		{"Ambiguous", sharedFinal},
		{"PDF", pdf.Document.URL},
		{"Failed", failedURL},
		{"Incomplete", incompleteURL},
		{"Foreign", "https://foreign.example.test/topic.html"},
		{"Wrong query", "https://static.example.test/guide/target.html?edition=red#static-mark"},
		{"Provenance", flareURL + "#flare-mark"},
	} {
		if got := hrefByText(t, sourcePage, tc.text); got != tc.want {
			t.Errorf("%s href = %q, want online %q", tc.text, got, tc.want)
		}
	}
	published := manifest.HTMLGuides["source"]
	if published.HTML.Topics[0].SHA256 == source.HTML.Topics[0].SHA256 {
		t.Fatal("cross-guide rewrite retained the pre-localization topic hash")
	}
	body, err := os.ReadFile(sourcePage)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if published.HTML.Topics[0].SHA256 != hex.EncodeToString(digest[:]) ||
		published.HTML.Topics[0].Size != int64(len(body)) {
		t.Fatal("published cross-guide topic metadata is stale")
	}
	if published.HTML.Integrity.CheckedLinks <= source.HTML.Integrity.CheckedLinks {
		t.Fatal("cross-guide local links were omitted from checked-link integrity")
	}
	warnings := strings.Join(published.Warnings, "\n")
	if !strings.Contains(warnings, "absent from source topic linked across guides") ||
		!strings.Contains(warnings, "Ambiguous cross-guide topic identity retained online") {
		t.Fatalf("bounded cross-guide diagnostics missing: %v", published.Warnings)
	}
	if got := hrefByText(t, sourcePage, "Guide index"); got != "../index.html" {
		t.Fatalf("generated guide navigation was rewritten: %q", got)
	}
	if got := hrefByText(t, sourcePage, "View publisher source"); got != sourceURL {
		t.Fatalf("publisher provenance link was rewritten: %q", got)
	}
	reopened, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatalf("rewritten library failed hash verification: %v", err)
	}

	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	zipPath, err := ExportZIP(context.Background(), run.Target)
	if err != nil {
		t.Fatal(err)
	}
	zipped, err := stdzip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zipped.Close()
	memberName := path.Join("10.10", "source", source.HTML.Topics[0].Path)
	for _, member := range zipped.File {
		if member.Name != memberName {
			continue
		}
		reader, err := member.Open()
		if err != nil {
			t.Fatal(err)
		}
		zippedBody, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(zippedBody, body) {
			t.Fatal("ZIP does not contain finalized cross-guide topic bytes")
		}
		return
	}
	t.Fatalf("ZIP member %s missing", memberName)
}

func TestCrossGuideIdentityNormalizationAndMeaningfulQuery(t *testing.T) {
	spaceKeys := crossGuideIdentityKeys("https://docs.example.test/guide/topic name.html#mark")
	if len(spaceKeys) != 1 || spaceKeys[0] != "url:https://docs.example.test/guide/topic%20name.html" {
		t.Fatalf("canonical URL identity was not normalized safely: %q", spaceKeys)
	}
	bluePublic := crossGuideIdentityKeys(
		"https://support.example.test/hpesc/public/docDisplay?mask=sh-rs&docId=doc-A&page=topic.html&edition=blue",
	)
	blueFetch := crossGuideIdentityKeys(
		"https://support.example.test/hpesc/public/api/document/doc-A?page=topic.html&edition=blue&ignorePayload=true",
	)
	redFetch := crossGuideIdentityKeys(
		"https://support.example.test/hpesc/public/api/document/doc-A?page=topic.html&edition=red&ignorePayload=true",
	)
	if len(bluePublic) != 2 || len(blueFetch) != 2 || bluePublic[1] != blueFetch[1] {
		t.Fatalf("HPE public/fetch aliases did not share identity: public=%q fetch=%q", bluePublic, blueFetch)
	}
	if bluePublic[1] == redFetch[1] {
		t.Fatal("meaningful HPE query identity was discarded")
	}
}

func TestCrossGuideLocalizationRejectsLocallyLostSourceBookmark(t *testing.T) {
	run, err := Open(t.TempDir(), "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	targetURL := "https://static.example.test/target.html"
	target := acceptFixtureHTML(t, run,
		model.Document{ID: "target", Title: "Target", Platform: "6300", Version: "10.10", Kind: "static", URL: targetURL},
		model.Topic{URL: targetURL, Title: "Target"}, "",
		`<main><h1 id="source-present">Target</h1></main>`)
	name := path.Join(run.stage, "target", target.HTML.Topics[0].Path)
	body, err := os.ReadFile(filepath.Join(run.Destination, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte(` id="source-present"`), nil, 1)
	if err := storage.Atomic(run.root, name, body); err != nil {
		t.Fatal(err)
	}
	result := run.guides["target"]
	hash, size, err := storage.Hash(run.root, name, maxCrossGuideHTMLBytes)
	if err != nil {
		t.Fatal(err)
	}
	result.HTML.Topics[0].SHA256, result.HTML.Topics[0].Size = hash, size
	run.guides["target"] = result
	if err := storage.WriteJSON(run.root, path.Join(run.stage, "target", "manifest.json"), result.HTML); err != nil {
		t.Fatal(err)
	}
	sourceURL := "https://source.example.test/source.html"
	acceptFixtureHTML(t, run,
		model.Document{ID: "source", Title: "Source", Platform: "6300", Version: "10.10", Kind: "static", URL: sourceURL},
		model.Topic{URL: sourceURL, Title: "Source"}, "",
		`<main><a href="`+targetURL+`#source-present">Lost target</a></main>`)
	if _, err := run.Publish(false); err == nil || !strings.Contains(err.Error(), "source bookmark #source-present was lost") {
		t.Fatalf("locally lost source bookmark did not stop publication: %v", err)
	}
}

func TestCrossGuideFinalizationFailurePreservesPublishedLibrary(t *testing.T) {
	base := t.TempDir()
	target, previous := publishPDF(t, base, "previous")
	before, err := os.ReadFile(filepath.Join(target, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	targetURL := "https://static.example.test/target.html"
	targetResult := acceptFixtureHTML(t, run,
		model.Document{ID: "target", Title: "Target", Platform: "6300", Version: "10.10", Kind: "static", URL: targetURL},
		model.Topic{URL: targetURL, Title: "Target"}, "", `<main><h1>Target</h1></main>`)
	sourceURL := "https://source.example.test/source.html"
	acceptFixtureHTML(t, run,
		model.Document{ID: "source", Title: "Source", Platform: "6300", Version: "10.10", Kind: "static", URL: sourceURL},
		model.Topic{URL: sourceURL, Title: "Source"}, "",
		`<main><a href="`+targetURL+`">Target</a></main>`)
	if err := storage.Atomic(run.root, path.Join(run.stage, "target", targetResult.HTML.Topics[0].Path), []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Publish(false); err == nil {
		t.Fatal("cross-guide pre-publication integrity failure was hidden")
	}
	after, err := os.ReadFile(filepath.Join(target, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed cross-guide finalization replaced the published library")
	}
	var current Manifest
	if err := json.Unmarshal(after, &current); err != nil {
		t.Fatal(err)
	}
	if current.RunID != previous.RunID {
		t.Fatal("failed cross-guide finalization changed the published run")
	}
}

func TestCancelledPublicationStillFinalizesAcceptedCrossGuideLinks(t *testing.T) {
	run, err := Open(t.TempDir(), "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	targetURL := "https://static.example.test/target.html"
	target := acceptFixtureHTML(t, run,
		model.Document{ID: "target", Title: "Target", Platform: "6300", Version: "10.10", Kind: "static", URL: targetURL},
		model.Topic{URL: targetURL, Title: "Target"}, "", `<main><h1>Target</h1></main>`)
	sourceURL := "https://source.example.test/source.html"
	source := acceptFixtureHTML(t, run,
		model.Document{ID: "source", Title: "Source", Platform: "6300", Version: "10.10", Kind: "static", URL: sourceURL},
		model.Topic{URL: sourceURL, Title: "Source"}, "",
		`<main><a href="`+targetURL+`">Target</a></main>`)
	manifest, err := run.Publish(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "incomplete" {
		t.Fatalf("cancelled publication status = %s", manifest.Status)
	}
	sourcePage := filepath.Join(run.Target, "source", filepath.FromSlash(source.HTML.Topics[0].Path))
	want := localCrossGuideHref("source", source.HTML.Topics[0], "target", target.HTML.Topics[0], "", "")
	if got := hrefByText(t, sourcePage, "Target"); got != want {
		t.Fatalf("cancelled publication omitted finalized cross-guide link: %q", got)
	}
}
