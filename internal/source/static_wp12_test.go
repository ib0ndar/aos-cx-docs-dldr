package source

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func staticWP12Document(home string) model.Document {
	return model.Document{
		ID: "static-wp12", Title: "Static WP12 Guide", Platform: "6300",
		Version: "10.18", Kind: "static", URL: home,
	}
}

func TestStaticAuthoredInlineCorpusPreservesBaseGroupsAndRepeatedPositions(t *testing.T) {
	root := "https://publisher.example.test/book/"
	home := root + "index.html"
	f := &planFixture{responses: map[string]model.Resource{}}
	f.set(home, `<html><head><base href="content/"><link rel="stylesheet" href="../styles/site.css"></head><body>
<nav class="wh_publication_toc" aria-label="TABLE OF CONTENTS"><ul>
<li><h2>Operations</h2><ul>
<li><a href="chapter.html?view=full#start">Chapter start</a></li>
<li><a href="chapter.html?view=full#repeat">Chapter repeated</a></li>
</ul></li>
<li><button type="button">Reference</button><ol>
<li><a href="../reference.xhtml?edition=2#commands">Commands</a></li>
</ol></li>
<li><a href="https://support.example.test/kb">Support</a></li>
</ul></nav></body></html>`, "text/html")

	plan, err := LoadPlan(context.Background(), f, staticWP12Document(home))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Inventory.Complete || plan.Inventory.Kind != "static" ||
		plan.Inventory.RootURL != root || plan.Inventory.TOCURL != home ||
		plan.Inventory.Entries != 7 || plan.Inventory.UniqueTopics != 3 {
		t.Fatalf("static inline inventory evidence incomplete: %+v", plan.Inventory)
	}
	if len(plan.Topics) != 3 ||
		plan.Topics[0].URL != home || plan.Topics[0].FetchURL != home ||
		plan.Topics[1].URL != root+"content/chapter.html?view=full" ||
		plan.Topics[2].URL != root+"reference.xhtml?edition=2" {
		t.Fatalf("public/fetch topic identity changed: %+v", plan.Topics)
	}
	if len(plan.TOC) != 4 || plan.TOC[1].Title != "Operations" ||
		plan.TOC[2].Title != "Reference" ||
		len(plan.TOC[1].Children) != 2 ||
		plan.TOC[1].Children[0].URL != root+"content/chapter.html?view=full#start" ||
		plan.TOC[1].Children[1].URL != root+"content/chapter.html?view=full#repeat" ||
		plan.TOC[2].Children[0].URL != root+"reference.xhtml?edition=2#commands" {
		t.Fatalf("static hierarchy/repeated positions changed: %+v", plan.TOC)
	}
	if !slices.Equal(plan.Styles, []string{root + "styles/site.css"}) ||
		len(plan.Notices) != 1 || plan.Notices[0].Kind != model.NoticeExternalLinkRetained ||
		len(f.calls) != 1 {
		t.Fatalf("static base/style/external handling incomplete: plan=%+v calls=%v", plan, f.calls)
	}
}

func TestStaticDetailedTOCUsesItsOwnBaseAndKeepsRedirectIdentity(t *testing.T) {
	root := "https://publisher.example.test/book/"
	home := root + "index.html"
	toc := root + "navigation/toc.html?edition=2"
	tocFinal := root + "navigation/canonical-toc.html?edition=2"
	f := &planFixture{responses: map[string]model.Resource{}}
	f.set(home, `<html><head><base href="navigation/"><link rel="stylesheet" href="../styles/front.css"></head>
<body><a href="toc.html?edition=2">Detailed Table of Contents</a><a href="shortcut.html">Shortcut</a></body></html>`, "text/html")
	f.set(toc, `<html><head><base href="../topics/"><link rel="stylesheet" href="../styles/toc.css"></head>
<body><nav role="DOC-TOC"><ol><li><span>Commands</span><ul>
<li><a href="chapter.html?mode=full#section">Chapter</a></li>
</ul></li></ol></nav></body></html>`, "text/html")
	resource := f.responses[toc]
	resource.URL = tocFinal
	f.responses[toc] = resource

	plan, err := LoadPlan(context.Background(), f, staticWP12Document(home))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Inventory.TOCURL != tocFinal || len(plan.Topics) != 3 ||
		plan.Topics[1].URL != toc || plan.Topics[1].FetchURL != tocFinal ||
		plan.Topics[2].URL != root+"topics/chapter.html?mode=full" ||
		plan.TOC[2].Children[0].URL != root+"topics/chapter.html?mode=full#section" {
		t.Fatalf("detailed TOC public/fetch/base identity changed: %+v", plan)
	}
	if !slices.Equal(plan.Styles, []string{root + "styles/front.css", root + "styles/toc.css"}) ||
		!slices.Equal(f.calls, []string{home, toc}) {
		t.Fatalf("detailed TOC style/request inventory changed: styles=%v calls=%v", plan.Styles, f.calls)
	}
}

func TestStaticRejectsAmbiguousOrOutOfScopeBaseURLs(t *testing.T) {
	root := "https://publisher.example.test/book/"
	home := root + "index.html"
	for _, test := range []struct {
		name string
		base string
		want string
	}{
		{"multiple", `<base href="./"><base href="topics/">`, "ambiguous"},
		{"empty", `<base href="">`, "invalid"},
		{"foreign", `<base href="https://outside.example.test/book/">`, "outside"},
		{"traversal", `<base href="../other/">`, "outside"},
		{"unsupported-scheme", `<base href="data:text/html,ignored">`, "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &planFixture{responses: map[string]model.Resource{}}
			f.set(home, `<html><head>`+test.base+`</head><body><nav id="toc"><ul>
<li><a href="topic.html">Topic</a></li></ul></nav></body></html>`, "text/html")
			_, err := LoadPlan(context.Background(), f, staticWP12Document(home))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("unsafe static base accepted: err=%v", err)
			}
		})
	}
}

func TestStaticRejectsLazyAndUnknownNavigationShapes(t *testing.T) {
	root := "https://publisher.example.test/book/"
	home := root + "index.html"
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"aria-busy", `<nav id="toc" aria-busy="TRUE"><ul><li><a href="topic.html">Topic</a></li></ul></nav>`, "lazy"},
		{"pending-state", `<nav id="toc"><ul><li data-state="pending"><a href="topic.html">Topic</a></li></ul></nav>`, "lazy"},
		{"not-ready", `<nav id="toc"><ul><li class="not-ready"><a href="topic.html">Topic</a></li></ul></nav>`, "lazy"},
		{"unloaded", `<nav id="toc" data-loaded="false"><ul><li><a href="topic.html">Topic</a></li></ul></nav>`, "lazy"},
		{"lazy-flag", `<nav id="toc"><ul data-lazy="true"><li><a href="topic.html">Topic</a></li></ul></nav>`, "lazy"},
		{"dynamic-placeholder", `<nav id="toc"><div data-toc-source="toc.json"></div><script>loadToc()</script></nav>`, "nested list"},
		{"generic-navigation", `<nav><ul><li><a href="topic.html">Topic</a></li></ul></nav>`, "explicit complete toc"},
		{"group-leaf", `<nav id="toc"><ul><li><button>Unresolved group</button></li></ul></nav>`, "leaf lacks"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &planFixture{responses: map[string]model.Resource{}}
			f.set(home, `<html><body>`+test.body+`</body></html>`, "text/html")
			_, err := LoadPlan(context.Background(), f, staticWP12Document(home))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("unsupported static navigation accepted: err=%v", err)
			}
		})
	}
}

func TestStaticPlannerHonorsCancellationDepthAndRedirectScope(t *testing.T) {
	root := "https://publisher.example.test/book/"
	home := root + "index.html"

	t.Run("cancelled", func(t *testing.T) {
		f := &planFixture{responses: map[string]model.Resource{}}
		f.set(home, `<nav id="toc"><ul><li><a href="topic.html">Topic</a></li></ul></nav>`, "text/html")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := LoadPlan(ctx, f, staticWP12Document(home)); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled static planner returned %v", err)
		}
	})

	t.Run("depth", func(t *testing.T) {
		var body strings.Builder
		body.WriteString(`<nav id="toc"><ul>`)
		for range maxDepth + 2 {
			body.WriteString(`<li><span>Group</span><ul>`)
		}
		body.WriteString(`<li><a href="topic.html">Topic</a></li>`)
		for range maxDepth + 2 {
			body.WriteString(`</ul></li>`)
		}
		body.WriteString(`</ul></nav>`)
		f := &planFixture{responses: map[string]model.Resource{}}
		f.set(home, body.String(), "text/html")
		if _, err := LoadPlan(context.Background(), f, staticWP12Document(home)); err == nil ||
			!strings.Contains(err.Error(), "nesting/entry limit") {
			t.Fatalf("oversized static nesting accepted: %v", err)
		}
	})

	t.Run("foreign-front-redirect", func(t *testing.T) {
		f := &planFixture{responses: map[string]model.Resource{}}
		f.set(home, `<nav id="toc"><ul><li><a href="topic.html">Topic</a></li></ul></nav>`, "text/html")
		resource := f.responses[home]
		resource.URL = "https://outside.example.test/book/index.html"
		resource.Headers = http.Header{"Content-Type": {"text/html"}}
		f.responses[home] = resource
		if _, err := LoadPlan(context.Background(), f, staticWP12Document(home)); err == nil ||
			!strings.Contains(err.Error(), "outside the selected document scope") {
			t.Fatalf("foreign static redirect accepted: %v", err)
		}
	})
}
