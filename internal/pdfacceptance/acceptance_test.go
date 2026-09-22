//go:build darwin

package pdfacceptance

import (
	"reflect"
	"testing"
)

func acceptanceLine(page, block int, text string, box [4]float64, size float64) AcceptanceLine {
	return AcceptanceLine{Page: page, Block: block, Text: text, BBox: box, MaxFontPoints: size}
}

func TestAdjacentBoundaryRequiresDistinctCommandRows(t *testing.T) {
	visible := "* e [2]:[0]:[0]:[00:13:01:00:00:10]:[10.2.3.1] 100.100.103.7 0 10"
	pages := []AcceptancePage{
		{Lines: []AcceptanceLine{acceptanceLine(1, 0, visible, [4]float64{0, 0, 100, 10}, 8)}},
		{Lines: []AcceptanceLine{acceptanceLine(2, 0, visible, [4]float64{0, 0, 100, 10}, 8)}},
	}
	distinct := SourceEvidence{
		TextCounts:   map[string]int{},
		TableHeaders: map[string]struct{}{},
		CommandRows: map[string]struct{}{
			visible + " 0 75501 75503 ?": {},
			visible + " 0 75503 ?":       {},
		},
	}
	if findings := adjacentBoundaryDuplicates(pages, distinct); len(findings) != 0 {
		t.Fatalf("distinct source command rows did not explain clipped prefix: %+v", findings)
	}
	duplicateIdentical := distinct
	duplicateIdentical.CommandRows = map[string]struct{}{visible + " 0 75501 75503 ?": {}}
	findings := adjacentBoundaryDuplicates(pages, duplicateIdentical)
	if len(findings) != 1 || !reflect.DeepEqual(findings[0]["unexplained"], []string{visible}) {
		t.Fatalf("one distinct command row incorrectly explained prefix: %+v", findings)
	}
	prose := SourceEvidence{
		TextCounts:   map[string]int{},
		TableHeaders: map[string]struct{}{},
		CommandRows:  map[string]struct{}{},
	}
	if findings := adjacentBoundaryDuplicates(pages, prose); len(findings) != 1 {
		t.Fatalf("prose/no command evidence incorrectly explained prefix: %+v", findings)
	}
}

func TestAnalyzeAcceptancePreservesGeometryFooterHeadingAndOutlinePolicy(t *testing.T) {
	document := AcceptanceDocument{Pages: []AcceptancePage{
		{
			Width: 612, Height: 792,
			Lines: []AcceptanceLine{
				acceptanceLine(1, 0, "Guide Title", [4]float64{50, 50, 150, 72}, 22),
			},
		},
		{
			Width: 612, Height: 792,
			Lines: []AcceptanceLine{
				acceptanceLine(2, 0, "Heading", [4]float64{49, 80, 120, 96}, 16),
				acceptanceLine(2, 1, "Following paragraph", [4]float64{49, 110, 200, 125}, 10),
				acceptanceLine(2, 2, "Contents", [4]float64{49, 748, 120, 760}, 8),
				acceptanceLine(2, 3, "2", [4]float64{550, 748, 556, 760}, 8),
			},
		},
	}}
	report, err := AnalyzeAcceptance(
		document,
		SourceEvidence{},
		[]OutlineEntry{
			{Depth: 1, PhysicalPage: 1, Title: "Cover"},
			{Depth: 1, PhysicalPage: 2, Title: "Contents"},
			{Depth: 1, PhysicalPage: 2, Title: "Root"},
			{Depth: 2, PhysicalPage: 2, Title: "Child"},
		},
		AcceptanceOptions{
			Title: "Guide Title", MaxCoverFontPoints: 22.1, MaxLargeRepeatPages: 2,
			HeadingAnchors: []string{"Heading"}, RequirePageFooters: true,
			ExpectedOutlineRoots:  []string{"Cover", "Contents", "Root"},
			OutlineDirectChildren: []string{"Root=1"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("valid acceptance fixture failed: %+v", report.Violations)
	}
	if report.Outline.EntryCount != 4 || report.Outline.DirectChildren["Root"] != 1 {
		t.Fatalf("unexpected outline record: %+v", report.Outline)
	}
	if report.PageFooters[1].Number == nil || *report.PageFooters[1].Number != "2" {
		t.Fatalf("physical footer glyph was not retained: %+v", report.PageFooters)
	}

	document.Pages[1].Lines[3].Text = "ii"
	report, err = AnalyzeAcceptance(document, SourceEvidence{}, nil, AcceptanceOptions{
		Title: "Guide Title", MaxCoverFontPoints: 22.1, MaxLargeRepeatPages: 2,
		RequirePageFooters: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || emptyJSONValue(report.Violations["page_footers"]) {
		t.Fatalf("wrong/roman footer was accepted: %+v", report.Violations)
	}
}

func TestMeaningfulOverlapThresholdsAndCap(t *testing.T) {
	lines := []AcceptanceLine{
		acceptanceLine(1, 0, "left text", [4]float64{0, 0, 100, 20}, 10),
		acceptanceLine(1, 1, "right text", [4]float64{10, 2, 90, 19}, 10),
		acceptanceLine(1, 2, "tiny edge", [4]float64{99, 19, 150, 30}, 10),
	}
	findings := meaningfulOverlaps(lines, 100)
	if len(findings) != 1 || findings[0]["left"] != "left text" || findings[0]["right"] != "right text" {
		t.Fatalf("overlap thresholds produced %+v", findings)
	}
	if findings := meaningfulOverlaps(lines, 0); len(findings) != 0 {
		t.Fatalf("zero remaining cap produced findings: %+v", findings)
	}
}

func TestNormalizeAcceptanceTextUsesNFKCAndWhitespaceCollapse(t *testing.T) {
	if got := NormalizeAcceptanceText("  Ａ\t B\n"); got != "A B" {
		t.Fatalf("normalized text=%q", got)
	}
}

func TestAuditOutlineRejectsAmbiguousChildCountAssertion(t *testing.T) {
	_, violations, err := auditOutline(
		[]OutlineEntry{{Depth: 1, Title: "Root"}, {Depth: 1, Title: "Root"}},
		nil, nil, []string{"Root=1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || violations[0]["reason"] != "child-count-match" {
		t.Fatalf("ambiguous outline title was accepted: %+v", violations)
	}
	if _, _, err := auditOutline(nil, nil, nil, []string{"broken"}); err == nil {
		t.Fatal("malformed child-count assertion was accepted")
	}
}

func TestReadingOrderDrivesAdjacentBoundaryDetection(t *testing.T) {
	repeated := "visually repeated boundary line"
	pages := []AcceptancePage{
		{Lines: []AcceptanceLine{
			acceptanceLine(1, 0, repeated, [4]float64{0, 700, 100, 710}, 8),
			acceptanceLine(1, 1, "painted later but visually above", [4]float64{0, 10, 100, 20}, 8),
		}},
		{Lines: []AcceptanceLine{
			acceptanceLine(2, 0, "painted first but visually below", [4]float64{0, 100, 100, 110}, 8),
			acceptanceLine(2, 1, repeated, [4]float64{0, 10, 100, 20}, 8),
		}},
	}
	for index := range pages {
		pages[index].Lines = readingOrderLines(pages[index].Lines)
	}
	findings := adjacentBoundaryDuplicates(pages, SourceEvidence{
		TextCounts: map[string]int{}, TableHeaders: map[string]struct{}{}, CommandRows: map[string]struct{}{},
	})
	if len(findings) != 1 || !reflect.DeepEqual(findings[0]["sequence"], []string{repeated}) {
		t.Fatalf("visual page boundary was not detected: %+v", findings)
	}
}

func TestTableHeaderWordBoundaryPreservesAcceptedSemantics(t *testing.T) {
	if !wordContains("prefix Header suffix", "Header") {
		t.Fatal("word-delimited table header was not recognized")
	}
	if wordContains("* command suffix", "* command") {
		t.Fatal("punctuation-led value was incorrectly treated as a word-boundary match")
	}
	if wordContains("prefix command *", "command *") {
		t.Fatal("punctuation-ended value was incorrectly treated as a word-boundary match")
	}
}

func TestTableHeaderFragmentLengthCountsUnicodeCodePoints(t *testing.T) {
	short := "界界界界界界界"
	long := short + "界"
	headers := map[string]struct{}{
		"prefix " + short + " suffix": {},
		"prefix " + long + " suffix":  {},
	}
	if isTableHeaderLine(short, headers) {
		t.Fatal("seven-rune multibyte fragment received table-header exemption")
	}
	if !isTableHeaderLine(long, headers) {
		t.Fatal("eight-rune multibyte fragment did not receive table-header exemption")
	}
}
