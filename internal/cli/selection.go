package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
	"aos-cx-docs-dldr/internal/storage"
)

type selectionChoice struct {
	Label    string
	Value    string
	Disabled string
	Emphasis string
}

type releaseAvailability struct {
	Version     string
	Mapped      int
	Diagnostics []string
	Disabled    string
}

type selectionFixed struct {
	Platform         bool
	Version          bool
	Guides           bool
	All              bool
	Destination      bool
	Refresh          bool
	PreferPDF        bool
	ConvertHTMLToPDF bool
}

type availabilityBackend struct {
	fetcher   model.Fetcher
	transport model.Transport
	observe   availabilityProgressObserver
	clearLive func()
}

type availabilityProgress struct {
	Completed int
	Total     int
}

type availabilityProgressObserver func(availabilityProgress)

type wizardStep string

const (
	stepPlatform    wizardStep = "platform"
	stepVersion     wizardStep = "version"
	stepMode        wizardStep = "mode"
	stepGuides      wizardStep = "guides"
	stepDestination wizardStep = "destination"
	stepPreference  wizardStep = "preference"
	stepConversion  wizardStep = "conversion"
	stepExisting    wizardStep = "existing"
)

type selectionWizard struct {
	catalog      model.Catalog
	prompts      promptDriver
	fixed        selectionFixed
	base         options
	state        options
	mode         string
	selectDraft  map[wizardStep]string
	guideDraft   []string
	inputDraft   string
	cwd          string
	fetcher      model.Fetcher
	transport    model.Transport
	observe      availabilityProgressObserver
	clearLive    func()
	message      func(string)
	sourceStatus map[string]model.SourceAvailability
	availability map[string]model.PDFAvailability
}

type selectedOutputSummary struct {
	DirectPublisherPDF           int
	VerifiedOptionalPublisherPDF int
	KnownHTML                    int
	PossibleHTMLFallback         int
}

func (s selectedOutputSummary) publisherPDFs() int {
	return s.DirectPublisherPDF + s.VerifiedOptionalPublisherPDF
}

func (s selectedOutputSummary) possibleHTML() int {
	return s.KnownHTML + s.PossibleHTMLFallback
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func (s selectedOutputSummary) conversionPrompt() (string, string) {
	publisher := s.publisherPDFs()
	var contextParts []string
	if publisher > 0 {
		contextParts = append(contextParts, fmt.Sprintf(
			"%d selected %s will use publisher PDFs",
			publisher, plural(publisher, "guide", "guides"),
		))
	}
	if s.KnownHTML > 0 {
		if publisher == 0 {
			contextParts = append(contextParts, fmt.Sprintf(
				"%d selected %s will use complete HTML",
				s.KnownHTML, plural(s.KnownHTML, "guide", "guides"),
			))
		} else {
			contextParts = append(contextParts, fmt.Sprintf(
				"%d will use complete HTML", s.KnownHTML,
			))
		}
	}
	if s.PossibleHTMLFallback > 0 {
		qualifier := "additional "
		if publisher == 0 && s.KnownHTML == 0 {
			qualifier = "selected "
		}
		contextParts = append(contextParts, fmt.Sprintf(
			"%d %s%s %s unknown publisher-PDF availability and may fall back to HTML",
			s.PossibleHTMLFallback, qualifier,
			plural(s.PossibleHTMLFallback, "guide", "guides"),
			plural(s.PossibleHTMLFallback, "has", "have"),
		))
	}
	context := strings.Join(contextParts, "; ") + "."
	count := s.possibleHTML()
	if s.PossibleHTMLFallback > 0 {
		return context, fmt.Sprintf(
			"Generate PDF companions for up to %d HTML %s?",
			count, plural(count, "guide", "guides"),
		)
	}
	return context, fmt.Sprintf(
		"Generate PDF companions for the %d HTML %s?",
		count, plural(count, "guide", "guides"),
	)
}

func releaseChoices(catalog model.Catalog, platform string) ([]selectionChoice, error) {
	if !slices.Contains(catalog.Platforms, platform) {
		return nil, fmt.Errorf("platform %q is not in the current catalogue", platform)
	}
	choices := make([]selectionChoice, 0, len(catalog.Versions))
	for _, version := range catalog.Versions {
		documents, diagnostics, err := source.ResolveDocuments(catalog, platform, version)
		if err != nil {
			return nil, err
		}
		availability := releaseAvailability{
			Version: version, Mapped: len(documents), Diagnostics: diagnostics,
		}
		switch {
		case len(documents) == 0 && len(diagnostics) > 0:
			availability.Disabled = fmt.Sprintf(
				"availability unknown: %d mapping or route error(s)", len(diagnostics),
			)
		case len(documents) == 0:
			availability.Disabled = "no mapped guides"
		}
		label := fmt.Sprintf("%s (%d mapped guide", version, availability.Mapped)
		if availability.Mapped != 1 {
			label += "s"
		}
		label += ")"
		if len(diagnostics) > 0 && len(documents) > 0 {
			label += fmt.Sprintf(" - partial; %d mapping or route error(s)", len(diagnostics))
		}
		choices = append(choices, selectionChoice{
			Label: label, Value: version, Disabled: availability.Disabled,
		})
	}
	return choices, nil
}

func availabilityLabel(
	document model.Document,
	sourceStatus model.SourceAvailability,
	pdfStatus model.PDFAvailability,
) string {
	label := sourceStatus.Format
	if label == "" {
		if document.Kind == "pdf" {
			label = "PDF native"
		} else {
			label = "HTML (" + document.Kind + ")"
		}
	}
	switch {
	case sourceStatus.Checked && !sourceStatus.Available:
		return label + " - unavailable: " + sourceStatus.Reason
	case !sourceStatus.Checked && sourceStatus.Reason != "":
		return label + " - availability unknown: " + sourceStatus.Reason
	case document.Kind != "pdf" && pdfStatus.Checked && pdfStatus.Available:
		return label + " / PDF native"
	}
	return label
}

func guideChoices(
	documents []model.Document,
	sourceStatuses map[string]model.SourceAvailability,
	pdfStatuses map[string]model.PDFAvailability,
) []selectionChoice {
	choices := make([]selectionChoice, 0, len(documents))
	for _, document := range documents {
		sourceStatus := sourceStatuses[document.URL]
		disabled := ""
		if sourceStatus.Checked && !sourceStatus.Available {
			disabled = sourceStatus.Reason
		}

		choices = append(choices, selectionChoice{
			Label: fmt.Sprintf("%s [%s]", document.Title,
				availabilityLabel(document, sourceStatus, pdfStatuses[document.URL])),
			Value: document.ID, Disabled: disabled,
		})
	}
	return choices
}

func applyAvailableSelection(
	o *options,
	documents []model.Document,
	statuses map[string]model.SourceAvailability,
) ([]model.Document, error) {
	selected := make([]model.Document, 0, len(documents))
	skipped := make([]model.SkippedUnavailableGuide, 0)
	for _, document := range documents {
		status := statuses[document.URL]
		if status.Checked && !status.Available {
			skipped = append(skipped, model.SkippedUnavailableGuide{
				ID: document.ID, Title: document.Title, Kind: document.Kind,
				MappedSourceURL: document.URL, ProbeURL: status.ProbeURL,
				FinalURL: status.FinalURL, Checked: status.Checked,
				Available: status.Available, Reason: status.Reason,
			})
			continue
		}
		selected = append(selected, document)
	}
	if len(selected) == 0 {
		return nil, errors.New("no available mapped guides remain after fresh source checks")
	}
	o.guides = make([]string, 0, len(selected))
	for _, document := range selected {
		o.guides = append(o.guides, document.ID)
	}
	o.skippedUnavailable = skipped
	o.sourceStatus = maps.Clone(statuses)
	o.allAvailableSnapshot = true
	return selected, nil
}

func availableModeChoice(
	documents []model.Document,
	statuses map[string]model.SourceAvailability,
	disabled string,
) selectionChoice {
	unavailable := 0
	for _, document := range documents {
		status := statuses[document.URL]
		if status.Checked && !status.Available {
			unavailable++
		}
	}
	if unavailable == 0 {
		return selectionChoice{
			Label: fmt.Sprintf("All mapped guides (%d)", len(documents)),
			Value: "all", Disabled: disabled,
		}
	}
	return selectionChoice{
		Label: fmt.Sprintf("All available mapped guides (%d)", len(documents)-unavailable),
		Value: "all", Disabled: disabled, Emphasis: "available",
	}
}

func selectDocuments(
	ctx context.Context,
	catalog model.Catalog,
	o *options,
	prompts promptDriver,
	message func(string),
) ([]model.Document, error) {
	if o.platform == "" {
		if prompts == nil {
			return nil, errors.New("non-interactive downloads require --platform")
		}
		choices := make([]selectionChoice, 0, len(catalog.Platforms))
		for _, platform := range catalog.Platforms {
			choices = append(choices, selectionChoice{Label: platform, Value: platform})
		}
		platform, err := prompts.Select(ctx, "Switch platform", choices, promptConfig{})
		if err != nil {
			return nil, err
		}
		o.platform = platform
	}
	if !slices.Contains(catalog.Platforms, o.platform) {
		return nil, fmt.Errorf("platform %q is not in the current catalogue", o.platform)
	}

	if o.version == "" {
		if prompts == nil {
			return nil, errors.New("non-interactive downloads require --version")
		}
		choices, err := releaseChoices(catalog, o.platform)
		if err != nil {
			return nil, err
		}
		version, err := prompts.Select(ctx, "Documentation version", choices, promptConfig{})
		if err != nil {
			return nil, err
		}
		o.version = version
	}
	if !slices.Contains(catalog.Versions, o.version) {
		return nil, fmt.Errorf("version %q is not in the current portal dropdown", o.version)
	}

	documents, diagnostics, err := source.ResolveDocuments(catalog, o.platform, o.version)
	if err != nil {
		return nil, err
	}
	for _, diagnostic := range diagnostics {
		message("Catalogue mapping error: " + diagnostic)
	}
	if len(documents) == 0 {
		if len(diagnostics) > 0 {
			return nil, fmt.Errorf(
				"cannot establish availability for %s/%s because mappings or routes failed: %s",
				o.platform, o.version, strings.Join(diagnostics, "; "),
			)
		}
		available := source.AvailableVersions(catalog, o.platform)
		detail := "none"
		if len(available) > 0 {
			detail = strings.Join(available, ", ")
		}
		return nil, fmt.Errorf(
			"version %q is not available for platform %q. Available versions: %s",
			o.version, o.platform, detail,
		)
	}
	if o.all || len(o.guides) > 0 {
		return selectedDocuments(catalog, *o)
	}
	if prompts == nil {
		return nil, errors.New("non-interactive downloads require --guides ID ... or --all")
	}

	allDisabled := ""
	if len(diagnostics) > 0 {
		allDisabled = "mapping or route failures prevent a complete all-guides selection"
	}
	mode, err := prompts.Select(ctx, "Documents to retrieve", []selectionChoice{
		{
			Label: fmt.Sprintf("All mapped guides (%d)", len(documents)),
			Value: "all", Disabled: allDisabled,
		},
		{Label: "Choose individual guides", Value: "some"},
	}, promptConfig{})
	if err != nil {
		return nil, err
	}
	if mode == "all" {
		o.all = true
		return selectedDocuments(catalog, *o)
	}
	selected, err := prompts.MultiSelect(
		ctx,
		"Select guides (Space toggles; Enter confirms)",
		guideChoices(documents, nil, nil),
		promptConfig{},
	)
	if err != nil {
		return nil, err
	}
	o.guides = selected
	return selectedDocuments(catalog, *o)
}

func completeDestination(
	ctx context.Context,
	o *options,
	prompts promptDriver,
	confirmExisting bool,
) error {
	if o.destination == "" {
		if prompts == nil {
			return errors.New("non-interactive downloads require --destination")
		}
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("determine default destination: %w", err)
		}
		value, err := prompts.Input(
			ctx,
			fmt.Sprintf("Save documentation under which directory? (%s)", cwd),
			promptConfig{},
		)
		if err != nil {
			return err
		}
		if strings.TrimSpace(value) == "" {
			value = cwd
		}
		o.destination = value
	}
	if prompts == nil || !confirmExisting || o.refresh {
		return nil
	}
	base, err := storage.ResolveBase(o.destination)
	if err != nil {
		return err
	}
	platform, err := storage.SafeComponent(o.platform)
	if err != nil {
		return err
	}
	version, err := storage.SafeComponent(o.version)
	if err != nil {
		return err
	}
	target := filepath.Join(base, platform, version)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("existing library target is not a real directory: %s", target)
	}
	choice, err := prompts.Select(ctx, "An existing library was found", []selectionChoice{
		{Label: "Resume using verified cached files", Value: "resume"},
		{Label: "Check sources for updates", Value: "refresh"},
	}, promptConfig{})
	if err != nil {
		return err
	}
	o.refresh = choice == "refresh"
	return nil
}

func completeInteractiveSelection(
	ctx context.Context,
	catalog model.Catalog,
	o *options,
	prompts promptDriver,
	message func(string),
	fixed selectionFixed,
	guided bool,
	backends ...availabilityBackend,
) ([]model.Document, error) {
	if prompts == nil || !guided {
		documents, err := selectDocuments(ctx, catalog, o, prompts, message)
		if err != nil {
			return nil, err
		}
		if len(backends) > 0 && backends[0].transport != nil {
			scanner := selectionWizard{
				catalog: catalog, state: *o, fetcher: backends[0].fetcher,
				transport: backends[0].transport, observe: backends[0].observe,
				clearLive: backends[0].clearLive, message: message,
				sourceStatus: map[string]model.SourceAvailability{},
				availability: map[string]model.PDFAvailability{},
			}
			if err := scanner.scanAvailability(ctx, documents, o.preferPDF); err != nil {
				return nil, err
			}
			if o.all {
				documents, err = applyAvailableSelection(o, documents, scanner.sourceStatus)
				if err != nil {
					return nil, err
				}
			}
			if err := validateSelectedSourceAvailability(catalog, *o, documents, scanner.sourceStatus); err != nil {
				return nil, err
			}
			o.preflightFresh = scanner.state.preflightFresh
		}
		if err := completeDestination(ctx, o, prompts, false); err != nil {
			return nil, err
		}
		return documents, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determine default destination: %w", err)
	}
	base, state := *o, *o
	base.guides = append([]string{}, o.guides...)
	state.guides = append([]string{}, o.guides...)
	var backend availabilityBackend
	if len(backends) > 0 {
		backend = backends[0]
	}
	if backend.transport == nil {
		fixed.PreferPDF = true
	}
	wizard := &selectionWizard{
		catalog: catalog, prompts: prompts, fixed: fixed, base: base, state: state,
		selectDraft: map[wizardStep]string{}, inputDraft: o.destination, cwd: cwd,
		fetcher: backend.fetcher, transport: backend.transport, message: message,
		observe: backend.observe, clearLive: backend.clearLive,
		sourceStatus: map[string]model.SourceAvailability{},
		availability: map[string]model.PDFAvailability{},
	}
	switch {
	case o.all:
		wizard.mode = "all"
	case len(o.guides) > 0:
		wizard.mode = "some"
		wizard.guideDraft = append([]string{}, o.guides...)
	}
	if err := wizard.run(ctx); err != nil {
		return nil, err
	}
	*o = wizard.state
	documents, diagnostics, err := source.ResolveDocuments(catalog, o.platform, o.version)
	if err != nil {
		return nil, err
	}
	for _, diagnostic := range diagnostics {
		message("Catalogue mapping error: " + diagnostic)
	}
	if len(documents) == 0 {
		return nil, fmt.Errorf("no mapped guides remain for %s/%s", o.platform, o.version)
	}
	return selectedDocuments(catalog, *o)
}

func (w *selectionWizard) run(ctx context.Context) error {
	if err := w.prepareSelectedAvailability(ctx); err != nil {
		return err
	}
	steps, err := w.steps()
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return w.validateComplete()
	}
	current := steps[0]
	history := []wizardStep{}
	for {
		back, err := w.ask(ctx, current, len(history) > 0)
		if err != nil {
			return err
		}
		if back {
			current = history[len(history)-1]
			history = history[:len(history)-1]
			continue
		}
		if err := w.prepareSelectedAvailability(ctx); err != nil {
			return err
		}
		steps, err = w.steps()
		if err != nil {
			return err
		}
		index := slices.Index(steps, current)
		if index < 0 {
			return fmt.Errorf("interactive selection step %q became invalid", current)
		}
		if index+1 == len(steps) {
			return w.validateComplete()
		}
		history = append(history, current)
		current = steps[index+1]
	}
}

func (w *selectionWizard) steps() ([]wizardStep, error) {
	steps := make([]wizardStep, 0, 8)
	if !w.fixed.Platform {
		steps = append(steps, stepPlatform)
	}
	if !w.fixed.Version {
		steps = append(steps, stepVersion)
	}
	if !w.fixed.All && !w.fixed.Guides {
		steps = append(steps, stepMode)
	}
	if w.mode == "some" && !w.fixed.Guides {
		steps = append(steps, stepGuides)
	}
	if !w.fixed.Destination {
		steps = append(steps, stepDestination)
	}
	relevant, err := w.preferenceRelevant()
	if err != nil {
		return nil, err
	}
	if !w.fixed.PreferPDF && relevant {
		steps = append(steps, stepPreference)
	} else if !w.fixed.PreferPDF {
		w.state.preferPDF = false
		delete(w.selectDraft, stepPreference)
	}
	conversionRelevant, err := w.conversionRelevant()
	if err != nil {
		return nil, err
	}
	if !w.fixed.ConvertHTMLToPDF && conversionRelevant {
		steps = append(steps, stepConversion)
	} else if !w.fixed.ConvertHTMLToPDF {
		w.state.convertHTMLToPDF = false
		delete(w.selectDraft, stepConversion)
	}
	exists, err := w.existingLibrary()
	if err != nil {
		return nil, err
	}
	if exists && !w.fixed.Refresh {
		steps = append(steps, stepExisting)
	}
	return steps, nil
}

func (w *selectionWizard) ask(ctx context.Context, step wizardStep, allowBack bool) (bool, error) {
	config := promptConfig{AllowBack: allowBack}
	w.clearLiveOutput()
	switch step {
	case stepPlatform:
		choices := make([]selectionChoice, 0, len(w.catalog.Platforms))
		for _, platform := range w.catalog.Platforms {
			choices = append(choices, selectionChoice{Label: platform, Value: platform})
		}
		config.Initial = firstNonempty(w.selectDraft[step], w.state.platform)
		value, err := w.prompts.Select(ctx, "Switch platform", choices, config)
		if errors.Is(err, errPromptBack) {
			w.selectDraft[step] = value
			return true, nil
		}
		if err != nil {
			return false, err
		}
		w.selectDraft[step] = value
		if value != w.state.platform {
			w.state.platform = value
			if !w.fixed.Version {
				w.state.version = ""
				delete(w.selectDraft, stepVersion)
			}
			w.revalidateGuides()
			w.resetExistingChoice()
		}
	case stepVersion:
		choices, err := releaseChoices(w.catalog, w.state.platform)
		if err != nil {
			return false, err
		}
		config.Initial = firstNonempty(w.selectDraft[step], w.state.version)
		value, err := w.prompts.Select(ctx, "Documentation version", choices, config)
		if errors.Is(err, errPromptBack) {
			w.selectDraft[step] = value
			return true, nil
		}
		if err != nil {
			return false, err
		}
		w.selectDraft[step] = value
		if value != w.state.version {
			w.state.version = value
			w.revalidateGuides()
			w.resetExistingChoice()
		}
	case stepMode:
		documents, diagnostics, err := source.ResolveDocuments(w.catalog, w.state.platform, w.state.version)
		if err != nil {
			return false, err
		}
		if len(documents) == 0 {
			if len(diagnostics) > 0 {
				return false, fmt.Errorf(
					"cannot establish availability for %s/%s because mappings or routes failed: %s",
					w.state.platform, w.state.version, strings.Join(diagnostics, "; "),
				)
			}
			available := source.AvailableVersions(w.catalog, w.state.platform)
			detail := "none"
			if len(available) > 0 {
				detail = strings.Join(available, ", ")
			}
			return false, fmt.Errorf(
				"version %q is not available for platform %q. Available versions: %s",
				w.state.version, w.state.platform, detail,
			)
		}
		if err := w.scanAvailability(ctx, documents, true); err != nil {
			return false, err
		}
		w.clearLiveOutput()
		allDisabled := ""
		if len(diagnostics) > 0 {
			allDisabled = "mapping or route failures prevent a complete all-guides selection"
		}
		config.Initial = firstNonempty(w.selectDraft[step], w.mode)
		value, err := w.prompts.Select(ctx, "Documents to retrieve", []selectionChoice{
			availableModeChoice(documents, w.sourceStatus, allDisabled),
			{Label: "Choose individual guides", Value: "some"},
		}, config)
		if errors.Is(err, errPromptBack) {
			w.selectDraft[step] = value
			return true, nil
		}
		if err != nil {
			return false, err
		}
		w.selectDraft[step], w.mode = value, value
		w.state.all = value == "all"
		if w.state.all {
			if _, err := applyAvailableSelection(&w.state, documents, w.sourceStatus); err != nil {
				return false, err
			}
		}
	case stepGuides:
		documents, _, err := source.ResolveDocuments(w.catalog, w.state.platform, w.state.version)
		if err != nil {
			return false, err
		}
		if err := w.scanAvailability(ctx, documents, true); err != nil {
			return false, err
		}
		w.clearLiveOutput()
		config.Selected = append([]string{}, w.guideDraft...)
		values, err := w.prompts.MultiSelect(
			ctx, "Select guides (Space toggles; Enter confirms)",
			guideChoices(documents, w.sourceStatus, w.availability), config,
		)
		if errors.Is(err, errPromptBack) {
			w.guideDraft = append([]string{}, values...)
			return true, nil
		}
		if err != nil {
			return false, err
		}
		w.guideDraft = append([]string{}, values...)
		w.state.guides = append([]string{}, values...)
		w.state.all = false
	case stepDestination:
		config.Initial = w.inputDraft
		value, err := w.prompts.Input(
			ctx, fmt.Sprintf("Save documentation under which directory? (%s)", w.cwd), config,
		)
		if errors.Is(err, errPromptBack) {
			w.inputDraft = value
			return true, nil
		}
		if err != nil {
			return false, err
		}
		previous := w.state.destination
		w.inputDraft = value
		if strings.TrimSpace(value) == "" {
			value = w.cwd
		}
		w.state.destination = value
		if value != previous {
			w.resetExistingChoice()
		}
	case stepPreference:
		config.Initial = firstNonempty(w.selectDraft[step], "html")
		value, err := w.prompts.Select(ctx, "Prefer a source-verified PDF when available?", []selectionChoice{
			{Label: "No - prefer complete HTML", Value: "html"},
			{Label: "Yes - prefer a verified publisher PDF", Value: "pdf"},
		}, config)
		if errors.Is(err, errPromptBack) {
			w.selectDraft[step] = value
			return true, nil
		}
		if err != nil {
			return false, err
		}
		w.selectDraft[step] = value
		w.state.preferPDF = value == "pdf"
	case stepConversion:
		summary, ready, err := w.selectedOutputSummary()
		if err != nil {
			return false, err
		}
		if !ready || summary.possibleHTML() == 0 {
			return false, errors.New("HTML conversion prompt became irrelevant")
		}
		contextLine, promptTitle := summary.conversionPrompt()
		config.Initial = firstNonempty(w.selectDraft[step], "html-only")
		config.Context = contextLine
		value, err := w.prompts.Select(ctx, promptTitle, []selectionChoice{
			{Label: "No - keep the complete browsable HTML only", Value: "html-only"},
			{Label: "Yes - also generate PDFs with pinned Chrome", Value: "convert"},
		}, config)
		if errors.Is(err, errPromptBack) {
			w.selectDraft[step] = value
			return true, nil
		}
		if err != nil {
			return false, err
		}
		w.selectDraft[step] = value
		w.state.convertHTMLToPDF = value == "convert"
	case stepExisting:
		config.Initial = firstNonempty(w.selectDraft[step], "resume")
		value, err := w.prompts.Select(ctx, "An existing library was found", []selectionChoice{
			{Label: "Resume using verified cached files", Value: "resume"},
			{Label: "Check sources for updates", Value: "refresh"},
		}, config)
		if errors.Is(err, errPromptBack) {
			w.selectDraft[step] = value
			return true, nil
		}
		if err != nil {
			return false, err
		}
		w.selectDraft[step] = value
		w.state.refresh = value == "refresh"
	default:
		return false, fmt.Errorf("unknown interactive selection step %q", step)
	}
	return false, nil
}

func (w *selectionWizard) validateComplete() error {
	if w.state.platform == "" || w.state.version == "" {
		return errors.New("interactive selection did not establish platform and version")
	}
	if w.mode == "all" {
		w.state.all = true
	} else if w.mode == "some" {
		w.state.all = false
		w.state.guides = append([]string{}, w.guideDraft...)
	}
	if w.state.destination == "" {
		return errors.New("interactive selection did not establish destination")
	}
	documents, err := selectedDocuments(w.catalog, w.state)
	if err != nil {
		return err
	}
	return validateSelectedSourceAvailability(w.catalog, w.state, documents, w.sourceStatus)
}

func (w *selectionWizard) revalidateGuides() {
	w.sourceStatus = map[string]model.SourceAvailability{}
	w.availability = map[string]model.PDFAvailability{}
	w.state.sourceStatus = nil
	w.state.skippedUnavailable = nil
	w.state.allAvailableSnapshot = false
	if !w.fixed.PreferPDF {
		w.state.preferPDF = false
		delete(w.selectDraft, stepPreference)
	}
	if !w.fixed.ConvertHTMLToPDF {
		w.state.convertHTMLToPDF = false
		delete(w.selectDraft, stepConversion)
	}
	if w.fixed.Guides {
		return
	}
	documents, _, err := source.ResolveDocuments(w.catalog, w.state.platform, w.state.version)
	if err != nil {
		w.guideDraft = nil
		w.state.guides = nil
		return
	}
	valid := make(map[string]bool, len(documents))
	for _, document := range documents {
		valid[document.ID] = true
	}
	w.guideDraft = slices.DeleteFunc(w.guideDraft, func(id string) bool { return !valid[id] })
	w.state.guides = slices.DeleteFunc(w.state.guides, func(id string) bool { return !valid[id] })
}

func (w *selectionWizard) scanAvailability(
	ctx context.Context,
	documents []model.Document,
	probeOptionalPDF bool,
) error {
	if w.transport == nil {
		return nil
	}
	if w.sourceStatus == nil {
		w.sourceStatus = map[string]model.SourceAvailability{}
	}
	if w.availability == nil {
		w.availability = map[string]model.PDFAvailability{}
	}
	type candidate struct {
		index    int
		document model.Document
	}
	var candidates []candidate
	for index, document := range documents {
		if document.Kind != "pdf" && document.Kind != "hpe" &&
			document.Kind != "flare" && document.Kind != "static" {
			continue
		}
		_, sourceFound := w.sourceStatus[document.URL]
		_, pdfFound := w.availability[document.URL]
		if sourceFound {
			sourceStatus := w.sourceStatus[document.URL]
			if !probeOptionalPDF || document.Kind == "pdf" || pdfFound || !sourceStatus.Available {
				continue
			}
		}
		candidates = append(candidates, candidate{index: index, document: document})
	}
	if len(candidates) == 0 {
		return nil
	}
	if w.observe != nil {
		w.observe(availabilityProgress{Total: len(candidates)})
	}
	type outcome struct {
		candidate
		sourceStatus model.SourceAvailability
		pdfStatus    model.PDFAvailability
		err          error
	}
	jobs := make(chan candidate, len(candidates))
	results := make(chan outcome, len(candidates))
	workers := min(w.state.workerCount(), len(candidates))
	var group sync.WaitGroup
	var progressMu sync.Mutex
	completed := 0
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for item := range jobs {
				sourceStatus, pdfStatus, err := source.ProbeDocumentAvailability(
					ctx, w.fetcher, w.transport, item.document, probeOptionalPDF,
				)
				if w.observe != nil && ctx.Err() == nil {
					progressMu.Lock()
					completed++
					w.observe(availabilityProgress{Completed: completed, Total: len(candidates)})
					progressMu.Unlock()
				}
				results <- outcome{
					candidate: item, sourceStatus: sourceStatus, pdfStatus: pdfStatus, err: err,
				}
			}
		}()
	}
	for _, item := range candidates {
		jobs <- item
	}
	close(jobs)
	group.Wait()
	close(results)
	ordered := make([]outcome, len(documents))
	for result := range results {
		ordered[result.index] = result
	}
	for _, item := range candidates {
		result := ordered[item.index]
		if result.err != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return result.err
		}
		w.sourceStatus[item.document.URL] = result.sourceStatus
		if result.sourceStatus.Checked && result.sourceStatus.Available &&
			result.sourceStatus.ProbeURL != "" {
			if w.state.preflightFresh == nil {
				w.state.preflightFresh = map[string]bool{}
			}
			w.state.preflightFresh[result.sourceStatus.ProbeURL] = true
		}
		if probeOptionalPDF && item.document.Kind != "pdf" &&
			(result.pdfStatus.Checked || result.pdfStatus.URL != "" ||
				result.pdfStatus.Origin != "" || result.pdfStatus.Reason != "") {
			w.availability[item.document.URL] = result.pdfStatus
		}
		if w.message != nil && result.sourceStatus.Reason != "" {
			if result.sourceStatus.Checked && !result.sourceStatus.Available {
				w.message(fmt.Sprintf("Mapped source unavailable for %s: %s (%s)",
					item.document.Title, result.sourceStatus.Reason, item.document.URL))
			} else if !result.sourceStatus.Checked {
				w.message(fmt.Sprintf("Could not establish mapped source availability for %s; remains selectable: %s",
					item.document.Title, result.sourceStatus.Reason))
			}
		}
		if w.message != nil && probeOptionalPDF && item.document.Kind != "pdf" &&
			!result.pdfStatus.Checked && result.pdfStatus.Reason != "" {
			w.message(fmt.Sprintf("Could not verify optional PDF availability for %s: %s",
				item.document.Title, result.pdfStatus.Reason))
		}
	}
	return nil
}

func (w *selectionWizard) scanPDFs(ctx context.Context, documents []model.Document) error {
	return w.scanAvailability(ctx, documents, true)
}

func (w *selectionWizard) clearLiveOutput() {
	if w.clearLive != nil {
		w.clearLive()
	}
}

func (w *selectionWizard) selectedForAvailability() ([]model.Document, bool, error) {
	if w.state.platform == "" || w.state.version == "" {
		return nil, false, nil
	}
	documents, _, err := source.ResolveDocuments(w.catalog, w.state.platform, w.state.version)
	if err != nil {
		return nil, false, err
	}
	if w.state.all || w.mode == "all" {
		if w.state.allAvailableSnapshot && len(w.state.guides) > 0 {
			wanted := make(map[string]bool, len(w.state.guides))
			for _, id := range w.state.guides {
				wanted[id] = true
			}
			selected := make([]model.Document, 0, len(wanted))
			for _, document := range documents {
				if wanted[document.ID] {
					selected = append(selected, document)
				}
			}
			return selected, len(selected) == len(wanted), nil
		}
		return documents, true, nil
	}
	ids := w.state.guides
	if len(ids) == 0 {
		ids = w.guideDraft
	}
	if len(ids) == 0 {
		return nil, false, nil
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	selected := make([]model.Document, 0, len(ids))
	for _, document := range documents {
		if wanted[document.ID] {
			selected = append(selected, document)
		}
	}
	return selected, len(selected) == len(wanted), nil
}

func (w *selectionWizard) prepareSelectedAvailability(ctx context.Context) error {
	documents, ready, err := w.selectedForAvailability()
	if err != nil || !ready {
		return err
	}
	probeOptionalPDF := !w.fixed.PreferPDF || w.state.preferPDF
	if err := w.scanAvailability(ctx, documents, probeOptionalPDF); err != nil {
		return err
	}
	if w.state.all || w.mode == "all" {
		documents, err = applyAvailableSelection(&w.state, documents, w.sourceStatus)
		if err != nil {
			return err
		}
	}
	if w.fixed.Guides || w.fixed.All {
		return validateSelectedSourceAvailability(w.catalog, w.state, documents, w.sourceStatus)
	}
	return nil
}

func (w *selectionWizard) preferenceRelevant() (bool, error) {
	documents, ready, err := w.selectedForAvailability()
	if err != nil || !ready {
		return false, err
	}
	for _, document := range documents {
		if document.Kind == "pdf" {
			continue
		}
		availability, found := w.availability[document.URL]
		if found && availability.Checked && availability.Available {
			return true, nil
		}
	}
	return false, nil
}

func (w *selectionWizard) conversionRelevant() (bool, error) {
	summary, ready, err := w.selectedOutputSummary()
	if err != nil || !ready {
		return false, err
	}
	return summary.possibleHTML() > 0, nil
}

func (w *selectionWizard) selectedOutputSummary() (selectedOutputSummary, bool, error) {
	documents, ready, err := w.selectedForAvailability()
	if err != nil || !ready {
		return selectedOutputSummary{}, ready, err
	}
	return classifySelectedOutputs(documents, w.availability, w.state.preferPDF), true, nil
}

func classifySelectedOutputs(
	documents []model.Document,
	availability map[string]model.PDFAvailability,
	preferPDF bool,
) selectedOutputSummary {
	var summary selectedOutputSummary
	for _, document := range documents {
		if document.Kind == "pdf" {
			summary.DirectPublisherPDF++
			continue
		}
		if !preferPDF {
			summary.KnownHTML++
			continue
		}
		pdfStatus, found := availability[document.URL]
		switch {
		case found && pdfStatus.Checked && pdfStatus.Available:
			summary.VerifiedOptionalPublisherPDF++
		case found && pdfStatus.Checked && !pdfStatus.Available:
			summary.KnownHTML++
		default:
			summary.PossibleHTMLFallback++
		}
	}
	return summary
}

func firstUnavailableSource(
	documents []model.Document,
	statuses map[string]model.SourceAvailability,
) (model.Document, model.SourceAvailability, bool) {
	for _, document := range documents {
		status, found := statuses[document.URL]
		if found && status.Checked && !status.Available {
			return document, status, true
		}
	}
	return model.Document{}, model.SourceAvailability{}, false
}

func validateSelectedSourceAvailability(
	catalog model.Catalog,
	o options,
	documents []model.Document,
	statuses map[string]model.SourceAvailability,
) error {
	document, status, unavailable := firstUnavailableSource(documents, statuses)
	if !unavailable {
		return nil
	}
	versions := mappedGuideVersions(catalog, o.platform, document.ID, o.version)
	detail := "none"
	if len(versions) > 0 {
		detail = strings.Join(versions, ", ")
	}
	checked := status.ProbeURL
	if status.FinalURL != "" {
		checked = status.FinalURL
	}
	return fmt.Errorf(
		"guide %q mapped source is unavailable for %s/%s: %s; mapped route: %s; checked route: %s. "+
			"Choose another independently mapped version and redownload. Other mapped versions for %s on platform %s: %s",
		document.ID, o.platform, o.version, status.Reason, document.URL, checked,
		document.ID, o.platform, detail,
	)
}

func mappedGuideVersions(catalog model.Catalog, platform, guideID, exclude string) []string {
	for _, guide := range catalog.Guides {
		if guide.ID != guideID {
			continue
		}
		var versions []string
		for _, version := range catalog.Versions {
			if version != exclude && guide.Mappings[version][platform] != "" {
				versions = append(versions, version)
			}
		}
		return versions
	}
	return nil
}

func (w *selectionWizard) resetExistingChoice() {
	if !w.fixed.Refresh {
		w.state.refresh = w.base.refresh
		delete(w.selectDraft, stepExisting)
	}
}

func (w *selectionWizard) existingLibrary() (bool, error) {
	if w.state.destination == "" || w.state.platform == "" || w.state.version == "" ||
		w.fixed.Refresh && w.state.refresh {
		return false, nil
	}
	base, err := storage.ResolveBase(w.state.destination)
	if err != nil {
		return false, err
	}
	if err := library.Preflight(w.state.destination, w.state.platform, w.state.version); err != nil {
		return false, err
	}
	platform, err := storage.SafeComponent(w.state.platform)
	if err != nil {
		return false, err
	}
	version, err := storage.SafeComponent(w.state.version)
	if err != nil {
		return false, err
	}
	target := filepath.Join(base, platform, version)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("existing library target is not a real directory: %s", target)
	}
	return true, nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
