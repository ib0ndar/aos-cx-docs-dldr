package archive

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

// A publisher can rename section and topic identifiers while its TOC data and
// links keep the originals; the target page's own markup says which element an
// original identifier stands for.
func TestHTMLArchiveAnchorsRenamedPublisherBookmarks(t *testing.T) {
	onThisPage := `<nav id="wh_topic_toc" aria-label="On this page"><ul>` +
		`<li class="topic-item"><a href="#readable-section" data-tocid="GUID-SECTION">Section</a></li></ul></nav>`
	for _, tc := range []struct {
		name, page, fragment, anchoredIn, warning string
	}{
		{
			name:     "on this page link",
			page:     onThisPage + `<main id="mc-main-content"><h1>Home</h1><section id="readable-section"><h2>Section</h2><p>Body</p></section></main>`,
			fragment: "GUID-SECTION", anchoredIn: `<section id="readable-section"><span id="GUID-SECTION"></span>`,
		},
		{
			name: "DITA topic identifiers",
			page: `<main id="mc-main-content"><h1>Home</h1><article class="- topic/topic concept/concept topic" id="renamed-topic">` +
				`<h2>Merged topic</h2><p id="GUID-TOPIC__P_1">One</p><div><p id="GUID-TOPIC__P_2">Two</p></div></article></main>`,
			fragment: "GUID-TOPIC", anchoredIn: `<article class="- topic/topic concept/concept topic" id="renamed-topic"><span id="GUID-TOPIC"></span>`,
		},
		{
			name:     "identifier of an enclosing element",
			page:     `<html><body id="page-top"><main id="mc-main-content"><h1>Home</h1><p>Body</p></main></body></html>`,
			fragment: "page-top", anchoredIn: `<span id="page-top"></span><h1`,
		},
		{
			name: "evidence agrees",
			page: onThisPage + `<main id="mc-main-content"><h1>Home</h1><article class="- topic/topic" id="readable-section">` +
				`<p id="GUID-SECTION__P_1">One</p></article></main>`,
			fragment: "GUID-SECTION", anchoredIn: `<article class="- topic/topic" id="readable-section"><span id="GUID-SECTION"></span>`,
		},
		{
			name: "evidence conflicts",
			page: onThisPage + `<main id="mc-main-content"><h1>Home</h1><section id="readable-section"><p>Body</p></section>` +
				`<article class="- topic/topic" id="other-topic"><p id="GUID-SECTION__P_1">One</p></article></main>`,
			fragment: "GUID-SECTION", warning: "Publisher bookmark #GUID-SECTION is absent",
		},
		{
			name: "identifiers in two topics",
			page: `<main id="mc-main-content"><h1>Home</h1><article class="- topic/topic" id="first"><p id="GUID-TOPIC__P_1">One</p></article>` +
				`<article class="- topic/topic" id="second"><p id="GUID-TOPIC__P_2">Two</p></article></main>`,
			fragment: "GUID-TOPIC", warning: "Publisher bookmark #GUID-TOPIC is absent",
		},
		{
			name: "target outside the content",
			page: `<nav id="wh_topic_toc"><a id="nav-only" href="#nav-only" data-tocid="GUID-NAV">Nav</a></nav>` +
				`<main id="mc-main-content"><h1>Home</h1><p>Body</p></main>`,
			fragment: "GUID-NAV", warning: "Publisher bookmark #GUID-NAV is absent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &archiveFixture{resources: map[string]model.Resource{
				testHome: fixtureResource(testHome, "text/html", []byte(tc.page)),
			}, calls: map[string]int{}}
			plan := flarePlan()
			plan.TOC = append(plan.TOC, model.TocEntry{Title: "Section", URL: testHome + "#" + tc.fragment})
			output := t.TempDir()
			result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 2<<20)
			if err != nil || result.Status != "complete" {
				t.Fatalf("status=%s err=%v errors=%v", result.Status, err, result.Errors)
			}
			page, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
			if err != nil {
				t.Fatal(err)
			}
			corrected := slices.ContainsFunc(result.Notices, func(notice model.Notice) bool {
				return notice.Kind == model.NoticePublisherLinkCorrected && strings.HasPrefix(notice.Message, testHome+": 1 bookmarks")
			})
			absent := slices.ContainsFunc(result.Warnings, func(value string) bool { return strings.Contains(value, "Publisher bookmark #") })
			if tc.warning != "" {
				if corrected || !slices.ContainsFunc(result.Warnings, func(value string) bool { return strings.Contains(value, tc.warning) }) ||
					strings.Contains(string(page), `<span id="`+tc.fragment+`">`) {
					t.Fatalf("unproven alias was anchored: notices=%+v warnings=%v", result.Notices, result.Warnings)
				}
				return
			}
			if !corrected || absent || !strings.Contains(string(page), tc.anchoredIn) {
				t.Fatalf("renamed bookmark was not anchored: notices=%+v warnings=%v\n%s", result.Notices, result.Warnings, page)
			}
			for _, name := range []string{"index.html", "toc.html"} {
				navigation, err := os.ReadFile(filepath.Join(output, name))
				if err != nil || !strings.Contains(string(navigation), "#"+tc.fragment) {
					t.Fatalf("%s lost the publisher TOC link: %v", name, err)
				}
			}
		})
	}
}

func TestBookmarkAnchorsStayOutsideTablesAndLists(t *testing.T) {
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", []byte(`<main id="mc-main-content"><h1>Home</h1>`+
			`<nav id="wh_topic_toc"><a href="#settings" data-tocid="GUID-TABLE">Settings</a></nav>`+
			`<table id="settings"><tbody><tr><td>Value</td></tr></tbody></table></main>`)),
	}, calls: map[string]int{}}
	plan := flarePlan()
	plan.TOC = append(plan.TOC, model.TocEntry{Title: "Settings", URL: testHome + "#GUID-TABLE"})
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, fetcher, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("status=%s err=%v errors=%v", result.Status, err, result.Errors)
	}
	page, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil || !strings.Contains(string(page), `<span id="GUID-TABLE"></span><table id="settings">`) {
		t.Fatalf("table anchor placement: %v\n%s", err, page)
	}
}

func TestHTMLArchiveReportsUnrecordedBookmarkEvidenceOnce(t *testing.T) {
	second := testRoot + "Content/second.htm"
	crowded := func(title string) []byte {
		var page strings.Builder
		page.WriteString(`<main id="mc-main-content"><h1>` + title + `</h1>`)
		for index := range maxRecordedBookmarksPage + 1 {
			page.WriteString(`<span id="b` + strconv.Itoa(index) + `"></span>`)
		}
		page.WriteString(`</main>`)
		return []byte(page.String())
	}
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		testHome: fixtureResource(testHome, "text/html", crowded("Home")),
		second:   fixtureResource(second, "text/html", crowded("Second")),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), flarePlan(
		model.Topic{URL: testHome, Title: "Home"},
		model.Topic{URL: second, Title: "Second"},
	), fetcher, t.TempDir(), false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("status=%s err=%v errors=%v", result.Status, err, result.Errors)
	}
	var reported []string
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "bookmark evidence") {
			reported = append(reported, warning)
		}
	}
	if len(reported) != 1 || !strings.HasPrefix(reported[0], "Source bookmark evidence for 2 topics exceeds archive limits") {
		t.Fatalf("unrecorded evidence is not reported once per guide: %q", reported)
	}
}
