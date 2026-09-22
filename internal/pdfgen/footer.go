package pdfgen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"aos-cx-docs-dldr/internal/publication"
)

const (
	FooterPlanSchemaVersion  = 2
	HeadingStyleVersion      = 1
	FooterStyleVersion       = 1
	footerTopTolerancePoints = 36.0
	letterPageHeightPoints   = 792.0
)

type footerEntry struct {
	PhysicalPage  int    `json:"physical_page"`
	Label         string `json:"label,omitempty"`
	DecimalNumber int    `json:"decimal_number"`
	Kind          string `json:"kind"`
}

type footerPlan struct {
	SchemaVersion int           `json:"schema_version"`
	Entries       []footerEntry `json:"entries"`
}

type footerDestination struct {
	page  int
	top   float64
	label string
	order int
}

type footerOverlayMetrics struct {
	PlanSHA256     string
	InputSHA256    string
	Entries        int
	Render         renderMetrics
	RenderDuration time.Duration
	MergeDuration  time.Duration
	MergePeakRSS   int64
}

func buildFooterPlan(document qpdfDocument, source publication.FooterSourcePlan) (footerPlan, string, error) {
	var plan footerPlan
	if source.SchemaVersion != publication.FooterSourceSchemaVersion ||
		source.ContentsTarget == "" || source.ProvenanceTarget == "" ||
		source.GuideTitle == "" || len(source.Topics) == 0 {
		return plan, "", errors.New("generated PDF footer source plan is invalid")
	}
	if len(document.Pages) < 2 {
		return plan, "", errors.New("generated PDF footer plan requires at least two physical pages")
	}
	contents, err := qpdfNamedDestinationPosition(document, source.ContentsTarget)
	if err != nil {
		return plan, "", fmt.Errorf("resolve Contents footer destination: %w", err)
	}
	if contents.page != 2 {
		return plan, "", fmt.Errorf("Contents starts on physical page %d, expected 2", contents.page)
	}
	provenance, err := qpdfNamedDestinationPosition(document, source.ProvenanceTarget)
	if err != nil {
		return plan, "", fmt.Errorf("resolve provenance footer destination: %w", err)
	}
	events := map[int][]footerDestination{}
	seen := map[string]bool{}
	firstContentPage := len(document.Pages) + 1
	for index, topic := range source.Topics {
		if topic.Target == "" || topic.Label == "" || seen[topic.Target] {
			return plan, "", errors.New("generated PDF footer source topic is empty or duplicated")
		}
		seen[topic.Target] = true
		position, err := qpdfNamedDestinationPosition(document, topic.Target)
		if err != nil {
			return plan, "", fmt.Errorf("resolve footer topic destination %q: %w", topic.Target, err)
		}
		position.label, position.order = topic.Label, index
		events[position.page] = append(events[position.page], position)
		firstContentPage = min(firstContentPage, position.page)
	}
	if firstContentPage <= contents.page {
		return plan, "", errors.New("generated PDF content overlaps the Contents page range")
	}
	for page := range events {
		sort.SliceStable(events[page], func(left, right int) bool {
			if events[page][left].top == events[page][right].top {
				return events[page][left].order < events[page][right].order
			}
			return events[page][left].top > events[page][right].top
		})
	}
	active := source.GuideTitle
	const contentTop = 792.0 - (0.72 * 72.0)
	for physicalPage := 1; physicalPage <= len(document.Pages); physicalPage++ {
		entry := footerEntry{PhysicalPage: physicalPage, DecimalNumber: physicalPage}
		switch {
		case physicalPage == 1:
			entry.Kind = "cover"
		case physicalPage < firstContentPage:
			entry.Kind, entry.Label = "contents", "Contents"
		default:
			entry.Kind, entry.Label = "content", active
			pageEvents := events[physicalPage]
			if len(pageEvents) > 0 {
				if pageEvents[0].top >= contentTop-footerTopTolerancePoints || entry.Label == "" {
					entry.Label = pageEvents[0].label
				}
				active = pageEvents[len(pageEvents)-1].label
			}
			if physicalPage == provenance.page && provenance.top >= contentTop-footerTopTolerancePoints {
				entry.Kind, entry.Label = "provenance", "Source and provenance"
				active = entry.Label
			} else if physicalPage > provenance.page {
				entry.Kind, entry.Label = "provenance", active
			}
		}
		if physicalPage > 1 && entry.Label == "" {
			return plan, "", fmt.Errorf("generated PDF footer page %d has no category label", physicalPage)
		}
		plan.Entries = append(plan.Entries, entry)
	}
	plan.SchemaVersion = FooterPlanSchemaVersion
	body, err := json.Marshal(plan)
	if err != nil {
		return footerPlan{}, "", err
	}
	sum := sha256.Sum256(body)
	return plan, hex.EncodeToString(sum[:]), nil
}

func qpdfNamedDestinationPosition(document qpdfDocument, target string) (footerDestination, error) {
	position, raw, err := qpdfNamedDestinationRaw(document, target)
	if err != nil {
		return footerDestination{}, err
	}
	// Chrome can associate an anchor just past a page boundary with the
	// preceding page and a small negative Y coordinate. Normalize that exact
	// one-page overflow for footer planning without rewriting the PDF's named
	// destination.
	if position.top < 0 && position.top >= -letterPageHeightPoints &&
		position.page < len(document.Pages) {
		position.page++
		position.top += letterPageHeightPoints
	}
	if position.top < 0 || position.top > letterPageHeightPoints+0.5 {
		return footerDestination{}, fmt.Errorf(
			"named destination page or top coordinate is invalid: page=%d top=%v value=%v",
			position.page, raw[3], raw,
		)
	}
	return position, nil
}

func qpdfNamedDestinationRaw(document qpdfDocument, target string) (footerDestination, []any, error) {
	_, catalog, err := qpdfCatalog(document)
	if err != nil {
		return footerDestination{}, nil, err
	}
	destinations, err := qpdfNamedDestinations(document, catalog)
	if err != nil {
		return footerDestination{}, nil, err
	}
	value, ok := destinations["/"+target]
	if !ok {
		return footerDestination{}, nil, fmt.Errorf("named destination %q is absent", target)
	}
	if reference, ok := value.(string); ok {
		value, err = qpdfValue(document, reference)
		if err != nil {
			return footerDestination{}, nil, err
		}
	}
	array, ok := value.([]any)
	if !ok || len(array) < 4 || array[1] != "/XYZ" {
		return footerDestination{}, nil, errors.New("named destination is not an explicit /XYZ coordinate")
	}
	pageReference, ok := array[0].(string)
	if !ok {
		return footerDestination{}, nil, errors.New("named destination has no page reference")
	}
	page := 0
	for index, candidate := range document.Pages {
		if candidate.Object == pageReference {
			page = index + 1
			break
		}
	}
	top, ok := numberValue(array[3])
	if page == 0 || !ok {
		return footerDestination{}, nil, fmt.Errorf(
			"named destination page or top coordinate is invalid: page=%d top=%v value=%v",
			page, array[3], array,
		)
	}
	return footerDestination{page: page, top: top}, array, nil
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		result, err := typed.Float64()
		return result, err == nil
	case string:
		result, err := strconv.ParseFloat(typed, 64)
		return result, err == nil
	default:
		return 0, false
	}
}

func footerOverlayHTML(plan footerPlan) ([]byte, string, error) {
	if plan.SchemaVersion != FooterPlanSchemaVersion || len(plan.Entries) < 2 {
		return nil, "", errors.New("generated PDF footer plan is invalid")
	}
	var out bytes.Buffer
	out.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'none'; connect-src 'none'"><style>`)
	out.WriteString(`@page{size:Letter;margin:0}html,body{margin:0;padding:0;background:transparent;font-family:"Open Sans",Arial,sans-serif}.footer-page{box-sizing:border-box;position:relative;width:8.5in;height:11in;break-after:page}.footer-page:last-child{break-after:auto}.footer-row{position:absolute;left:.68in;right:.68in;bottom:.24in;height:.2in;display:flex;align-items:flex-end;justify-content:space-between;gap:.25in;color:#4b5563;font-size:9pt;line-height:1}.footer-label{min-width:0;white-space:nowrap}.footer-number{font-weight:600;color:#202124;white-space:nowrap}</style></head><body>`)
	for index, entry := range plan.Entries {
		if entry.PhysicalPage != index+1 || entry.DecimalNumber != index+1 {
			return nil, "", errors.New("generated PDF footer plan physical numbering is inconsistent")
		}
		fmt.Fprintf(&out, `<section class="footer-page" data-physical-page="%d" data-kind="%s">`,
			entry.PhysicalPage, html.EscapeString(entry.Kind))
		if entry.PhysicalPage > 1 {
			if entry.Label == "" {
				return nil, "", errors.New("generated PDF footer label is empty")
			}
			fmt.Fprintf(&out, `<footer class="footer-row"><span class="footer-label">%s</span><strong class="footer-number">%d</strong></footer>`,
				html.EscapeString(entry.Label), entry.DecimalNumber)
		}
		out.WriteString(`</section>`)
	}
	out.WriteString(`</body></html>`)
	body := out.Bytes()
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

func overlayFooterPDF(
	ctx context.Context,
	sidecar QPDFSidecar,
	basePDF, overlayPDF, outputPDF string,
	maxPDFBytes, maxRSSBytes int64,
	tempRoot string,
) (peakRSS int64, duration time.Duration, err error) {
	started := time.Now()
	confirmed, err := reverifyQPDFSidecar(sidecar.Path)
	if err != nil {
		return 0, 0, fmt.Errorf("revalidate qpdf sidecar immediately before footer overlay: %w", err)
	}
	if confirmed != sidecar {
		return 0, 0, errors.New("qpdf sidecar identity changed before footer overlay")
	}
	inspectionRoot, err := os.MkdirTemp(tempRoot, "aos-cx-docs-dldr-qpdf-footer-")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(inspectionRoot)
	base, peak, err := inspectWithQPDF(ctx, sidecar, basePDF, inspectionRoot, "base", maxRSSBytes)
	peakRSS = max(peakRSS, peak)
	if err != nil {
		return peakRSS, 0, err
	}
	overlay, peak, err := inspectWithQPDF(ctx, sidecar, overlayPDF, inspectionRoot, "overlay", maxRSSBytes)
	peakRSS = max(peakRSS, peak)
	if err != nil {
		return peakRSS, 0, err
	}
	if len(base.Pages) != len(overlay.Pages) {
		return peakRSS, 0, fmt.Errorf("footer overlay has %d pages, expected %d", len(overlay.Pages), len(base.Pages))
	}
	if _, err := os.Lstat(outputPDF); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return peakRSS, 0, fmt.Errorf("qpdf footer output already exists: %s", outputPDF)
		}
		return peakRSS, 0, err
	}
	output, err := os.OpenFile(outputPDF, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return peakRSS, 0, err
	}
	writer := &limitedFileWriter{file: output, limit: maxPDFBytes}
	peak, runErr := runQPDF(ctx, sidecar, writer, maxRSSBytes,
		basePDF, "--overlay", overlayPDF, "--", "-")
	closeErr := errors.Join(output.Sync(), output.Close())
	peakRSS = max(peakRSS, peak)
	if err := errors.Join(runErr, closeErr); err != nil {
		return peakRSS, 0, fmt.Errorf("apply generated PDF footer overlay with qpdf: %w", err)
	}
	result, peak, err := inspectWithQPDF(ctx, sidecar, outputPDF, inspectionRoot, "merged", maxRSSBytes)
	peakRSS = max(peakRSS, peak)
	if err != nil {
		return peakRSS, 0, err
	}
	if len(result.Pages) != len(base.Pages) {
		return peakRSS, 0, errors.New("footer overlay changed the physical page count")
	}
	if err := compareNamedDestinationPositions(base, result); err != nil {
		return peakRSS, 0, fmt.Errorf("footer overlay changed named destinations: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return peakRSS, 0, err
	}
	return peakRSS, time.Since(started), nil
}

func compareNamedDestinationPositions(before, after qpdfDocument) error {
	_, beforeCatalog, err := qpdfCatalog(before)
	if err != nil {
		return err
	}
	beforeDestinations, err := qpdfNamedDestinations(before, beforeCatalog)
	if err != nil {
		return err
	}
	_, afterCatalog, err := qpdfCatalog(after)
	if err != nil {
		return err
	}
	afterDestinations, err := qpdfNamedDestinations(after, afterCatalog)
	if err != nil {
		return err
	}
	if len(beforeDestinations) != len(afterDestinations) {
		return errors.New("named destination count changed")
	}
	for raw := range beforeDestinations {
		name := strings.TrimPrefix(raw, "/")
		left, _, err := qpdfNamedDestinationRaw(before, name)
		if err != nil {
			return err
		}
		right, _, err := qpdfNamedDestinationRaw(after, name)
		if err != nil {
			return err
		}
		if left.page != right.page || left.top != right.top {
			return fmt.Errorf("%q moved from page %d top %.3f to page %d top %.3f",
				name, left.page, left.top, right.page, right.top)
		}
	}
	return nil
}

func writeFooterOverlayHTML(path string, plan footerPlan) (string, error) {
	body, hash, err := footerOverlayHTML(plan)
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(body)
	closeErr := errors.Join(file.Sync(), file.Close())
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", err
	}
	return hash, nil
}
