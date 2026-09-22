package archive

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
	"golang.org/x/net/html"
)

const maxTableStyleCandidates = 4096

type resourceStatusError struct {
	URL    string
	Status int
}

func (e *resourceStatusError) Error() string {
	return fmt.Sprintf("publisher resource returned HTTP %d: %s", e.Status, e.URL)
}

type tableStyleReference struct {
	URL      string
	Basename string
	Families []string
}

type tableStyleCandidate struct {
	URL      string
	Basename string
	Families map[string]bool
	State    *assetState
}

type tableStyleResolution struct {
	candidate *tableStyleCandidate
	status    int
	err       error
}

func (a *htmlArchiver) collectTableStyleEvidence(doc, content *html.Node, page *pageInput) error {
	usedFamilies := map[string]bool{}
	mcReferences := map[string]map[string]bool{}
	walk(content, func(node *html.Node) {
		if node.Type != html.ElementNode || node.Data != "table" {
			return
		}
		families := tableStyleClassFamilies(node)
		for _, family := range families {
			usedFamilies[family] = true
		}
		raw := strings.TrimSpace(attr(node, "mc-table-style"))
		if raw == "" || len(families) == 0 {
			return
		}
		resolved, err := resolveReference(page.baseURL, raw)
		if err != nil {
			return
		}
		expected, ok := tableStyleFamilyFromURL(resolved)
		for _, family := range families {
			if ok && family == expected {
				if mcReferences[resolved] == nil {
					mcReferences[resolved] = map[string]bool{}
				}
				mcReferences[resolved][family] = true
			}
		}
	})

	references := map[string]map[string]bool{}
	walk(doc, func(node *html.Node) {
		if node.Type != html.ElementNode || node.Data != "link" || attr(node, "disabled") != "" ||
			!slices.Contains(strings.Fields(strings.ToLower(attr(node, "rel"))), "stylesheet") {
			return
		}
		resolved, err := resolveReference(page.baseURL, attr(node, "href"))
		if err != nil {
			return
		}
		expected, ok := tableStyleFamilyFromURL(resolved)
		tableTyped := strings.EqualFold(strings.TrimSpace(attr(node, "data-mc-stylesheet-type")), "table")
		if !tableTyped && mcReferences[resolved] == nil {
			return
		}
		if !ok || !usedFamilies[expected] {
			return
		}
		if references[resolved] == nil {
			references[resolved] = map[string]bool{}
		}
		references[resolved][expected] = true
	})
	for resolved, families := range mcReferences {
		if len(families) == 0 {
			continue
		}
		if references[resolved] == nil {
			references[resolved] = map[string]bool{}
		}
		for family := range families {
			references[resolved][family] = true
		}
	}
	if len(references) == 0 {
		return nil
	}
	page.tableStyles = make(map[string]tableStyleReference, len(references))
	for raw, families := range references {
		basename, ok := tableStyleBasename(raw)
		if !ok {
			continue
		}
		sortedFamilies := mapsKeys(families)
		slices.Sort(sortedFamilies)
		page.tableStyles[canonicalNoFragment(raw)] = tableStyleReference{
			URL: raw, Basename: basename, Families: sortedFamilies,
		}
	}
	return nil
}

func mapsKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}

func tableStyleClassFamilies(node *html.Node) []string {
	var families []string
	for _, class := range strings.Fields(attr(node, "class")) {
		if strings.HasPrefix(class, "TableStyle-") && len(class) > len("TableStyle-") &&
			!slices.Contains(families, class) {
			families = append(families, class)
		}
	}
	return families
}

func tableStyleBasename(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	basename, err := url.PathUnescape(path.Base(parsed.EscapedPath()))
	if err != nil || basename == "" || basename == "." || basename == "/" ||
		!strings.EqualFold(path.Ext(basename), ".css") {
		return "", false
	}
	return basename, true
}

func tableStyleFamilyFromURL(raw string) (string, bool) {
	basename, ok := tableStyleBasename(raw)
	if !ok {
		return "", false
	}
	return "TableStyle-" + strings.TrimSuffix(basename, path.Ext(basename)), true
}

func (a *htmlArchiver) prepareTableStyleCandidates() error {
	if a.plan.Document.Kind != "flare" {
		return nil
	}
	needed := false
	for _, page := range a.pages {
		for _, reference := range page.tableStyles {
			if a.recoverableTableStyleReference(reference.URL) {
				needed = true
				break
			}
		}
		if needed {
			break
		}
	}
	if !needed {
		return nil
	}
	advertised := map[string]tableStyleReference{}
	for _, page := range a.pages {
		for key, reference := range page.tableStyles {
			if a.guideLocalURL(reference.URL) {
				advertised[key] = reference
			}
		}
	}
	if len(advertised) > maxTableStyleCandidates {
		a.warnings = append(a.warnings,
			"Flare table stylesheet candidate inventory exceeds the recovery limit; broken publisher styles will remain fail-closed.")
		return nil
	}
	keys := make([]string, 0, len(advertised))
	for key := range advertised {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		reference := advertised[key]
		state, err := a.assets.probe(reference.URL, dependencyCSS, 0)
		if err != nil {
			continue
		}
		if !a.verifiedGuideLocalCandidate(reference.URL, state) {
			continue
		}
		decoded, err := decodeCSS(state.sourceBody, state.record.ContentType)
		if err != nil {
			continue
		}
		families, err := tableStyleFamiliesInCSS(decoded)
		if err != nil {
			continue
		}
		candidate := tableStyleCandidate{
			URL: reference.URL, Basename: reference.Basename, Families: families, State: state,
		}
		folded := strings.ToLower(reference.Basename)
		a.tableStyleCandidates[folded] = append(a.tableStyleCandidates[folded], candidate)
	}
	return nil
}

func (a *htmlArchiver) guideLocalURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	for _, root := range a.guideRoots {
		base, err := url.Parse(root)
		if err == nil && parsed.Scheme == base.Scheme && parsed.Host == base.Host &&
			strings.HasPrefix(parsed.Path, base.Path) {
			return true
		}
	}
	return false
}

func (a *htmlArchiver) sameGuideOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	for _, root := range a.guideRoots {
		base, err := url.Parse(root)
		if err == nil && parsed.Scheme == base.Scheme && parsed.Host == base.Host {
			return true
		}
	}
	return false
}

func (a *htmlArchiver) recoverableTableStyleReference(raw string) bool {
	return a.plan.Document.Kind == "flare" && !a.guideLocalURL(raw) && a.sameGuideOrigin(raw)
}

func (a *htmlArchiver) verifiedGuideLocalCandidate(requested string, state *assetState) bool {
	if state == nil || state.record.Status != "complete" ||
		media(state.record.ContentType) != "text/css" ||
		!a.guideLocalURL(requested) || !a.guideLocalURL(state.record.FinalURL) {
		return false
	}
	requestedKey, requestedErr := fetch.CanonicalURL(requested)
	finalKey, finalErr := fetch.CanonicalURL(state.record.FinalURL)
	return requestedErr == nil && finalErr == nil && requestedKey == finalKey &&
		state.record.SourceSHA256 != "" && state.record.SourceSize > 0
}

func tableStyleFamiliesInCSS(body []byte) (map[string]bool, error) {
	parser := css.NewParser(parse.NewInputBytes(body), false)
	families := map[string]bool{}
	for {
		grammar, _, _ := parser.Next()
		switch grammar {
		case css.ErrorGrammar:
			if parser.HasParseError() {
				return nil, fmt.Errorf("CSS parse error: %w", parser.Err())
			}
			if err := parser.Err(); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return families, nil
		case css.BeginRulesetGrammar:
			tokens := parser.Values()
			for i := 0; i+1 < len(tokens); i++ {
				if tokens[i].TokenType != css.DelimToken || string(tokens[i].Data) != "." ||
					tokens[i+1].TokenType != css.IdentToken {
					continue
				}
				class, err := cssUnescape(string(tokens[i+1].Data))
				if err == nil && strings.HasPrefix(class, "TableStyle-") {
					families[class] = true
				}
			}
		}
	}
}

func (candidate tableStyleCandidate) covers(families []string) bool {
	for _, family := range families {
		covered := false
		for selectorClass := range candidate.Families {
			if selectorClass == family || strings.HasPrefix(selectorClass, family+"-") {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func (a *htmlArchiver) localPageStyle(raw string, page *pageInput) (string, error) {
	key := canonicalNoFragment(raw)
	if recovery, ok := page.ordinaryStyleRecoveries[key]; ok {
		return a.localOrdinaryPageStyle(raw, page, recovery)
	}
	reference, tableStyle := page.tableStyles[key]
	if !tableStyle || !a.recoverableTableStyleReference(raw) {
		return a.assets.local(raw, page.baseURL, page.path, dependencyCSS)
	}
	resolution, seen := a.tableStyleResolutions[key]
	if !seen {
		state, err := a.assets.probe(raw, dependencyCSS, 0)
		if err == nil {
			return localReference(page.path, state.record.Path), nil
		}
		status, missing := permanentMissingHTTPStatus(err)
		if !missing {
			_, recordedErr := a.assets.recordFailure(raw, err)
			a.tableStyleResolutions[key] = tableStyleResolution{err: recordedErr}
			return "", recordedErr
		}
		candidate := uniqueTableStyleCandidate(a.tableStyleCandidates[strings.ToLower(reference.Basename)], reference)
		if candidate != nil {
			resolution = tableStyleResolution{candidate: candidate, status: status}
			a.tableStyleResolutions[key] = resolution
		} else {
			_, recordedErr := a.assets.recordFailure(raw, err)
			a.tableStyleResolutions[key] = tableStyleResolution{status: status, err: recordedErr}
			return "", recordedErr
		}
	}
	if resolution.err != nil {
		return "", resolution.err
	}
	if resolution.candidate == nil || resolution.candidate.State == nil ||
		resolution.candidate.State.record.Status != "complete" ||
		!resolution.candidate.covers(reference.Families) {
		err := errors.New("source-verified table stylesheet replacement does not cover every affected table class family")
		_, recordedErr := a.assets.recordFailure(raw, err)
		a.tableStyleResolutions[key] = tableStyleResolution{status: resolution.status, err: recordedErr}
		return "", recordedErr
	}
	a.recordStylesheetRecovery(key, resolution.status, *resolution.candidate, reference.Families, page.topic.URL)
	return localReference(page.path, resolution.candidate.State.record.Path), nil
}

func uniqueTableStyleCandidate(candidates []tableStyleCandidate, reference tableStyleReference) *tableStyleCandidate {
	if len(candidates) != 1 || candidates[0].Basename != reference.Basename ||
		!candidates[0].covers(reference.Families) {
		return nil
	}
	return &candidates[0]
}

func permanentMissingHTTPStatus(err error) (int, bool) {
	var statusError *fetch.StatusError
	if errors.As(err, &statusError) && (statusError.Status == http.StatusNotFound || statusError.Status == http.StatusGone) {
		return statusError.Status, true
	}
	var resourceError *resourceStatusError
	if errors.As(err, &resourceError) && (resourceError.Status == http.StatusNotFound || resourceError.Status == http.StatusGone) {
		return resourceError.Status, true
	}
	return 0, false
}

func (a *htmlArchiver) recordStylesheetRecovery(
	broken string,
	status int,
	candidate tableStyleCandidate,
	families []string,
	topic string,
) {
	for index := range a.stylesheetRecoveries {
		recovery := &a.stylesheetRecoveries[index]
		if recovery.BrokenURL != broken || recovery.ReplacementURL != candidate.URL {
			continue
		}
		for _, family := range families {
			if !slices.Contains(recovery.TableStyleFamilies, family) {
				recovery.TableStyleFamilies = append(recovery.TableStyleFamilies, family)
			}
		}
		if !slices.Contains(recovery.AffectedTopics, topic) {
			recovery.AffectedTopics = append(recovery.AffectedTopics, topic)
		}
		slices.Sort(recovery.TableStyleFamilies)
		slices.Sort(recovery.AffectedTopics)
		return
	}
	record := model.StylesheetRecovery{
		BrokenURL: broken, HTTPStatus: status,
		ReplacementURL: candidate.URL, ReplacementFinalURL: candidate.State.record.FinalURL,
		ReplacementSourceSHA256: candidate.State.record.SourceSHA256,
		ReplacementSourceSize:   candidate.State.record.SourceSize,
		TableStyleFamilies:      slices.Clone(families), AffectedTopics: []string{topic},
	}
	slices.Sort(record.TableStyleFamilies)
	a.stylesheetRecoveries = append(a.stylesheetRecoveries, record)
	a.warnings = append(a.warnings, fmt.Sprintf(
		"Publisher advertised out-of-guide table stylesheet %s returned HTTP %d; used unique source-verified same-guide stylesheet %s for %s.",
		broken, status, candidate.URL, strings.Join(record.TableStyleFamilies, ", ")))
}

func cloneStylesheetRecoveries(values []model.StylesheetRecovery) []model.StylesheetRecovery {
	cloned := make([]model.StylesheetRecovery, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].BrokenDeclarations = slices.Clone(value.BrokenDeclarations)
		cloned[index].TableStyleFamilies = slices.Clone(value.TableStyleFamilies)
		cloned[index].AffectedTopics = slices.Clone(value.AffectedTopics)
	}
	return cloned
}
