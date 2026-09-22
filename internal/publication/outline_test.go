package publication

import (
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func TestNormalizeOutlineWrappersPromotesValidatedFlareNavigation(t *testing.T) {
	home := "https://docs.example.test/guide/Content/home.htm"
	contents := "https://docs.example.test/guide/Content/contents.htm"
	about := "https://docs.example.test/guide/Content/about.htm"
	plan := model.DocumentPlan{
		Document: model.Document{
			Title: "Fundamentals Guide", Platform: "8360", Version: "10.13",
			URL: home, Kind: "flare",
		},
		Topics: []model.Topic{{URL: home}, {URL: contents}, {URL: about}},
		TOC: []model.TocEntry{
			{Title: "Fundamentals Guide", URL: home},
			{Title: "Table of Contents", URL: contents},
			{
				Title: "Home", URL: home,
				Children: []model.TocEntry{{
					Title: "AOS-CX 10.13 Fundamentals Guide",
					Children: []model.TocEntry{
						{Title: "About this document", URL: about},
						{Title: "About AOS-CX", Children: []model.TocEntry{{
							Title: "AOS-CX CLI", URL: "https://docs.example.test/guide/Content/cli.htm",
						}}},
					},
				}},
			},
			{Title: "Support", URL: "https://support.example.test/contact"},
			{Title: "Knowledge Base", URL: "https://community.example.test/kb"},
		},
	}
	entries, err := normalizeOutlineWrappers(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 || entries[0].Title != "About this document" ||
		entries[1].Title != "About AOS-CX" || len(entries[1].Children) != 1 ||
		entries[2].Title != "Support" || entries[3].Title != "Knowledge Base" {
		t.Fatalf("validated Flare wrappers were not promoted: %+v", entries)
	}
}

func TestNormalizeOutlineWrappersOmitsValidatedFlareTitlePageLeaf(t *testing.T) {
	home := "https://docs.example.test/guide/Content/home.htm"
	contents := "https://docs.example.test/guide/Content/fir-int.htm"
	titlePage := "https://docs.example.test/guide/Content/tit.htm"
	plan := model.DocumentPlan{
		Document: model.Document{
			Title: "High Availability Guide", Platform: "8360", Version: "10.13",
			URL: home, Kind: "flare",
		},
		Topics: []model.Topic{{URL: home}, {URL: contents}, {URL: titlePage}},
		TOC: []model.TocEntry{
			{Title: "High Availability Guide", URL: home},
			{Title: "Table of Contents", URL: contents},
			{
				Title: "Home", URL: home,
				Children: []model.TocEntry{{
					Title: "AOS-CX 10.12 High Availability Guide",
					Children: []model.TocEntry{
						{Title: "About this document", URL: "https://docs.example.test/guide/Content/about.htm"},
						{Title: "High availability", URL: "https://docs.example.test/guide/Content/ha.htm"},
						{Title: "BFD", URL: "https://docs.example.test/guide/Content/bfd.htm"},
					},
				}},
			},
			{Title: "Support", URL: "https://support.example.test/contact"},
			{Title: "Title", URL: titlePage},
			{Title: "Knowledge Base", URL: "https://community.example.test/kb"},
		},
	}
	entries, err := normalizeOutlineWrappers(plan)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Title)
	}
	want := []string{"About this document", "High availability", "BFD", "Support", "Knowledge Base"}
	if !slices.Equal(got, want) {
		t.Fatalf("validated Flare title page was not omitted: got=%v want=%v", got, want)
	}
}

func TestFlareTitlePageNavigationLeafRequiresExactTrailingIdentity(t *testing.T) {
	home := "https://docs.example.test/guide/Content/home.htm"
	valid := "https://docs.example.test/guide/Content/tit.htm"
	base := model.DocumentPlan{
		Document: model.Document{Title: "Guide", URL: home, Kind: "flare"},
		Topics:   []model.Topic{{URL: valid}},
	}
	tests := []struct {
		name  string
		entry model.TocEntry
		plan  func(model.DocumentPlan) model.DocumentPlan
	}{
		{name: "valid", entry: model.TocEntry{Title: "Title", URL: valid}},
		{name: "wrong path", entry: model.TocEntry{Title: "Title", URL: "https://docs.example.test/guide/Content/topic.htm"}},
		{name: "substantive title", entry: model.TocEntry{Title: "System title", URL: valid}},
		{name: "children", entry: model.TocEntry{Title: "Title", URL: valid, Children: []model.TocEntry{{Title: "Child"}}}},
		{name: "query", entry: model.TocEntry{Title: "Title", URL: valid + "?print=true"}},
		{name: "fragment", entry: model.TocEntry{Title: "Title", URL: valid + "#cover"}},
		{name: "foreign", entry: model.TocEntry{Title: "Title", URL: "https://foreign.example.test/guide/Content/tit.htm"},
			plan: func(plan model.DocumentPlan) model.DocumentPlan {
				plan.Topics = []model.Topic{{URL: "https://foreign.example.test/guide/Content/tit.htm"}}
				return plan
			}},
		{name: "hpe", entry: model.TocEntry{Title: "Title", URL: valid},
			plan: func(plan model.DocumentPlan) model.DocumentPlan {
				plan.Document.Kind = "hpe"
				return plan
			}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := base
			if test.plan != nil {
				plan = test.plan(plan)
			}
			got := isSourceTitlePageNavigationLeaf(test.entry, plan)
			if got != (test.name == "valid") {
				t.Fatalf("title-page classification=%v for %+v", got, test.entry)
			}
		})
	}
}

func TestNormalizeOutlineWrappersRejectsDuplicateFlareTitlePageLeaves(t *testing.T) {
	home := "https://docs.example.test/guide/Content/home.htm"
	titlePage := "https://docs.example.test/guide/Content/tit.htm"
	plan := model.DocumentPlan{
		Document: model.Document{Title: "Guide", URL: home, Kind: "flare"},
		Topics:   []model.Topic{{URL: home}, {URL: titlePage}},
		TOC: []model.TocEntry{
			{Title: "Guide", URL: home},
			{Title: "Home", URL: home, Children: []model.TocEntry{{Title: "Section"}}},
			{Title: "Title", URL: titlePage},
			{Title: "Title", URL: titlePage},
		},
	}
	if _, err := normalizeOutlineWrappers(plan); err == nil ||
		!strings.Contains(err.Error(), "duplicate title-page") {
		t.Fatalf("duplicate title-page leaves were accepted: %v", err)
	}
}

func TestNormalizeOutlineWrappersRejectsAmbiguousFlareTitlePageLeaves(t *testing.T) {
	home := "https://docs.example.test/guide/Content/home.htm"
	valid := "https://docs.example.test/guide/Content/tit.htm"
	tests := []struct {
		name  string
		entry model.TocEntry
		kind  string
	}{
		{name: "wrong path", entry: model.TocEntry{Title: "Title", URL: "https://docs.example.test/guide/Content/topic.htm"}},
		{name: "substantive label", entry: model.TocEntry{Title: "System title", URL: valid}},
		{name: "children", entry: model.TocEntry{Title: "Title", URL: valid, Children: []model.TocEntry{{Title: "Child"}}}},
		{name: "query", entry: model.TocEntry{Title: "Title", URL: valid + "?print=true"}},
		{name: "foreign", entry: model.TocEntry{Title: "Title", URL: "https://foreign.example.test/guide/Content/tit.htm"}},
		{name: "hpe", entry: model.TocEntry{Title: "Title", URL: valid}, kind: "hpe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind := test.kind
			if kind == "" {
				kind = "flare"
			}
			plan := model.DocumentPlan{
				Document: model.Document{Title: "Guide", URL: home, Kind: kind},
				Topics: []model.Topic{
					{URL: home},
					{URL: test.entry.URL},
				},
				TOC: []model.TocEntry{
					{Title: "Guide", URL: home},
					{Title: "Home", URL: home, Children: []model.TocEntry{{Title: "Section"}}},
					test.entry,
				},
			}
			if _, err := normalizeOutlineWrappers(plan); err == nil ||
				!strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("ambiguous title-page leaf was accepted: %v", err)
			}
		})
	}
}

func TestNormalizeOutlineWrappersPreservesHPEAndNestedLookalikes(t *testing.T) {
	plan := model.DocumentPlan{
		Document: model.Document{
			Title: "Job Scheduler Guide", Platform: "6300", Version: "10.18.xxxx",
			URL: "https://support.example.test/docDisplay?docId=a", Kind: "hpe",
		},
		TOC: []model.TocEntry{
			{Title: "Overview", URL: "https://support.example.test/docDisplay?docId=a&page=index.html"},
			{
				Title: "Operations",
				Children: []model.TocEntry{
					{Title: "Home", URL: "https://support.example.test/docDisplay?docId=a&page=home.html"},
					{Title: "Table of Contents", URL: "https://support.example.test/docDisplay?docId=a&page=toc.html"},
				},
			},
		},
	}
	entries, err := normalizeOutlineWrappers(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || len(entries[1].Children) != 2 ||
		entries[1].Children[0].Title != "Home" ||
		entries[1].Children[1].Title != "Table of Contents" {
		t.Fatalf("HPE or nested substantive lookalikes changed: %+v", entries)
	}
}

func TestNormalizeOutlineWrappersPreservesNestedFlareTitleLeaf(t *testing.T) {
	home := "https://docs.example.test/guide/Content/home.htm"
	titlePage := "https://docs.example.test/guide/Content/tit.htm"
	plan := model.DocumentPlan{
		Document: model.Document{Title: "Guide", URL: home, Kind: "flare"},
		Topics:   []model.Topic{{URL: titlePage}},
		TOC: []model.TocEntry{{
			Title: "Commands",
			Children: []model.TocEntry{{
				Title: "Title", URL: titlePage,
			}},
		}},
	}
	entries, err := normalizeOutlineWrappers(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].Children) != 1 ||
		entries[0].Children[0].Title != "Title" {
		t.Fatalf("nested substantive Title changed: %+v", entries)
	}
}

func TestNormalizeOutlineWrappersAcceptsNavigationLeafBeforeSubstantiveRoots(t *testing.T) {
	home := "https://docs.example.test/guide/Content/home.htm"
	plan := model.DocumentPlan{
		Document: model.Document{
			Title: "Guide", Platform: "6300", Version: "10.16", URL: home, Kind: "flare",
		},
		Topics: []model.Topic{{URL: home}},
		TOC: []model.TocEntry{
			{Title: "Guide", URL: home},
			{Title: "Substantive", URL: "https://docs.example.test/guide/Content/topic.htm"},
			{Title: "Commands", URL: "https://docs.example.test/guide/Content/commands.htm"},
		},
	}
	entries, err := normalizeOutlineWrappers(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Title != "Substantive" || entries[1].Title != "Commands" {
		t.Fatalf("substantive roots after a navigation leaf changed: %+v", entries)
	}
}

func TestNormalizeOutlineWrappersRejectsAmbiguousRootShapes(t *testing.T) {
	home := "https://docs.example.test/guide/Content/home.htm"
	plan := model.DocumentPlan{
		Document: model.Document{
			Title: "Guide", Platform: "6300", Version: "10.16", URL: home, Kind: "flare",
		},
		Topics: []model.Topic{{URL: home}},
		TOC: []model.TocEntry{
			{Title: "Guide", URL: home},
			{Title: "Substantive", URL: "https://docs.example.test/guide/Content/topic.htm"},
			{Title: "Home", URL: home, Children: []model.TocEntry{{Title: "One"}}},
		},
	}
	if _, err := normalizeOutlineWrappers(plan); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("wrapper after substantive root content was accepted: %v", err)
	}

	plan.TOC = []model.TocEntry{
		{Title: "Home", URL: home, Children: []model.TocEntry{{Title: "One"}}},
		{Title: "Two"},
	}
	if _, err := normalizeOutlineWrappers(plan); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("wrapper beside substantive root content was accepted: %v", err)
	}
}

func TestGuideWrapperTitleRequiresDocumentIdentityPrefix(t *testing.T) {
	document := model.Document{Title: "Fundamentals Guide", Platform: "8360", Version: "10.13"}
	for _, title := range []string{
		"Fundamentals Guide",
		"AOS-CX 10.13 Fundamentals Guide",
		"AOS-CX 10.14 Fundamentals Guide",
		"8360 Fundamentals Guide",
	} {
		if !guideWrapperTitle(title, document) {
			t.Fatalf("document wrapper title %q was rejected", title)
		}
	}
	for _, title := range []string{
		"Other Fundamentals Guide",
		"10.14 Other Fundamentals Guide",
		"Fundamentals Guide Reference",
	} {
		if guideWrapperTitle(title, document) {
			t.Fatalf("unrelated wrapper title %q was accepted", title)
		}
	}
}

func TestNormalizeOutlineWrappersUsesContentsTopicRoleForSourceTitleAlias(t *testing.T) {
	home := "https://docs.example.test/guide/Content/home.htm"
	contents := "https://docs.example.test/guide/Content/contents.htm"
	plan := model.DocumentPlan{
		Document: model.Document{
			Title:    "Multiprotocol Label Switching (MPLS) Guide",
			Platform: "6400", Version: "10.14", URL: home, Kind: "flare",
		},
		Topics: []model.Topic{{URL: home}, {URL: contents}},
		TOC: []model.TocEntry{
			{Title: "Multiprotocol Label Switching (MPLS) Guide", URL: home},
			{Title: "Table of Contents", URL: contents},
			{
				Title: "Home", URL: home,
				Children: []model.TocEntry{{
					Title: "AOS-CX 10.12 MPLS guide", URL: contents,
					Children: []model.TocEntry{{Title: "MPLS overview"}},
				}},
			},
		},
	}
	entries, err := normalizeOutlineWrappers(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Title != "MPLS overview" {
		t.Fatalf("source-title alias wrapper was not promoted: %+v", entries)
	}
}
