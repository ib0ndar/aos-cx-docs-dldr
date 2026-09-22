package archive

import (
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

const ordinaryStylesheetRecoveryKind = "same-page-redundant-stylesheet"

type ordinaryStyleReference struct {
	URL      string
	Basename string
}

type ordinaryStyleRecovery struct {
	Broken    []model.BrokenStylesheetReference
	Candidate ordinaryStyleReference
	State     *assetState
}

type ordinaryStyleResolution struct {
	CandidateURL string
	Status       int
	Err          error
}

type ordinaryStyleProbe struct {
	State  *assetState
	Status int
	Err    error
}

func (a *htmlArchiver) collectOrdinaryStyleEvidence(doc *html.Node, page *pageInput) {
	walk(doc, func(node *html.Node) {
		if node.Type != html.ElementNode || node.Data != "link" || attr(node, "disabled") != "" ||
			!slices.Contains(strings.Fields(strings.ToLower(attr(node, "rel"))), "stylesheet") ||
			strings.EqualFold(strings.TrimSpace(attr(node, "data-mc-stylesheet-type")), "table") {
			return
		}
		resolved, err := resolveReference(page.baseURL, attr(node, "href"))
		if err != nil {
			return
		}
		if strings.EqualFold(attr(node, "data-mc-generated"), "true") &&
			strings.Contains(strings.ToLower(mustURL(resolved).Path), "/skins/") {
			return
		}
		basename, ok := stylesheetBasename(resolved)
		if !ok {
			return
		}
		reference := ordinaryStyleReference{URL: canonicalNoFragment(resolved), Basename: basename}
		if !slices.Contains(page.ordinaryStyles, reference) {
			page.ordinaryStyles = append(page.ordinaryStyles, reference)
		}
	})
}

func stylesheetBasename(raw string) (string, bool) {
	parsed := mustURL(raw)
	if parsed == nil {
		return "", false
	}
	basename := path.Base(parsed.Path)
	if basename == "" || basename == "." || basename == "/" || path.Ext(basename) != ".css" {
		return "", false
	}
	return basename, true
}

func (a *htmlArchiver) prepareOrdinaryStyleRecoveries() error {
	if a.plan.Document.Kind != "flare" {
		return nil
	}
	for _, page := range a.pages {
		groups := map[string][]ordinaryStyleReference{}
		folded := map[string]string{}
		collisions := map[string]bool{}
		for _, reference := range page.ordinaryStyles {
			groups[reference.Basename] = append(groups[reference.Basename], reference)
			fold := strings.ToLower(reference.Basename)
			if previous, found := folded[fold]; found && previous != reference.Basename {
				collisions[fold] = true
			} else {
				folded[fold] = reference.Basename
			}
		}
		for basename, references := range groups {
			if len(references) < 2 || collisions[strings.ToLower(basename)] ||
				!sameOrdinaryStyleQuery(references) {
				continue
			}
			valid := true
			var candidates []ordinaryStyleReference
			var broken []model.BrokenStylesheetReference
			for _, reference := range references {
				if !a.ordinaryGuideURL(reference.URL) {
					valid = false
					break
				}
				probe := a.probeOrdinaryStyle(reference.URL)
				switch {
				case probe.Err == nil &&
					a.verifiedOrdinaryStyleCandidate(reference.URL, probe.State):
					candidates = append(candidates, reference)
				case probe.Status == http.StatusNotFound || probe.Status == http.StatusGone:
					broken = append(broken, model.BrokenStylesheetReference{
						URL: reference.URL, HTTPStatus: probe.Status,
					})
				default:
					valid = false
				}
			}
			if !valid || len(candidates) != 1 || len(broken) == 0 {
				continue
			}
			candidate := candidates[0]
			state := a.ordinaryStyleProbes[candidate.URL].State
			recovery := ordinaryStyleRecovery{
				Broken: slices.Clone(broken), Candidate: candidate, State: state,
			}
			if page.ordinaryStyleRecoveries == nil {
				page.ordinaryStyleRecoveries = map[string]ordinaryStyleRecovery{}
			}
			for _, reference := range broken {
				key := canonicalNoFragment(reference.URL)
				page.ordinaryStyleRecoveries[key] = recovery
				a.ordinaryStyleResolutions[key] = ordinaryStyleResolution{
					CandidateURL: candidate.URL, Status: reference.HTTPStatus,
				}
			}
		}
	}
	return nil
}

func sameOrdinaryStyleQuery(references []ordinaryStyleReference) bool {
	if len(references) == 0 {
		return false
	}
	first := mustURL(references[0].URL)
	if first == nil {
		return false
	}
	for _, reference := range references[1:] {
		parsed := mustURL(reference.URL)
		if parsed == nil || parsed.RawQuery != first.RawQuery {
			return false
		}
	}
	return true
}

func (a *htmlArchiver) probeOrdinaryStyle(raw string) ordinaryStyleProbe {
	key := canonicalNoFragment(raw)
	if probe, found := a.ordinaryStyleProbes[key]; found {
		return probe
	}
	state, err := a.assets.probe(raw, dependencyCSS, 0)
	status, _ := permanentMissingHTTPStatus(err)
	probe := ordinaryStyleProbe{State: state, Status: status, Err: err}
	a.ordinaryStyleProbes[key] = probe
	return probe
}

func (a *htmlArchiver) localOrdinaryPageStyle(
	raw string,
	page *pageInput,
	recovery ordinaryStyleRecovery,
) (string, error) {
	key := canonicalNoFragment(raw)
	resolution, seen := a.ordinaryStyleResolutions[key]
	if !seen {
		state, err := a.assets.probe(raw, dependencyCSS, 0)
		if err == nil {
			return localReference(page.path, state.record.Path), nil
		}
		status, missing := permanentMissingHTTPStatus(err)
		if !missing {
			_, recordedErr := a.assets.recordFailure(raw, err)
			a.ordinaryStyleResolutions[key] = ordinaryStyleResolution{Err: recordedErr}
			return "", recordedErr
		}
		resolution = ordinaryStyleResolution{
			CandidateURL: recovery.Candidate.URL,
			Status:       status,
		}
		a.ordinaryStyleResolutions[key] = resolution
	}
	if resolution.Err != nil {
		return "", resolution.Err
	}
	if resolution.CandidateURL != recovery.Candidate.URL {
		cause := fmt.Errorf("same-page stylesheet recovery candidate is ambiguous for %s", raw)
		_, recordedErr := a.assets.recordFailure(raw, cause)
		a.ordinaryStyleResolutions[key] = ordinaryStyleResolution{Err: recordedErr}
		return "", recordedErr
	}
	if recovery.State == nil || !a.verifiedOrdinaryStyleCandidate(recovery.Candidate.URL, recovery.State) {
		cause := fmt.Errorf("same-page stylesheet recovery candidate is no longer valid for %s", raw)
		_, recordedErr := a.assets.recordFailure(raw, cause)
		a.ordinaryStyleResolutions[key] = ordinaryStyleResolution{Err: recordedErr}
		return "", recordedErr
	}
	a.recordOrdinaryStylesheetRecovery(recovery, page.topic.URL)
	return localReference(page.path, recovery.State.record.Path), nil
}

func (a *htmlArchiver) ordinaryGuideLocalURL(raw string) bool {
	parsed := mustURL(raw)
	for _, root := range a.guideRoots {
		base := mustURL(root)
		contentPath := strings.TrimSuffix(base.Path, "/") + "/Content/"
		if parsed.Scheme == base.Scheme && parsed.Host == base.Host &&
			strings.HasPrefix(parsed.Path, contentPath) {
			return true
		}
	}
	return false
}

func (a *htmlArchiver) ordinaryGuideURL(raw string) bool {
	parsed := mustURL(raw)
	if parsed == nil {
		return false
	}
	for _, root := range a.guideRoots {
		base := mustURL(root)
		rootPath := strings.TrimSuffix(base.Path, "/") + "/"
		if parsed.Scheme == base.Scheme && parsed.Host == base.Host &&
			strings.HasPrefix(parsed.Path, rootPath) {
			return true
		}
	}
	return false
}

func (a *htmlArchiver) verifiedOrdinaryStyleCandidate(requested string, state *assetState) bool {
	basename, ok := stylesheetBasename(requested)
	if state == nil || state.record.Status != "complete" ||
		!ok ||
		media(state.record.ContentType) != "text/css" ||
		!a.ordinaryGuideLocalURL(requested) ||
		!a.ordinaryGuideLocalURL(state.record.FinalURL) {
		return false
	}
	finalBasename, finalOK := stylesheetBasename(state.record.FinalURL)
	requestedKey, requestedErr := fetch.CanonicalURL(requested)
	finalKey, finalErr := fetch.CanonicalURL(state.record.FinalURL)
	return requestedErr == nil && finalErr == nil && requestedKey == finalKey &&
		finalOK && finalBasename == basename &&
		state.record.SourceSHA256 != "" && state.record.SourceSize > 0
}

func (a *htmlArchiver) recordOrdinaryStylesheetRecovery(
	recovery ordinaryStyleRecovery,
	topic string,
) {
	for index := range a.stylesheetRecoveries {
		record := &a.stylesheetRecoveries[index]
		if record.Kind != ordinaryStylesheetRecoveryKind ||
			record.ReplacementURL != recovery.Candidate.URL ||
			!slices.Equal(record.BrokenDeclarations, recovery.Broken) {
			continue
		}
		if !slices.Contains(record.AffectedTopics, topic) {
			record.AffectedTopics = append(record.AffectedTopics, topic)
			slices.Sort(record.AffectedTopics)
		}
		return
	}
	first := recovery.Broken[0]
	record := model.StylesheetRecovery{
		Kind:      ordinaryStylesheetRecoveryKind,
		BrokenURL: first.URL, HTTPStatus: first.HTTPStatus,
		BrokenDeclarations:      slices.Clone(recovery.Broken),
		ReplacementURL:          recovery.Candidate.URL,
		ReplacementFinalURL:     recovery.State.record.FinalURL,
		ReplacementSourceSHA256: recovery.State.record.SourceSHA256,
		ReplacementSourceSize:   recovery.State.record.SourceSize,
		TableStyleFamilies:      []string{}, AffectedTopics: []string{topic},
	}
	a.stylesheetRecoveries = append(a.stylesheetRecoveries, record)
	identities := make([]string, 0, len(recovery.Broken))
	for _, reference := range recovery.Broken {
		identities = append(identities, fmt.Sprintf("%s (HTTP %d)", reference.URL, reference.HTTPStatus))
	}
	a.warnings = append(a.warnings, fmt.Sprintf(
		"Publisher advertised redundant same-page stylesheets %s; used the exact same-page source-verified same-guide stylesheet %s.",
		strings.Join(identities, ", "), recovery.Candidate.URL))
}
