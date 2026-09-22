package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

func TestHPEOverviewIsSeparateFromContents(t *testing.T) {
	for _, layout := range []string{"siblings", "wrapper", "topic-entrypoint"} {
		t.Run(layout, func(t *testing.T) {
			plan := hpeArchivePlan()
			front := plan.TOC[0]
			topic := front.Children[0]
			if layout != "wrapper" {
				front.Children = nil
				plan.TOC = []model.TocEntry{front, topic}
			}
			if layout == "topic-entrypoint" {
				plan.Document.URL = plan.Topics[1].URL
			}
			before, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			output := t.TempDir()
			result, err := HTML(context.Background(), plan,
				&archiveFixture{resources: hpeResources(), calls: map[string]int{}},
				output, false, 2<<20, 8<<20)
			if err != nil || result.Status != "complete" {
				t.Fatalf("HPE navigation archive failed: %+v %v", result, err)
			}
			after, err := json.Marshal(plan)
			if err != nil || !bytes.Equal(before, after) || len(result.HTML.Topics) != 2 {
				t.Fatal("presentation changed the source plan or removed archived front matter")
			}
			for _, name := range []string{"index.html", "toc.html"} {
				body, err := os.ReadFile(filepath.Join(output, name))
				if err != nil {
					t.Fatal(err)
				}
				doc, err := html.Parse(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				overview := findNode(doc, func(n *html.Node) bool { return hasClass(n, "archive-overview") })
				tree := findNode(doc, func(n *html.Node) bool { return hasClass(n, "toc-tree") })
				if overview == nil || tree == nil {
					t.Fatalf("%s: overview must be separate from the contents tree", name)
				}
				link := findNode(overview, func(n *html.Node) bool { return n.Data == "a" })
				if link == nil || attr(link, "href") != result.HTML.Topics[0].Path+"#front" ||
					attr(link, "id") == "" || !strings.Contains(nodeText(overview), "Guide overview") {
					t.Fatalf("%s: overview lost its local destination, fragment or TOC target", name)
				}
				var chapters []*html.Node
				walk(tree, func(n *html.Node) {
					if n.Data == "a" {
						chapters = append(chapters, n)
					}
				})
				if len(chapters) != 1 || attr(chapters[0], "href") != result.HTML.Topics[1].Path+"#topic" {
					t.Fatalf("%s: contents must retain the chapter, not the overview", name)
				}
				if strings.Index(string(body), `class="archive-overview"`) >
					strings.Index(string(body), "<h2>Contents</h2>") {
					t.Fatalf("%s: overview must precede the Contents heading", name)
				}
			}
		})
	}
}

func TestFlareFrontRemainsInContents(t *testing.T) {
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), &archiveFixture{
		resources: map[string]model.Resource{
			testHome: fixtureResource(testHome, "text/html", []byte(`<main id="mc-main-content"><h1>Home</h1></main>`)),
		},
		calls: map[string]int{},
	}, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("Flare navigation archive failed: %+v %v", result, err)
	}
	body, err := os.ReadFile(filepath.Join(output, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if findNode(doc, func(n *html.Node) bool { return hasClass(n, "archive-overview") }) != nil {
		t.Fatal("HPE overview presentation leaked into Flare")
	}
	tree := findNode(doc, func(n *html.Node) bool { return hasClass(n, "toc-tree") })
	if tree == nil || findNode(tree, func(n *html.Node) bool {
		return n.Data == "a" && attr(n, "href") == result.HTML.Topics[0].Path
	}) == nil {
		t.Fatal("Flare front topic disappeared from Contents")
	}
}

func TestGuideSearchStartsWithEmptyHiddenResults(t *testing.T) {
	output := t.TempDir()
	result, err := HTML(context.Background(), flarePlan(), &archiveFixture{
		resources: map[string]model.Resource{
			testHome: fixtureResource(testHome, "text/html", []byte(`<main id="mc-main-content"><h1>Home</h1></main>`)),
		},
		calls: map[string]int{},
	}, output, false, 1<<20, 2<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("search archive failed: %+v %v", result, err)
	}
	body, err := os.ReadFile(filepath.Join(output, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	list := findNode(doc, func(n *html.Node) bool { return hasAttr(n, "data-guide-results") })
	input := findNode(doc, func(n *html.Node) bool { return hasAttr(n, "data-guide-search") })
	status := findNode(doc, func(n *html.Node) bool { return hasAttr(n, "data-guide-search-status") })
	if list == nil || !hasAttr(list, "hidden") || list.FirstChild != nil || input == nil || status == nil ||
		attr(input, "aria-describedby") != attr(status, "id") || attr(status, "role") != "status" {
		t.Fatal("search must start with an empty hidden result list and an accessible prompt")
	}
	if strings.Contains(guideSearchJS, "!q ||") {
		t.Fatal("empty search must not match every topic")
	}
}
