package pdfgen

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/publication"
)

func footerTestDocument(t *testing.T) qpdfDocument {
	t.Helper()
	objects := map[string]json.RawMessage{}
	add := func(key string, value map[string]any) {
		body, err := json.Marshal(qpdfObject{Value: value})
		if err != nil {
			t.Fatal(err)
		}
		objects[key] = body
	}
	destinations := map[string]any{
		"/contents":   []any{"4 0 R", "/XYZ", float64(0), float64(740), float64(0)},
		"/topic-a":    []any{"6 0 R", "/XYZ", float64(0), float64(740), float64(0)},
		"/topic-b":    []any{"7 0 R", "/XYZ", float64(0), float64(400), float64(0)},
		"/topic-c":    []any{"7 0 R", "/XYZ", float64(0), float64(200), float64(0)},
		"/topic-d":    []any{"8 0 R", "/XYZ", float64(0), float64(740), float64(0)},
		"/provenance": []any{"10 0 R", "/XYZ", float64(0), float64(740), float64(0)},
	}
	add("trailer", map[string]any{"/Root": "1 0 R"})
	add("obj:1 0 R", map[string]any{"/Type": "/Catalog", "/Pages": "2 0 R", "/Dests": destinations})
	kids := make([]any, 8)
	pages := make([]qpdfPage, 8)
	for index := range pages {
		reference := fmt.Sprintf("%d 0 R", index+3)
		kids[index] = reference
		pages[index] = qpdfPage{Object: reference, PagePosition: index + 1}
		add("obj:"+reference, map[string]any{"/Type": "/Page", "/Parent": "2 0 R"})
	}
	add("obj:2 0 R", map[string]any{"/Type": "/Pages", "/Count": float64(8), "/Kids": kids})
	return qpdfDocument{
		Header:  qpdfHeader{JSONVersion: 2, MaxObjectID: 10},
		Objects: objects,
		Pages:   pages,
	}
}

func TestFooterPlanUsesContentsContinuationAndPageTopCategoryPolicy(t *testing.T) {
	source := publication.FooterSourcePlan{
		SchemaVersion:  publication.FooterSourceSchemaVersion,
		GuideTitle:     "Guide",
		ContentsTarget: "contents", ProvenanceTarget: "provenance",
		Topics: []publication.FooterSourceTopic{
			{Target: "topic-a", Label: "Category A"},
			{Target: "topic-b", Label: "Category B"},
			{Target: "topic-c", Label: "Category C"},
			{Target: "topic-d", Label: "Category D"},
		},
	}
	plan, hash, err := buildFooterPlan(footerTestDocument(t), source)
	if err != nil {
		t.Fatal(err)
	}
	want := []footerEntry{
		{PhysicalPage: 1, DecimalNumber: 1, Kind: "cover"},
		{PhysicalPage: 2, Label: "Contents", DecimalNumber: 2, Kind: "contents"},
		{PhysicalPage: 3, Label: "Contents", DecimalNumber: 3, Kind: "contents"},
		{PhysicalPage: 4, Label: "Category A", DecimalNumber: 4, Kind: "content"},
		{PhysicalPage: 5, Label: "Category A", DecimalNumber: 5, Kind: "content"},
		{PhysicalPage: 6, Label: "Category D", DecimalNumber: 6, Kind: "content"},
		{PhysicalPage: 7, Label: "Category D", DecimalNumber: 7, Kind: "content"},
		{PhysicalPage: 8, Label: "Source and provenance", DecimalNumber: 8, Kind: "provenance"},
	}
	if hash == "" || len(plan.Entries) != len(want) {
		t.Fatalf("footer plan hash/count is invalid: hash=%q plan=%+v", hash, plan)
	}
	for index := range want {
		if plan.Entries[index] != want[index] {
			t.Fatalf("footer page %d = %+v, want %+v", index+1, plan.Entries[index], want[index])
		}
	}
}

func TestFooterOverlayHTMLHasExactArabicPhysicalNumbersAndEscapedLabels(t *testing.T) {
	plan := footerPlan{SchemaVersion: FooterPlanSchemaVersion, Entries: []footerEntry{
		{PhysicalPage: 1, DecimalNumber: 1, Kind: "cover"},
		{PhysicalPage: 2, Label: "Contents", DecimalNumber: 2, Kind: "contents"},
		{PhysicalPage: 3, Label: `Switch & "system"`, DecimalNumber: 3, Kind: "content"},
	}}
	body, hash, err := footerOverlayHTML(plan)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if hash == "" || strings.Contains(text, `>1</strong>`) ||
		!strings.Contains(text, `>2</strong>`) ||
		!strings.Contains(text, `>3</strong>`) ||
		!strings.Contains(text, `Switch &amp; &#34;system&#34;`) ||
		strings.Contains(text, "position:fixed") ||
		strings.Contains(text, "counter(page") {
		t.Fatalf("footer overlay HTML violates explicit physical-number policy:\n%s", text)
	}
}

func TestFooterPlanRejectsNonPhysicalContentsAndMissingDestination(t *testing.T) {
	source := publication.FooterSourcePlan{
		SchemaVersion: publication.FooterSourceSchemaVersion,
		GuideTitle:    "Guide", ContentsTarget: "contents", ProvenanceTarget: "provenance",
		Topics: []publication.FooterSourceTopic{{Target: "topic-a", Label: "Category"}},
	}
	document := footerTestDocument(t)
	mutateQPDFObject(t, document.Objects, "1 0 R", func(value map[string]any) {
		value["/Dests"].(map[string]any)["/contents"] = []any{"3 0 R", "/XYZ", float64(0), float64(740), float64(0)}
	})
	if _, _, err := buildFooterPlan(document, source); err == nil ||
		!strings.Contains(err.Error(), "expected 2") {
		t.Fatalf("wrong Contents page was accepted: %v", err)
	}
	source.ContentsTarget = "missing"
	if _, _, err := buildFooterPlan(footerTestDocument(t), source); err == nil ||
		!strings.Contains(err.Error(), "absent") {
		t.Fatalf("missing Contents destination was accepted: %v", err)
	}
}

func TestFooterDestinationNormalizesOnePageChromeOverflow(t *testing.T) {
	document := footerTestDocument(t)
	mutateQPDFObject(t, document.Objects, "1 0 R", func(value map[string]any) {
		value["/Dests"].(map[string]any)["/topic-a"] =
			[]any{"6 0 R", "/XYZ", float64(0), float64(-27.75), float64(0)}
	})
	position, err := qpdfNamedDestinationPosition(document, "topic-a")
	if err != nil {
		t.Fatal(err)
	}
	if position.page != 5 || position.top != 764.25 {
		t.Fatalf("normalized destination = page %d top %.2f, want page 5 top 764.25", position.page, position.top)
	}

	mutateQPDFObject(t, document.Objects, "1 0 R", func(value map[string]any) {
		value["/Dests"].(map[string]any)["/topic-a"] =
			[]any{"10 0 R", "/XYZ", float64(0), float64(-27.75), float64(0)}
	})
	if _, err := qpdfNamedDestinationPosition(document, "topic-a"); err == nil {
		t.Fatal("last-page negative destination was accepted")
	}
}

func TestFooterDestinationComparisonUsesRawCoordinates(t *testing.T) {
	before := footerTestDocument(t)
	after := footerTestDocument(t)
	mutateQPDFObject(t, before.Objects, "1 0 R", func(value map[string]any) {
		value["/Dests"].(map[string]any)["/topic-a"] =
			[]any{"6 0 R", "/XYZ", float64(0), float64(-27.75), float64(0)}
	})
	mutateQPDFObject(t, after.Objects, "1 0 R", func(value map[string]any) {
		value["/Dests"].(map[string]any)["/topic-a"] =
			[]any{"7 0 R", "/XYZ", float64(0), float64(764.25), float64(0)}
	})
	if err := compareNamedDestinationPositions(before, after); err == nil ||
		!strings.Contains(err.Error(), "moved") {
		t.Fatalf("equivalent normalized but changed raw destination was accepted: %v", err)
	}
}
