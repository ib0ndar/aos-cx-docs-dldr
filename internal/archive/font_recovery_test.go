package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
)

func TestFontFaceSourceRewriteDropsOnlyUnavailableSources(t *testing.T) {
	type call struct {
		reference string
		kind      dependencyKind
	}
	gone := func(reference string) model.BrokenStylesheetReference {
		return model.BrokenStylesheetReference{URL: reference, HTTPStatus: http.StatusNotFound}
	}
	for _, test := range []struct {
		name      string
		input     string
		inline    bool
		want      string
		wantFaces []fontFaceRecovery
		wantErr   bool
	}{
		{
			name:  "all sources unavailable removes the rule and keeps family stacks",
			input: `@font-face{font-family:'A';src:url(gone-a.woff2) format('woff2'),url(gone-a.woff) format('woff');font-weight:300}p{font-family:A,Arial,sans-serif}`,
			want:  `p{font-family:A,Arial,sans-serif;}`,
			wantFaces: []fontFaceRecovery{{
				Family: "A", Dropped: []model.BrokenStylesheetReference{gone("gone-a.woff2"), gone("gone-a.woff")}, Removed: true,
			}},
		},
		{
			name:  "remaining sources keep the rule",
			input: `@font-face{font-family:"B";src:url(gone-b.woff2) format("woff2"),url(b.woff) format("woff")}`,
			want:  `@font-face{font-family:"B";src:url("local/b.woff") format("woff");}`,
			wantFaces: []fontFaceRecovery{{
				Family: "B", Dropped: []model.BrokenStylesheetReference{gone("gone-b.woff2")},
			}},
		},
		{
			name:  "local source remains",
			input: `@font-face{font-family:C;src:local(C),url(gone-c.woff2)}`,
			want:  `@font-face{font-family:C;src:local(C);}`,
			wantFaces: []fontFaceRecovery{{
				Family: "C", Dropped: []model.BrokenStylesheetReference{gone("gone-c.woff2")},
			}},
		},
		{
			name:  "emptied last src descriptor removes the rule",
			input: `@font-face{font-family:D;src:url(d.eot);src:url(gone-d.woff2) format("woff2")}`,
			want:  ``,
			wantFaces: []fontFaceRecovery{{
				Family: "D", Dropped: []model.BrokenStylesheetReference{gone("gone-d.woff2")}, Removed: true,
			}},
		},
		{
			name:  "emptied earlier src descriptor is omitted",
			input: `@font-face{font-family:E;src:url(gone-e.eot);src:url(e.woff2)}`,
			want:  `@font-face{font-family:E;src:url("local/e.woff2");}`,
			wantFaces: []fontFaceRecovery{{
				Family: "E", Dropped: []model.BrokenStylesheetReference{gone("gone-e.eot")},
			}},
		},
		{
			name:  "rule inside a conditional group",
			input: `@media print{@font-face{font-family:F;src:url(gone-f.woff2)}p{color:red}}`,
			want:  `@media print{p{color:red;}}`,
			wantFaces: []fontFaceRecovery{{
				Family: "F", Dropped: []model.BrokenStylesheetReference{gone("gone-f.woff2")}, Removed: true,
			}},
		},
		{
			name:  "available sources are unchanged",
			input: `@font-face{font-family:G;src:url(g.woff2) format("woff2"),url(g.woff) format("woff")}`,
			want:  `@font-face{font-family:G;src:url("local/g.woff2") format("woff2"),url("local/g.woff") format("woff");}`,
		},
		{name: "font outside src", input: `@font-face{font-family:H;src:url(h.woff2)}p{background:url(gone-h.woff2)}`, wantErr: true},
		{name: "two URLs in one entry", input: `@font-face{font-family:I;src:url(gone-i.woff2) url(i.woff)}`, wantErr: true},
		{name: "escaped at-rule", input: `@\66ont-face{font-family:J;src:url(gone-j.woff2)}`, wantErr: true},
		{name: "vendor-prefixed at-rule", input: `@-webkit-font-face{font-family:K;src:url(gone-k.woff2)}`, wantErr: true},
		{name: "style attribute", input: `@font-face{src:url(gone-l.woff2)}`, inline: true, wantErr: true},
		{name: "unavailable import", input: `@import url(gone.css);`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []call
			output, diagnostics, err := rewriteCSS([]byte(test.input), test.inline, func(reference string, kind dependencyKind) (string, error) {
				calls = append(calls, call{reference, kind})
				if strings.HasPrefix(reference, "gone") {
					if kind != dependencyFontSource {
						return "", errors.New("required resource failed: " + reference)
					}
					return "", &unavailableFontError{URL: reference, Status: http.StatusNotFound, cause: errors.New("HTTP 404: " + reference)}
				}
				return "local/" + reference, nil
			})
			if test.wantErr {
				if err == nil || len(diagnostics.FontFaces) != 0 {
					t.Fatalf("failure was recovered: output=%s faces=%+v err=%v", output, diagnostics.FontFaces, err)
				}
				return
			}
			if err != nil || string(output) != test.want || !reflect.DeepEqual(diagnostics.FontFaces, test.wantFaces) {
				t.Fatalf("output=%q faces=%+v err=%v; want %q faces=%+v", output, diagnostics.FontFaces, err, test.want, test.wantFaces)
			}
			for _, call := range calls {
				if call.kind != dependencyFontSource {
					t.Fatalf("@font-face src entry resolved as dependency kind %d: %+v", call.kind, calls)
				}
			}
		})
	}
}

const fontRecoveryRoot = "https://oxygen.example.test/guide/"

const (
	fontRecoveryHome       = fontRecoveryRoot + "topics/home.html"
	fontRecoverySecond     = fontRecoveryRoot + "topics/second.html"
	fontRecoveryStylesheet = fontRecoveryRoot + "oxygen-webhelp/css/fonts.css?buildId=1"
	fontRecoveryFonts      = fontRecoveryRoot + "oxygen-webhelp/fonts/"
	fontRecoveryThirdParty = "https://fonts.example.test/MetricWeb-Light.woff"
	fontRecoveryRobots     = "https://fonts.example.test/robots.txt"
	fontRecoveryInline     = fontRecoveryRoot + "fonts/inline.woff2"
	fontRecoveryDiagram    = fontRecoveryRoot + "images/diagram.svg"
	fontRecoveryDiagramWeb = fontRecoveryRoot + "fonts/diagram.woff2"
)

func fontRecoveryPlan() model.DocumentPlan {
	return model.DocumentPlan{
		Document: model.Document{
			ID: "fonts", Title: "Font Recovery Guide", Platform: "8320",
			Version: "10.18", Kind: "static", URL: fontRecoveryHome,
		},
		Topics: []model.Topic{{URL: fontRecoveryHome, Title: "Home"}, {URL: fontRecoverySecond, Title: "Second"}},
		TOC:    []model.TocEntry{{Title: "Home", URL: fontRecoveryHome}, {Title: "Second", URL: fontRecoverySecond}},
		Inventory: &model.InventoryEvidence{
			Complete: true, Kind: "static", RootURL: fontRecoveryRoot,
			TOCURL: fontRecoveryHome, Entries: 2, UniqueTopics: 2,
		},
	}
}

// fontRecoveryStyles mirrors the Oxygen WebHelp fonts.css: the Metric
// sources are refused by the publisher and the third-party fallback host.
func fontRecoveryStyles(thirdParty string) string {
	return `@font-face{font-family:'Metric';src:url(../fonts/Metric-Light.woff2) format('woff2'),url(` + thirdParty + `) format('woff');font-weight:300}
@font-face{font-family:'Mixed';src:url(../fonts/mixed-gone.woff2) format('woff2'),url(../fonts/mixed.woff) format('woff')}
@font-face{font-family:'HPE Glyphs';src:url(../fonts/glyphs.woff2) format('woff2'),url(../fonts/glyphs.woff) format('woff')}
h1{font-family:Metric,'HPE Glyphs',Arial,sans-serif}`
}

func fontRecoveryPage(title string) model.Resource {
	return fixtureResource(fontRecoveryRoot+"topics/"+strings.ToLower(title)+".html", "text/html", []byte(
		`<html><head><link rel="stylesheet" href="../oxygen-webhelp/css/fonts.css?buildId=1">
<style>@font-face{font-family:"Inline Face";src:local("Inline Face"),url("../fonts/inline.woff2") format("woff2")}</style></head>
<body><main class="wh_topic_content"><h1 id="top">`+title+`</h1><p>Topic text</p><img alt="Diagram" src="../images/diagram.svg"></main></body></html>`))
}

func fontRecoveryFixture() *archiveFixture {
	woff2 := append([]byte("wOF2"), bytes.Repeat([]byte{0}, 12)...)
	woff := append([]byte("wOFF"), bytes.Repeat([]byte{0}, 12)...)
	missing := func(raw string, status int) model.Resource {
		return model.Resource{URL: raw, Status: status, Headers: http.Header{}, Body: http.NoBody}
	}
	return &archiveFixture{
		resources: map[string]model.Resource{
			fontRecoveryHome:                         fontRecoveryPage("Home"),
			fontRecoverySecond:                       fontRecoveryPage("Second"),
			fontRecoveryStylesheet:                   fixtureResource(fontRecoveryStylesheet, "text/css", []byte(fontRecoveryStyles(fontRecoveryThirdParty))),
			fontRecoveryFonts + "Metric-Light.woff2": missing(fontRecoveryFonts+"Metric-Light.woff2", http.StatusNotFound),
			fontRecoveryFonts + "mixed.woff":         fixtureResource(fontRecoveryFonts+"mixed.woff", "font/woff", woff),
			fontRecoveryFonts + "glyphs.woff2":       fixtureResource(fontRecoveryFonts+"glyphs.woff2", "font/woff2", woff2),
			fontRecoveryFonts + "glyphs.woff":        fixtureResource(fontRecoveryFonts+"glyphs.woff", "font/woff", woff),
			fontRecoveryInline:                       missing(fontRecoveryInline, http.StatusGone),
			fontRecoveryDiagramWeb:                   missing(fontRecoveryDiagramWeb, http.StatusNotFound),
			fontRecoveryDiagram: fixtureResource(fontRecoveryDiagram, "image/svg+xml", []byte(
				`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 40 20"><style>@font-face{font-family:"Diagram";src:url("../fonts/diagram.woff2")}</style><text font-family="Diagram">Port</text></svg>`)),
		},
		calls: map[string]int{},
		failures: map[string]error{
			// The transport reports refusals this way.
			fontRecoveryThirdParty: &fetch.RetrievalError{
				RequestURL: fontRecoveryThirdParty, FinalURL: fontRecoveryThirdParty, Stage: fetch.StageHeaders,
				ApplicationAttempts: 1, Cause: &fetch.StatusError{URL: fontRecoveryThirdParty, Status: http.StatusGone},
			},
			fontRecoveryFonts + "mixed-gone.woff2": &fetch.RetrievalError{
				RequestURL: fontRecoveryFonts + "mixed-gone.woff2", FinalURL: fontRecoveryFonts + "mixed-gone.woff2",
				Stage: fetch.StageHeaders, ApplicationAttempts: 1,
				Cause: &fetch.StatusError{URL: fontRecoveryFonts + "mixed-gone.woff2", Status: http.StatusForbidden},
			},
		},
	}
}

func fontRecoveryRecord(stylesheet, family string, removed bool, dropped ...model.BrokenStylesheetReference) model.StylesheetRecovery {
	return model.StylesheetRecovery{
		Kind: fontFaceRecoveryKind, BrokenURL: dropped[0].URL, HTTPStatus: dropped[0].HTTPStatus,
		BrokenDeclarations: dropped, TableStyleFamilies: []string{}, AffectedTopics: []string{},
		StylesheetURL: stylesheet, FontFamily: family, FontFaceRemoved: removed,
	}
}

func sortedRecoveries(values []model.StylesheetRecovery) []model.StylesheetRecovery {
	sorted := cloneStylesheetRecoveries(values)
	slices.SortFunc(sorted, func(a, b model.StylesheetRecovery) int {
		return strings.Compare(a.StylesheetURL+"\x00"+a.FontFamily, b.StylesheetURL+"\x00"+b.FontFamily)
	})
	return sorted
}

func TestUnavailablePublisherFontSourcesAreDroppedAndRecorded(t *testing.T) {
	fetcher := fontRecoveryFixture()
	output := t.TempDir()
	result, err := HTML(context.Background(), fontRecoveryPlan(), fetcher, output, false, 1<<20, 4<<20)
	if err != nil || result.Status != "complete" || len(result.Errors) != 0 || len(result.MissingResources) != 0 {
		t.Fatalf("unavailable font sources did not leave a complete guide: result=%+v err=%v", result, err)
	}
	metricLight := fontRecoveryFonts + "Metric-Light.woff2"
	inline := model.BrokenStylesheetReference{URL: fontRecoveryInline, HTTPStatus: http.StatusGone}
	want := sortedRecoveries([]model.StylesheetRecovery{
		fontRecoveryRecord(fontRecoveryStylesheet, "Metric", true,
			model.BrokenStylesheetReference{URL: metricLight, HTTPStatus: http.StatusNotFound},
			model.BrokenStylesheetReference{URL: fontRecoveryThirdParty, HTTPStatus: http.StatusGone}),
		fontRecoveryRecord(fontRecoveryStylesheet, "Mixed", false,
			model.BrokenStylesheetReference{URL: fontRecoveryFonts + "mixed-gone.woff2", HTTPStatus: http.StatusForbidden}),
		fontRecoveryRecord(fontRecoveryHome, "Inline Face", false, inline),
		fontRecoveryRecord(fontRecoverySecond, "Inline Face", false, inline),
		fontRecoveryRecord(fontRecoveryDiagram, "Diagram", true,
			model.BrokenStylesheetReference{URL: fontRecoveryDiagramWeb, HTTPStatus: http.StatusNotFound}),
	})
	if got := sortedRecoveries(result.StylesheetRecoveries); !reflect.DeepEqual(got, want) {
		t.Fatalf("font recoveries:\n got %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(result.StylesheetRecoveries, result.HTML.StylesheetRecoveries) {
		t.Fatalf("result and archive recoveries differ: %+v %+v", result.StylesheetRecoveries, result.HTML.StylesheetRecoveries)
	}
	data, err := os.ReadFile(filepath.Join(output, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk model.HTMLArchive
	if err := json.Unmarshal(data, &disk); err != nil || !reflect.DeepEqual(disk.StylesheetRecoveries, result.HTML.StylesheetRecoveries) {
		t.Fatalf("manifest recoveries do not round-trip: %v\n%s", err, data)
	}
	for _, raw := range []string{metricLight, fontRecoveryThirdParty, fontRecoveryFonts + "mixed-gone.woff2", fontRecoveryInline, fontRecoveryDiagramWeb} {
		if fetcher.calls[raw] != 1 {
			t.Fatalf("unavailable font %s requested %d times", raw, fetcher.calls[raw])
		}
	}

	var stylesheet *model.FileRecord
	for index, record := range result.HTML.Assets {
		if record.Status != "complete" || strings.Contains(record.URL, "gone") ||
			record.URL == metricLight || record.URL == fontRecoveryThirdParty ||
			record.URL == fontRecoveryInline || record.URL == fontRecoveryDiagramWeb {
			t.Fatalf("unavailable font became an asset record: %+v", record)
		}
		if record.URL == fontRecoveryStylesheet {
			stylesheet = &result.HTML.Assets[index]
		}
	}
	if stylesheet == nil || len(result.HTML.Assets) != 5 {
		t.Fatalf("unexpected archived assets: %+v", result.HTML.Assets)
	}
	css, err := os.ReadFile(filepath.Join(output, stylesheet.Path))
	if err != nil {
		t.Fatal(err)
	}
	rules := strings.Split(string(css), "@font-face")
	if len(rules) != 3 || bytes.Contains(css, []byte("Metric-Light")) || bytes.Contains(css, []byte("MetricWeb")) ||
		bytes.Contains(css, []byte("mixed-gone")) || bytes.Contains(css, []byte("font-family:'Metric'")) ||
		!bytes.Contains(css, []byte("h1{font-family:Metric,'HPE Glyphs',Arial,sans-serif;}")) {
		t.Fatalf("archived stylesheet did not drop exactly the unavailable sources:\n%s", css)
	}
	for _, rule := range rules[1:] {
		urls := strings.Count(rule[:strings.IndexByte(rule, '}')], "url(")
		if strings.Contains(rule, "'Mixed'") && urls != 1 || strings.Contains(rule, "'HPE Glyphs'") && urls != 2 {
			t.Fatalf("remaining @font-face sources changed:\n%s", css)
		}
	}
	home, err := os.ReadFile(filepath.Join(output, result.HTML.Topics[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(home, []byte(`src:local("Inline Face");`)) || bytes.Contains(home, []byte("inline.woff2")) {
		t.Fatalf("page <style> did not keep only its available source:\n%s", home)
	}
	var diagram []byte
	for _, record := range result.HTML.Assets {
		if record.URL == fontRecoveryDiagram {
			diagram, err = os.ReadFile(filepath.Join(output, record.Path))
		}
	}
	if err != nil || bytes.Contains(diagram, []byte("@font-face")) || !bytes.Contains(diagram, []byte("Port")) {
		t.Fatalf("SVG <style> rule was not removed: %v\n%s", err, diagram)
	}
	warned := slices.ContainsFunc(result.Warnings, func(warning string) bool {
		return strings.Contains(warning, fontRecoveryStylesheet) &&
			strings.Contains(warning, "HTTP 403 (1), HTTP 404 (1), HTTP 410 (1)") &&
			strings.Contains(warning, "removed 1 of 2 affected rules") &&
			strings.Contains(warning, "text in Metric falls back to the next font") &&
			strings.Contains(warning, "The other rule keeps another source, and Mixed is archived from it.")
	})
	if !warned {
		t.Fatalf("stylesheet recovery was not reported as a warning: %q", result.Warnings)
	}
}

func TestUnavailableFontRecoveryKeepsOtherFailuresFailClosed(t *testing.T) {
	metricLight := fontRecoveryFonts + "Metric-Light.woff2"
	retrieval := func(raw string, stage fetch.RetrievalStage, cause error) error {
		return &fetch.RetrievalError{RequestURL: raw, FinalURL: raw, Stage: stage, ApplicationAttempts: 3, ApplicationRetries: 2, Cause: cause}
	}
	stylesheet := func(body string) func(*archiveFixture) {
		return func(f *archiveFixture) {
			f.resources[fontRecoveryStylesheet] = fixtureResource(fontRecoveryStylesheet, "text/css", []byte(body))
		}
	}
	missing := func(raw string, status int) func(*archiveFixture) {
		return func(f *archiveFixture) {
			f.resources[raw] = model.Resource{URL: raw, Status: status, Headers: http.Header{}, Body: http.NoBody}
		}
	}
	failure := func(raw string, err error) func(*archiveFixture) {
		return func(f *archiveFixture) { f.failures[raw] = err }
	}
	loopback := "http://127.0.0.1/MetricWeb-Light.woff"
	for _, test := range []struct {
		name   string
		change []func(*archiveFixture)
		want   string
		// recovered is set when the stylesheet itself was archived, so its
		// recovery stands although another use of the font failed.
		recovered bool
	}{
		{"publisher 5xx response", []func(*archiveFixture){missing(metricLight, http.StatusServiceUnavailable)}, metricLight, false},
		{"5xx after retries", []func(*archiveFixture){failure(metricLight, retrieval(metricLight, fetch.StageHeaders,
			&fetch.StatusError{URL: metricLight, Status: http.StatusBadGateway}))}, metricLight, false},
		{"timeout", []func(*archiveFixture){failure(metricLight, retrieval(metricLight, fetch.StageHeaders,
			fmt.Errorf("per-attempt deadline exceeded at headers: %w", context.DeadlineExceeded)))}, metricLight, false},
		{"connection error", []func(*archiveFixture){failure(metricLight, retrieval(metricLight, fetch.StageHeaders,
			errors.New("HTTP request failed: connection reset by peer")))}, metricLight, false},
		{"unauthorized", []func(*archiveFixture){missing(metricLight, http.StatusUnauthorized)}, metricLight, false},
		{"robots disallow", []func(*archiveFixture){failure(fontRecoveryThirdParty, retrieval(fontRecoveryThirdParty, fetch.StageRobots,
			errors.New("publisher robots.txt disallows "+fontRecoveryThirdParty)))}, fontRecoveryThirdParty, false},
		{"robots unavailable", []func(*archiveFixture){failure(fontRecoveryThirdParty, retrieval(fontRecoveryThirdParty, fetch.StageRobots,
			fmt.Errorf("cannot establish publisher robots rules: %w",
				&fetch.StatusError{URL: fontRecoveryRobots, Status: http.StatusServiceUnavailable})))}, fontRecoveryThirdParty, false},
		{"robots rate limited", []func(*archiveFixture){failure(fontRecoveryThirdParty, retrieval(fontRecoveryThirdParty, fetch.StageRobots,
			fmt.Errorf("cannot establish publisher robots rules: %w",
				&fetch.StatusError{URL: fontRecoveryRobots, Status: http.StatusTooManyRequests})))}, fontRecoveryThirdParty, false},
		{"destination policy", []func(*archiveFixture){
			stylesheet(fontRecoveryStyles(loopback)),
			failure(loopback, retrieval(loopback, fetch.StageScope, fetch.CheckDestination(loopback, false))),
		}, loopback, false},
		{"oversized body", []func(*archiveFixture){func(f *archiveFixture) {
			f.resources[metricLight] = fixtureResource(metricLight, "font/woff2", append([]byte("wOF2"), make([]byte, 1<<20)...))
		}}, metricLight, false},
		{"error page body", []func(*archiveFixture){func(f *archiveFixture) {
			f.resources[metricLight] = fixtureResource(metricLight, "font/woff2", []byte("<!doctype html><p>Not found</p>"))
		}}, metricLight, false},
		{"invalid font body", []func(*archiveFixture){func(f *archiveFixture) {
			f.resources[metricLight] = fixtureResource(metricLight, "font/woff2", []byte("not a font"))
		}}, metricLight, false},
		{"missing image", []func(*archiveFixture){
			stylesheet(fontRecoveryStyles(fontRecoveryThirdParty) + "\nh1{background:url(../images/gone.png)}"),
			missing(fontRecoveryRoot+"oxygen-webhelp/images/gone.png", http.StatusNotFound),
		}, fontRecoveryRoot + "oxygen-webhelp/images/gone.png", false},
		{"missing import", []func(*archiveFixture){
			stylesheet(`@import url(gone.css);` + fontRecoveryStyles(fontRecoveryThirdParty)),
			missing(fontRecoveryRoot+"oxygen-webhelp/css/gone.css", http.StatusNotFound),
		}, fontRecoveryRoot + "oxygen-webhelp/css/gone.css", false},
		{"stylesheet server error", []func(*archiveFixture){missing(fontRecoveryStylesheet, http.StatusServiceUnavailable)}, fontRecoveryStylesheet, false},
		{"font outside @font-face after recovery", []func(*archiveFixture){
			stylesheet(fontRecoveryStyles(fontRecoveryThirdParty) + "\nh1{background:url(../fonts/Metric-Light.woff2)}"),
		}, metricLight, false},
		{"font outside @font-face before recovery", []func(*archiveFixture){
			stylesheet("h1{background:url(../fonts/Metric-Light.woff2)}\n" + fontRecoveryStyles(fontRecoveryThirdParty)),
		}, metricLight, false},
		{"font as image after recovery", []func(*archiveFixture){func(f *archiveFixture) {
			page := fontRecoveryPage("Second")
			body, _ := io.ReadAll(page.Body)
			f.resources[fontRecoverySecond] = fixtureResource(fontRecoverySecond, "text/html",
				bytes.Replace(body, []byte(`<p>Topic text</p>`), []byte(`<p>Topic text</p><img alt="Glyph" src="../oxygen-webhelp/fonts/Metric-Light.woff2">`), 1))
		}}, metricLight, true},
		{"escaped @font-face", []func(*archiveFixture){
			stylesheet(`@\66ont-face{font-family:'Escaped';src:url(../fonts/Metric-Light.woff2)}`),
		}, metricLight, false},
		{"two URLs in one source", []func(*archiveFixture){
			stylesheet(`@font-face{font-family:'Pair';src:url(../fonts/Metric-Light.woff2) url(../fonts/glyphs.woff)}`),
		}, metricLight, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fetcher := fontRecoveryFixture()
			for _, change := range test.change {
				change(fetcher)
			}
			result, err := HTML(context.Background(), fontRecoveryPlan(), fetcher, t.TempDir(), false, 1<<20, 8<<20)
			if err != nil || result.Status != "incomplete" ||
				!slices.ContainsFunc(result.Errors, func(message string) bool { return strings.Contains(message, test.want) }) {
				t.Fatalf("failure was not kept fail-closed: result=%+v err=%v", result, err)
			}
			if slices.ContainsFunc(result.StylesheetRecoveries, func(recovery model.StylesheetRecovery) bool {
				return recovery.StylesheetURL == fontRecoveryStylesheet
			}) != test.recovered {
				t.Fatalf("stylesheet recovery recorded=%t, want %t: %+v",
					!test.recovered, test.recovered, result.StylesheetRecoveries)
			}
		})
	}
}

func TestUnavailableFontStatusRequiresAPermanentRefusal(t *testing.T) {
	raw := "https://fonts.example.test/font.woff2"
	final := "https://cdn.example.test/font.woff2"
	robots := "https://fonts.example.test/robots.txt"
	robotsRefusal := func(status int) error {
		return &fetch.RetrievalError{Stage: fetch.StageRobots, Cause: fmt.Errorf(
			"cannot establish publisher robots rules: %w", &fetch.StatusError{URL: robots, Status: status})}
	}
	for _, test := range []struct {
		name   string
		err    error
		status int
		final  string
		want   bool
	}{
		{"403", &fetch.RetrievalError{Stage: fetch.StageHeaders, Cause: &fetch.StatusError{URL: raw, Status: 403}}, 403, raw, true},
		{"404 after redirect", &fetch.RetrievalError{Stage: fetch.StageHeaders, Cause: &fetch.StatusError{URL: final, Status: 404}}, 404, final, true},
		{"410", &fetch.RetrievalError{Stage: fetch.StageHeaders, Cause: &fetch.StatusError{URL: raw, Status: 410}}, 410, raw, true},
		{"wrapped", fmt.Errorf("cache: %w", &fetch.RetrievalError{Stage: fetch.StageHeaders, Cause: &fetch.StatusError{URL: raw, Status: 404}}), 404, raw, true},
		{"response", &resourceStatusError{URL: raw, Status: 404}, 404, raw, true},
		{"401", &fetch.RetrievalError{Stage: fetch.StageHeaders, Cause: &fetch.StatusError{URL: raw, Status: 401}}, 0, "", false},
		{"500", &resourceStatusError{URL: raw, Status: 500}, 0, "", false},
		// A robots.txt the transport could not read is never a refusal of
		// the font; 4xx answers no longer reach here at all.
		{"robots 403", robotsRefusal(403), 0, "", false},
		{"robots 429", robotsRefusal(429), 0, "", false},
		{"robots 503", robotsRefusal(503), 0, "", false},
		{"robots disallow", &fetch.RetrievalError{Stage: fetch.StageRobots, Cause: errors.New("publisher robots.txt disallows " + raw)}, 0, "", false},
		{"scope", &fetch.RetrievalError{Stage: fetch.StageScope, Cause: &fetch.StatusError{URL: raw, Status: 404}}, 0, "", false},
		{"body", &fetch.RetrievalError{Stage: fetch.StageBody, Cause: &fetch.StatusError{URL: raw, Status: 404}}, 0, "", false},
		{"wrapped status", &fetch.RetrievalError{Stage: fetch.StageHeaders, Cause: fmt.Errorf("x: %w", &fetch.StatusError{URL: raw, Status: 404})}, 0, "", false},
		{"canceled", context.Canceled, 0, "", false},
		{"bare status", &fetch.StatusError{URL: raw, Status: 404}, 0, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, final, ok := unavailableFontStatus(test.err)
			if ok != test.want || ok && (status != test.status || final != test.final) {
				t.Fatalf("unavailableFontStatus(%v) = %d, %q, %t", test.err, status, final, ok)
			}
		})
	}
}

func TestFontFaceRecoveryWarningSaysWhetherTheFontIsArchived(t *testing.T) {
	gone := func(name string) []model.BrokenStylesheetReference {
		return []model.BrokenStylesheetReference{{URL: "https://publisher.example/fonts/" + name, HTTPStatus: 404}}
	}
	metric := func(weight string, removed bool) fontFaceRecovery {
		return fontFaceRecovery{Family: "Metric", Dropped: gone("Metric-" + weight + ".woff2"), Removed: removed}
	}
	for _, tc := range []struct {
		name       string
		recoveries []fontFaceRecovery
		want       string
	}{
		{"one rule keeps a source", []fontFaceRecovery{metric("Light", false)},
			"HTTP 404 (1). Dropped them; the affected rule keeps another source, and Metric is archived from it."},
		{"every rule keeps a source", []fontFaceRecovery{metric("Light", false), metric("Regular", false), metric("Black", false)},
			"HTTP 404 (3). Dropped them; all 3 affected rules keep another source, and Metric is archived from those."},
		{"some rules lose every source", []fontFaceRecovery{metric("Light", true), metric("Regular", false), metric("Black", false),
			{Family: "Glyphs", Dropped: gone("glyphs.woff2"), Removed: true}},
			"HTTP 404 (4). Dropped them and removed 2 of 4 affected rules that had no source left; text in Glyphs, Metric falls " +
				"back to the next font in each font-family stack. The other 2 rules keep another source, and Metric is archived from those."},
		{"every rule loses every source", []fontFaceRecovery{metric("Light", true), metric("Regular", true)},
			"HTTP 404 (2). Dropped them and removed 2 of 2 affected rules that had no source left; text in Metric falls back to " +
				"the next font in each font-family stack."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warnings []string
			var recoveries []model.StylesheetRecovery
			m := &assetManager{warnings: &warnings, recoveries: &recoveries}
			m.recordFontFaceRecoveries("https://publisher.example/fonts.css", tc.recoveries)
			prefix := "Publisher CSS in https://publisher.example/fonts.css declares unavailable @font-face sources for "
			if len(warnings) != 1 || !strings.HasPrefix(warnings[0], prefix) || !strings.HasSuffix(warnings[0], tc.want) {
				t.Fatalf("warning = %q\nwant suffix %q", warnings, tc.want)
			}
			if fallback := strings.Contains(warnings[0], "falls back"); fallback != strings.Contains(tc.want, "falls back") {
				t.Fatalf("fallback mentioned = %v: %q", fallback, warnings[0])
			}
		})
	}
}
