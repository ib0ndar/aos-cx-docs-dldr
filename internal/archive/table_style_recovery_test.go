package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

const (
	tableGuideRoot = "https://docs.example.test/techdocs/AOS-CX/10.13/HTML/diagnostics_6300-6400/"
	tableTopic     = tableGuideRoot + "Content/Chp_DebugLog/DebugLog_cmds/diag-eve-tra-ena.htm"
	tableEvidence  = tableGuideRoot + "Content/table-style-evidence.htm"
)

func TestFlareRecoversSourceVerifiedTableStyles(t *testing.T) {
	names := []string{"Parameter-Description", "Command-Info-3-Column", "Command-History"}
	resources := map[string]model.Resource{}
	var brokenLinks, evidenceLinks, brokenTables, evidenceTables strings.Builder
	for _, name := range names {
		broken := brokenTableStyleURL(name)
		candidate := verifiedTableStyleURL(name)
		brokenLinks.WriteString(`<link rel="stylesheet" data-mc-stylesheet-type="table" href="../../../../../../AOS-CX 10.16/aos-cx/CLI/Content/Resources/TableStyles/` + name + `.css">`)
		evidenceLinks.WriteString(`<link rel="stylesheet" data-mc-stylesheet-type="table" href="Resources/TableStyles/` + name + `.css">`)
		brokenTables.WriteString(`<table class="TableStyle-` + name + `" mc-table-style="../../../../../../AOS-CX 10.16/aos-cx/CLI/Content/Resources/TableStyles/` + name + `.css"><tr><td>` + name + `</td></tr></table>`)
		evidenceTables.WriteString(`<table class="TableStyle-` + name + `" mc-table-style="Resources/TableStyles/` + name + `.css"><tr><td>evidence</td></tr></table>`)
		resources[broken] = model.Resource{URL: broken, Status: http.StatusNotFound, Headers: http.Header{}, Body: ioNopString("not found")}
		resources[candidate] = fixtureResource(candidate, "text/css", []byte(`table.TableStyle-`+name+`{border:1px solid #123}.TableStyle-`+name+`-Head-Header1{font-weight:bold}`))
	}
	resources[tableTopic] = fixtureResource(tableTopic, "text/html", []byte(`<html><head>`+brokenLinks.String()+`</head><body><main id="mc-main-content"><h1>Diagnostics</h1>`+brokenTables.String()+`</main></body></html>`))
	resources[tableEvidence] = fixtureResource(tableEvidence, "text/html", []byte(`<html><head>`+evidenceLinks.String()+`</head><body><main id="mc-main-content"><h1>Table evidence</h1>`+evidenceTables.String()+`</main></body></html>`))
	fetcher := &archiveFixture{resources: resources, calls: map[string]int{}}
	output := t.TempDir()
	result, err := HTML(context.Background(), tableStylePlan(tableTopic, tableEvidence), fetcher, output, false, 1<<20, 8<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("verified table styles were not recovered: result=%+v err=%v", result, err)
	}
	if len(result.StylesheetRecoveries) != 3 || len(result.HTML.StylesheetRecoveries) != 3 ||
		len(result.Errors) != 0 || len(result.HTML.Assets) != 3 || len(result.Warnings) != 3 {
		t.Fatalf("unexpected recovery result: %+v", result)
	}
	for _, name := range names {
		broken := brokenTableStyleURL(name)
		candidate := verifiedTableStyleURL(name)
		if fetcher.calls[broken] != 1 || fetcher.calls[candidate] != 1 {
			t.Fatalf("stylesheet fetches were not canonical-deduplicated: broken=%d candidate=%d calls=%v",
				fetcher.calls[broken], fetcher.calls[candidate], fetcher.calls)
		}
		recovery := findRecovery(result.StylesheetRecoveries, broken)
		if recovery == nil || recovery.HTTPStatus != 404 || recovery.ReplacementURL != candidate ||
			recovery.ReplacementFinalURL != candidate || recovery.ReplacementSourceSHA256 == "" ||
			recovery.ReplacementSourceSize == 0 ||
			!slices.Equal(recovery.TableStyleFamilies, []string{"TableStyle-" + name}) ||
			!slices.Equal(recovery.AffectedTopics, []string{tableTopic}) {
			t.Fatalf("recovery provenance incomplete for %s: %+v", name, recovery)
		}
		if !slices.ContainsFunc(result.Warnings, func(warning string) bool {
			return strings.Contains(warning, broken) && strings.Contains(warning, candidate) &&
				strings.Contains(warning, "HTTP 404")
		}) {
			t.Fatalf("actionable recovery warning missing for %s: %v", name, result.Warnings)
		}
	}
	pageBody, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	for _, recovery := range result.StylesheetRecoveries {
		record := findAsset(result.HTML.Assets, recovery.ReplacementURL)
		if record == nil || record.Status != "complete" || !bytes.Contains(pageBody, []byte(filepath.Base(record.Path))) ||
			bytes.Contains(pageBody, []byte(recovery.BrokenURL)) {
			t.Fatalf("recovered stylesheet was not localized exactly: recovery=%+v record=%+v page=%s", recovery, record, pageBody)
		}
	}
}

func TestFlareTableStyleRecoveryDeduplicatesAffectedTopics(t *testing.T) {
	second := tableGuideRoot + "Content/second.htm"
	name := "Parameter-Description"
	broken, candidate := brokenTableStyleURL(name), verifiedTableStyleURL(name)
	brokenPage := func(title string) []byte {
		return []byte(`<html><head><link rel="stylesheet" data-mc-stylesheet-type="table" href="` + broken + `"></head><body><main id="mc-main-content"><h1>` + title + `</h1><table class="TableStyle-` + name + `" mc-table-style="` + broken + `"><tr><td>x</td></tr></table></main></body></html>`)
	}
	resources := map[string]model.Resource{
		tableTopic:    fixtureResource(tableTopic, "text/html", brokenPage("One")),
		second:        fixtureResource(second, "text/html", brokenPage("Two")),
		tableEvidence: fixtureResource(tableEvidence, "text/html", []byte(`<html><head><link rel="stylesheet" data-mc-stylesheet-type="table" href="Resources/TableStyles/`+name+`.css"></head><body><main id="mc-main-content"><table class="TableStyle-`+name+`"><tr><td>evidence</td></tr></table></main></body></html>`)),
		broken:        {URL: broken, Status: http.StatusGone, Headers: http.Header{}, Body: ioNopString("gone")},
		candidate:     fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{border:1px solid}`)),
	}
	fetcher := &archiveFixture{resources: resources, calls: map[string]int{}}
	result, err := HTML(context.Background(), tableStylePlan(tableTopic, second, tableEvidence), fetcher, t.TempDir(), false, 1<<20, 8<<20)
	if err != nil || result.Status != "complete" || len(result.StylesheetRecoveries) != 1 ||
		len(result.Warnings) != 1 || fetcher.calls[broken] != 1 {
		t.Fatalf("shared recovery was not deduplicated: result=%+v calls=%v err=%v", result, fetcher.calls, err)
	}
	if got := result.StylesheetRecoveries[0]; got.HTTPStatus != 410 ||
		!slices.Equal(got.AffectedTopics, []string{tableTopic, second}) {
		t.Fatalf("affected-topic provenance was not retained: %+v", got)
	}
}

func TestFlareTableStyleRecoveryFailsClosed(t *testing.T) {
	name := "Parameter-Description"
	broken, candidate := brokenTableStyleURL(name), verifiedTableStyleURL(name)
	tests := []struct {
		name           string
		brokenResource model.Resource
		candidates     map[string]model.Resource
		affectedLink   string
		affectedTable  string
		brokenError    error
		wantRecovery   bool
		wantComplete   bool
		wantExactAsset bool
	}{
		{
			name: "missing candidate", brokenResource: statusResource(broken, 404),
		},
		{
			name: "ambiguous candidates", brokenResource: statusResource(broken, 404),
			candidates: map[string]model.Resource{
				candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{color:black}`)),
				tableGuideRoot + "Content/Resources/Alternate/" + name + ".css": fixtureResource(tableGuideRoot+"Content/Resources/Alternate/"+name+".css", "text/css", []byte(`.TableStyle-`+name+`{color:blue}`)),
			},
		},
		{
			name: "basename case mismatch", brokenResource: statusResource(strings.Replace(broken, name+".css", strings.ToUpper(name)+".css", 1), 404),
			candidates:   map[string]model.Resource{candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{color:black}`))},
			affectedLink: strings.Replace(broken, name+".css", strings.ToUpper(name)+".css", 1),
		},
		{
			name: "selector family absent", brokenResource: statusResource(broken, 404),
			candidates: map[string]model.Resource{candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-Other{color:black}`))},
		},
		{
			name: "malformed candidate", brokenResource: statusResource(broken, 404),
			candidates: map[string]model.Resource{candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{background:url("unterminated)`))},
		},
		{
			name: "wrong candidate MIME", brokenResource: statusResource(broken, 404),
			candidates: map[string]model.Resource{candidate: fixtureResource(candidate, "application/octet-stream", []byte(`.TableStyle-`+name+`{color:black}`))},
		},
		{
			name: "candidate foreign redirect", brokenResource: statusResource(broken, 404),
			candidates: map[string]model.Resource{candidate: fixtureResource("https://foreign.example.test/"+name+".css", "text/css", []byte(`.TableStyle-`+name+`{color:black}`))},
		},
		{
			name: "candidate 404", brokenResource: statusResource(broken, 404),
			candidates: map[string]model.Resource{candidate: statusResource(candidate, 404)},
		},
		{
			name: "broken 500", brokenResource: statusResource(broken, 500),
			candidates: map[string]model.Resource{candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{color:black}`))},
		},
		{
			name: "broken timeout", brokenError: errors.New("publisher request timed out"),
			candidates: map[string]model.Resource{candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{color:black}`))},
		},
		{
			name: "broken cancellation", brokenError: context.Canceled,
			candidates: map[string]model.Resource{candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{color:black}`))},
		},
		{
			name: "valid outside source remains authoritative", brokenResource: fixtureResource(broken, "text/css", []byte(`.TableStyle-`+name+`{color:red}`)),
			candidates:   map[string]model.Resource{candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{color:black}`))},
			wantComplete: true, wantExactAsset: true,
		},
		{
			name: "ordinary stylesheet has no recovery", brokenResource: statusResource(broken, 404),
			candidates:   map[string]model.Resource{candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{color:black}`))},
			affectedLink: `<ordinary>`,
		},
		{
			name: "no corresponding table class", brokenResource: statusResource(broken, 404),
			candidates:    map[string]model.Resource{candidate: fixtureResource(candidate, "text/css", []byte(`.TableStyle-`+name+`{color:black}`))},
			affectedTable: `<table class="TableStyle-Other"><tr><td>x</td></tr></table>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actualBroken := broken
			if test.affectedLink != "" && test.affectedLink != "<ordinary>" {
				actualBroken = test.affectedLink
			}
			linkType := ` data-mc-stylesheet-type="table"`
			if test.affectedLink == "<ordinary>" {
				linkType = ""
			}
			table := test.affectedTable
			if table == "" {
				table = `<table class="TableStyle-` + name + `"><tr><td>x</td></tr></table>`
			}
			resources := map[string]model.Resource{
				tableTopic:   fixtureResource(tableTopic, "text/html", []byte(`<html><head><link rel="stylesheet"`+linkType+` href="`+actualBroken+`"></head><body><main id="mc-main-content">`+table+`</main></body></html>`)),
				actualBroken: test.brokenResource,
			}
			var topics []model.Topic
			topics = append(topics, model.Topic{URL: tableTopic, Title: "Affected"})
			for candidateURL, resource := range test.candidates {
				evidenceURL := tableGuideRoot + "Content/evidence-" + sourceHash([]byte(candidateURL))[:8] + ".htm"
				resources[evidenceURL] = fixtureResource(evidenceURL, "text/html", []byte(`<html><head><link rel="stylesheet" data-mc-stylesheet-type="table" href="`+candidateURL+`"></head><body><main id="mc-main-content"><table class="TableStyle-`+name+`"><tr><td>evidence</td></tr></table></main></body></html>`))
				resources[candidateURL] = resource
				topics = append(topics, model.Topic{URL: evidenceURL, Title: "Evidence"})
			}
			fetcher := &archiveFixture{resources: resources, calls: map[string]int{}, failures: map[string]error{actualBroken: test.brokenError}}
			result, err := HTML(context.Background(), tableStylePlanFromTopics(topics), fetcher, t.TempDir(), false, 1<<20, 8<<20)
			if err != nil {
				t.Fatalf("archive returned fatal error instead of durable incomplete result: %v", err)
			}
			if (result.Status == "complete") != test.wantComplete || len(result.StylesheetRecoveries) != 0 {
				t.Fatalf("fail-closed result mismatch: %+v", result)
			}
			if !test.wantComplete && (len(result.Errors) == 0 || !slices.ContainsFunc(result.HTML.Assets, func(record model.FileRecord) bool {
				return record.URL == actualBroken && record.Status == "failed"
			})) {
				t.Fatalf("failed required stylesheet was not retained: %+v", result)
			}
			if test.wantExactAsset && !slices.ContainsFunc(result.HTML.Assets, func(record model.FileRecord) bool {
				return record.URL == actualBroken && record.Status == "complete"
			}) {
				t.Fatalf("valid exact publisher stylesheet was substituted: %+v", result.HTML.Assets)
			}
			for raw, calls := range fetcher.calls {
				if calls > 1 && strings.HasSuffix(raw, ".css") {
					t.Fatalf("stylesheet request was repeated after probe: %s calls=%d all=%v", raw, calls, fetcher.calls)
				}
			}
		})
	}
}

func TestPermanentMissingHTTPStatusRejectsTransientFailures(t *testing.T) {
	for _, err := range []error{
		errors.New("timeout"),
		context.Canceled,
		context.DeadlineExceeded,
		&resourceStatusError{URL: "https://example.test/style.css", Status: 500},
	} {
		if _, ok := permanentMissingHTTPStatus(err); ok {
			t.Fatalf("transient/non-permanent failure became recoverable: %v", err)
		}
	}
}

func TestUniqueTableStyleCandidateRequiresExactBasenameAndSelectorFamily(t *testing.T) {
	state := &assetState{record: model.FileRecord{Status: "complete"}}
	reference := tableStyleReference{
		Basename: "Parameter-Description.css",
		Families: []string{"TableStyle-Parameter-Description"},
	}
	valid := tableStyleCandidate{
		Basename: reference.Basename,
		Families: map[string]bool{"TableStyle-Parameter-Description-Body-Body1": true},
		State:    state,
	}
	if uniqueTableStyleCandidate([]tableStyleCandidate{valid}, reference) == nil {
		t.Fatal("exact unique candidate was rejected")
	}
	for _, candidates := range [][]tableStyleCandidate{
		{valid, valid},
		{{Basename: "parameter-description.css", Families: valid.Families, State: state}},
		{{Basename: reference.Basename, Families: map[string]bool{"TableStyle-Other": true}, State: state}},
	} {
		if uniqueTableStyleCandidate(candidates, reference) != nil {
			t.Fatalf("ambiguous, case-colliding, or selector-mismatched candidate was accepted: %+v", candidates)
		}
	}
}

func TestTableStyleRecoveryDoesNotGeneralizeBeyondEligibleFlareReference(t *testing.T) {
	name := "Parameter-Description"
	local := verifiedTableStyleURL(name)
	foreign := "https://foreign.example.test/" + name + ".css"
	for _, test := range []struct {
		name, kind, affected string
	}{
		{"guide local", "flare", local},
		{"foreign origin", "flare", foreign},
		{"static source", "static", brokenTableStyleURL(name)},
		{"HPE source", "hpe", brokenTableStyleURL(name)},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := &htmlArchiver{
				plan:       model.DocumentPlan{Document: model.Document{Kind: test.kind}},
				guideRoots: []string{tableGuideRoot},
			}
			if a.recoverableTableStyleReference(test.affected) {
				t.Fatalf("ineligible reference passed the source/scope gate: kind=%s reference=%s", test.kind, test.affected)
			}
		})
	}
}

func TestFlareLocalMCTableStyleWithoutBrokenReferenceIsNotSpeculativelyFetched(t *testing.T) {
	name := "Parameter-Description"
	candidate := verifiedTableStyleURL(name)
	body := `<html><body><main id="mc-main-content"><table class="TableStyle-` + name +
		`" mc-table-style="Resources/TableStyles/` + name + `.css"><tr><td>x</td></tr></table></main></body></html>`
	fetcher := &archiveFixture{resources: map[string]model.Resource{
		tableTopic: fixtureResource(tableTopic, "text/html", []byte(body)),
		candidate:  statusResource(candidate, 404),
	}, calls: map[string]int{}}
	result, err := HTML(context.Background(), tableStylePlan(tableTopic), fetcher, t.TempDir(), false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" || fetcher.calls[candidate] != 0 ||
		len(result.StylesheetRecoveries) != 0 || len(result.HTML.Assets) != 0 {
		t.Fatalf("local authoring-only style was speculatively required: result=%+v calls=%v err=%v", result, fetcher.calls, err)
	}
}

func tableStylePlan(urls ...string) model.DocumentPlan {
	topics := make([]model.Topic, 0, len(urls))
	for _, raw := range urls {
		topics = append(topics, model.Topic{URL: raw, Title: pathBase(raw)})
	}
	return tableStylePlanFromTopics(topics)
}

func tableStylePlanFromTopics(topics []model.Topic) model.DocumentPlan {
	toc := make([]model.TocEntry, 0, len(topics))
	for _, topic := range topics {
		toc = append(toc, model.TocEntry{Title: topic.Title, URL: topic.URL})
	}
	return model.DocumentPlan{
		Document: model.Document{ID: "diagnostics", Title: "Diagnostics Guide", Platform: "6400", Version: "10.13", URL: tableTopic, Kind: "flare"},
		Topics:   topics, TOC: toc,
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "flare", RootURL: tableGuideRoot, Entries: len(topics), UniqueTopics: len(topics)},
	}
}

func brokenTableStyleURL(name string) string {
	return "https://docs.example.test/techdocs/AOS-CX/AOS-CX%2010.16/aos-cx/CLI/Content/Resources/TableStyles/" + name + ".css"
}

func verifiedTableStyleURL(name string) string {
	return tableGuideRoot + "Content/Resources/TableStyles/" + name + ".css"
}

func statusResource(raw string, status int) model.Resource {
	return model.Resource{URL: raw, Status: status, Headers: http.Header{}, Body: ioNopString(http.StatusText(status))}
}

func ioNopString(value string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(value))
}

func pathBase(raw string) string {
	parsed, _ := url.Parse(raw)
	return path.Base(parsed.Path)
}

func findRecovery(values []model.StylesheetRecovery, broken string) *model.StylesheetRecovery {
	for index := range values {
		if values[index].BrokenURL == broken {
			return &values[index]
		}
	}
	return nil
}

func findAsset(values []model.FileRecord, raw string) *model.FileRecord {
	for index := range values {
		if values[index].URL == raw {
			return &values[index]
		}
	}
	return nil
}
