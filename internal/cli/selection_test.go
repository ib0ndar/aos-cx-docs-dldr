package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/model"
)

type selectionProbeTransport struct {
	requests []model.Request
	err      error
	mu       sync.Mutex
}

func createCurrentSelectionLibrary(t *testing.T, base, platform, version string) {
	t.Helper()
	run, err := library.Open(base, platform, version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Publish(true); err != nil {
		run.Close()
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func (p *selectionProbeTransport) Download(_ context.Context, request model.Request, consume func(model.Resource) error) (model.Resource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	if p.err != nil {
		return model.Resource{}, p.err
	}
	resource := model.Resource{
		URL: request.URL, Status: http.StatusOK,
		Headers: http.Header{"Content-Type": {"application/pdf"}},
		Body:    http.NoBody,
	}
	if err := consume(resource); err != nil {
		return model.Resource{}, err
	}
	return resource, nil
}

type selectionAvailabilityFetcher struct {
	mu        sync.Mutex
	responses map[string]model.Resource
	errors    map[string]error
	calls     []string
	refreshes []bool
	err       error
}

func hpeAvailabilityFetcher(ids ...string) *selectionAvailabilityFetcher {
	responses := map[string]model.Resource{}
	for _, id := range ids {
		raw := "https://support.hpe.com/hpesc/public/api/document/" + id + "?ignorePayload=true"
		responses[raw] = model.Resource{
			URL: raw, Status: http.StatusOK,
			Headers: http.Header{
				"Content-Type": {"multiPage;charset=UTF-8"},
				"Doc-Id":       {id},
			},
			Body: io.NopCloser(strings.NewReader(
				`<html><body><main class="ditasrc"><h1>Guide</h1></main></body></html>`,
			)),
		}
	}
	return &selectionAvailabilityFetcher{responses: responses}
}

func (f *selectionAvailabilityFetcher) Get(_ context.Context, raw string, refresh bool) (model.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, raw)
	f.refreshes = append(f.refreshes, refresh)
	if f.err != nil {
		return model.Resource{}, f.err
	}
	if err := f.errors[raw]; err != nil {
		return model.Resource{}, err
	}
	resource, ok := f.responses[raw]
	if !ok {
		return model.Resource{}, errors.New("unexpected source request: " + raw)
	}
	body, err := io.ReadAll(resource.Body)
	if err != nil {
		return model.Resource{}, err
	}
	resource.Body = io.NopCloser(bytes.NewReader(body))
	f.responses[raw] = model.Resource{
		URL: resource.URL, Status: resource.Status, Headers: resource.Headers,
		Body: io.NopCloser(bytes.NewReader(body)),
	}
	return resource, nil
}

type scriptedPrompts struct {
	selects []string
	multi   []string
	input   string
	calls   []string
}

func (p *scriptedPrompts) Select(_ context.Context, title string, choices []selectionChoice, _ promptConfig) (string, error) {
	p.calls = append(p.calls, title)
	if strings.HasPrefix(title, "Generate PDF companions for ") &&
		(len(p.selects) == 0 || (p.selects[0] != "convert" && p.selects[0] != "html-only")) {
		return "html-only", nil
	}
	if len(p.selects) == 0 {
		return "", context.Canceled
	}
	value := p.selects[0]
	p.selects = p.selects[1:]
	for _, choice := range choices {
		if choice.Value == value && choice.Disabled == "" {
			return value, nil
		}
	}
	return "", context.Canceled
}

func (p *scriptedPrompts) MultiSelect(_ context.Context, title string, _ []selectionChoice, _ promptConfig) ([]string, error) {
	p.calls = append(p.calls, title)
	return append([]string{}, p.multi...), nil
}

func (p *scriptedPrompts) Input(_ context.Context, title string, _ promptConfig) (string, error) {
	p.calls = append(p.calls, title)
	return p.input, nil
}

type wizardReply struct {
	value  string
	values []string
	back   bool
	err    error
}

type wizardCall struct {
	title   string
	config  promptConfig
	choices []selectionChoice
}

type wizardPrompts struct {
	replies []wizardReply
	calls   []wizardCall
}

func (p *wizardPrompts) reply(title string, choices []selectionChoice, config promptConfig) (wizardReply, error) {
	p.calls = append(p.calls, wizardCall{
		title: title, config: config, choices: append([]selectionChoice{}, choices...),
	})
	if len(p.replies) == 0 {
		return wizardReply{}, context.Canceled
	}
	reply := p.replies[0]
	p.replies = p.replies[1:]
	if reply.err != nil {
		return reply, reply.err
	}
	if reply.back {
		return reply, errPromptBack
	}
	return reply, nil
}

func (p *wizardPrompts) Select(_ context.Context, title string, choices []selectionChoice, config promptConfig) (string, error) {
	if strings.HasPrefix(title, "Generate PDF companions for ") &&
		(len(p.replies) == 0 || (p.replies[0].value != "convert" && p.replies[0].value != "html-only")) {
		p.calls = append(p.calls, wizardCall{
			title: title, config: config, choices: append([]selectionChoice{}, choices...),
		})
		return "html-only", nil
	}
	reply, err := p.reply(title, choices, config)
	return reply.value, err
}

func (p *wizardPrompts) MultiSelect(_ context.Context, title string, choices []selectionChoice, config promptConfig) ([]string, error) {
	reply, err := p.reply(title, choices, config)
	return append([]string{}, reply.values...), err
}

func (p *wizardPrompts) Input(_ context.Context, title string, config promptConfig) (string, error) {
	reply, err := p.reply(title, nil, config)
	return reply.value, err
}

type progressOrderingPrompts struct {
	destination string
	order       *[]string
}

func (p *progressOrderingPrompts) Select(
	_ context.Context,
	title string,
	_ []selectionChoice,
	_ promptConfig,
) (string, error) {
	*p.order = append(*p.order, "prompt:"+title)
	if title == "Prefer a source-verified PDF when available?" {
		return "html", nil
	}
	if strings.HasPrefix(title, "Generate PDF companions for ") {
		return "html-only", nil
	}
	return "", fmt.Errorf("unexpected ordered Select prompt %q", title)
}

func (p *progressOrderingPrompts) MultiSelect(
	_ context.Context,
	title string,
	_ []selectionChoice,
	_ promptConfig,
) ([]string, error) {
	return nil, fmt.Errorf("unexpected ordered MultiSelect prompt %q", title)
}

func (p *progressOrderingPrompts) Input(
	_ context.Context,
	title string,
	_ promptConfig,
) (string, error) {
	*p.order = append(*p.order, "prompt:"+title)
	return p.destination, nil
}

func selectionCatalog() model.Catalog {
	return model.Catalog{
		Platforms: []string{"6300", "6400"},
		Versions:  []string{"10.18.xxxx", "10.17", "10.16"},
		Guides: []model.Guide{
			{
				ID: "first", Title: "First",
				Mappings: map[string]map[string]string{
					"10.18.xxxx": {"6300": "https://example.test/first.pdf"},
					"10.17":      {"6300": "https://example.test/first.pdf"},
				},
			},
			{
				ID: "second", Title: "Second",
				Mappings: map[string]map[string]string{
					"10.18.xxxx": {"6300": "https://example.test/second.pdf"},
				},
			},
		},
	}
}

func backCatalog() model.Catalog {
	return model.Catalog{
		Platforms: []string{"6300", "6400"},
		Versions:  []string{"10.18", "10.17", "10.16"},
		Guides: []model.Guide{
			{
				ID: "first", Title: "First",
				Mappings: map[string]map[string]string{
					"10.18": {"6300": "https://example.test/first.pdf"},
					"10.17": {"6300": "https://example.test/first.pdf"},
					"10.16": {"6400": "https://example.test/first.pdf"},
				},
			},
			{
				ID: "second", Title: "Second",
				Mappings: map[string]map[string]string{
					"10.18": {"6300": "https://example.test/second.pdf"},
				},
			},
			{
				ID: "third", Title: "Third",
				Mappings: map[string]map[string]string{
					"10.16": {"6400": "https://example.test/third.pdf"},
				},
			},
		},
	}
}

func TestReleaseChoicesDistinguishUnavailableAndUnknown(t *testing.T) {
	catalog := selectionCatalog()
	catalog.Guides = append(catalog.Guides, model.Guide{
		ID: "failed", Title: "Failed", Mappings: map[string]map[string]string{},
		Error: "mapping request failed",
	})
	choices, err := releaseChoices(catalog, "6300")
	if err != nil {
		t.Fatal(err)
	}
	if choices[0].Disabled != "" || !strings.Contains(choices[0].Label, "partial") {
		t.Fatalf("mapped partial release should remain selectable: %+v", choices[0])
	}
	if choices[2].Disabled == "" || !strings.Contains(choices[2].Disabled, "unknown") {
		t.Fatalf("failed mapping was presented as confirmed unavailable: %+v", choices[2])
	}

	catalog.Guides = catalog.Guides[:2]
	choices, err = releaseChoices(catalog, "6300")
	if err != nil {
		t.Fatal(err)
	}
	if choices[2].Disabled != "no mapped guides" {
		t.Fatalf("confirmed zero-mapping release was not disabled: %+v", choices[2])
	}
}

func TestAvailabilityLabelsAreExact(t *testing.T) {
	for _, tc := range []struct {
		document     model.Document
		source       model.SourceAvailability
		availability model.PDFAvailability
		want         string
	}{
		{model.Document{Kind: "pdf"}, model.SourceAvailability{}, model.PDFAvailability{}, "PDF native"},
		{model.Document{Kind: "flare"}, model.SourceAvailability{}, model.PDFAvailability{Checked: true}, "HTML (flare)"},
		{model.Document{Kind: "hpe"}, model.SourceAvailability{}, model.PDFAvailability{Checked: true, Available: true}, "HTML (hpe) / PDF native"},
		{model.Document{Kind: "static"}, model.SourceAvailability{}, model.PDFAvailability{Checked: true, Available: true}, "HTML (static) / PDF native"},
		{model.Document{Kind: "flare"}, model.SourceAvailability{Format: "HTML (flare)", Checked: true, Reason: "mapped source HTTP 404"}, model.PDFAvailability{}, "HTML (flare) - unavailable: mapped source HTTP 404"},
	} {
		if got := availabilityLabel(tc.document, tc.source, tc.availability); got != tc.want {
			t.Fatalf("availability label=%q want=%q", got, tc.want)
		}
	}
}

func TestWizardDisablesDefinitivelyUnavailableSourceButKeepsOtherGuidesSelectable(t *testing.T) {
	const (
		missing = "https://publisher.example/missing/Content/home.htm"
		working = "https://publisher.example/working/index.html"
	)
	catalog := model.Catalog{
		Platforms: []string{"6000"}, Versions: []string{"10.17", "10.17.1000"},
		Guides: []model.Guide{
			{ID: "webUI", Title: "Introduction to the WebUI Guide", Mappings: map[string]map[string]string{
				"10.17": {"6000": missing}, "10.17.1000": {"6000": working},
			}},
			{ID: "other", Title: "Other Guide", Mappings: map[string]map[string]string{
				"10.17": {"6000": working},
			}},
		},
	}
	fetcher := &selectionAvailabilityFetcher{
		errors: map[string]error{
			missing: &fetch.StatusError{URL: missing, Status: http.StatusNotFound},
		},
		responses: map[string]model.Resource{
			working: {
				URL: working, Status: http.StatusOK,
				Headers: http.Header{"Content-Type": {"text/html"}},
				Body:    io.NopCloser(strings.NewReader(`<html><body><main>Other</main></body></html>`)),
			},
		},
	}
	destination := filepath.Join(t.TempDir(), "library")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "some"}, {values: []string{"other"}}, {value: destination},
	}}
	o := options{platform: "6000", version: "10.17"}
	documents, err := completeInteractiveSelection(
		context.Background(), catalog, &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true}, true,
		availabilityBackend{fetcher: fetcher, transport: &selectionProbeTransport{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].ID != "other" {
		t.Fatalf("available guide was not independently selectable: %+v", documents)
	}
	mode := prompts.calls[0]
	if len(mode.choices) != 2 || mode.choices[0].Value != "all" ||
		mode.choices[0].Disabled != "" ||
		mode.choices[0].Label != "All available mapped guides (1)" ||
		mode.choices[0].Emphasis != "available" {
		t.Fatalf("available-subset mode is incorrect: %+v", mode.choices)
	}
	guides := prompts.calls[1]
	if len(guides.choices) != 2 ||
		!strings.Contains(guides.choices[0].Label, "HTML (flare) - unavailable: mapped source HTTP 404") ||
		guides.choices[0].Disabled != "mapped source HTTP 404" ||
		guides.choices[1].Disabled != "" {
		t.Fatalf("individual guide availability choices are incorrect: %+v", guides.choices)
	}
	if len(fetcher.calls) != 2 {
		t.Fatalf("availability scan repeated a mapped front: %v", fetcher.calls)
	}
}

func TestWizardAllAvailableIncludesUnknownAndSkipsDefinitiveUnavailable(t *testing.T) {
	documents := []model.Document{
		{ID: "missing", Title: "Missing", Kind: "flare", URL: "https://example.test/missing"},
		{ID: "available", Title: "Available", Kind: "static", URL: "https://example.test/available"},
		{ID: "unknown", Title: "Unknown", Kind: "hpe", URL: "https://example.test/unknown"},
	}
	statuses := map[string]model.SourceAvailability{
		documents[0].URL: {Checked: true, Available: false, Reason: "HTTP 404", ProbeURL: documents[0].URL},
		documents[1].URL: {Checked: true, Available: true, ProbeURL: documents[1].URL},
		documents[2].URL: {Checked: false, Available: false, Reason: "timeout", ProbeURL: documents[2].URL},
	}
	o := options{all: true}
	selected, err := applyAvailableSelection(&o, documents, statuses)
	if err != nil || len(selected) != 2 || selected[0].ID != "available" ||
		selected[1].ID != "unknown" || len(o.skippedUnavailable) != 1 ||
		o.skippedUnavailable[0].ID != "missing" || !o.allAvailableSnapshot {
		t.Fatalf("all-available selection=%+v options=%+v err=%v", selected, o, err)
	}
	choice := availableModeChoice(documents, statuses, "")
	if choice.Label != "All available mapped guides (2)" || choice.Emphasis != "available" ||
		choice.Disabled != "" {
		t.Fatalf("all-available choice=%+v", choice)
	}
	allMapped := availableModeChoice(documents[1:], map[string]model.SourceAvailability{
		documents[1].URL: statuses[documents[1].URL],
		documents[2].URL: statuses[documents[2].URL],
	}, "")
	if allMapped.Label != "All mapped guides (2)" || allMapped.Emphasis != "" {
		t.Fatalf("no-unavailable label changed: %+v", allMapped)
	}
}

func TestAllAvailableRejectsZeroAvailable(t *testing.T) {
	document := model.Document{ID: "missing", Title: "Missing", Kind: "pdf", URL: "https://example.test/missing.pdf"}
	_, err := applyAvailableSelection(&options{all: true}, []model.Document{document},
		map[string]model.SourceAvailability{document.URL: {
			Checked: true, Available: false, Reason: "HTTP 404",
		}})
	if err == nil || !strings.Contains(err.Error(), "no available mapped guides") {
		t.Fatalf("zero-available selection was accepted: %v", err)
	}
}

func TestUnknownSourceAvailabilityRemainsSelectable(t *testing.T) {
	document := model.Document{ID: "guide", Title: "Guide", Kind: "flare", URL: "https://publisher.example/guide"}
	status := model.SourceAvailability{
		URL: document.URL, Format: "HTML (flare)",
		Reason: "publisher returned HTTP 503",
	}
	choices := guideChoices(
		[]model.Document{document},
		map[string]model.SourceAvailability{document.URL: status},
		nil,
	)
	if len(choices) != 1 || choices[0].Disabled != "" ||
		!strings.Contains(choices[0].Label, "availability unknown: publisher returned HTTP 503") {
		t.Fatalf("transient source failure was disabled or hidden: %+v", choices)
	}
}

func TestInteractiveSelectionFillsOnlyMissingValuesAndPreservesOrder(t *testing.T) {
	catalog := selectionCatalog()
	prompts := &scriptedPrompts{selects: []string{"10.18.xxxx", "some"}, multi: []string{"second", "first"}}
	o := options{platform: "6300"}
	documents, err := selectDocuments(context.Background(), catalog, &o, prompts, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if o.platform != "6300" || o.version != "10.18.xxxx" ||
		strings.Join(o.guides, ",") != "second,first" ||
		len(documents) != 2 || documents[0].ID != "second" || documents[1].ID != "first" {
		t.Fatalf("partial interactive selection changed supplied values or order: options=%+v documents=%+v", o, documents)
	}
	if strings.Join(prompts.calls, "|") != "Documentation version|Documents to retrieve|Select guides (Space toggles; Enter confirms)" {
		t.Fatalf("unexpected prompts: %v", prompts.calls)
	}
}

func TestWizardBackToPlatformRebuildsReleaseAvailability(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "library")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "6300"},
		{value: "10.17", back: true},
		{value: "6400"},
		{value: "10.16"},
		{value: "all"},
		{value: destination},
	}}
	o := options{}
	documents, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if o.platform != "6400" || o.version != "10.16" || !o.all ||
		len(documents) != 2 || documents[0].ID != "first" || documents[1].ID != "third" {
		t.Fatalf("Back retained stale platform/release routes: options=%+v documents=%+v", o, documents)
	}
	if got := strings.Join(callTitles(prompts.calls), "|"); got !=
		"Switch platform|Documentation version|Switch platform|Documentation version|Documents to retrieve|Save documentation under which directory? ("+mustGetwd(t)+")" {
		t.Fatalf("unexpected Back flow: %s", got)
	}
	releases := prompts.calls[3].choices
	if releases[0].Disabled == "" || releases[1].Disabled == "" || releases[2].Disabled != "" {
		t.Fatalf("platform change did not rebuild release availability: %+v", releases)
	}
}

func TestWizardRestoresReleaseCursorWhenPlatformIsUnchanged(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "library")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "6300"},
		{value: "10.17", back: true},
		{value: "6300"},
		{value: "10.17"},
		{value: "all"},
		{value: destination},
	}}
	o := options{}
	if _, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{}, true,
	); err != nil {
		t.Fatal(err)
	}
	if prompts.calls[3].config.Initial != "10.17" {
		t.Fatalf("release cursor was not restored: %+v", prompts.calls[3].config)
	}
}

func TestWizardBackAcrossSomeFlowRestoresDraftsAndInvalidatesGuides(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "final")
	partial := filepath.Join(t.TempDir(), "partial path")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "6300"},
		{value: "10.18"},
		{value: "some"},
		{values: []string{"second"}},
		{value: partial, back: true},
		{values: []string{"second"}, back: true},
		{value: "some", back: true},
		{value: "10.17"},
		{value: "some"},
		{values: []string{"first"}},
		{value: destination},
	}}
	o := options{}
	documents, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if o.version != "10.17" || o.all || strings.Join(o.guides, ",") != "first" ||
		len(documents) != 1 || documents[0].ID != "first" {
		t.Fatalf("release change retained stale guides: options=%+v documents=%+v", o, documents)
	}
	secondGuides := prompts.calls[9]
	if len(secondGuides.config.Selected) != 0 {
		t.Fatalf("guide unavailable in the changed release was restored: %+v", secondGuides.config.Selected)
	}
	firstRestoredGuides := prompts.calls[5]
	if strings.Join(firstRestoredGuides.config.Selected, ",") != "second" {
		t.Fatalf("checked guides were not restored before the release changed: %+v", firstRestoredGuides.config.Selected)
	}
	if prompts.calls[6].config.Initial != "some" {
		t.Fatalf("mode cursor was not restored: %+v", prompts.calls[6].config)
	}
	secondDestination := prompts.calls[10]
	if secondDestination.config.Initial != partial {
		t.Fatalf("partially typed destination was not restored: %+v", secondDestination.config)
	}
}

func TestWizardBackFromAllDestinationAndExistingLibrary(t *testing.T) {
	base := t.TempDir()
	createCurrentSelectionLibrary(t, base, "6300", "10.18")
	final := filepath.Join(t.TempDir(), "different")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "all"},
		{value: base},
		{value: "resume", back: true},
		{value: base, back: true},
		{value: "all"},
		{value: final},
	}}
	o := options{platform: "6300", version: "10.18"}
	documents, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if o.destination != final || !o.all || o.refresh || len(documents) != 2 {
		t.Fatalf("existing-library Back did not re-evaluate the destination: options=%+v documents=%+v", o, documents)
	}
	if got := strings.Join(callTitles(prompts.calls), "|"); got !=
		"Documents to retrieve|Save documentation under which directory? ("+mustGetwd(t)+")|An existing library was found|Save documentation under which directory? ("+mustGetwd(t)+")|Documents to retrieve|Save documentation under which directory? ("+mustGetwd(t)+")" {
		t.Fatalf("unexpected existing-library Back flow: %s", got)
	}
}

func TestWizardFixedFlagsAreNotEditableAndCancellationCreatesNothing(t *testing.T) {
	base := filepath.Join(t.TempDir(), "not-created")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "some"},
		{values: []string{"first"}},
		{value: base, err: context.Canceled},
	}}
	o := options{platform: "6300", version: "10.17"}
	_, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true}, true,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prompt cancellation was not preserved: %v", err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("selection created output before final confirmation: %v", err)
	}
	if got := strings.Join(callTitles(prompts.calls), "|"); got !=
		"Documents to retrieve|Select guides (Space toggles; Enter confirms)|Save documentation under which directory? ("+mustGetwd(t)+")" {
		t.Fatalf("fixed flags became editable: %s", got)
	}
	if prompts.calls[0].config.AllowBack {
		t.Fatal("first editable question misleadingly offered Back")
	}
}

func TestWizardPreservesFixedGuidesWhileCompletingPlatformAndVersion(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "library")
	originalGuides := []string{"first"}
	o := options{guides: originalGuides, destination: destination}
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "6300"},
		{value: "10.17"},
	}}
	documents, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{Guides: true, Destination: true}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(o.guides, ",") != "first" || strings.Join(originalGuides, ",") != "first" ||
		len(documents) != 1 || documents[0].ID != "first" {
		t.Fatalf("wizard changed fixed guide IDs: options=%+v original=%v documents=%+v", o, originalGuides, documents)
	}
	if got := strings.Join(callTitles(prompts.calls), "|"); got != "Switch platform|Documentation version" {
		t.Fatalf("fixed guide/destination unexpectedly became editable: %s", got)
	}
}

func TestWizardRejectsEveryUnresolvedFixedGuideWithoutMutatingFlags(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "must-not-exist")
	originalGuides := []string{"first", "missing"}
	o := options{version: "10.16", guides: originalGuides, destination: destination}
	prompts := &wizardPrompts{replies: []wizardReply{{value: "6400"}}}
	_, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{Version: true, Guides: true, Destination: true}, true,
	)
	if err == nil || !strings.Contains(err.Error(), `guide "missing" is not resolved`) {
		t.Fatalf("unresolved fixed guide was silently dropped: %v", err)
	}
	if strings.Join(o.guides, ",") != "first,missing" ||
		strings.Join(originalGuides, ",") != "first,missing" {
		t.Fatalf("fixed guide slice was mutated: options=%v original=%v", o.guides, originalGuides)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("invalid fixed guide selection created output: %v", err)
	}
}

func TestWizardBackFromExistingSkipsFixedDestinationAndRefreshFlag(t *testing.T) {
	base := t.TempDir()
	createCurrentSelectionLibrary(t, base, "6300", "10.18")

	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "all"},
		{value: "resume", back: true},
		{value: "all"},
		{value: "refresh"},
	}}
	o := options{platform: "6300", version: "10.18", destination: base}
	documents, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true, Destination: true}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !o.all || !o.refresh || len(documents) != 2 {
		t.Fatalf("Back across fixed destination lost the final choice: options=%+v documents=%+v", o, documents)
	}
	if got := strings.Join(callTitles(prompts.calls), "|"); got !=
		"Documents to retrieve|An existing library was found|Documents to retrieve|An existing library was found" {
		t.Fatalf("Back visited a fixed destination: %s", got)
	}

	prompts = &wizardPrompts{replies: []wizardReply{{value: "all"}}}
	o = options{platform: "6300", version: "10.18", destination: base, refresh: true}
	if _, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true, Destination: true, Refresh: true}, true,
	); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(callTitles(prompts.calls), "|"); got != "Documents to retrieve" {
		t.Fatalf("explicit --refresh was made editable: %s", got)
	}
}

func TestWizardPreflightsHPELabelsAndPreservesPreferenceBack(t *testing.T) {
	const public = "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-A"
	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18"},
		Guides: []model.Guide{{
			ID: "hpe", Title: "HPE Guide",
			Mappings: map[string]map[string]string{"10.18": {"6300": public}},
		}},
	}
	firstDestination := filepath.Join(t.TempDir(), "first")
	finalDestination := filepath.Join(t.TempDir(), "final")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "some"},
		{values: []string{"hpe"}},
		{value: firstDestination},
		{value: "html", back: true},
		{value: finalDestination},
		{value: "pdf"},
	}}
	probe := &selectionProbeTransport{}
	fetcher := hpeAvailabilityFetcher("doc-A")
	o := options{platform: "6300", version: "10.18"}
	documents, err := completeInteractiveSelection(
		context.Background(), catalog, &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true}, true,
		availabilityBackend{fetcher: fetcher, transport: probe},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || !o.preferPDF || o.destination != finalDestination {
		t.Fatalf("guided preference result incorrect: options=%+v documents=%+v", o, documents)
	}
	if len(probe.requests) != 1 || probe.requests[0].Method != http.MethodHead ||
		probe.requests[0].URL != "https://support.hpe.com/hpesc/public/api/document/doc-A/exportpdf?exportType=all" {
		t.Fatalf("guide label preflight was not one exact HEAD: %+v", probe.requests)
	}
	guideCall := prompts.calls[1]
	if len(guideCall.choices) != 1 || !strings.Contains(guideCall.choices[0].Label, "[HTML (hpe) / PDF native]") {
		t.Fatalf("successful preflight did not label the guide accurately: %+v", guideCall.choices)
	}
	preferenceCalls := 0
	for _, call := range prompts.calls {
		if call.title == "Prefer a source-verified PDF when available?" {
			preferenceCalls++
		}
	}
	if preferenceCalls != 2 || prompts.calls[4].config.Initial != firstDestination {
		t.Fatalf("preference Back did not restore the destination draft: %+v", prompts.calls)
	}
}

func TestWizardFlareAvailabilityControlsLabelAndPreferenceQuestion(t *testing.T) {
	const (
		home = "https://publisher.example/guide/Content/home.htm"
		pdf  = "https://publisher.example/guide/PDF/original.pdf"
	)
	catalog := model.Catalog{
		Platforms: []string{"8360"}, Versions: []string{"10.16"},
		Guides: []model.Guide{{
			ID: "ha", Title: "High Availability",
			Mappings: map[string]map[string]string{"10.16": {"8360": home}},
		}},
	}
	run := func(t *testing.T, advertisement string, replies []wizardReply) (options, *wizardPrompts, *selectionAvailabilityFetcher, *selectionProbeTransport) {
		t.Helper()
		fetcher := &selectionAvailabilityFetcher{responses: map[string]model.Resource{
			home: {
				URL: home, Status: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html"}},
				Body: io.NopCloser(strings.NewReader(`<html><head>` + advertisement +
					`</head><body><main id="mc-main-content"><h1>HA</h1></main></body></html>`)),
			},
		}}
		transport := &selectionProbeTransport{}
		prompts := &wizardPrompts{replies: replies}
		o := options{platform: "8360", version: "10.16"}
		if _, err := completeInteractiveSelection(
			context.Background(), catalog, &o, prompts, func(string) {},
			selectionFixed{Platform: true, Version: true}, true,
			availabilityBackend{fetcher: fetcher, transport: transport},
		); err != nil {
			t.Fatal(err)
		}
		return o, prompts, fetcher, transport
	}

	t.Run("unique-native-pdf", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "library")
		o, prompts, fetcher, transport := run(t,
			`<link rel="alternate" type="application/pdf" href="../PDF/original.pdf">`,
			[]wizardReply{
				{value: "some"}, {values: []string{"ha"}}, {value: destination}, {value: "pdf"},
			},
		)
		if !o.preferPDF || o.destination != destination {
			t.Fatalf("verified Flare preference was not retained: %+v", o)
		}
		if len(fetcher.calls) != 1 || fetcher.calls[0] != home || !fetcher.refreshes[0] ||
			len(transport.requests) != 1 || transport.requests[0].Method != http.MethodHead ||
			transport.requests[0].URL != pdf {
			t.Fatalf("Flare scan was not one fresh front plus one HEAD: front=%v refresh=%v head=%v",
				fetcher.calls, fetcher.refreshes, transport.requests)
		}
		if label := prompts.calls[1].choices[0].Label; label != "High Availability [HTML (flare) / PDF native]" {
			t.Fatalf("positive Flare label is not explicit: %q", label)
		}
		if !slices.Contains(callTitles(prompts.calls), "Prefer a source-verified PDF when available?") {
			t.Fatalf("verified optional PDF did not offer preference: %v", callTitles(prompts.calls))
		}
		if slices.ContainsFunc(callTitles(prompts.calls), func(title string) bool {
			return strings.HasPrefix(title, "Generate PDF companions for ")
		}) || o.convertHTMLToPDF {
			t.Fatalf("all-publisher-PDF selection asked for HTML conversion: options=%+v calls=%v",
				o, callTitles(prompts.calls))
		}
	})

	t.Run("no-advertisement", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "library")
		o, prompts, fetcher, transport := run(t, "", []wizardReply{
			{value: "some"}, {values: []string{"ha"}}, {value: destination},
		})
		if o.preferPDF {
			t.Fatalf("unavailable Flare PDF left preference enabled: %+v", o)
		}
		if label := prompts.calls[1].choices[0].Label; label != "High Availability [HTML (flare)]" {
			t.Fatalf("unavailable Flare label changed: %q", label)
		}
		if slices.Contains(callTitles(prompts.calls), "Prefer a source-verified PDF when available?") ||
			len(fetcher.calls) != 1 || len(transport.requests) != 0 {
			t.Fatalf("unavailable Flare PDF was offered or body-probed: calls=%v front=%v head=%v",
				callTitles(prompts.calls), fetcher.calls, transport.requests)
		}
	})

	t.Run("explicit-preference-remains-fixed", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "library")
		fetcher := &selectionAvailabilityFetcher{responses: map[string]model.Resource{
			home: {
				URL: home, Status: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html"}},
				Body: io.NopCloser(strings.NewReader(
					`<html><body><main id="mc-main-content"><h1>HA</h1></main></body></html>`,
				)),
			},
		}}
		transport := &selectionProbeTransport{}
		prompts := &wizardPrompts{replies: []wizardReply{
			{value: "some"}, {values: []string{"ha"}}, {value: destination},
		}}
		o := options{platform: "8360", version: "10.16", preferPDF: true}
		if _, err := completeInteractiveSelection(
			context.Background(), catalog, &o, prompts, func(string) {},
			selectionFixed{Platform: true, Version: true, PreferPDF: true}, true,
			availabilityBackend{fetcher: fetcher, transport: transport},
		); err != nil {
			t.Fatal(err)
		}
		if !o.preferPDF || len(fetcher.calls) != 1 || len(transport.requests) != 0 ||
			slices.Contains(callTitles(prompts.calls), "Prefer a source-verified PDF when available?") ||
			prompts.calls[1].choices[0].Label != "High Availability [HTML (flare)]" {
			t.Fatalf("explicit preference was not fixed while labels were checked: options=%+v calls=%v front=%v head=%v",
				o, callTitles(prompts.calls), fetcher.calls, transport.requests)
		}
	})
}

func TestSelectedOutputSummaryClassifiesPublisherPDFHTMLAndFallbacks(t *testing.T) {
	documents := []model.Document{
		{ID: "direct", Kind: "pdf", URL: "https://example.test/direct.pdf"},
		{ID: "verified", Kind: "flare", URL: "https://example.test/verified.htm"},
		{ID: "html", Kind: "hpe", URL: "https://example.test/html"},
		{ID: "unknown", Kind: "static", URL: "https://example.test/unknown.htm"},
	}
	availability := map[string]model.PDFAvailability{
		documents[1].URL: {Checked: true, Available: true},
		documents[2].URL: {Checked: true, Available: false},
	}
	preferred := classifySelectedOutputs(documents, availability, true)
	if preferred != (selectedOutputSummary{
		DirectPublisherPDF: 1, VerifiedOptionalPublisherPDF: 1,
		KnownHTML: 1, PossibleHTMLFallback: 1,
	}) {
		t.Fatalf("preferred output summary=%+v", preferred)
	}
	contextLine, prompt := preferred.conversionPrompt()
	if contextLine != "2 selected guides will use publisher PDFs; 1 will use complete HTML; 1 additional guide has unknown publisher-PDF availability and may fall back to HTML." ||
		prompt != "Generate PDF companions for up to 2 HTML guides?" {
		t.Fatalf("fallback prompt context=%q prompt=%q", contextLine, prompt)
	}
	htmlPreferred := classifySelectedOutputs(documents, availability, false)
	if htmlPreferred.DirectPublisherPDF != 1 || htmlPreferred.VerifiedOptionalPublisherPDF != 0 ||
		htmlPreferred.KnownHTML != 3 || htmlPreferred.PossibleHTMLFallback != 0 {
		t.Fatalf("HTML-preferred output summary=%+v", htmlPreferred)
	}
	contextLine, prompt = htmlPreferred.conversionPrompt()
	if contextLine != "1 selected guide will use publisher PDFs; 3 will use complete HTML." ||
		prompt != "Generate PDF companions for the 3 HTML guides?" {
		t.Fatalf("known HTML prompt context=%q prompt=%q", contextLine, prompt)
	}
}

func TestDirectPublisherPDFOnlyNeedsNoPreferenceOrConversion(t *testing.T) {
	summary := classifySelectedOutputs([]model.Document{{
		ID: "direct", Kind: "pdf", URL: "https://example.test/direct.pdf",
	}}, nil, false)
	if summary.publisherPDFs() != 1 || summary.possibleHTML() != 0 {
		t.Fatalf("direct PDF summary=%+v", summary)
	}
}

func TestWizardHPEUnavailableLabelSkipsPreference(t *testing.T) {
	const public = "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-unavailable"
	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18"},
		Guides: []model.Guide{{
			ID: "hpe", Title: "HPE Guide",
			Mappings: map[string]map[string]string{"10.18": {"6300": public}},
		}},
	}
	destination := filepath.Join(t.TempDir(), "library")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "some"}, {values: []string{"hpe"}}, {value: destination}, {value: "convert"},
	}}
	export := "https://support.hpe.com/hpesc/public/api/document/doc-unavailable/exportpdf?exportType=all"
	transport := &selectionProbeTransport{err: &fetch.StatusError{URL: export, Status: http.StatusNotFound}}
	fetcher := hpeAvailabilityFetcher("doc-unavailable")
	o := options{platform: "6300", version: "10.18"}
	if _, err := completeInteractiveSelection(
		context.Background(), catalog, &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true}, true,
		availabilityBackend{fetcher: fetcher, transport: transport},
	); err != nil {
		t.Fatal(err)
	}
	if o.preferPDF || !o.convertHTMLToPDF || len(transport.requests) != 1 ||
		prompts.calls[1].choices[0].Label != "HPE Guide [HTML (hpe)]" ||
		slices.Contains(callTitles(prompts.calls), "Prefer a source-verified PDF when available?") ||
		!slices.ContainsFunc(callTitles(prompts.calls), func(title string) bool {
			return strings.HasPrefix(title, "Generate PDF companions for ")
		}) {
		t.Fatalf("HPE 404 was offered as PDF: options=%+v calls=%v label=%q requests=%v",
			o, callTitles(prompts.calls), prompts.calls[1].choices[0].Label, transport.requests)
	}
	for _, call := range prompts.calls {
		if strings.HasPrefix(call.title, "Generate PDF companions for ") {
			if call.title != "Generate PDF companions for the 1 HTML guide?" ||
				call.config.Context != "1 selected guide will use complete HTML." {
				t.Fatalf("known HTML conversion context=%+v", call)
			}
		}
	}
}

func TestWizardConversionBackRestoresDestination(t *testing.T) {
	const public = "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-convert"
	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18"},
		Guides: []model.Guide{{
			ID: "hpe", Title: "HPE Guide",
			Mappings: map[string]map[string]string{"10.18": {"6300": public}},
		}},
	}
	firstDestination := filepath.Join(t.TempDir(), "first")
	finalDestination := filepath.Join(t.TempDir(), "final")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "some"}, {values: []string{"hpe"}}, {value: firstDestination},
		{value: "html-only", back: true}, {value: finalDestination}, {value: "convert"},
	}}
	export := "https://support.hpe.com/hpesc/public/api/document/doc-convert/exportpdf?exportType=all"
	transport := &selectionProbeTransport{err: &fetch.StatusError{URL: export, Status: http.StatusNotFound}}
	o := options{platform: "6300", version: "10.18"}
	if _, err := completeInteractiveSelection(
		context.Background(), catalog, &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true}, true,
		availabilityBackend{fetcher: hpeAvailabilityFetcher("doc-convert"), transport: transport},
	); err != nil {
		t.Fatal(err)
	}
	conversionCalls := 0
	for _, call := range prompts.calls {
		if strings.HasPrefix(call.title, "Generate PDF companions for ") {
			conversionCalls++
		}
	}
	if conversionCalls != 2 || !o.convertHTMLToPDF || o.destination != finalDestination ||
		prompts.calls[4].config.Initial != firstDestination {
		t.Fatalf("conversion Back did not restore destination: options=%+v calls=%+v", o, prompts.calls)
	}
}

func TestWizardBackPreferenceDynamicallyAddsConversionStep(t *testing.T) {
	const public = "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-toggle"
	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18"},
		Guides: []model.Guide{{
			ID: "hpe", Title: "HPE Guide",
			Mappings: map[string]map[string]string{"10.18": {"6300": public}},
		}},
	}
	base := t.TempDir()
	createCurrentSelectionLibrary(t, base, "6300", "10.18")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "some"}, {values: []string{"hpe"}}, {value: base},
		{value: "pdf"}, {value: "resume", back: true},
		{value: "html"}, {value: "convert"}, {value: "resume"},
	}}
	o := options{platform: "6300", version: "10.18"}
	if _, err := completeInteractiveSelection(
		context.Background(), catalog, &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true}, true,
		availabilityBackend{
			fetcher:   hpeAvailabilityFetcher("doc-toggle"),
			transport: &selectionProbeTransport{},
		},
	); err != nil {
		t.Fatal(err)
	}
	titles := callTitles(prompts.calls)
	conversions := 0
	for _, title := range titles {
		if strings.HasPrefix(title, "Generate PDF companions for ") {
			conversions++
		}
	}
	if o.preferPDF || !o.convertHTMLToPDF || conversions != 1 ||
		strings.Join(titles, "|") !=
			"Documents to retrieve|Select guides (Space toggles; Enter confirms)|Save documentation under which directory? ("+
				mustGetwd(t)+")|Prefer a source-verified PDF when available?|An existing library was found|"+
				"Prefer a source-verified PDF when available?|Generate PDF companions for the 1 HTML guide?|An existing library was found" {
		t.Fatalf("dynamic preference/conversion flow options=%+v calls=%v", o, titles)
	}
}

func TestWizardAllFixedGuidesAndExplicitPreferenceTraffic(t *testing.T) {
	const public = "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-all"
	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18"},
		Guides: []model.Guide{
			{ID: "hpe", Title: "HPE", Mappings: map[string]map[string]string{"10.18": {"6300": public}}},
			{ID: "direct", Title: "Direct", Mappings: map[string]map[string]string{
				"10.18": {"6300": "https://publisher.example/direct.pdf"},
			}},
		},
	}
	run := func(t *testing.T, o options, fixed selectionFixed, replies []wizardReply) (options, *wizardPrompts, *selectionProbeTransport) {
		t.Helper()
		prompts := &wizardPrompts{replies: replies}
		transport := &selectionProbeTransport{}
		if _, err := completeInteractiveSelection(
			context.Background(), catalog, &o, prompts, func(string) {}, fixed, true,
			availabilityBackend{fetcher: hpeAvailabilityFetcher("doc-all"), transport: transport},
		); err != nil {
			t.Fatal(err)
		}
		return o, prompts, transport
	}

	t.Run("all", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "all")
		o, prompts, transport := run(t,
			options{platform: "6300", version: "10.18", all: true},
			selectionFixed{Platform: true, Version: true, All: true},
			[]wizardReply{{value: destination}, {value: "pdf"}},
		)
		if !o.preferPDF || len(transport.requests) != 2 ||
			!slices.Contains(callTitles(prompts.calls), "Prefer a source-verified PDF when available?") ||
			slices.ContainsFunc(callTitles(prompts.calls), func(title string) bool {
				return strings.HasPrefix(title, "Generate PDF companions for ")
			}) {
			t.Fatalf("--all did not preflight before preference: options=%+v calls=%v requests=%v",
				o, callTitles(prompts.calls), transport.requests)
		}
	})

	t.Run("fixed-guides", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "fixed")
		o, prompts, transport := run(t,
			options{platform: "6300", version: "10.18", guides: []string{"hpe"}},
			selectionFixed{Platform: true, Version: true, Guides: true},
			[]wizardReply{{value: destination}, {value: "pdf"}},
		)
		if !o.preferPDF || len(transport.requests) != 1 ||
			!slices.Contains(callTitles(prompts.calls), "Prefer a source-verified PDF when available?") ||
			slices.ContainsFunc(callTitles(prompts.calls), func(title string) bool {
				return strings.HasPrefix(title, "Generate PDF companions for ")
			}) {
			t.Fatalf("fixed guides did not preflight before preference: options=%+v calls=%v requests=%v",
				o, callTitles(prompts.calls), transport.requests)
		}
	})

	for _, tc := range []struct {
		name      string
		preferPDF bool
	}{
		{"explicit-no-preference", false},
		{"explicit-preference", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), tc.name)
			o, prompts, transport := run(t,
				options{
					platform: "6300", version: "10.18", guides: []string{"hpe"},
					preferPDF: tc.preferPDF,
				},
				selectionFixed{Platform: true, Version: true, Guides: true, PreferPDF: true},
				[]wizardReply{{value: destination}},
			)
			wantRequests := 0
			if tc.preferPDF {
				wantRequests = 1
			}
			if o.preferPDF != tc.preferPDF || len(transport.requests) != wantRequests ||
				slices.Contains(callTitles(prompts.calls), "Prefer a source-verified PDF when available?") {
				t.Fatalf("explicit preference was not immutable or correctly preflighted: options=%+v calls=%v requests=%v",
					o, callTitles(prompts.calls), transport.requests)
			}
			hasConversion := slices.ContainsFunc(callTitles(prompts.calls), func(title string) bool {
				return strings.HasPrefix(title, "Generate PDF companions for ")
			})
			if hasConversion == tc.preferPDF {
				t.Fatalf("fixed preference conversion relevance wrong: prefer=%v calls=%v",
					tc.preferPDF, callTitles(prompts.calls))
			}
		})
	}

	t.Run("direct-only", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "direct")
		o, prompts, transport := run(t,
			options{platform: "6300", version: "10.18", guides: []string{"direct"}},
			selectionFixed{Platform: true, Version: true, Guides: true},
			[]wizardReply{{value: destination}},
		)
		if o.preferPDF || len(transport.requests) != 1 ||
			slices.Contains(callTitles(prompts.calls), "Prefer a source-verified PDF when available?") ||
			slices.ContainsFunc(callTitles(prompts.calls), func(title string) bool {
				return strings.HasPrefix(title, "Generate PDF companions for ")
			}) {
			t.Fatalf("direct-only selection did not limit traffic to its source HEAD: options=%+v calls=%v requests=%v",
				o, callTitles(prompts.calls), transport.requests)
		}
	})
}

type boundedSelectionTransport struct {
	active atomic.Int32
	max    atomic.Int32
	err    error
}

func (t *boundedSelectionTransport) Download(_ context.Context, request model.Request, consume func(model.Resource) error) (model.Resource, error) {
	active := t.active.Add(1)
	for {
		maximum := t.max.Load()
		if active <= maximum || t.max.CompareAndSwap(maximum, active) {
			break
		}
	}
	time.Sleep(15 * time.Millisecond)
	t.active.Add(-1)
	if t.err != nil {
		return model.Resource{}, fmt.Errorf("probe failed for %s: %w", request.URL, t.err)
	}
	resource := model.Resource{
		URL: request.URL, Status: http.StatusOK,
		Headers: http.Header{"Content-Type": {"application/pdf"}}, Body: http.NoBody,
	}
	if err := consume(resource); err != nil {
		return model.Resource{}, err
	}
	return resource, nil
}

func TestAvailabilityScanIsBoundedAndCancellationStopsWizard(t *testing.T) {
	documents := make([]model.Document, 0, 5)
	for index := range 5 {
		id := fmt.Sprintf("doc-%d", index)
		documents = append(documents, model.Document{
			ID: id, Title: id, Kind: "pdf",
			URL: "https://publisher.example/" + id + ".pdf",
		})
	}
	transport := &boundedSelectionTransport{}
	var progress []availabilityProgress
	wizard := &selectionWizard{
		state: options{workers: 2}, transport: transport,
		availability: map[string]model.PDFAvailability{},
		observe:      func(event availabilityProgress) { progress = append(progress, event) },
	}
	if err := wizard.scanPDFs(context.Background(), documents); err != nil {
		t.Fatal(err)
	}
	if maximum := transport.max.Load(); maximum != 2 || len(wizard.sourceStatus) != len(documents) {
		t.Fatalf("availability concurrency/order changed: max=%d availability=%v", maximum, wizard.sourceStatus)
	}
	if len(progress) != len(documents)+1 || progress[0] != (availabilityProgress{Total: len(documents)}) {
		t.Fatalf("availability progress start/count changed: %v", progress)
	}
	for index, event := range progress[1:] {
		if event.Completed != index+1 || event.Total != len(documents) {
			t.Fatalf("availability completion was not monotonic/exactly once: %v", progress)
		}
	}
	if err := wizard.scanPDFs(context.Background(), documents); err != nil {
		t.Fatal(err)
	}
	if len(progress) != len(documents)+1 {
		t.Fatalf("cached same-context scan replayed progress: %v", progress)
	}
	var messages []string
	var failureProgress []availabilityProgress
	failing := &selectionWizard{
		state: options{workers: 3}, transport: &boundedSelectionTransport{err: errors.New("publisher failure")},
		availability: map[string]model.PDFAvailability{},
		message:      func(message string) { messages = append(messages, message) },
		observe:      func(event availabilityProgress) { failureProgress = append(failureProgress, event) },
	}
	if err := failing.scanPDFs(context.Background(), documents[:3]); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 ||
		!strings.Contains(messages[0], "doc-0") ||
		!strings.Contains(messages[1], "doc-1") ||
		!strings.Contains(messages[2], "doc-2") {
		t.Fatalf("concurrent availability diagnostics changed document order: %v", messages)
	}
	if len(failureProgress) != 4 || failureProgress[3] != (availabilityProgress{Completed: 3, Total: 3}) {
		t.Fatalf("diagnostic outcomes were not counted as completed work: %v", failureProgress)
	}
	var timeoutMessages []string
	timedOut := &selectionWizard{
		state: options{workers: 1}, transport: &boundedSelectionTransport{err: context.DeadlineExceeded},
		availability: map[string]model.PDFAvailability{},
		message:      func(message string) { timeoutMessages = append(timeoutMessages, message) },
	}
	if err := timedOut.scanPDFs(context.Background(), documents[:1]); err != nil {
		t.Fatalf("per-request timeout canceled the wizard: %v", err)
	}
	if len(timeoutMessages) != 1 ||
		!strings.Contains(timeoutMessages[0], context.DeadlineExceeded.Error()) {
		t.Fatalf("per-request timeout was not retained as a non-offer diagnostic: %v", timeoutMessages)
	}

	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18"},
		Guides: []model.Guide{{
			ID: "cancel", Title: "Cancel",
			Mappings: map[string]map[string]string{
				"10.18": {"6300": "https://publisher.example/cancel.pdf"},
			},
		}},
	}
	o := options{platform: "6300", version: "10.18", guides: []string{"cancel"}}
	cancelContext, cancel := context.WithCancel(context.Background())
	cancel()
	var cancellationProgress []availabilityProgress
	err := func() error {
		_, err := completeInteractiveSelection(
			cancelContext, catalog, &o, &wizardPrompts{}, func(string) {},
			selectionFixed{Platform: true, Version: true, Guides: true}, true,
			availabilityBackend{
				transport: &selectionProbeTransport{err: context.Canceled},
				observe:   func(event availabilityProgress) { cancellationProgress = append(cancellationProgress, event) },
			},
		)
		return err
	}()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("availability cancellation did not stop the wizard: %v", err)
	}
	if len(cancellationProgress) != 1 || cancellationProgress[0] != (availabilityProgress{Total: 1}) {
		t.Fatalf("availability cancellation claimed completed work: %v", cancellationProgress)
	}
}

func TestAvailabilityLiveOutputClearsBeforePrompt(t *testing.T) {
	const public = "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-progress"
	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18"},
		Guides: []model.Guide{{
			ID: "hpe", Title: "HPE",
			Mappings: map[string]map[string]string{"10.18": {"6300": public}},
		}},
	}
	var order []string
	prompts := &progressOrderingPrompts{
		destination: filepath.Join(t.TempDir(), "library"),
		order:       &order,
	}
	o := options{platform: "6300", version: "10.18", guides: []string{"hpe"}}
	if _, err := completeInteractiveSelection(
		context.Background(), catalog, &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true, Guides: true}, true,
		availabilityBackend{
			fetcher:   hpeAvailabilityFetcher("doc-progress"),
			transport: &selectionProbeTransport{},
			observe: func(event availabilityProgress) {
				order = append(order, fmt.Sprintf("progress:%d/%d", event.Completed, event.Total))
			},
			clearLive: func() { order = append(order, "clear") },
		},
	); err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{
		"progress:0/1", "progress:1/1", "clear",
		"prompt:Save documentation under which directory? (" + mustGetwd(t) + ")",
	}
	if len(order) < len(wantPrefix) || fmt.Sprint(order[:len(wantPrefix)]) != fmt.Sprint(wantPrefix) {
		t.Fatalf("prompt began before availability progress cleanup: %v", order)
	}
}

func TestWizardBackAcrossContextInvalidatesAvailability(t *testing.T) {
	catalog := model.Catalog{
		Platforms: []string{"6300", "6400"}, Versions: []string{"10.18"},
		Guides: []model.Guide{{
			ID: "hpe", Title: "HPE",
			Mappings: map[string]map[string]string{"10.18": {
				"6300": "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-a",
				"6400": "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-b",
			}},
		}},
	}
	firstDestination := filepath.Join(t.TempDir(), "first")
	finalDestination := filepath.Join(t.TempDir(), "final")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "6300"}, {value: "10.18"}, {value: "some"}, {values: []string{"hpe"}},
		{value: firstDestination}, {value: "html", back: true},
		{value: firstDestination, back: true}, {values: []string{"hpe"}, back: true},
		{value: "some", back: true}, {value: "10.18", back: true},
		{value: "6400"}, {value: "10.18"}, {value: "some"}, {values: []string{"hpe"}},
		{value: finalDestination}, {value: "pdf"},
	}}
	transport := &selectionProbeTransport{}
	var progress []availabilityProgress
	o := options{}
	if _, err := completeInteractiveSelection(
		context.Background(), catalog, &o, prompts, func(string) {}, selectionFixed{}, true,
		availabilityBackend{
			fetcher:   hpeAvailabilityFetcher("doc-a", "doc-b"),
			transport: transport,
			observe:   func(event availabilityProgress) { progress = append(progress, event) },
		},
	); err != nil {
		t.Fatal(err)
	}
	if o.platform != "6400" || !o.preferPDF || o.destination != finalDestination ||
		len(transport.requests) != 2 ||
		!strings.Contains(transport.requests[0].URL, "/doc-a/") ||
		!strings.Contains(transport.requests[1].URL, "/doc-b/") {
		t.Fatalf("context Back reused stale availability: options=%+v requests=%v", o, transport.requests)
	}
	wantProgress := []availabilityProgress{
		{Total: 1}, {Completed: 1, Total: 1},
		{Total: 1}, {Completed: 1, Total: 1},
	}
	if fmt.Sprint(progress) != fmt.Sprint(wantProgress) {
		t.Fatalf("context Back did not rerun exact availability progress: %v", progress)
	}
}

func TestFixedNoPreferenceSkipsWizardQuestion(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "library")
	prompts := &wizardPrompts{replies: []wizardReply{{value: "all"}}}
	o := options{platform: "6300", version: "10.18", destination: destination}
	if _, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true, Destination: true, PreferPDF: true}, true,
		availabilityBackend{transport: &selectionProbeTransport{}},
	); err != nil {
		t.Fatal(err)
	}
	if o.preferPDF || strings.Contains(strings.Join(callTitles(prompts.calls), "|"), "Prefer a source-verified PDF") {
		t.Fatalf("fixed --no-prefer-pdf became editable: options=%+v calls=%v", o, callTitles(prompts.calls))
	}
}

func TestFixedConversionFlagSkipsWizardQuestion(t *testing.T) {
	const public = "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-fixed"
	catalog := model.Catalog{
		Platforms: []string{"6300"}, Versions: []string{"10.18"},
		Guides: []model.Guide{{
			ID: "hpe", Title: "HPE Guide",
			Mappings: map[string]map[string]string{"10.18": {"6300": public}},
		}},
	}
	o := options{
		platform: "6300", version: "10.18", guides: []string{"hpe"},
		destination: filepath.Join(t.TempDir(), "library"), convertHTMLToPDF: true,
	}
	prompts := &wizardPrompts{}
	documents, err := completeInteractiveSelection(
		context.Background(), catalog, &o, prompts, func(string) {},
		selectionFixed{
			Platform: true, Version: true, Guides: true, Destination: true,
			PreferPDF: true, ConvertHTMLToPDF: true,
		}, true,
		availabilityBackend{fetcher: hpeAvailabilityFetcher("doc-fixed"), transport: &selectionProbeTransport{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || !o.convertHTMLToPDF ||
		slices.ContainsFunc(callTitles(prompts.calls), func(title string) bool {
			return strings.HasPrefix(title, "Generate PDF companions for ")
		}) {
		t.Fatalf("fixed conversion flag became editable: options=%+v calls=%v", o, callTitles(prompts.calls))
	}
}

func callTitles(calls []wizardCall) []string {
	titles := make([]string, len(calls))
	for index, call := range calls {
		titles[index] = call.title
	}
	return titles
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestAllFailsClosedButExplicitKnownSubsetSurvivesMappingFailure(t *testing.T) {
	catalog := selectionCatalog()
	catalog.Guides = append(catalog.Guides, model.Guide{
		ID: "failed", Title: "Failed", Mappings: map[string]map[string]string{},
		Error: "mapping request failed",
	})
	all := options{platform: "6300", version: "10.18.xxxx", all: true}
	if _, err := selectedDocuments(catalog, all); err == nil || !strings.Contains(err.Error(), "cannot determine all") {
		t.Fatalf("all selection did not fail closed: %v", err)
	}
	subset := options{platform: "6300", version: "10.18.xxxx", guides: []string{"second", "second", "first"}}
	documents, err := selectedDocuments(catalog, subset)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 || documents[0].ID != "second" || documents[1].ID != "first" {
		t.Fatalf("explicit known subset lost order/deduplication: %+v", documents)
	}
}

func TestAllUsesSamePortablePathValidationAsExplicitSelection(t *testing.T) {
	catalog := selectionCatalog()
	catalog.Guides[0].ID = "Guide"
	catalog.Guides[1].ID = "guide"
	o := options{platform: "6300", version: "10.18.xxxx", all: true}
	if _, err := selectedDocuments(catalog, o); err == nil || !strings.Contains(err.Error(), "collide") {
		t.Fatalf("all selection bypassed portable path checks: %v", err)
	}
}

func TestInteractiveBlankDestinationUsesCurrentDirectoryAndPromptsForExistingLibrary(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	o := options{platform: "6300", version: "10.16"}
	prompts := &scriptedPrompts{input: ""}
	if err := completeDestination(context.Background(), &o, prompts, true); err != nil {
		t.Fatal(err)
	}
	if o.destination != cwd {
		t.Fatalf("blank destination=%q, want %q", o.destination, cwd)
	}

	base := t.TempDir()
	target := filepath.Join(base, "6300", "10.16")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	o.destination = base
	prompts = &scriptedPrompts{selects: []string{"refresh"}}
	if err := completeDestination(context.Background(), &o, prompts, true); err != nil {
		t.Fatal(err)
	}
	if !o.refresh || len(prompts.calls) != 1 || prompts.calls[0] != "An existing library was found" {
		t.Fatalf("existing library choice not applied: options=%+v calls=%v", o, prompts.calls)
	}
}

func TestNonInteractiveSelectionNeverPromptsOrCreatesDestination(t *testing.T) {
	catalog := selectionCatalog()
	base := filepath.Join(t.TempDir(), "not-created")
	o := options{platform: "6300", version: "10.18.xxxx", guides: []string{"missing"}, destination: base}
	if _, err := selectDocuments(context.Background(), catalog, &o, nil, func(string) {}); err == nil {
		t.Fatal("unknown non-interactive guide accepted")
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatal("invalid selection created destination")
	}
}
