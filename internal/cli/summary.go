package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/model"
)

// maxSummaryTitles bounds how many missing topics a guide's summary line names.
const maxSummaryTitles = 3

// librarySummary describes a published library after a download: whether the
// selected guides are all there, how many have a PDF, and, guide by guide,
// what is missing and why. selected are the guides this run downloaded, in
// order; the manifest's skipped guides were selected too.
func librarySummary(manifest library.Manifest, selected []model.Document, convert bool) []string {
	published := map[string]library.Guide{}
	for _, guide := range manifest.Guides {
		published[guide.ID] = guide
	}
	attempts := map[string]library.Attempt{}
	for _, attempt := range manifest.Attempts {
		attempts[attempt.ID] = attempt
	}
	total := len(selected) + len(manifest.SkippedUnavailable)
	present, complete, degraded, publisherPDFs, generatedPDFs := 0, 0, 0, 0, 0
	var missing []string
	for _, document := range selected {
		guide, ok := published[document.ID]
		attempt, attempted := attempts[document.ID]
		title := document.Title
		if ok {
			present++
			switch guide.Status {
			case "complete":
				complete++
			case "degraded":
				degraded++
			}
			switch {
			case guide.Format == "pdf":
				publisherPDFs++
			case generatedPDF(manifest, guide.Path):
				generatedPDFs++
			case convert:
				missing = append(missing, fmt.Sprintf("%s: no PDF; %s", title, pdfFailure(manifest, guide.Path, attempt)))
			}
		}
		switch {
		case !attempted:
		case attempt.Status == "failed" && attempt.RetainedPrevious:
			missing = append(missing, fmt.Sprintf("%s: this download failed, so the previous copy is kept: %s",
				title, firstError(attempt)))
		case attempt.Status == "failed" || !ok:
			missing = append(missing, fmt.Sprintf("%s: not downloaded: %s", title, firstError(attempt)))
		case attempt.RetainedPrevious && attempt.Status == "degraded":
			missing = append(missing, fmt.Sprintf("%s: this download lacked content the previous copy has, so the previous copy is kept", title))
		case attempt.RetainedPrevious:
			missing = append(missing, fmt.Sprintf("%s: creating the PDF failed, so the previous copy is kept: %s", title, firstError(attempt)))
		default:
			missing = append(missing, unservedContent(title, attempt)...)
		}
	}
	for _, skipped := range manifest.SkippedUnavailable {
		missing = append(missing, fmt.Sprintf("%s: not available from the publisher (%s at %s)",
			skipped.Title, skipped.Reason, skipped.MappedSourceURL))
	}

	var lines []string
	counts := fmt.Sprintf("%d complete", complete)
	if degraded > 0 {
		counts += fmt.Sprintf(", %d degraded (the publisher does not serve all of their content)", degraded)
	}
	all := fmt.Sprintf("all %d selected guides are", total)
	if total == 1 {
		all = "the selected guide is"
	}
	switch {
	case present == total && complete == total:
		lines = append(lines, fmt.Sprintf("Guides: %s present and complete.", all))
	case present == total:
		lines = append(lines, fmt.Sprintf("Guides: %s present: %s.", all, counts))
	default:
		lines = append(lines, fmt.Sprintf("Guides: %d of %d selected guides %s present (%s); %d missing.",
			present, total, plural(present, "is", "are"), counts, total-present))
	}
	if others := len(manifest.Guides) - present; others > 0 {
		lines = append(lines, fmt.Sprintf("The library also holds %d %s from earlier downloads.",
			others, plural(others, "guide", "guides")))
	}
	if pdfs := publisherPDFs + generatedPDFs; pdfs > 0 || convert {
		lines = append(lines, pdfSummary(present, publisherPDFs, generatedPDFs))
	}
	if len(missing) > 0 {
		lines = append(lines, "Missing:")
		for _, line := range missing {
			lines = append(lines, "  "+line)
		}
	}
	return lines
}

// unservedContent describes what a published guide lacks because the
// publisher does not serve it.
func unservedContent(title string, attempt library.Attempt) []string {
	var lines []string
	if topics := attempt.UnpublishedTopics; len(topics) > 0 {
		var names []string
		for _, topic := range topics[:min(len(topics), maxSummaryTitles)] {
			names = append(names, fmt.Sprintf("%q (HTTP %d)", topic.Title, topic.HTTPStatus))
		}
		if more := len(topics) - len(names); more > 0 {
			names = append(names, fmt.Sprintf("%d more", more))
		}
		noun := "topic"
		if len(topics) > 1 {
			noun = "topics"
		}
		lines = append(lines, fmt.Sprintf("%s: %d %s the publisher does not serve: %s; the contents entry links to the publisher's page",
			title, len(topics), noun, strings.Join(names, ", ")))
	}
	if images := model.ImagePlaceholderCount(attempt.MissingResources); images > 0 {
		statuses := map[int]bool{}
		for _, resource := range attempt.MissingResources {
			statuses[resource.HTTPStatus] = true
		}
		var codes []string
		for _, status := range slices.Sorted(maps.Keys(statuses)) {
			codes = append(codes, fmt.Sprintf("HTTP %d", status))
		}
		noun := "image"
		if images > 1 {
			noun = "images"
		}
		lines = append(lines, fmt.Sprintf("%s: %d %s the publisher does not serve (%s), shown as placeholders",
			title, images, noun, strings.Join(codes, ", ")))
	}
	return lines
}

func generatedPDF(manifest library.Manifest, path string) bool {
	result, ok := manifest.HTMLGuides[path]
	return ok && result.HTML != nil && result.HTML.GeneratedPDF != nil && result.HTML.GeneratedPDF.Status == "complete"
}

// pdfFailure explains why a published HTML guide has no generated PDF.
func pdfFailure(manifest library.Manifest, path string, attempt library.Attempt) string {
	if result, ok := manifest.HTMLGuides[path]; ok && result.HTML != nil && result.HTML.GeneratedPDF != nil &&
		result.HTML.GeneratedPDF.Error != "" {
		return "creating it failed: " + shorten(result.HTML.GeneratedPDF.Error)
	}
	if len(attempt.Errors) > 0 {
		return "creating it failed: " + firstError(attempt)
	}
	return "none was created"
}

func firstError(attempt library.Attempt) string {
	if len(attempt.Errors) == 0 {
		return "no error was recorded"
	}
	return shorten(attempt.Errors[0])
}

func shorten(message string) string {
	const limit = 200
	if runes := []rune(message); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return message
}

// pdfSummary says how many of the present guides have a PDF file and where
// each came from: downloaded from the publisher, or converted from the
// guide's downloaded web pages.
func pdfSummary(present, publisher, converted int) string {
	pdfs := publisher + converted
	var source string
	switch {
	case publisher > 0 && converted > 0:
		source = fmt.Sprintf(": %d downloaded from the publisher, %d converted from %s downloaded web pages",
			publisher, converted, plural(converted, "its", "their"))
	case publisher > 0:
		source = ", downloaded from the publisher"
	case converted > 0:
		source = ", converted from " + plural(pdfs, "its", "their") + " downloaded web pages"
	}
	switch {
	case pdfs == 0:
		return fmt.Sprintf("PDF files: none of the %d %s has one; see Missing.", present, plural(present, "guide", "guides"))
	case present == 1:
		return "PDF file: the guide has one" + source + "."
	case pdfs == present:
		return fmt.Sprintf("PDF files: all %d guides have one%s.", present, source)
	}
	return fmt.Sprintf("PDF files: %d of the %d guides %s one%s; the guides without one are listed under Missing.",
		pdfs, present, plural(pdfs, "has", "have"), source)
}
