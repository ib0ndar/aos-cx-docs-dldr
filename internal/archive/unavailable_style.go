package archive

import (
	"fmt"
	"slices"

	"aos-cx-docs-dldr/internal/model"
)

// unavailableStylesheetKind records a page stylesheet that the publisher
// answers with HTTP 404 or 410. Publishing tools link stylesheets that were
// never deployed, such as an Oxygen WebHelp fonts_wh.css: the publisher's own
// pages render without them, so the archived pages omit the link. Every other
// failure, including 403, 5xx, timeouts, robots rules, invalid CSS and a
// stylesheet whose own references fail, still makes the guide incomplete.
const unavailableStylesheetKind = "unavailable-stylesheet"

// servedPageStyle returns the local path of a page stylesheet, or "" when the
// publisher does not serve it and the page is archived without it.
func (a *htmlArchiver) servedPageStyle(raw string, page *pageInput) (string, error) {
	if !onlineURL(raw) {
		return a.assets.local(raw, page.baseURL, page.path, dependencyCSS)
	}
	state, err := a.assets.probe(raw, dependencyCSS, 0)
	if err == nil {
		return localReference(page.path, state.record.Path), nil
	}
	if status, missing := permanentMissingHTTPStatus(err); missing {
		return a.omitStyle(canonicalNoFragment(raw), status, page)
	}
	_, recordedErr := a.assets.recordFailure(raw, err)
	return "", recordedErr
}

// omitStyle archives page without the stylesheet key, which the publisher
// answers with status, and records the page against it.
func (a *htmlArchiver) omitStyle(key string, status int, page *pageInput) (string, error) {
	a.unavailableStyles[key] = status
	a.recordUnavailableStylesheet(key, status, page.topic.URL)
	return "", nil
}

func (a *htmlArchiver) recordUnavailableStylesheet(stylesheet string, status int, topic string) {
	for index := range a.stylesheetRecoveries {
		record := &a.stylesheetRecoveries[index]
		if record.Kind != unavailableStylesheetKind || record.BrokenURL != stylesheet {
			continue
		}
		if !slices.Contains(record.AffectedTopics, topic) {
			record.AffectedTopics = append(record.AffectedTopics, topic)
			slices.Sort(record.AffectedTopics)
		}
		return
	}
	a.stylesheetRecoveries = append(a.stylesheetRecoveries, model.StylesheetRecovery{
		Kind: unavailableStylesheetKind, BrokenURL: stylesheet, HTTPStatus: status,
		TableStyleFamilies: []string{}, AffectedTopics: []string{topic},
	})
	a.warnings = append(a.warnings, fmt.Sprintf(
		"Publisher stylesheet %s returned HTTP %d; the pages that link it are archived without it, as the publisher's own pages render.",
		stylesheet, status))
}
