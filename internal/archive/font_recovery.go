package archive

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
)

const fontFaceRecoveryKind = "unavailable-font-face-sources"

// unavailableFontError is a font source whose host refused it permanently.
// FinalURL is the URL that answered with Status. The asset manager returns
// it only for dependencyFontSource, so that the @font-face rewriter can drop
// the source. Any other use of the URL fails.
type unavailableFontError struct {
	URL      string
	FinalURL string
	Status   int
	cause    error
}

func (e *unavailableFontError) Error() string { return e.cause.Error() }
func (e *unavailableFontError) Unwrap() error { return e.cause }

// unavailableFontStatus reports the HTTP 403, 404 or 410 with which a host
// refused a font after the transport's redirects and retries, and the URL
// that answered. A robots.txt answered with 4xx places no restrictions (RFC
// 9309), so the font itself is requested; a robots.txt that cannot be read,
// robots rules that disallow the font, scope, destination, timeout,
// transient, size and body failures are not refusals and stay failures.
func unavailableFontStatus(err error) (int, string, bool) {
	var retrieval *fetch.RetrievalError
	var resource *resourceStatusError
	switch {
	case errors.As(err, &retrieval):
		statusError, _ := retrieval.Cause.(*fetch.StatusError)
		if retrieval.Stage != fetch.StageHeaders || statusError == nil {
			return 0, "", false
		}
		return statusError.Status, statusError.URL, refusedFontStatus(statusError.Status)
	case errors.As(err, &resource):
		return resource.Status, resource.URL, refusedFontStatus(resource.Status)
	}
	return 0, "", false
}

func refusedFontStatus(status int) bool {
	return status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone
}

// unavailable records a font source whose host refused it permanently. The
// state is neither a failure nor an archived asset: the rule that declared
// the source records the recovery.
func (m *assetManager) unavailable(state *assetState, status int, final string, cause error) (*assetState, error) {
	state.processing = false
	state.record.Status = "unavailable"
	state.record.FinalURL = final
	state.record.Error = cause.Error()
	state.unavailable = &unavailableFontError{URL: state.record.URL, FinalURL: final, Status: status, cause: cause}
	m.complete(state)
	return state, state.unavailable
}

// recordFontFaceRecoveries records the @font-face rules that lost sources in
// the CSS of one stylesheet, SVG document or topic page, once that CSS has
// been archived.
func (m *assetManager) recordFontFaceRecoveries(stylesheet string, recoveries []fontFaceRecovery) {
	if len(recoveries) == 0 {
		return
	}
	var families []string
	statuses := map[string]int{}
	removed := 0
	for _, recovery := range recoveries {
		record := model.StylesheetRecovery{
			Kind:      fontFaceRecoveryKind,
			BrokenURL: recovery.Dropped[0].URL, HTTPStatus: recovery.Dropped[0].HTTPStatus,
			BrokenDeclarations: slices.Clone(recovery.Dropped),
			TableStyleFamilies: []string{}, AffectedTopics: []string{},
			StylesheetURL: stylesheet, FontFamily: recovery.Family, FontFaceRemoved: recovery.Removed,
		}
		if !slices.ContainsFunc(*m.recoveries, func(existing model.StylesheetRecovery) bool {
			return existing.Kind == record.Kind && existing.StylesheetURL == record.StylesheetURL &&
				existing.FontFamily == record.FontFamily && existing.FontFaceRemoved == record.FontFaceRemoved &&
				slices.Equal(existing.BrokenDeclarations, record.BrokenDeclarations)
		}) {
			*m.recoveries = append(*m.recoveries, record)
		}
		if recovery.Removed {
			removed++
		}
		if recovery.Family != "" && !slices.Contains(families, recovery.Family) {
			families = append(families, recovery.Family)
		}
		for _, source := range recovery.Dropped {
			statuses[fmt.Sprintf("HTTP %d", source.HTTPStatus)]++
		}
	}
	slices.Sort(families)
	named := "an unnamed family"
	if len(families) > 0 {
		named = strings.Join(families, ", ")
	}
	*m.warnings = append(*m.warnings, fmt.Sprintf(
		"Publisher CSS in %s declares unavailable @font-face sources for %s: %s. Dropped them and removed %d of %d affected rules that had no source left; text falls back to the next font in each font-family stack.",
		stylesheet, named, summarizeCSSCounts(statuses), removed, len(recoveries)))
}
