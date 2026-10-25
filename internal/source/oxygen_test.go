package source

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

const oxygenRoot = "https://publisher.example.test/techdocs/AOS-CX/10.18.xxxx/HTML/Guide/"

// oxygenTopicPage is an authored Oxygen WebHelp topic page: the publication
// TOC lists the top level, with the current branch expanded, and the topic
// links its neighbours with rel="prev" and rel="next".
func oxygenTopicPage(title, prev, next, toc string) string {
	links := ""
	if prev != "" {
		links += `<a class="- topic/link link" href="` + prev + `" rel="prev" aria-label="Previous topic">Previous</a>`
	}
	if next != "" {
		links += `<a class="- topic/link link" href="` + next + `" rel="next" aria-label="Next topic">Next</a>`
	}
	return `<!DOCTYPE html><html><head><title>` + title + `</title>
<link rel="stylesheet" href="../oxygen-webhelp/app/topic.css?buildId=1">
<script src="../oxygen-webhelp/app/topic.js?buildId=1"></script></head>
<body class="wh_topic_page"><nav id="wh_publication_toc" aria-label="Table of Contents Container">
<div id="wh_publication_toc_content"><div class=" wh_publication_toc ">` + toc + `</div></div></nav>
<nav class="wh_navigation_links">` + links + `</nav>
<main role="main"><article><div class=" wh_topic_content body "><h1>` + title + `</h1><p>Content.</p></div></article></main>
</body></html>`
}

func oxygenTOCItem(tocID, state, href, title string) string {
	return `<li role="treeitem"><div data-tocid="` + tocID + `" class="topicref" data-id="x" data-state="` + state + `">` +
		`<span role="button" class="wh-expand-btn"></span><div class="title"><a href="` + href + `" id="` + tocID + `-link">` +
		title + `</a><div class="wh-tooltip"><p class="shortdesc"></p></div></div></div></li>`
}

// oxygenTOCBranch is an expanded TOC entry with its rendered children.
func oxygenTOCBranch(tocID, href, title, children string) string {
	return `<li role="treeitem" aria-expanded="true" class="active"><div data-tocid="` + tocID + `" class="topicref" data-state="expanded">` +
		`<span role="button" class="wh-expand-btn"></span><div class="title"><a href="` + href + `" id="` + tocID + `-link">` +
		title + `</a></div></div><ul role="group">` + children + `</ul></li>`
}

func oxygenNavLinks(id, topics string) string {
	return `oxy_webhelp_navlinks = {"` + id + `":{"topics":[` + topics + `]}};`
}

// oxygenFixtureTOC is the publication TOC of the About pages: the About
// branch expanded, Chapter collapsed, a leaf and an external link.
var oxygenFixtureTOC = `<ul role="tree" aria-label="Table of Contents">` +
	oxygenTOCBranch("T1", "about.html", "About", oxygenTOCItem("T1a", "leaf", "sub.html", "Sub")) +
	oxygenTOCItem("T2", "not-ready", "../chapter/chapter.html", "Chapter") +
	oxygenTOCItem("T3", "leaf", "../leaf/leaf.html", "Leaf") +
	oxygenTOCItem("T4", "leaf", "https://other.example.test/elsewhere.html", "Elsewhere") +
	`</ul>`

// The rendered children of the Chapter branch, as pages under chapter/ link
// them.
var (
	oxygenSectionItem = oxygenTOCItem("T2a", "leaf", "chapter.html#GUID-SECTION", "Section")
	oxygenChildItem   = oxygenTOCItem("T2b", "not-ready", "child.html", "Child")
	oxygenInlineItem  = oxygenTOCItem("T2c", "not-ready", "inline.html", "Inline")
)

// oxygenChapterTOC is the publication TOC of a page in the Chapter branch.
func oxygenChapterTOC(children string) string {
	return `<ul role="tree" aria-label="Table of Contents">` +
		oxygenTOCItem("T1", "not-ready", "../about/about.html", "About") +
		oxygenTOCBranch("T2", "chapter.html", "Chapter", children) +
		oxygenTOCItem("T3", "leaf", "../leaf/leaf.html", "Leaf") +
		oxygenTOCItem("T4", "leaf", "https://other.example.test/elsewhere.html", "Elsewhere") +
		`</ul>`
}

// oxygenT2Topics is the nav-links data of the Chapter branch: a section of
// the chapter page, a child with its own data file and a child with inline
// children.
var oxygenT2Topics = strings.Join([]string{
	`{"title":"Section","href":"chapter/chapter.html#GUID-SECTION","menu":{"hasChildren":false},"tocID":"T2a","topics":[]}`,
	`{"title":"Child","href":"chapter/child.html","menu":{"hasChildren":false},"tocID":"T2b","next":"T2b"}`,
	`{"title":"Inline","href":"chapter/inline.html","menu":{"hasChildren":true},"tocID":"T2c","topics":[` +
		`{"title":"Deep","href":"chapter/deep.html","menu":{"hasChildren":false},"tocID":"T2d","topics":[]}]}`,
}, ",")

func oxygenFixture() *planFixture {
	f := &planFixture{responses: map[string]model.Resource{}, failures: map[string]error{}}
	f.set(oxygenRoot+"index.html", `<!DOCTYPE html><html><head><title>Guide</title></head>
<body class="wh_main_page"><main><div class="wh_tiles">
<div class="wh_tile"><div class="wh_tile_text"><div class="wh_tile_title"><a href="about/about.html">About</a></div></div></div>
<div class="wh_tile"><div class="wh_tile_text"><div class="wh_tile_title"><a href="chapter/chapter.html">Chapter</a></div></div></div>
</div></main></body></html>`, "text/html")
	toc := oxygenFixtureTOC
	f.set(oxygenRoot+"about/about.html", oxygenTopicPage("About", "../concept/legal.html", "sub.html", toc), "text/html")
	f.set(oxygenRoot+"concept/legal.html", oxygenTopicPage("Legal disclaimer", "", "", toc), "text/html")
	f.set(oxygenRoot+"chapter/chapter.html", oxygenTopicPage("Chapter", "", "",
		oxygenChapterTOC(oxygenSectionItem+oxygenChildItem+oxygenInlineItem)), "text/html")
	f.set(oxygenRoot+"chapter/child.html", oxygenTopicPage("Child", "", "", oxygenChapterTOC(oxygenSectionItem+
		oxygenTOCBranch("T2b", "child.html", "Child", oxygenTOCItem("T2b1", "leaf", "grand.html", "Grandchild"))+
		oxygenInlineItem)), "text/html")
	f.set(oxygenRoot+"chapter/inline.html", oxygenTopicPage("Inline", "", "", oxygenChapterTOC(oxygenSectionItem+oxygenChildItem+
		oxygenTOCBranch("T2c", "inline.html", "Inline", oxygenTOCItem("T2d", "leaf", "deep.html", "Deep")))), "text/html")
	f.set(oxygenRoot+"leaf/leaf.html", oxygenTopicPage("Leaf", "../chapter/deep.html", "../back/notes.html", toc), "text/html")
	f.set(oxygenRoot+"back/notes.html", oxygenTopicPage("Notes", "../leaf/leaf.html", "", toc), "text/html")
	nav := oxygenRoot + "oxygen-webhelp/app/nav-links/json/"
	f.set(nav+"T1.js", oxygenNavLinks("T1",
		`{"title":"  <span class=\"ph\" id=\"GUID-1\">AOS-CX</span> sub","shortdesc":"<p class=\"shortdesc\"\/>","href":"about/sub.html","attributes":{"data-id":"x"},"menu":{"hasChildren":false},"tocID":"T1a","topics":[]}`),
		"application/javascript")
	f.set(nav+"T2.js", oxygenNavLinks("T2", oxygenT2Topics), "application/javascript")
	f.set(nav+"T2b.js", oxygenNavLinks("T2b",
		`{"title":"Grandchild","href":"chapter/grand.html","menu":{"hasChildren":false},"tocID":"T2b1","topics":[]}`),
		"application/javascript")
	return f
}

func oxygenDocument() model.Document {
	return model.Document{ID: "guide", Title: "Guide", Platform: "8360", Version: "10.18", Kind: "static", URL: oxygenRoot + "index.html"}
}

func tocOutline(entries []model.TocEntry, depth int) []string {
	var lines []string
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("%s%s %s", strings.Repeat("  ", depth), entry.Title, strings.TrimPrefix(entry.URL, oxygenRoot)))
		lines = append(lines, tocOutline(entry.Children, depth+1)...)
	}
	return lines
}

func TestOxygenPlanExpandsNavLinksAndFollowsMatter(t *testing.T) {
	f := oxygenFixture()
	plan, err := LoadPlan(context.Background(), f, oxygenDocument())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Legal disclaimer concept/legal.html",
		"About about/about.html",
		"  AOS-CX sub about/sub.html",
		"Chapter chapter/chapter.html",
		"  Section chapter/chapter.html#GUID-SECTION",
		"  Child chapter/child.html",
		"    Grandchild chapter/grand.html",
		"  Inline chapter/inline.html",
		"    Deep chapter/deep.html",
		"Leaf leaf/leaf.html",
		"Elsewhere https://other.example.test/elsewhere.html",
		"Notes back/notes.html",
	}
	if got := tocOutline(plan.TOC, 0); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Oxygen TOC:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var topics []string
	for _, topic := range plan.Topics {
		topics = append(topics, strings.TrimPrefix(topic.URL, oxygenRoot))
	}
	wantTopics := "concept/legal.html about/about.html about/sub.html chapter/chapter.html chapter/child.html " +
		"chapter/grand.html chapter/inline.html chapter/deep.html leaf/leaf.html back/notes.html"
	if strings.Join(topics, " ") != wantTopics {
		t.Fatalf("topics in reading order: %v", topics)
	}
	nav := oxygenRoot + "oxygen-webhelp/app/nav-links/json/"
	if plan.Document.Kind != "static" || plan.Inventory == nil || !plan.Inventory.Complete ||
		plan.Inventory.RootURL != oxygenRoot || plan.Inventory.TOCURL != oxygenRoot+"about/about.html" ||
		strings.Join(plan.Inventory.ChunkURLs, " ") != nav+"T1.js "+nav+"T2.js "+nav+"T2b.js" ||
		plan.Inventory.Entries != 12 || plan.Inventory.UniqueTopics != 10 {
		t.Fatalf("inventory evidence: kind=%s %+v", plan.Document.Kind, plan.Inventory)
	}
	if len(plan.Styles) != 1 || plan.Styles[0] != oxygenRoot+"oxygen-webhelp/app/topic.css?buildId=1" {
		t.Fatalf("styles: %v", plan.Styles)
	}
	external := false
	for _, notice := range plan.Notices {
		external = external || strings.Contains(notice.Message, "https://other.example.test/elsewhere.html")
	}
	if !external {
		t.Fatalf("external TOC link was not retained as navigation: %+v", plan.Notices)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "T3.js") || strings.Contains(call, "T2a.js") || strings.Contains(call, "T2d.js") {
			t.Fatalf("leaf or inline entry fetched TOC data: %v", f.calls)
		}
	}
}

func TestOxygenPlanFailsClosed(t *testing.T) {
	nav := oxygenRoot + "oxygen-webhelp/app/nav-links/json/"
	for _, tc := range []struct {
		name, want string
		edit       func(*planFixture)
	}{
		{"missing data", "nav-links/json/T2b.js", func(f *planFixture) { delete(f.responses, nav+"T2b.js") }},
		{"wrong id", `does not describe "T2b"`, func(f *planFixture) {
			f.set(nav+"T2b.js", oxygenNavLinks("other", ``), "application/javascript")
		}},
		{"two ids", `does not describe "T2b"`, func(f *planFixture) {
			f.set(nav+"T2b.js", `oxy_webhelp_navlinks = {"T2b":{"topics":[]},"x":{"topics":[]}};`, "application/javascript")
		}},
		{"script", "expected one oxy_webhelp_navlinks assignment", func(f *planFixture) {
			f.set(nav+"T2b.js", `oxy_webhelp_navlinks = {"T2b":{"topics":[]}}; fetch("x");`, "application/javascript")
		}},
		{"cycle", `"T2" is referenced more than once`, func(f *planFixture) {
			f.set(nav+"T2b.js", oxygenNavLinks("T2b", `{"title":"Loop","href":"chapter/grand.html","tocID":"T2b1","next":"T2"}`), "application/javascript")
		}},
		{"bad id", `invalid Oxygen TOC identifier "../T2"`, func(f *planFixture) {
			f.set(nav+"T2.js", oxygenNavLinks("T2", strings.Replace(oxygenT2Topics, `"next":"T2b"`, `"next":"../T2"`, 1)), "application/javascript")
		}},
		{"empty title", "empty title", func(f *planFixture) {
			f.set(nav+"T2b.js", oxygenNavLinks("T2b", `{"title":"<span></span>","href":"chapter/grand.html","tocID":"T2b1"}`), "application/javascript")
		}},
		{"html data", "received text/html", func(f *planFixture) { f.set(nav+"T2b.js", oxygenNavLinks("T2b", ``), "text/html") }},
		{"no toc", "must contain one tree", func(f *planFixture) {
			f.set(oxygenRoot+"about/about.html", oxygenTopicPage("About", "../concept/legal.html", "", ""), "text/html")
		}},
		{"untitled matter", "has no title", func(f *planFixture) {
			f.set(oxygenRoot+"concept/legal.html", strings.Replace(oxygenTopicPage("x", "", "", ""), "<title>x</title>", "", 1), "text/html")
		}},
		{"rendered TOC disagrees", `lists different topics than the rendered TOC`, func(f *planFixture) {
			f.set(oxygenRoot+"chapter/chapter.html", oxygenTopicPage("Chapter", "", "",
				oxygenChapterTOC(oxygenSectionItem+oxygenChildItem)), "text/html")
		}},
		{"conflicting prev", `conflicting rel="prev" links`, func(f *planFixture) {
			page := strings.Replace(oxygenTopicPage("About", "../concept/legal.html", "sub.html", oxygenFixtureTOC),
				`<nav class="wh_navigation_links">`, `<nav class="wh_navigation_links"><a href="../concept/other.html" rel="prev">Other</a>`, 1)
			f.set(oxygenRoot+"about/about.html", page, "text/html")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := oxygenFixture()
			tc.edit(f)
			plan, err := LoadPlan(context.Background(), f, oxygenDocument())
			if err == nil {
				t.Fatalf("unsafe Oxygen source planned: %s", strings.Join(tocOutline(plan.TOC, 0), "\n"))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestOxygenMatterStopsAtTOCAndPublicationBoundary(t *testing.T) {
	f := oxygenFixture()
	// The front matter links back into the TOC and the back matter leaves the
	// publication; neither is followed.
	f.set(oxygenRoot+"concept/legal.html", oxygenTopicPage("Legal disclaimer", "../leaf/leaf.html", "", ""), "text/html")
	f.set(oxygenRoot+"back/notes.html", oxygenTopicPage("Notes", "", "https://other.example.test/next.html", ""), "text/html")
	plan, err := LoadPlan(context.Background(), f, oxygenDocument())
	if err != nil {
		t.Fatal(err)
	}
	outline := tocOutline(plan.TOC, 0)
	if outline[0] != "Legal disclaimer concept/legal.html" || outline[len(outline)-1] != "Notes back/notes.html" {
		t.Fatalf("matter was not bounded by the TOC and publication: %v", outline)
	}
}

func TestStaticNonOxygenLazyTOCIsStillRefused(t *testing.T) {
	f := &planFixture{responses: map[string]model.Resource{}, failures: map[string]error{}}
	home := "https://publisher.example.test/book/index.html"
	f.set(home, `<html><body><nav class="wh_publication_toc"><ul><li><div class="topicref" data-state="not-ready">`+
		`<a href="topic.html">Topic</a></div></li></ul></nav></body></html>`, "text/html")
	doc := model.Document{ID: "guide", Title: "Guide", Platform: "6300", Version: "10.18", Kind: "static", URL: home}
	if _, err := LoadPlan(context.Background(), f, doc); err == nil || !strings.Contains(err.Error(), "lazy-loaded") {
		t.Fatalf("lazy TOC outside an Oxygen main page was accepted: %v", err)
	}
}

func TestOxygenRenderedTOCReplacesUnpublishedDataLinks(t *testing.T) {
	// The publisher's TOC data can name a topic file that was never published,
	// while the rendered TOC of the branch page links the published file.
	f := oxygenFixture()
	nav := oxygenRoot + "oxygen-webhelp/app/nav-links/json/"
	f.set(nav+"T2.js", oxygenNavLinks("T2", strings.Replace(oxygenT2Topics,
		`"href":"chapter/child.html"`, `"href":"GUID-CHILD.html"`, 1)), "application/javascript")
	plan, err := LoadPlan(context.Background(), f, oxygenDocument())
	if err != nil {
		t.Fatal(err)
	}
	outline := strings.Join(tocOutline(plan.TOC, 0), "\n")
	if !strings.Contains(outline, "  Child chapter/child.html\n    Grandchild chapter/grand.html") || strings.Contains(outline, "GUID-CHILD") {
		t.Fatalf("unpublished data link was kept:\n%s", outline)
	}
	want := `Publisher TOC data links "Child" to ` + oxygenRoot + `GUID-CHILD.html; using ` + oxygenRoot +
		`chapter/child.html from the rendered TOC of ` + oxygenRoot + `chapter/chapter.html`
	if !slices.Contains(plan.Notices, model.Notice{Kind: model.NoticePublisherLinkCorrected, Message: want}) || len(plan.Warnings) != 0 {
		t.Fatalf("correction is not recorded as a notice: %+v %q", plan.Notices, plan.Warnings)
	}
	if slices.Contains(f.calls, oxygenRoot+"GUID-CHILD.html") {
		t.Fatalf("unpublished topic was requested: %v", f.calls)
	}
}

// levelFetcher holds the named requests until all of them are in flight.
type levelFetcher struct {
	*planFixture
	together map[string]bool
	arrived  chan struct{}
	open     chan struct{}
}

func (l *levelFetcher) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	if l.together[raw] {
		l.arrived <- struct{}{}
		select {
		case <-l.open:
		case <-time.After(5 * time.Second):
			return model.Resource{}, errors.New("level files were not requested together: " + raw)
		}
	}
	return l.planFixture.Get(ctx, raw, refresh)
}

func TestOxygenPlanReadsEachLevelTogether(t *testing.T) {
	serial, err := LoadPlan(context.Background(), oxygenFixture(), oxygenDocument())
	if err != nil {
		t.Fatal(err)
	}
	nav := oxygenRoot + "oxygen-webhelp/app/nav-links/json/"
	l := &levelFetcher{planFixture: oxygenFixture(), together: map[string]bool{nav + "T1.js": true, nav + "T2.js": true},
		arrived: make(chan struct{}, 2), open: make(chan struct{})}
	go func() {
		<-l.arrived
		<-l.arrived
		close(l.open)
	}()
	plan, err := LoadPlanWithWorkers(context.Background(), l, oxygenDocument(), false, false, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(tocOutline(plan.TOC, 0), "\n"), strings.Join(tocOutline(serial.TOC, 0), "\n"); got != want {
		t.Fatalf("concurrent TOC:\n%s\nserial TOC:\n%s", got, want)
	}
	inputs := func(plan model.DocumentPlan) []string {
		var result []string
		for _, input := range plan.Inputs {
			result = append(result, input.Role+" "+input.RequestedURL)
		}
		return result
	}
	if !slices.Equal(inputs(plan), inputs(serial)) || !slices.Equal(plan.Inventory.ChunkURLs, serial.Inventory.ChunkURLs) ||
		len(plan.Topics) != len(serial.Topics) {
		t.Fatalf("concurrent inputs %v\nserial inputs %v", inputs(plan), inputs(serial))
	}
}
