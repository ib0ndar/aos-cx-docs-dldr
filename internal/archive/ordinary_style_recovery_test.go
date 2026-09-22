package archive

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

const (
	ordinaryGuideRoot   = "https://docs.example.test/techdocs/AOS-CX/10.13/HTML/vxlan/"
	ordinaryTopic       = ordinaryGuideRoot + "Content/chapter/topic.htm"
	ordinarySecond      = ordinaryGuideRoot + "Content/chapter/second.htm"
	ordinaryCandidate   = ordinaryGuideRoot + "Content/Resources/Stylesheets/main.css"
	ordinaryBroken      = ordinaryGuideRoot + "Resources/Stylesheets/main.css"
	ordinaryLinkNested  = ordinaryGuideRoot + "Content/Book/Feature_1/Resources/Stylesheets/main.css"
	ordinaryVXLANNested = ordinaryGuideRoot + "Content/chapter/Resources/Stylesheets/main.css"
)

func TestFlareRecoversSamePageDuplicateOrdinaryStylesheet(t *testing.T) {
	body := `<html><head>
<link rel="stylesheet" href="../Resources/Stylesheets/main.css">
<link rel="stylesheet" href="../../Resources/Stylesheets/main.css">
</head><body><main id="mc-main-content"><h1>VXLAN</h1><p class="used">Content</p></main></body></html>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		ordinaryTopic:     fixtureResource(ordinaryTopic, "text/html", []byte(body)),
		ordinarySecond:    fixtureResource(ordinarySecond, "text/html", []byte(body)),
		ordinaryCandidate: fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{color:#123}`)),
		ordinaryBroken:    statusResource(ordinaryBroken, http.StatusNotFound),
	}, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), ordinaryStylePlan("flare", ordinaryTopic, ordinarySecond), fetcher, output, false, 1<<20, 8<<20)
	if err != nil || result.Status != "complete" || len(result.Errors) != 0 ||
		len(result.StylesheetRecoveries) != 1 || len(result.Warnings) != 1 {
		t.Fatalf("same-page duplicate stylesheet was not recovered: result=%+v err=%v", result, err)
	}
	recovery := result.StylesheetRecoveries[0]
	if recovery.Kind != ordinaryStylesheetRecoveryKind ||
		recovery.BrokenURL != ordinaryBroken || recovery.HTTPStatus != http.StatusNotFound ||
		recovery.ReplacementURL != ordinaryCandidate || recovery.ReplacementFinalURL != ordinaryCandidate ||
		recovery.ReplacementSourceSHA256 == "" || recovery.ReplacementSourceSize == 0 ||
		!slices.Equal(recovery.BrokenDeclarations, []model.BrokenStylesheetReference{{
			URL: ordinaryBroken, HTTPStatus: http.StatusNotFound,
		}}) ||
		len(recovery.TableStyleFamilies) != 0 ||
		!slices.Equal(recovery.AffectedTopics, []string{ordinarySecond, ordinaryTopic}) {
		t.Fatalf("ordinary stylesheet provenance is incomplete: %+v", recovery)
	}
	if fetcher.calls[ordinaryCandidate] != 1 || fetcher.calls[ordinaryBroken] != 1 ||
		len(result.HTML.Assets) != 1 || result.HTML.Assets[0].URL != ordinaryCandidate {
		t.Fatalf("ordinary stylesheet requests/assets were not deduplicated: calls=%v assets=%+v",
			fetcher.calls, result.HTML.Assets)
	}
	page, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(page, []byte(filepath.Base(result.HTML.Assets[0].Path))) != 1 ||
		bytes.Contains(page, []byte(ordinaryBroken)) {
		t.Fatalf("recovered page did not contain one localized stylesheet: %s", page)
	}
}

func TestFlareRecoversNestedSamePageDuplicateOrdinaryStylesheets(t *testing.T) {
	tests := []struct {
		name   string
		broken []string
		links  string
	}{
		{
			name:   "link aggregation nested book",
			broken: []string{ordinaryLinkNested},
			links: `<link rel="stylesheet" href="../Resources/Stylesheets/main.css">
<link rel="stylesheet" href="../Book/Feature_1/Resources/Stylesheets/main.css">`,
		},
		{
			name:   "vxlan nested chapter",
			broken: []string{ordinaryVXLANNested},
			links: `<link rel="stylesheet" href="../Resources/Stylesheets/main.css">
<link rel="stylesheet" href="Resources/Stylesheets/main.css">`,
		},
		{
			name:   "nested gone",
			broken: []string{ordinaryVXLANNested},
			links: `<link rel="stylesheet" href="../Resources/Stylesheets/main.css">
<link rel="stylesheet" href="Resources/Stylesheets/main.css">`,
		},
		{
			name:   "multiple permanent variants",
			broken: []string{ordinaryLinkNested, ordinaryVXLANNested, ordinaryBroken},
			links: `<link rel="stylesheet" href="../Resources/Stylesheets/main.css">
<link rel="stylesheet" href="../Book/Feature_1/Resources/Stylesheets/main.css">
<link rel="stylesheet" href="Resources/Stylesheets/main.css">
<link rel="stylesheet" href="../../Resources/Stylesheets/main.css">`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resources := map[string]model.Resource{
				ordinaryTopic: fixtureResource(ordinaryTopic, "text/html", []byte(
					`<html><head>`+test.links+`</head><body><main id="mc-main-content"><h1>Guide</h1><p class="used">Text</p></main></body></html>`)),
				ordinaryCandidate: fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{color:#123}`)),
			}
			expectedStatus := http.StatusNotFound
			if test.name == "nested gone" {
				expectedStatus = http.StatusGone
			}
			for _, broken := range test.broken {
				resources[broken] = statusResource(broken, expectedStatus)
			}
			fetcher := &archiveFixture{resources: resources, calls: map[string]int{}}
			output := t.TempDir()
			result, err := HTML(context.Background(), ordinaryStylePlan("flare", ordinaryTopic),
				fetcher, output, false, 1<<20, 8<<20)
			if err != nil || result.Status != "complete" || len(result.Errors) != 0 ||
				len(result.StylesheetRecoveries) != 1 ||
				len(result.Warnings) != 1 || len(result.HTML.Assets) != 1 {
				t.Fatalf("nested same-page duplicates were not recovered: result=%+v err=%v", result, err)
			}
			recovery := result.StylesheetRecoveries[0]
			expectedBroken := make([]model.BrokenStylesheetReference, 0, len(test.broken))
			for _, broken := range test.broken {
				expectedBroken = append(expectedBroken, model.BrokenStylesheetReference{
					URL: broken, HTTPStatus: expectedStatus,
				})
				if fetcher.calls[broken] != 1 {
					t.Fatalf("missing nested recovery request for %s: calls=%v", broken, fetcher.calls)
				}
			}
			if recovery.Kind != ordinaryStylesheetRecoveryKind ||
				recovery.ReplacementURL != ordinaryCandidate ||
				recovery.ReplacementFinalURL != ordinaryCandidate ||
				recovery.ReplacementSourceSHA256 == "" ||
				recovery.ReplacementSourceSize == 0 ||
				!slices.Equal(recovery.BrokenDeclarations, expectedBroken) ||
				!slices.Equal(recovery.AffectedTopics, []string{ordinaryTopic}) {
				t.Fatalf("missing nested group provenance: %+v", recovery)
			}
			if fetcher.calls[ordinaryCandidate] != 1 {
				t.Fatalf("canonical candidate was not deduplicated: %v", fetcher.calls)
			}
			page, readErr := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
			if readErr != nil || bytes.Count(page, []byte(filepath.Base(result.HTML.Assets[0].Path))) != 1 {
				t.Fatalf("recovered nested page retained duplicate styles: %s err=%v", page, readErr)
			}
		})
	}
}

func TestFlareOrdinaryStylesheetRecoveryIsUniversalAcrossPathsNamesAndOrder(t *testing.T) {
	tests := []struct {
		name         string
		candidateURL string
		links        []string
	}{
		{
			name:         "arbitrary basename candidate first",
			candidateURL: ordinaryGuideRoot + "Content/styles/theme-dark.css?v=1",
			links: []string{
				"../styles/theme-dark.css?v=1",
				"../../assets/theme-dark.css?v=1",
			},
		},
		{
			name:         "deep candidate last",
			candidateURL: ordinaryGuideRoot + "Content/chapter/styles/layout.css",
			links: []string{
				"../sibling/Resources/Stylesheets/layout.css",
				"styles/layout.css",
			},
		},
		{
			name:         "candidate in middle",
			candidateURL: ordinaryGuideRoot + "Content/theme/colors.css",
			links: []string{
				"missing/colors.css",
				"../theme/colors.css",
				"../other/colors.css",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var links strings.Builder
			resources := map[string]model.Resource{
				test.candidateURL: fixtureResource(test.candidateURL, "text/css", []byte(
					`article{display:block}@media print{.used{color:#123}}#content{max-width:100%}`,
				)),
			}
			for _, reference := range test.links {
				fmt.Fprintf(&links, `<link rel="stylesheet" href="%s">`, reference)
				resolved, err := resolveReference(ordinaryTopic, reference)
				if err != nil {
					t.Fatal(err)
				}
				resolved = canonicalNoFragment(resolved)
				if resolved != test.candidateURL {
					resources[resolved] = statusResource(resolved, http.StatusNotFound)
				}
			}
			resources[ordinaryTopic] = fixtureResource(ordinaryTopic, "text/html", []byte(
				`<html><head>`+links.String()+`</head><body><main id="mc-main-content"><article id="content" class="used"><h1>Guide</h1></article></main></body></html>`,
			))
			fetcher := &archiveFixture{resources: resources, calls: map[string]int{}}
			plan := ordinaryStylePlan("flare", ordinaryTopic)
			plan.Document.ID = "arbitrary-" + strings.ReplaceAll(test.name, " ", "-")
			plan.Document.Platform = "10000"
			plan.Document.Version = "future-label"
			result, err := HTML(context.Background(), plan, fetcher, t.TempDir(), false, 1<<20, 8<<20)
			if err != nil || result.Status != "complete" || len(result.StylesheetRecoveries) != 1 ||
				result.StylesheetRecoveries[0].ReplacementURL != test.candidateURL ||
				len(result.HTML.Assets) != 1 || result.HTML.Assets[0].URL != test.candidateURL {
				t.Fatalf("universal same-page recovery failed: result=%+v err=%v", result, err)
			}
			for raw, calls := range fetcher.calls {
				if strings.HasSuffix(strings.Split(raw, "?")[0], ".css") && calls != 1 {
					t.Fatalf("stylesheet request was not deduplicated: %s=%d", raw, calls)
				}
			}
		})
	}
}

func TestFlareOrdinaryStylesheetRecoveryHandlesIndependentGroups(t *testing.T) {
	candidateA := ordinaryGuideRoot + "Content/theme/alpha.css"
	brokenA := ordinaryGuideRoot + "Content/chapter/missing/alpha.css"
	candidateB := ordinaryGuideRoot + "Content/theme/beta.css"
	brokenB := ordinaryGuideRoot + "Resources/beta.css"
	body := `<html><head>
<link rel="stylesheet" href="../theme/alpha.css">
<link rel="stylesheet" href="missing/alpha.css">
<link rel="stylesheet" href="../../Resources/beta.css">
<link rel="stylesheet" href="../theme/beta.css">
</head><body><main id="mc-main-content"><h1>Guide</h1></main></body></html>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		ordinaryTopic: fixtureResource(ordinaryTopic, "text/html", []byte(body)),
		candidateA:    fixtureResource(candidateA, "text/css", []byte(`article{display:block}`)),
		brokenA:       statusResource(brokenA, http.StatusNotFound),
		candidateB:    fixtureResource(candidateB, "text/css", []byte(`main{display:block}`)),
		brokenB:       statusResource(brokenB, http.StatusGone),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), ordinaryStylePlan("flare", ordinaryTopic),
		fetcher, t.TempDir(), false, 1<<20, 8<<20)
	if err != nil || result.Status != "complete" || len(result.StylesheetRecoveries) != 2 ||
		len(result.HTML.Assets) != 2 {
		t.Fatalf("independent same-page groups were not recovered: result=%+v err=%v", result, err)
	}
}

func TestFlareOrdinaryStylesheetRecoveryRequiresSamePageEvidence(t *testing.T) {
	first := `<html><head><link rel="stylesheet" href="../Resources/Stylesheets/main.css"></head>
<body><main id="mc-main-content"><h1>One</h1></main></body></html>`
	second := `<html><head><link rel="stylesheet" href="Resources/Stylesheets/main.css"></head>
<body><main id="mc-main-content"><h1>Two</h1></main></body></html>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		ordinaryTopic:       fixtureResource(ordinaryTopic, "text/html", []byte(first)),
		ordinarySecond:      fixtureResource(ordinarySecond, "text/html", []byte(second)),
		ordinaryCandidate:   fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{color:#123}`)),
		ordinaryVXLANNested: statusResource(ordinaryVXLANNested, http.StatusNotFound),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), ordinaryStylePlan("flare", ordinaryTopic, ordinarySecond),
		fetcher, t.TempDir(), false, 1<<20, 8<<20)
	if err != nil || result.Status != "incomplete" || len(result.Errors) == 0 ||
		len(result.StylesheetRecoveries) != 0 {
		t.Fatalf("cross-page-only stylesheet evidence was accepted: result=%+v err=%v", result, err)
	}
}

func TestFlareOrdinaryStylesheetRecoveryFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		links     string
		candidate model.Resource
		broken    model.Resource
		failure   error
	}{
		{
			name: "missing same-page candidate", kind: "flare",
			links:  `<link rel="stylesheet" href="../../Resources/Stylesheets/main.css">`,
			broken: statusResource(ordinaryBroken, http.StatusNotFound),
		},
		{
			name: "ambiguous candidate", kind: "flare",
			links: `<link rel="stylesheet" href="../Resources/Stylesheets/main.css">
<link rel="stylesheet" href="../Alternate/main.css">
<link rel="stylesheet" href="../../Resources/Stylesheets/main.css">`,
			candidate: fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{color:#123}`)),
			broken:    statusResource(ordinaryBroken, http.StatusNotFound),
		},
		{
			name: "case collision", kind: "flare",
			links: `<link rel="stylesheet" href="../Resources/Stylesheets/Main.css">
<link rel="stylesheet" href="../../Resources/Stylesheets/main.css">`,
			broken: statusResource(ordinaryBroken, http.StatusNotFound),
		},
		{
			name: "distinct basename", kind: "flare",
			links: `<link rel="stylesheet" href="../Resources/Stylesheets/document.css">
<link rel="stylesheet" href="../../Resources/Stylesheets/main.css">`,
			broken: statusResource(ordinaryBroken, http.StatusNotFound),
		},
		{
			name: "query identity conflict", kind: "flare",
			links: `<link rel="stylesheet" href="../Resources/Stylesheets/main.css?v=1">
<link rel="stylesheet" href="../../Resources/Stylesheets/main.css?v=2">`,
			candidate: fixtureResource(ordinaryCandidate+"?v=1", "text/css", []byte(`.used{color:#123}`)),
			broken:    statusResource(ordinaryBroken+"?v=2", http.StatusNotFound),
		},
		{
			name: "candidate redirect", kind: "flare",
			links: duplicateOrdinaryLinks(),
			candidate: fixtureResource(ordinaryGuideRoot+"Content/Resources/Stylesheets/redirected.css",
				"text/css", []byte(`.used{color:#123}`)),
			broken: statusResource(ordinaryBroken, http.StatusNotFound),
		},
		{
			name: "candidate MIME", kind: "flare",
			links:     duplicateOrdinaryLinks(),
			candidate: fixtureResource(ordinaryCandidate, "application/octet-stream", []byte(`.used{color:#123}`)),
			broken:    statusResource(ordinaryBroken, http.StatusNotFound),
		},
		{
			name: "candidate parse", kind: "flare",
			links:     duplicateOrdinaryLinks(),
			candidate: fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{background:url("unterminated)`)),
			broken:    statusResource(ordinaryBroken, http.StatusNotFound),
		},
		{
			name: "broken server error", kind: "flare",
			links:     duplicateOrdinaryLinks(),
			candidate: fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{color:#123}`)),
			broken:    statusResource(ordinaryBroken, http.StatusInternalServerError),
		},
		{
			name: "broken timeout", kind: "flare",
			links:     duplicateOrdinaryLinks(),
			candidate: fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{color:#123}`)),
			failure:   context.DeadlineExceeded,
		},
		{
			name: "nested broken server error", kind: "flare",
			links: `<link rel="stylesheet" href="../Resources/Stylesheets/main.css">
<link rel="stylesheet" href="../Book/Feature_1/Resources/Stylesheets/main.css">`,
			candidate: fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{color:#123}`)),
			broken:    statusResource(ordinaryLinkNested, http.StatusInternalServerError),
		},
		{
			name: "both valid remain distinct", kind: "flare",
			links:     duplicateOrdinaryLinks(),
			candidate: fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{color:#123}`)),
			broken:    fixtureResource(ordinaryBroken, "text/css", []byte(`.used{background:#fff}`)),
		},
		{
			name: "static is ineligible", kind: "static",
			links:     duplicateOrdinaryLinks(),
			candidate: fixtureResource(ordinaryCandidate, "text/css", []byte(`.used{color:#123}`)),
			broken:    statusResource(ordinaryBroken, http.StatusNotFound),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resources := map[string]model.Resource{
				ordinaryTopic: fixtureResource(ordinaryTopic, "text/html", []byte(
					`<html><head>`+test.links+`</head><body><main id="mc-main-content"><h1>Guide</h1><p class="used">Text</p></main></body></html>`)),
			}
			if test.candidate.Body != nil {
				candidateURL := ordinaryCandidate
				if test.name == "query identity conflict" {
					candidateURL += "?v=1"
				}
				resources[candidateURL] = test.candidate
			}
			if test.broken.Body != nil {
				brokenURL := ordinaryBroken
				if test.name == "nested broken server error" {
					brokenURL = ordinaryLinkNested
				} else if test.name == "query identity conflict" {
					brokenURL += "?v=2"
				}
				resources[brokenURL] = test.broken
			}
			alternate := ordinaryGuideRoot + "Content/Alternate/main.css"
			if strings.Contains(test.links, "../Alternate/main.css") {
				resources[alternate] = fixtureResource(alternate, "text/css", []byte(`.used{color:#456}`))
			}
			upper := ordinaryGuideRoot + "Content/Resources/Stylesheets/Main.css"
			if strings.Contains(test.links, "Main.css") {
				resources[upper] = fixtureResource(upper, "text/css", []byte(`.used{color:#456}`))
			}
			document := ordinaryStylePlan(test.kind, ordinaryTopic)
			if test.kind == "static" {
				resources[ordinaryTopic] = fixtureResource(ordinaryTopic, "text/html", []byte(
					`<html><head>`+test.links+`</head><body><main><h1>Guide</h1><p class="used">Text</p></main></body></html>`))
			}
			fetcher := &archiveFixture{resources: resources, calls: map[string]int{},
				failures: map[string]error{ordinaryBroken: test.failure}}
			result, err := HTML(context.Background(), document, fetcher, t.TempDir(), false, 1<<20, 8<<20)
			if err != nil {
				t.Fatalf("archive returned fatal error instead of durable incomplete result: %v", err)
			}
			if len(result.StylesheetRecoveries) != 0 {
				t.Fatalf("ineligible ordinary stylesheet recovery was accepted: %+v", result.StylesheetRecoveries)
			}
			if test.name == "both valid remain distinct" {
				if result.Status != "complete" || len(result.HTML.Assets) != 2 {
					t.Fatalf("two valid publisher styles were not preserved: %+v", result)
				}
				return
			}
			if result.Status == "complete" || len(result.Errors) == 0 {
				t.Fatalf("ineligible ordinary stylesheet did not fail closed: %+v", result)
			}
			for raw, calls := range fetcher.calls {
				if calls > 1 && strings.HasSuffix(raw, ".css") {
					t.Fatalf("stylesheet request was repeated: %s calls=%d all=%v", raw, calls, fetcher.calls)
				}
			}
		})
	}
}

func duplicateOrdinaryLinks() string {
	return `<link rel="stylesheet" href="../Resources/Stylesheets/main.css">
<link rel="stylesheet" href="../../Resources/Stylesheets/main.css">`
}

func ordinaryStylePlan(kind string, urls ...string) model.DocumentPlan {
	topics := make([]model.Topic, 0, len(urls))
	toc := make([]model.TocEntry, 0, len(urls))
	for _, raw := range urls {
		topics = append(topics, model.Topic{URL: raw, Title: "Guide"})
		toc = append(toc, model.TocEntry{Title: "Guide", URL: raw})
	}
	return model.DocumentPlan{
		Document: model.Document{
			ID: "vxlan", Title: "VXLAN Guide", Platform: "6400", Version: "10.13",
			URL: ordinaryTopic, Kind: kind,
		},
		Topics: topics,
		TOC:    toc,
		Inventory: &model.InventoryEvidence{
			Complete: true, Kind: kind, RootURL: ordinaryGuideRoot, Entries: len(toc), UniqueTopics: len(topics),
		},
	}
}
