//go:build darwin

package pdfacceptance

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	AcceptanceMaxOverlapFindings = 100
)

type AcceptanceLine struct {
	BBox          [4]float64 `json:"bbox"`
	Block         int        `json:"block"`
	MaxFontPoints float64    `json:"max_font_points"`
	Page          int        `json:"page"`
	Text          string     `json:"text"`
}

type AcceptancePage struct {
	Height float64
	Lines  []AcceptanceLine
	Width  float64
}

type AcceptanceDocument struct {
	Pages []AcceptancePage
}

type SourceEvidence struct {
	CommandRows  map[string]struct{}
	TableHeaders map[string]struct{}
	TextCounts   map[string]int
}

type OutlineEntry struct {
	Depth        int
	PhysicalPage int
	Title        string
}

type OutlineRecord struct {
	DirectChildren map[string]int `json:"direct_children"`
	EntryCount     int            `json:"entry_count"`
	MaxDepth       int            `json:"max_depth"`
	RootTitles     []string       `json:"root_titles"`
}

type FooterRecord struct {
	Label        *string     `json:"label"`
	LabelBBox    *[4]float64 `json:"label_bbox"`
	Number       *string     `json:"number"`
	NumberBBox   *[4]float64 `json:"number_bbox"`
	PhysicalPage int         `json:"physical_page"`
}

type SampleRecord struct {
	AnchorPages   map[string]*int `json:"anchor_pages"`
	Montage       string          `json:"montage"`
	PhysicalPages []int           `json:"physical_pages"`
}

type AcceptanceToolRecord struct {
	LicensePath            string     `json:"license_path"`
	LicenseSHA256          string     `json:"license_sha256"`
	MuPDFDependencyClosure []ToolFile `json:"mupdf_dependency_closure"`
	MuPDFExecutableSHA256  string     `json:"mupdf_executable_sha256"`
	MuPDFIdentityManifest  string     `json:"mupdf_identity_manifest"`
	MuPDFIdentitySHA256    string     `json:"mupdf_identity_manifest_sha256"`
	MuPDFPath              string     `json:"mupdf_path"`
	MuPDFVersion           string     `json:"mupdf_version"`
	QPDFBundleSHA256       string     `json:"qpdf_bundle_sha256"`
	QPDFExecutableSHA256   string     `json:"qpdf_executable_sha256"`
	QPDFPath               string     `json:"qpdf_path"`
	QPDFVersion            string     `json:"qpdf_version"`
}

type ToolFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type AcceptanceReport struct {
	CanonicalTitleMatches []AcceptanceLine     `json:"canonical_title_matches"`
	Headings              []map[string]any     `json:"headings"`
	MaxCoverFontPoints    float64              `json:"max_cover_font_points"`
	Outline               OutlineRecord        `json:"outline"`
	PageFooters           []FooterRecord       `json:"page_footers"`
	Passed                bool                 `json:"passed"`
	PDF                   string               `json:"pdf"`
	PhysicalPages         int                  `json:"physical_pages"`
	Samples               *SampleRecord        `json:"samples"`
	SourceCommandValues   int                  `json:"source_command_values"`
	SourceGuideRoot       *string              `json:"source_guide_root"`
	SourceTextValues      int                  `json:"source_text_values"`
	SourceTheadValues     int                  `json:"source_thead_values"`
	Title                 string               `json:"title"`
	Tool                  AcceptanceToolRecord `json:"tool"`
	Violations            map[string]any       `json:"violations"`
}

type AcceptanceOptions struct {
	ExpectedOutlineRoots   []string
	ForbiddenOutlineTitles []string
	ForbiddenRunningText   []string
	HeadingAnchors         []string
	MaxCoverFontPoints     float64
	MaxLargeRepeatPages    int
	OutlineDirectChildren  []string
	RequirePageFooters     bool
	Title                  string
}

func NormalizeAcceptanceText(value string) string {
	return strings.Join(strings.Fields(norm.NFKC.String(value)), " ")
}

func AnalyzeAcceptance(
	document AcceptanceDocument,
	source SourceEvidence,
	outline []OutlineEntry,
	options AcceptanceOptions,
) (AcceptanceReport, error) {
	if len(document.Pages) == 0 {
		return AcceptanceReport{}, fmt.Errorf("generated PDF contains no physical pages")
	}
	if options.Title == "" {
		return AcceptanceReport{}, fmt.Errorf("acceptance title must not be empty")
	}
	if math.IsNaN(options.MaxCoverFontPoints) || math.IsInf(options.MaxCoverFontPoints, 0) {
		return AcceptanceReport{}, fmt.Errorf("maximum cover font size must be finite")
	}
	if options.MaxCoverFontPoints <= 0 {
		options.MaxCoverFontPoints = 22.1
	}
	if options.MaxLargeRepeatPages < 0 {
		return AcceptanceReport{}, fmt.Errorf("maximum large-repeat page count must not be negative")
	}
	if source.TextCounts == nil {
		source.TextCounts = map[string]int{}
	}
	if source.TableHeaders == nil {
		source.TableHeaders = map[string]struct{}{}
	}
	if source.CommandRows == nil {
		source.CommandRows = map[string]struct{}{}
	}
	for index := range document.Pages {
		document.Pages[index].Lines = readingOrderLines(document.Pages[index].Lines)
	}

	var flattened []AcceptanceLine
	pageText := make([]string, len(document.Pages))
	mediaFailures := []int{}
	for index, page := range document.Pages {
		flattened = append(flattened, page.Lines...)
		var values []string
		for _, line := range page.Lines {
			values = append(values, line.Text)
		}
		pageText[index] = NormalizeAcceptanceText(strings.Join(values, " "))
		if math.Abs(page.Width-612) > 0.5 || math.Abs(page.Height-792) > 0.5 {
			mediaFailures = append(mediaFailures, index+1)
		}
	}
	title := NormalizeAcceptanceText(options.Title)
	titleLines := make([]AcceptanceLine, 0)
	for _, line := range flattened {
		if line.Text == title {
			titleLines = append(titleLines, line)
		}
	}
	forbidden := map[string]any{}
	for _, requested := range options.ForbiddenRunningText {
		value := NormalizeAcceptanceText(requested)
		var pages []int
		for index, text := range pageText {
			if strings.Contains(text, value) {
				pages = append(pages, index+1)
			}
		}
		if len(pages) > 0 {
			forbidden[value] = pages
		}
	}

	largePages := map[string]map[int]struct{}{}
	largeOrder := []string{}
	for _, line := range flattened {
		if utf8.RuneCountInString(line.Text) < 20 || line.MaxFontPoints < 14 {
			continue
		}
		if largePages[line.Text] == nil {
			largePages[line.Text] = map[int]struct{}{}
			largeOrder = append(largeOrder, line.Text)
		}
		largePages[line.Text][line.Page] = struct{}{}
	}
	repeatedLarge := []map[string]any{}
	for _, text := range largeOrder {
		pages := sortedSet(largePages[text])
		_, tableHeader := source.TableHeaders[text]
		if len(pages) > options.MaxLargeRepeatPages &&
			source.TextCounts[text] < len(pages) && !tableHeader {
			repeatedLarge = append(repeatedLarge, map[string]any{
				"physical_pages": pages,
				"text":           text,
			})
		}
	}

	overlaps := []map[string]any{}
	for _, page := range document.Pages {
		overlaps = append(overlaps, meaningfulOverlaps(page.Lines, AcceptanceMaxOverlapFindings-len(overlaps))...)
		if len(overlaps) >= AcceptanceMaxOverlapFindings {
			break
		}
	}
	boundaries := adjacentBoundaryDuplicates(document.Pages, source)
	footerRecords, footerViolations := auditFooters(document.Pages)
	headingRecords, headingViolations := auditHeadings(document.Pages, options.HeadingAnchors)
	outlineRecord, outlineViolations, err := auditOutline(
		outline,
		options.ExpectedOutlineRoots,
		options.ForbiddenOutlineTitles,
		options.OutlineDirectChildren,
	)
	if err != nil {
		return AcceptanceReport{}, err
	}

	coverCandidates := []AcceptanceLine{}
	for _, line := range titleLines {
		if line.Page == 1 && line.MaxFontPoints > 0 {
			coverCandidates = append(coverCandidates, line)
		}
	}
	first := document.Pages[0]
	titleGeometryValid := len(coverCandidates) == 1 &&
		coverCandidates[0].MaxFontPoints <= options.MaxCoverFontPoints &&
		coverCandidates[0].BBox[0] >= 0 &&
		coverCandidates[0].BBox[1] >= 0 &&
		coverCandidates[0].BBox[2] <= first.Width &&
		coverCandidates[0].BBox[3] <= first.Height
	coverViolation := []map[string]any{}
	if !titleGeometryValid {
		coverViolation = append(coverViolation, map[string]any{
			"cover_candidates": coverCandidates,
			"expected":         options.Title,
		})
	}
	violations := map[string]any{
		"adjacent_boundary_duplication": boundaries,
		"canonical_cover_title":         coverViolation,
		"forbidden_running_text":        forbidden,
		"heading_pagination":            headingViolations,
		"letter_media":                  mediaFailures,
		"meaningful_text_overlap":       overlaps,
		"outline":                       outlineViolations,
		"repeated_large_text":           repeatedLarge,
	}
	if options.RequirePageFooters {
		nonempty := map[string]any{}
		for key, value := range footerViolations {
			if !emptyJSONValue(value) {
				nonempty[key] = value
			}
		}
		violations["page_footers"] = nonempty
	}
	passed := true
	for _, value := range violations {
		if !emptyJSONValue(value) {
			passed = false
			break
		}
	}
	return AcceptanceReport{
		CanonicalTitleMatches: titleLines,
		Headings:              headingRecords,
		MaxCoverFontPoints:    options.MaxCoverFontPoints,
		Outline:               outlineRecord,
		PageFooters:           footerRecords,
		Passed:                passed,
		PhysicalPages:         len(document.Pages),
		SourceCommandValues:   len(source.CommandRows),
		SourceTextValues:      len(source.TextCounts),
		SourceTheadValues:     len(source.TableHeaders),
		Title:                 options.Title,
		Violations:            violations,
	}, nil
}

func meaningfulOverlaps(lines []AcceptanceLine, remaining int) []map[string]any {
	if remaining <= 0 {
		return nil
	}
	result := []map[string]any{}
	for index, left := range lines {
		leftWidth := left.BBox[2] - left.BBox[0]
		leftHeight := left.BBox[3] - left.BBox[1]
		if leftWidth <= 0 || leftHeight <= 0 {
			continue
		}
		for _, right := range lines[index+1:] {
			if left.Text == right.Text {
				continue
			}
			rightWidth := right.BBox[2] - right.BBox[0]
			rightHeight := right.BBox[3] - right.BBox[1]
			if rightWidth <= 0 || rightHeight <= 0 {
				continue
			}
			width, height, area := lineIntersection(left, right)
			smaller := math.Min(leftWidth*leftHeight, rightWidth*rightHeight)
			if height/math.Min(leftHeight, rightHeight) < 0.55 ||
				width/math.Min(leftWidth, rightWidth) < 0.25 ||
				area/smaller < 0.18 {
				continue
			}
			result = append(result, map[string]any{
				"intersection_ratio": math.Round((area/smaller)*10_000) / 10_000,
				"left":               left.Text,
				"left_bbox":          left.BBox,
				"physical_page":      left.Page,
				"right":              right.Text,
				"right_bbox":         right.BBox,
			})
			if len(result) >= remaining {
				return result
			}
		}
	}
	return result
}

func lineIntersection(left, right AcceptanceLine) (float64, float64, float64) {
	x0 := math.Max(left.BBox[0], right.BBox[0])
	y0 := math.Max(left.BBox[1], right.BBox[1])
	x1 := math.Min(left.BBox[2], right.BBox[2])
	y1 := math.Min(left.BBox[3], right.BBox[3])
	width := math.Max(0, x1-x0)
	height := math.Max(0, y1-y0)
	return width, height, width * height
}

func adjacentBoundaryDuplicates(pages []AcceptancePage, source SourceEvidence) []map[string]any {
	frequency := map[string]int{}
	for _, page := range pages {
		seen := map[string]struct{}{}
		for _, line := range page.Lines {
			seen[line.Text] = struct{}{}
		}
		for text := range seen {
			frequency[text]++
		}
	}
	tableHeaderLine := func(value string) bool {
		return isTableHeaderLine(value, source.TableHeaders)
	}
	sourceAuthoredRepeat := func(value string) bool {
		if source.TextCounts[value] >= 2 {
			return true
		}
		if utf8.RuneCountInString(value) < 32 {
			return false
		}
		distinct := 0
		for row := range source.CommandRows {
			if utf8.RuneCountInString(row) > utf8.RuneCountInString(value) && strings.HasPrefix(row, value) {
				distinct++
			}
		}
		return distinct >= 2
	}
	result := []map[string]any{}
	for index := 0; index+1 < len(pages); index++ {
		left := filteredBoundaryLines(pages[index].Lines, frequency)
		right := filteredBoundaryLines(pages[index+1].Lines, frequency)
		for length := min(3, len(left), len(right)); length > 0; length-- {
			suffix := left[len(left)-length:]
			prefix := right[:length]
			if !equalStrings(suffix, prefix) {
				continue
			}
			unexplained := []string{}
			for _, line := range suffix {
				if !sourceAuthoredRepeat(line) && !tableHeaderLine(line) {
					unexplained = append(unexplained, line)
				}
			}
			if len(unexplained) > 0 {
				result = append(result, map[string]any{
					"physical_pages": []int{index + 1, index + 2},
					"sequence":       suffix,
					"unexplained":    unexplained,
				})
			}
			break
		}
	}
	return result
}

func isTableHeaderLine(value string, tableHeaders map[string]struct{}) bool {
	for header := range tableHeaders {
		if value == header ||
			(utf8.RuneCountInString(value) >= 8 && wordContains(header, value)) {
			return true
		}
	}
	return false
}

func filteredBoundaryLines(lines []AcceptanceLine, frequency map[string]int) []string {
	result := []string{}
	for _, line := range lines {
		if utf8.RuneCountInString(line.Text) >= 8 && frequency[line.Text] < 3 {
			result = append(result, line.Text)
		}
	}
	return result
}

func auditFooters(pages []AcceptancePage) ([]FooterRecord, map[string]any) {
	first := pages[0]
	footerTop := first.Height - 0.72*72
	expectedLeft := 0.68 * 72
	expectedRight := first.Width - expectedLeft
	records := make([]FooterRecord, 0, len(pages))
	violations := map[string]any{
		"bounds":               []AcceptanceLine{},
		"cover":                []AcceptanceLine{},
		"missing_or_duplicate": []map[string]any{},
		"overlap":              []map[string]any{},
		"physical_number":      []AcceptanceLine{},
		"roman_numbers":        []int{},
	}
	roman := regexp.MustCompile(`(?i)^[ivxlcdm]+$`)
	for index, page := range pages {
		physical := index + 1
		candidates := []AcceptanceLine{}
		for _, line := range page.Lines {
			if line.BBox[1] >= footerTop {
				candidates = append(candidates, line)
			}
		}
		if physical == 1 {
			if len(candidates) > 0 {
				violations["cover"] = candidates
			}
			records = append(records, FooterRecord{PhysicalPage: physical})
			continue
		}
		numbers, labels, unexpected := []AcceptanceLine{}, []AcceptanceLine{}, []AcceptanceLine{}
		for _, line := range candidates {
			if line.BBox[0] < first.Width/2 {
				labels = append(labels, line)
			} else if line.Text == strconv.Itoa(physical) {
				numbers = append(numbers, line)
			} else {
				unexpected = append(unexpected, line)
			}
		}
		if len(numbers) != 1 || len(labels) != 1 || len(unexpected) > 0 {
			value := map[string]any{
				"labels":           labels,
				"numbers":          numbers,
				"physical_page":    physical,
				"unexpected_right": unexpected,
			}
			violations["missing_or_duplicate"] = append(
				violations["missing_or_duplicate"].([]map[string]any), value,
			)
		}
		var number, label *AcceptanceLine
		if len(numbers) == 1 {
			number = &numbers[0]
		}
		if len(labels) == 1 {
			label = &labels[0]
		}
		if number != nil && number.Text != strconv.Itoa(physical) {
			violations["physical_number"] = append(
				violations["physical_number"].([]AcceptanceLine), *number,
			)
		}
		for _, line := range candidates {
			if line.BBox[0] >= first.Width/2 && roman.MatchString(line.Text) {
				violations["roman_numbers"] = append(violations["roman_numbers"].([]int), physical)
				break
			}
		}
		for _, line := range []*AcceptanceLine{label, number} {
			if line != nil && (line.BBox[0] < expectedLeft-1 || line.BBox[2] > expectedRight+1 ||
				line.BBox[1] < footerTop || line.BBox[3] > first.Height) {
				violations["bounds"] = append(violations["bounds"].([]AcceptanceLine), *line)
			}
		}
		if label != nil && number != nil {
			_, _, area := lineIntersection(*label, *number)
			if area > 0 {
				violations["overlap"] = append(
					violations["overlap"].([]map[string]any),
					map[string]any{"label": *label, "number": *number, "physical_page": physical},
				)
			}
		}
		record := FooterRecord{PhysicalPage: physical}
		if label != nil {
			record.Label = &label.Text
			record.LabelBBox = &label.BBox
		}
		if number != nil {
			record.Number = &number.Text
			record.NumberBBox = &number.BBox
		}
		records = append(records, record)
	}
	return records, violations
}

func auditHeadings(pages []AcceptancePage, anchors []string) ([]map[string]any, []map[string]any) {
	records := []map[string]any{}
	violations := []map[string]any{}
	for _, requested := range anchors {
		target := NormalizeAcceptanceText(requested)
		matches := []map[string]any{}
		for pageIndex, page := range pages {
			blocks := map[int][]AcceptanceLine{}
			for _, line := range page.Lines {
				blocks[line.Block] = append(blocks[line.Block], line)
			}
			type block struct {
				id    int
				lines []AcceptanceLine
			}
			ordered := make([]block, 0, len(blocks))
			for id, lines := range blocks {
				ordered = append(ordered, block{id: id, lines: lines})
			}
			sort.Slice(ordered, func(i, j int) bool {
				left, right := minimumY(ordered[i].lines), minimumY(ordered[j].lines)
				return left < right || (left == right && ordered[i].id < ordered[j].id)
			})
			for blockIndex, candidate := range ordered {
				text := blockText(candidate.lines)
				if text != target {
					continue
				}
				var following any
				for _, next := range ordered[blockIndex+1:] {
					nextText := blockText(next.lines)
					if nextText != "" {
						following = map[string]any{
							"bbox":          enclosingBox(next.lines),
							"physical_page": pageIndex + 1,
							"text":          nextText,
						}
						break
					}
				}
				match := map[string]any{
					"bbox":            enclosingBox(candidate.lines),
					"following":       following,
					"line_count":      len(candidate.lines),
					"max_font_points": maximumFont(candidate.lines),
					"physical_page":   pageIndex + 1,
					"requested":       requested,
					"text":            text,
				}
				matches = append(matches, match)
			}
		}
		records = append(records, matches...)
		if len(matches) != 1 {
			violations = append(violations, map[string]any{
				"matches": matches, "reason": "match-count", "requested": requested,
			})
			continue
		}
		match := matches[0]
		following, ok := match["following"].(map[string]any)
		if match["line_count"].(int) != 1 || match["max_font_points"].(float64) > 16.1 ||
			!ok || following["physical_page"] != match["physical_page"] {
			violations = append(violations, map[string]any{
				"match": match, "reason": "typography-or-orphan", "requested": requested,
			})
		}
	}
	return records, violations
}

func auditOutline(
	entries []OutlineEntry,
	expectedRoots, forbiddenTitles, childCounts []string,
) (OutlineRecord, []map[string]any, error) {
	roots := []string{}
	maxDepth := 0
	for _, entry := range entries {
		if entry.Depth < 1 {
			return OutlineRecord{}, nil, fmt.Errorf("outline entry %q has invalid depth %d", entry.Title, entry.Depth)
		}
		maxDepth = max(maxDepth, entry.Depth)
		if entry.Depth == 1 {
			roots = append(roots, entry.Title)
		}
	}
	violations := []map[string]any{}
	if len(expectedRoots) > 0 {
		actual := roots
		if len(actual) > len(expectedRoots) {
			actual = actual[:len(expectedRoots)]
		}
		if !equalStrings(actual, expectedRoots) {
			violations = append(violations, map[string]any{
				"actual": actual, "expected": expectedRoots, "reason": "root-prefix",
			})
		}
	}
	forbidden := map[string]struct{}{}
	for _, title := range forbiddenTitles {
		forbidden[title] = struct{}{}
	}
	found := []map[string]any{}
	for _, entry := range entries {
		if _, exists := forbidden[entry.Title]; exists {
			found = append(found, map[string]any{
				"depth": entry.Depth, "physical_page": entry.PhysicalPage, "title": entry.Title,
			})
		}
	}
	if len(found) > 0 {
		violations = append(violations, map[string]any{"entries": found, "reason": "forbidden-title"})
	}
	measured := map[string]int{}
	for _, raw := range childCounts {
		index := strings.LastIndex(raw, "=")
		if index <= 0 {
			return OutlineRecord{}, nil, fmt.Errorf("invalid --outline-direct-children value: %s", raw)
		}
		title := raw[:index]
		expected, err := strconv.Atoi(raw[index+1:])
		if err != nil {
			return OutlineRecord{}, nil, fmt.Errorf("invalid --outline-direct-children value: %s", raw)
		}
		matches := []int{}
		for index, entry := range entries {
			if entry.Title == title {
				matches = append(matches, index)
			}
		}
		if len(matches) != 1 {
			violations = append(violations, map[string]any{
				"matches": len(matches), "reason": "child-count-match", "title": title,
			})
			continue
		}
		start := matches[0]
		depth := entries[start].Depth
		actual := 0
		for _, entry := range entries[start+1:] {
			if entry.Depth <= depth {
				break
			}
			if entry.Depth == depth+1 {
				actual++
			}
		}
		measured[title] = actual
		if actual != expected {
			violations = append(violations, map[string]any{
				"actual": actual, "expected": expected, "reason": "child-count", "title": title,
			})
		}
	}
	return OutlineRecord{
		DirectChildren: measured,
		EntryCount:     len(entries),
		MaxDepth:       maxDepth,
		RootTitles:     roots,
	}, violations, nil
}

func blockText(lines []AcceptanceLine) string {
	values := make([]string, len(lines))
	for index, line := range lines {
		values[index] = line.Text
	}
	return NormalizeAcceptanceText(strings.Join(values, " "))
}

func enclosingBox(lines []AcceptanceLine) [4]float64 {
	box := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for _, line := range lines {
		box[0] = math.Min(box[0], line.BBox[0])
		box[1] = math.Min(box[1], line.BBox[1])
		box[2] = math.Max(box[2], line.BBox[2])
		box[3] = math.Max(box[3], line.BBox[3])
	}
	return box
}

func minimumY(lines []AcceptanceLine) float64 {
	value := math.Inf(1)
	for _, line := range lines {
		value = math.Min(value, line.BBox[1])
	}
	return value
}

func maximumFont(lines []AcceptanceLine) float64 {
	value := 0.0
	for _, line := range lines {
		value = math.Max(value, line.MaxFontPoints)
	}
	return value
}

func sortedSet(values map[int]struct{}) []int {
	result := make([]int, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Ints(result)
	return result
}

func sortedKeys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func wordContains(text, value string) bool {
	first, _ := utf8.DecodeRuneInString(value)
	last, _ := utf8.DecodeLastRuneInString(value)
	firstWord, lastWord := unicodeWord(first), unicodeWord(last)
	index := strings.Index(text, value)
	for index >= 0 {
		previousWord := false
		if index > 0 {
			before, _ := utf8.DecodeLastRuneInString(text[:index])
			previousWord = unicodeWord(before)
		}
		end := index + len(value)
		nextWord := false
		if end < len(text) {
			after, _ := utf8.DecodeRuneInString(text[end:])
			nextWord = unicodeWord(after)
		}
		leftOK := previousWord != firstWord
		rightOK := lastWord != nextWord
		if leftOK && rightOK {
			return true
		}
		next := strings.Index(text[index+1:], value)
		if next < 0 {
			return false
		}
		index += next + 1
	}
	return false
}

func readingOrderLines(lines []AcceptanceLine) []AcceptanceLine {
	result := append([]AcceptanceLine(nil), lines...)
	sort.SliceStable(result, func(i, j int) bool {
		left, right := result[i], result[j]
		switch {
		case left.BBox[1] != right.BBox[1]:
			return left.BBox[1] < right.BBox[1]
		case left.BBox[0] != right.BBox[0]:
			return left.BBox[0] < right.BBox[0]
		case left.Block != right.Block:
			return left.Block < right.Block
		default:
			return false
		}
	})
	return result
}

func unicodeWord(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value)
}

func emptyJSONValue(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case []int:
		return len(value) == 0
	case []string:
		return len(value) == 0
	case []AcceptanceLine:
		return len(value) == 0
	case []map[string]any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	default:
		return false
	}
}
