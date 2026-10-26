package cli

import (
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/model"
)

// summaryManifest builds a published manifest: guides lists each guide's ID,
// format, status and whether its HTML has a generated PDF.
func summaryManifest(guides ...struct {
	id, format, status string
	pdf                bool
}) library.Manifest {
	m := library.Manifest{PDFGuides: map[string]model.ArchiveResult{}, HTMLGuides: map[string]model.ArchiveResult{}}
	for _, g := range guides {
		m.Guides = append(m.Guides, library.Guide{ID: g.id, Title: strings.ToUpper(g.id) + " Guide", Path: g.id,
			Format: g.format, Status: g.status})
		m.Attempts = append(m.Attempts, library.Attempt{ID: g.id, Title: strings.ToUpper(g.id) + " Guide", Status: g.status, Format: g.format})
		if g.format == "pdf" {
			m.PDFGuides[g.id] = model.ArchiveResult{Status: g.status, Format: "pdf"}
			continue
		}
		archive := &model.HTMLArchive{}
		if g.pdf {
			archive.GeneratedPDF = &model.GeneratedPDFRecord{Status: "complete"}
		}
		m.HTMLGuides[g.id] = model.ArchiveResult{Status: g.status, Format: "html", HTML: archive}
	}
	return m
}

func selectedGuides(ids ...string) []model.Document {
	var documents []model.Document
	for _, id := range ids {
		documents = append(documents, model.Document{ID: id, Title: strings.ToUpper(id) + " Guide"})
	}
	return documents
}

type summaryGuide = struct {
	id, format, status string
	pdf                bool
}

func TestSummarySaysEveryGuideIsPresentAndComplete(t *testing.T) {
	m := summaryManifest(summaryGuide{"cli", "pdf", "complete", false}, summaryGuide{"acls", "html", "complete", true},
		summaryGuide{"vsx", "html", "complete", true})
	got := strings.Join(librarySummary(m, selectedGuides("cli", "acls", "vsx"), true, false), "\n")
	want := "Guides: all 3 selected guides are present and complete.\n" +
		"PDF files: all 3 guides have one: 1 downloaded from the publisher, 2 converted from their downloaded web pages."
	if got != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", got, want)
	}
}

func TestSummaryNamesWhatADegradedGuideLacks(t *testing.T) {
	m := summaryManifest(summaryGuide{"multicast", "html", "degraded", true}, summaryGuide{"nae", "html", "degraded", true},
		summaryGuide{"qos", "html", "degraded", true}, summaryGuide{"vsx", "html", "complete", true})
	m.Attempts[0].UnpublishedTopics = []model.UnpublishedTopic{{Title: "Use Case 3", HTTPStatus: 404}}
	m.Attempts[1].UnpublishedTopics = []model.UnpublishedTopic{
		{Title: "One", HTTPStatus: 404}, {Title: "Two", HTTPStatus: 404}, {Title: "Three", HTTPStatus: 410}, {Title: "Four", HTTPStatus: 404},
	}
	m.Attempts[2].MissingResources = []model.MissingResource{
		{PlaceholderID: "a", HTTPStatus: 404}, {PlaceholderID: "b", HTTPStatus: 404}, {PlaceholderID: "c", HTTPStatus: 410},
	}
	got := strings.Join(librarySummary(m, selectedGuides("multicast", "nae", "qos", "vsx"), true, false), "\n")
	want := "Guides: all 4 selected guides are present: 1 complete, 3 degraded (the publisher does not serve all of their content).\n" +
		"PDF files: all 4 guides have one, converted from their downloaded web pages.\n" +
		"Missing:\n" +
		`  MULTICAST Guide: 1 topic the publisher does not serve: "Use Case 3" (HTTP 404); the contents entry links to the publisher's page` + "\n" +
		`  NAE Guide: 4 topics the publisher does not serve: "One" (HTTP 404), "Two" (HTTP 404), "Three" (HTTP 410), 1 more; the contents entry links to the publisher's page` + "\n" +
		"  QOS Guide: 3 images the publisher does not serve (HTTP 404, HTTP 410), shown as placeholders"
	if got != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", got, want)
	}
}

func TestSummaryNamesGuidesThatAreMissing(t *testing.T) {
	m := summaryManifest(summaryGuide{"acls", "html", "complete", true}, summaryGuide{"cli", "html", "complete", true},
		summaryGuide{"vsx", "html", "complete", false}, summaryGuide{"old", "html", "complete", true})
	// cli failed this time; its previous complete copy stays published.
	m.Attempts[1] = library.Attempt{ID: "cli", Title: "CLI Guide", Status: "failed", Errors: []string{"retrieval failed: HTTP 503"}, RetainedPrevious: true}
	// vsx is published without the PDF that was asked for.
	m.Attempts[2].Status = "incomplete"
	m.HTMLGuides["vsx"].HTML.GeneratedPDF = &model.GeneratedPDFRecord{Status: "failed", Error: "Chrome did not start"}
	// copp was not published at all.
	m.Attempts = append(m.Attempts, library.Attempt{ID: "copp", Title: "COPP Guide", Status: "failed", Errors: []string{"plan failed"}})
	m.SkippedUnavailable = []model.SkippedUnavailableGuide{{ID: "webUI", Title: "Web UI Guide", Reason: "mapped source HTTP 404",
		MappedSourceURL: "https://publisher.example/webui/index.html"}}
	got := strings.Join(librarySummary(m, selectedGuides("acls", "cli", "vsx", "copp"), true, false), "\n")
	want := "Guides: 3 of 5 selected guides are present (3 complete); 2 missing.\n" +
		"The library also holds 1 guide from earlier downloads.\n" +
		"PDF files: 2 of the 3 guides have one, converted from their downloaded web pages; the guides without one are listed under Missing.\n" +
		"Missing:\n" +
		"  CLI Guide: this download failed, so the previous copy is kept: retrieval failed: HTTP 503\n" +
		"  VSX Guide: no PDF; creating it failed: Chrome did not start\n" +
		"  COPP Guide: not downloaded: plan failed\n" +
		"  Web UI Guide: not available from the publisher (mapped source HTTP 404 at https://publisher.example/webui/index.html)"
	if got != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", got, want)
	}
}

func TestSummaryLeavesPDFsOutWhenNoneWereAskedFor(t *testing.T) {
	m := summaryManifest(summaryGuide{"acls", "html", "complete", false})
	got := strings.Join(librarySummary(m, selectedGuides("acls"), false, false), "\n")
	if got != "Guides: the selected guide is present and complete." {
		t.Fatalf("summary: %q", got)
	}
}

func TestPDFSummarySaysWhereEachPDFCameFrom(t *testing.T) {
	for _, tc := range []struct {
		present, publisher, converted int
		want                          string
	}{
		{27, 0, 27, "PDF files: all 27 guides have one, converted from their downloaded web pages."},
		{3, 3, 0, "PDF files: all 3 guides have one, downloaded from the publisher."},
		{26, 1, 25, "PDF files: all 26 guides have one: 1 downloaded from the publisher, 25 converted from their downloaded web pages."},
		{1, 0, 1, "PDF file: the guide has one, converted from its downloaded web pages."},
		{1, 1, 0, "PDF file: the guide has one, downloaded from the publisher."},
		{4, 0, 0, "PDF files: none of the 4 guides has one; see Missing."},
		{5, 1, 3, "PDF files: 4 of the 5 guides have one: 1 downloaded from the publisher, 3 converted from their downloaded web pages; the guides without one are listed under Missing."},
	} {
		if got := pdfSummary(tc.present, tc.publisher, tc.converted); got != tc.want {
			t.Errorf("pdfSummary(%d, %d, %d) = %q\nwant %q", tc.present, tc.publisher, tc.converted, got, tc.want)
		}
	}
}

func TestSummarySaysWhichGuidesAreOnlyAPDFInTheReleaseFolder(t *testing.T) {
	m := summaryManifest(summaryGuide{"cli", "pdf", "complete", false}, summaryGuide{"acls", "html", "complete", true},
		summaryGuide{"vsx", "html", "complete", false})
	m.Guides[0].AtReleaseRoot, m.Guides[1].AtReleaseRoot = true, true
	m.HTMLGuides["vsx"].HTML.GeneratedPDF = &model.GeneratedPDFRecord{Status: "failed", Error: "renderer stopped"}
	m.Attempts[2].Status = "incomplete"
	got := strings.Join(librarySummary(m, selectedGuides("cli", "acls", "vsx"), true, true), "\n")
	want := "Guides: all 3 selected guides are present and complete.\n" +
		"PDF files: 2 of the 3 guides have one: 1 downloaded from the publisher, 1 converted from its downloaded web pages; " +
		"the guides without one are listed under Missing.\n" +
		"Guide folders: removed for 2 of the 3 guides, whose PDF files are in the release folder; the others keep their folders.\n" +
		"Missing:\n" +
		"  VSX Guide: no PDF, so its web pages are kept; creating it failed: renderer stopped"
	if got != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", got, want)
	}
	m = summaryManifest(summaryGuide{"acls", "html", "complete", true}, summaryGuide{"vsx", "html", "complete", true})
	m.Guides[0].AtReleaseRoot, m.Guides[1].AtReleaseRoot = true, true
	got = strings.Join(librarySummary(m, selectedGuides("acls", "vsx"), true, true), "\n")
	if !strings.HasSuffix(got, "\nGuide folders: removed; all 2 PDF files are in the release folder.") {
		t.Fatalf("summary:\n%s", got)
	}
}
