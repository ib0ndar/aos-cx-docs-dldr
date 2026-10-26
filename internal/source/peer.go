package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/pdfcheck"
)

// A publisher mapping can name a document the publisher does not serve. The
// 10.16 CoPP mapping sends 8325 to "acopp_8100-83xx-9300-10000", which answers
// 404, while 8100, 8320, 8360, 9300 and 10000, the platforms that share 8325's
// CoPP Guide in 10.15 and in 10.17, map to "copp_8100-83xx-9300-10000", whose
// HTML answers 404 as well. The publisher serves that guide only as a PDF at
// the route its adapter uses for the release's PDFs. The 10.16 Introduction
// to the Web UI Guide for 4100i, 5420 and the 6000 to 6400 series is mapped
// to "web_ui_4100i-5420-6xxx", whose HTML answers 404 while the release's PDF
// of that name is served.
//
// FindReplacements derives candidates from the publisher's own data and
// nothing else: the target the platform's peers map to, and the release's PDF
// route for the platform's own and its peers' target. They are only
// candidates: the caller tries them in order when the mapped source answers
// 404 or 410, and uses the first the publisher serves. A PDF candidate counts
// only when the PDF's own title names the guide and, for the peers' target,
// the release and the platform; see CheckPDFTitle.

const (
	// RouteOriginPeerMapping marks a document whose route comes from the
	// mapping of the platform's peers instead of its own.
	RouteOriginPeerMapping = "publisher-peer-mapping"
	// RouteOriginReleasePDF marks a PDF of the platform's own target at the
	// route the publisher's adapter uses for the release's PDFs.
	RouteOriginReleasePDF = "publisher-release-pdf"
	// RouteOriginPeerReleasePDF marks a PDF of the target the platform's
	// peers map to, at that route.
	RouteOriginPeerReleasePDF = "publisher-peer-release-pdf"
)

// IsReleasePDF reports whether d is a release PDF candidate, which only its
// own title identifies; see CheckPDFTitle.
func IsReleasePDF(d model.Document) bool {
	return d.RouteOrigin == RouteOriginReleasePDF || d.RouteOrigin == RouteOriginPeerReleasePDF
}

// Replacement is a candidate for a mapped document the publisher does not
// serve.
type Replacement struct {
	// Mapped is the document the publisher maps for the platform.
	Mapped model.Document
	// Document is the candidate, resolved from Target.
	Document model.Document
	Target   string
	// Peers are the platforms that share the platform's document in
	// Releases, the nearest mapped releases before and after, and map to
	// Target here. They are empty when Target is the platform's own.
	Peers    []string
	Releases []string
	// Title is a PDF candidate's own title, once CheckPDFTitle accepted it.
	Title string
}

// Note describes the replacement for messages and the release manifest.
func (r Replacement) Note() string {
	note := fmt.Sprintf("%s for %s %s: the publisher maps %s, which it does not serve; using %s instead",
		r.Mapped.Title, r.Mapped.Platform, r.Mapped.Version, r.Mapped.URL, r.Document.URL)
	if len(r.Peers) > 0 {
		note += fmt.Sprintf(", from the mapping of %s, which share %s's document in %s",
			strings.Join(r.Peers, ", "), r.Mapped.Platform, strings.Join(r.Releases, " and "))
	}
	if IsReleasePDF(r.Document) {
		note += fmt.Sprintf(", the publisher's PDF of %s at its route for %s PDFs, titled %q",
			r.Target, r.Mapped.Version, strings.TrimSpace(r.Title))
	}
	return note
}

// FindReplacements returns the candidates for document d in the order to try
// them: the HTML document of the platform's peers, then the release's PDF of
// the platform's own target, then that of its peers' target.
func FindReplacements(c model.Catalog, d model.Document) []Replacement {
	index := slices.IndexFunc(c.Guides, func(g model.Guide) bool { return g.ID == d.ID })
	if index < 0 || c.Guides[index].Error != "" {
		return nil
	}
	guide := c.Guides[index]
	own, mapped := guide.Mappings[d.Version][d.Platform]
	if !mapped {
		return nil
	}
	var replacements []Replacement
	target, peers, releases, found := peerTarget(c, guide, d)
	if found {
		if document, err := resolve(guide, d.Platform, d.Version, target, c.RouteRoot); err == nil && document.URL != d.URL {
			document.RouteOrigin = RouteOriginPeerMapping
			replacements = append(replacements, Replacement{
				Mapped: d, Document: document, Target: target, Peers: peers, Releases: releases,
			})
		}
	}
	if d.Kind == "pdf" || d.RouteOrigin != "adapter-derived-book-stem" {
		return replacements
	}
	candidates := []Replacement{{Mapped: d, Target: own}}
	if found {
		candidates = append(candidates, Replacement{Mapped: d, Target: target, Peers: peers, Releases: releases})
	}
	for _, candidate := range candidates {
		origin := RouteOriginReleasePDF
		if len(candidate.Peers) > 0 {
			origin = RouteOriginPeerReleasePDF
		}
		document, ok := releasePDF(guide, d, candidate.Target, c.RouteRoot, origin)
		if !ok || slices.ContainsFunc(replacements, func(r Replacement) bool { return r.Document.URL == document.URL }) {
			continue
		}
		candidate.Document = document
		replacements = append(replacements, candidate)
	}
	return replacements
}

// peerTarget returns the target the platform's peers map to in d's release:
// in each nearest release before and after it that maps d's platform, other
// platforms share its document, and all of them that are mapped in d's
// release map to one other target.
func peerTarget(c model.Catalog, guide model.Guide, d model.Document) (string, []string, []string, bool) {
	before, after := "", ""
	for release, platforms := range guide.Mappings {
		if _, ok := platforms[d.Platform]; !ok || release == d.Version {
			continue
		}
		switch order := compareRelease(release, d.Version); {
		case order < 0 && (before == "" || compareRelease(release, before) > 0):
			before = release
		case order > 0 && (after == "" || compareRelease(release, after) < 0):
			after = release
		}
	}
	peers := map[string]bool{}
	var releases []string
	for _, release := range []string{before, after} {
		if release == "" {
			continue
		}
		own, shared := guide.Mappings[release][d.Platform], false
		for platform, other := range guide.Mappings[release] {
			if platform != d.Platform && other == own {
				peers[platform], shared = true, true
			}
		}
		if !shared {
			return "", nil, nil, false
		}
		releases = append(releases, release)
	}
	replacement := ""
	var agreeing []string
	for platform := range peers {
		other, ok := guide.Mappings[d.Version][platform]
		if !ok {
			continue
		}
		if replacement != "" && other != replacement {
			return "", nil, nil, false
		}
		replacement = other
		agreeing = append(agreeing, platform)
	}
	if replacement == "" || replacement == guide.Mappings[d.Version][d.Platform] {
		return "", nil, nil, false
	}
	slices.SortFunc(agreeing, func(a, b string) int { return cmpPlatform(c.Platforms, a, b) })
	slices.SortFunc(releases, compareRelease)
	return replacement, agreeing, releases, true
}

// releasePDF resolves target at the route the publisher's adapter uses for
// PDFs in d's release, its Command Line Interface Guide route. Releases that
// route that guide to HTML or to archived PDFs have none.
func releasePDF(guide model.Guide, d model.Document, target, root, origin string) (model.Document, bool) {
	if compareRelease(d.Version, "10.12") <= 0 {
		return model.Document{}, false
	}
	route, err := resolve(model.Guide{ID: "cli"}, d.Platform, d.Version, target, root)
	if err != nil || route.Kind != "pdf" {
		return model.Document{}, false
	}
	document := model.Document{ID: guide.ID, Title: guide.Title, Platform: d.Platform, Version: d.Version,
		URL: route.URL, Kind: "pdf", RouteOrigin: origin}
	return document, true
}

// CheckPDFTitle accepts release PDF d only when its own title names the
// guide: every distinctive word of the guide's title, read in the singular,
// appears in it. A PDF of the platform's own target needs no more, as the
// publisher's mapping and route place it in the release for the platform; a
// PDF of its peers' target must also name the release and the platform.
func CheckPDFTitle(d model.Document, title string) error {
	words := " " + singularWords(title) + " "
	for _, word := range strings.Fields(singularWords(d.Title)) {
		if !genericTitleWords[word] && !strings.Contains(words, " "+word+" ") {
			return fmt.Errorf("publisher PDF titled %q does not name %q", strings.TrimSpace(title), d.Title)
		}
	}
	if d.RouteOrigin != RouteOriginPeerReleasePDF {
		return nil
	}
	exact := " " + titleWords(title) + " "
	for _, part := range []string{d.Version, d.Platform} {
		if !strings.Contains(exact, " "+titleWords(part)+" ") {
			return fmt.Errorf("publisher PDF titled %q does not name %q", strings.TrimSpace(title), part)
		}
	}
	return nil
}

// genericTitleWords do not tell one guide from another.
var genericTitleWords = map[string]bool{
	"a": true, "an": true, "and": true, "for": true, "guide": true, "introduction": true,
	"of": true, "the": true, "to": true,
}

func titleWords(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// singularWords is titleWords with plural endings removed, so that
// "Classifier Policies" names "Classifiers Policy".
func singularWords(value string) string {
	words := strings.Fields(titleWords(value))
	for i, word := range words {
		switch {
		case len(word) > 4 && strings.HasSuffix(word, "ies"):
			words[i] = strings.TrimSuffix(word, "ies") + "y"
		case len(word) > 3 && strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss"):
			words[i] = strings.TrimSuffix(word, "s")
		}
	}
	return strings.Join(words, " ")
}

// ProbeReleasePDF retrieves a release PDF candidate through the raw cache and
// reports it available only when it is a complete PDF whose title names the
// guide, release and platform. It returns that title.
func ProbeReleasePDF(ctx context.Context, fetcher model.Fetcher, d model.Document) (model.SourceAvailability, string, error) {
	availability := model.SourceAvailability{URL: d.URL, ProbeURL: d.URL, Format: sourceFormat(d)}
	if fetcher == nil {
		availability.Reason = "source availability requires a raw-cache fetcher"
		return availability, "", nil
	}
	scope := DocumentScope(d)
	resource, err := fetcher.Get(fetch.WithRequestScope(ctx, scope), d.URL, true)
	if err != nil {
		availability, err = classifySourceFailure(ctx, availability, err)
		return availability, "", err
	}
	defer resource.Body.Close()
	availability.Checked = true
	availability.FinalURL = resource.URL
	if resource.Status != 200 {
		availability.Reason = fmt.Sprintf("publisher PDF returned HTTP %d", resource.Status)
		return availability, "", nil
	}
	if err := scope(resource.URL); err != nil {
		availability.Reason = err.Error()
		return availability, "", nil
	}
	file, ok := resource.Body.(interface {
		io.ReaderAt
		Stat() (os.FileInfo, error)
	})
	if !ok {
		return availability, "", errors.New("raw-cache fetcher did not return a file-backed PDF")
	}
	info, err := file.Stat()
	if err != nil {
		return availability, "", err
	}
	if err := pdfcheck.Validate(file, info.Size(), resource.Headers.Get("Content-Type")); err != nil {
		availability.Reason = "publisher PDF is not complete: " + err.Error()
		return availability, "", nil
	}
	title, err := pdfcheck.DocumentTitle(file, info.Size())
	if err == nil {
		err = CheckPDFTitle(d, title)
	}
	if err != nil {
		availability.Reason = "publisher PDF is not identified as this guide: " + err.Error()
		return availability, "", nil
	}
	availability.Available = true
	return availability, title, nil
}

// cmpPlatform orders platforms as the portal's platform selector lists them.
func cmpPlatform(order []string, a, b string) int {
	i, j := slices.Index(order, a), slices.Index(order, b)
	switch {
	case i < 0 && j < 0:
		return strings.Compare(a, b)
	case i < 0:
		return 1
	case j < 0:
		return -1
	}
	return i - j
}
