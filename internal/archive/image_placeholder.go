package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"golang.org/x/net/html"
)

const (
	maxMissingResourceMessage = 1024
	maxPlaceholderText        = 120
)

type rasterFailure struct {
	requested string
	final     string
	class     string
	status    int
	stage     string
	attempts  int
	retries   int
	message   string
	observed  string
	expected  string
}

type imageCandidate struct {
	raw      string
	expected string
}

func (a *htmlArchiver) rewriteImageSlots(content *html.Node, page *pageInput) error {
	var slots []*html.Node
	walk(content, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		if node.Data == "picture" || node.Data == "img" && !hasAncestor(node, "picture") {
			slots = append(slots, node)
		}
	})
	for _, slot := range slots {
		if slot.Parent == nil {
			continue
		}
		handled, err := a.rewriteImageSlot(slot, page)
		if err != nil {
			return err
		}
		if handled {
			continue
		}
	}
	return nil
}

func (a *htmlArchiver) rewriteImageSlot(slot *html.Node, page *pageInput) (bool, error) {
	image := slot
	elementType := "img"
	if slot.Data == "picture" {
		elementType = "picture"
		image = findNode(slot, func(node *html.Node) bool {
			return node.Type == html.ElementNode && node.Data == "img"
		})
		if image == nil {
			return false, nil
		}
	}
	candidates, rasterOnly, err := imageSlotCandidates(slot, image)
	if err != nil {
		return true, err
	}
	if !rasterOnly || len(candidates) == 0 {
		return false, nil
	}
	var failures []rasterFailure
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToLower(candidate.raw), "data:") {
			normalizeImageSlot(slot, image, candidate.raw)
			markImageSlotProcessed(slot, image)
			return true, nil
		}
		resolved, err := resolveReference(page.baseURL, candidate.raw)
		if err != nil {
			return true, err
		}
		state, failure, err := a.assets.tryRaster(resolved, candidate.expected)
		if err != nil {
			return true, err
		}
		if state != nil {
			normalizeImageSlot(slot, image, localReference(page.path, state.record.Path))
			markImageSlotProcessed(slot, image)
			return true, nil
		}
		failures = append(failures, failure)
	}
	if len(failures) == 0 {
		return false, nil
	}
	placeholderID := "image-placeholder-" + sourceHash([]byte(
		page.topic.URL + "\n" + strings.Join(failureURLs(failures), "\n") + "\n" + strconv.Itoa(len(a.missingResources)),
	))[:16]
	alt := boundedText(attr(image, "alt"), 256)
	title := boundedText(attr(image, "title"), 256)
	ariaLabel := boundedText(attr(image, "aria-label"), 256)
	caption := imageCaption(slot)
	width, height := declaredImageDimensions(image)
	placeholder := imagePlaceholderNode(
		placeholderID, failures[0], alt, title, ariaLabel, width, height,
	)
	replaceNode(slot, placeholder)
	for _, failure := range failures {
		record := model.MissingResource{
			RequestedURL: failure.requested, FinalURL: failure.final,
			ReferringTopicURL: page.topic.URL, GeneratedPagePath: page.path,
			FailureClass: failure.class, HTTPStatus: failure.status,
			RetrievalStage: failure.stage, ApplicationAttempts: failure.attempts,
			ApplicationRetries: failure.retries,
			Message:            boundedText(failure.message, maxMissingResourceMessage),
			ElementType:        elementType, Alt: alt, Title: title, Caption: caption,
			DeclaredWidth: width, DeclaredHeight: height,
			ExpectedRasterType: failure.expected,
			PlaceholderID:      placeholderID, ObservedAt: failure.observed,
		}
		a.missingResources = append(a.missingResources, record)
		a.warnings = append(a.warnings, fmt.Sprintf(
			"Image unavailable at %s (%s); generated an offline placeholder.",
			failure.requested, failure.class,
		))
	}
	return true, nil
}

func imageSlotCandidates(slot, image *html.Node) ([]imageCandidate, bool, error) {
	var result []imageCandidate
	rasterOnly := true
	add := func(raw, declared string) error {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return errors.New("declared image source is empty")
		}
		if strings.HasPrefix(strings.ToLower(raw), "data:") {
			result = append(result, imageCandidate{raw: raw, expected: "data"})
			return nil
		}
		expected, ok := rasterType(raw, declared)
		if !ok {
			rasterOnly = false
			return nil
		}
		candidate := imageCandidate{raw: raw, expected: expected}
		if !slices.Contains(result, candidate) {
			result = append(result, candidate)
		}
		return nil
	}
	if slot.Data == "picture" {
		for child := slot.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != html.ElementNode || child.Data != "source" {
				continue
			}
			declared := attr(child, "type")
			for _, key := range []string{"src", "srcset", "data-srcset"} {
				value, found := attrFound(child, key)
				if !found {
					continue
				}
				if key == "src" {
					if err := add(value, declared); err != nil {
						return nil, true, err
					}
					continue
				}
				parsed, err := parseSrcset(value)
				if err != nil {
					return nil, true, err
				}
				for _, candidate := range parsed {
					if err := add(candidate[0], declared); err != nil {
						return nil, true, err
					}
				}
			}
		}
	}
	for _, key := range []string{"data-original", "data-mc-popup-src", "data-src", "src"} {
		value, found := attrFound(image, key)
		if !found {
			continue
		}
		if err := add(value, attr(image, "type")); err != nil {
			return nil, true, err
		}
	}
	for _, key := range []string{"data-srcset", "srcset"} {
		value, found := attrFound(image, key)
		if !found {
			continue
		}
		parsed, err := parseSrcset(value)
		if err != nil {
			return nil, true, err
		}
		for _, candidate := range parsed {
			if err := add(candidate[0], attr(image, "type")); err != nil {
				return nil, true, err
			}
		}
	}
	if len(result) == 0 {
		return nil, false, nil
	}
	if !rasterOnly {
		return nil, false, nil
	}
	for _, candidate := range result {
		if candidate.expected == "" {
			return nil, false, nil
		}
	}
	return result, true, nil
}

func rasterType(raw, declared string) (string, bool) {
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	declaredKinds := map[string]string{
		"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif",
		"image/webp": "webp", "image/avif": "avif",
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	extension := strings.ToLower(path.Ext(parsed.Path))
	extensionKind := ""
	switch extension {
	case ".png":
		extensionKind = "png"
	case ".jpg", ".jpeg":
		extensionKind = "jpeg"
	case ".gif":
		extensionKind = "gif"
	case ".webp":
		extensionKind = "webp"
	case ".avif":
		extensionKind = "avif"
	}
	if declared != "" {
		declaredKind := declaredKinds[declared]
		if declaredKind == "" || extension == ".svg" ||
			(extensionKind != "" && extensionKind != declaredKind) {
			return "", false
		}
		return declaredKind, true
	}
	if extensionKind == "" {
		return "", false
	}
	return extensionKind, true
}

func (m *assetManager) tryRaster(raw, expected string) (*assetState, rasterFailure, error) {
	if err := m.ctx.Err(); err != nil {
		return nil, rasterFailure{}, err
	}
	key, err := fetch.CanonicalURL(raw)
	if err != nil {
		return nil, rasterFailure{}, err
	}
	cacheKey := key + "\x00" + expected
	if failure, ok := m.imageFailures[cacheKey]; ok {
		return nil, failure, nil
	}
	if existing := m.states[key]; existing != nil && existing.record.Status == "complete" {
		if classifyAsset(key, existing.record.ContentType, existing.sourceBody, dependencyAsset) != expected {
			return nil, rasterFailure{}, fmt.Errorf("image candidate type is ambiguous: %s", key)
		}
		return existing, rasterFailure{}, nil
	}
	resource, body, err := readResource(m.ctx, m.fetcher, key, m.refresh, m.maxBytes)
	if err != nil {
		failure, eligible := eligibleRasterFailure(key, err)
		if !eligible {
			if _, recordErr := m.recordFailure(key, err); recordErr != nil {
				return nil, rasterFailure{}, recordErr
			}
			return nil, rasterFailure{}, err
		}
		failure.expected = expected
		m.imageFailures[cacheKey] = failure
		m.reportMissingRaster(key, failure)
		return nil, failure, nil
	}
	if !sameOrigin(key, resource.URL) {
		return nil, rasterFailure{}, fmt.Errorf("image redirect left selected source origin: %s -> %s", key, resource.URL)
	}
	actualMedia := media(resource.Headers.Get("Content-Type"))
	if !rasterMIMEMatches(expected, actualMedia) {
		if err := m.budget.add(int64(len(body))); err != nil {
			return nil, rasterFailure{}, errors.New("archive aggregate source-byte limit exceeded")
		}
		failure := newRasterFailure(key, resource.URL, "mime-mismatch", 0, "", 1, 0,
			fmt.Sprintf("expected %s image, received %s", expected, actualMedia))
		failure.expected = expected
		m.imageFailures[cacheKey] = failure
		m.reportMissingRaster(key, failure)
		return nil, failure, nil
	}
	if err := validateBinaryAsset(expected, body); err != nil {
		if budgetErr := m.budget.add(int64(len(body))); budgetErr != nil {
			return nil, rasterFailure{}, errors.New("archive aggregate source-byte limit exceeded")
		}
		failure := newRasterFailure(key, resource.URL, "invalid-raster", 0, "", 1, 0, err.Error())
		failure.expected = expected
		m.imageFailures[cacheKey] = failure
		m.reportMissingRaster(key, failure)
		return nil, failure, nil
	}

	if len(m.states) >= maxHTMLAssets {
		return nil, rasterFailure{}, errors.New("asset inventory exceeds limit")
	}
	state, err := m.installLoaded(key, resource, body, dependencyAsset, 0)
	if err != nil {
		return nil, rasterFailure{}, err
	}
	return state, rasterFailure{}, nil
}

func (m *assetManager) reportMissingRaster(key string, failure rasterFailure) {
	if m.states[key] != nil {
		return
	}
	state := m.install(key)
	state.record.FinalURL = failure.final
	state.record.Status = "substituted"
	state.record.Error = failure.message
	m.complete(state)
}

func eligibleRasterFailure(requested string, err error) (rasterFailure, bool) {
	if errors.Is(err, context.Canceled) {
		return rasterFailure{}, false
	}
	var retrieval *fetch.RetrievalError
	if errors.As(err, &retrieval) {
		if retrieval.Stage == fetch.StageScope || errors.Is(retrieval.Cause, context.Canceled) {
			return rasterFailure{}, false
		}
		class := ""
		status := 0
		var statusError *fetch.StatusError
		switch {
		case errors.As(retrieval.Cause, &statusError) &&
			(statusError.Status == http.StatusNotFound || statusError.Status == http.StatusGone):
			class, status = fmt.Sprintf("http-%d", statusError.Status), statusError.Status
		case errors.Is(retrieval.Cause, context.DeadlineExceeded):
			class = "timeout"
		case errors.Is(retrieval.Cause, io.ErrUnexpectedEOF) ||
			strings.Contains(strings.ToLower(retrieval.Cause.Error()), "unexpected eof"):
			class = "incomplete-body"
		case retrieval.ApplicationRetries > 0:
			class = "retry-exhausted"
		default:
			return rasterFailure{}, false
		}
		return newRasterFailure(
			requested, retrieval.FinalURL, class, status, string(retrieval.Stage),
			retrieval.ApplicationAttempts, retrieval.ApplicationRetries, retrieval.Error(),
		), true
	}
	var statusError *resourceStatusError
	if errors.As(err, &statusError) &&
		(statusError.Status == http.StatusNotFound || statusError.Status == http.StatusGone) {
		return newRasterFailure(
			requested, statusError.URL, fmt.Sprintf("http-%d", statusError.Status),
			statusError.Status, "headers", 1, 0, statusError.Error(),
		), true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newRasterFailure(requested, requested, "timeout", 0, "body", 1, 0, err.Error()), true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) ||
		strings.Contains(strings.ToLower(err.Error()), "unexpected eof") {
		return newRasterFailure(requested, requested, "incomplete-body", 0, "body", 1, 0, err.Error()), true
	}
	return rasterFailure{}, false
}

func newRasterFailure(
	requested, final, class string,
	status int,
	stage string,
	attempts, retries int,
	message string,
) rasterFailure {
	if final == "" {
		final = requested
	}
	return rasterFailure{
		requested: requested, final: final, class: class, status: status,
		stage: stage, attempts: attempts, retries: retries,
		message:  boundedText(message, maxMissingResourceMessage),
		observed: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func rasterMIMEMatches(expected, actual string) bool {
	if actual == "" || actual == "application/octet-stream" || actual == "binary/octet-stream" {
		return true
	}
	switch expected {
	case "png":
		return actual == "image/png"
	case "jpeg":
		return actual == "image/jpeg"
	case "gif":
		return actual == "image/gif"
	case "webp":
		return actual == "image/webp"
	case "avif":
		return actual == "image/avif"
	default:
		return false
	}
}

func sameOrigin(left, right string) bool {
	a, errA := url.Parse(left)
	b, errB := url.Parse(right)
	return errA == nil && errB == nil &&
		strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func normalizeImageSlot(slot, image *html.Node, local string) {
	if slot.Data == "picture" {
		for child := slot.FirstChild; child != nil; {
			next := child.NextSibling
			if child.Type == html.ElementNode && child.Data == "source" {
				source := strings.TrimSpace(attr(child, "src"))
				srcset := strings.TrimSpace(attr(child, "srcset"))
				if !strings.HasPrefix(strings.ToLower(source), "data:") &&
					!strings.HasPrefix(strings.ToLower(srcset), "data:") {
					detach(child)
				}
			}
			child = next
		}
	}
	setAttr(image, "src", local)
	for _, key := range []string{
		"srcset", "data-srcset", "sizes", "data-original", "data-mc-popup-src", "data-src",
	} {
		removeAttr(image, key)
	}
}

func imagePlaceholderNode(
	id string,
	failure rasterFailure,
	alt, title, ariaLabel string,
	width, height int,
) *html.Node {
	identity := compactImageIdentity(failure.requested)
	text := "Image unavailable - " + failure.class
	if identity != "" {
		text += " (" + identity + ")"
	}
	text = boundedText(text, maxPlaceholderText)
	accessible := ariaLabel
	if accessible == "" {
		accessible = alt
	}
	label := text
	if accessible != "" {
		label = accessible + ". " + text
	}
	tooltip := boundedText(failure.message, 512)
	if title != "" {
		tooltip = title + ". " + tooltip
	}
	class := "archive-image-placeholder"
	if width > 0 && width < 96 || height > 0 && height < 48 {
		class += " archive-image-placeholder-small"
	}
	node := &html.Node{Type: html.ElementNode, Data: "span", Attr: []html.Attribute{
		{Key: "id", Val: id},
		{Key: "class", Val: class},
		{Key: "role", Val: "img"},
		{Key: "aria-label", Val: label},
		{Key: "title", Val: tooltip},
	}}
	if width > 0 || height > 0 {
		var style []string
		if width > 0 {
			style = append(style, "width:"+strconv.Itoa(min(width, 960))+"px")
		}
		if height > 0 {
			style = append(style, "height:"+strconv.Itoa(min(height, 480))+"px")
		}
		node.Attr = append(node.Attr, html.Attribute{Key: "style", Val: strings.Join(style, ";")})
	}
	node.AppendChild(&html.Node{Type: html.TextNode, Data: text})
	return node
}

func compactImageIdentity(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return boundedText(raw, 72)
	}
	return boundedText(parsed.Host+"/.../"+path.Base(parsed.Path), 72)
}

func declaredImageDimensions(image *html.Node) (int, int) {
	parse := func(value string) int {
		value = strings.TrimSpace(strings.TrimSuffix(value, "px"))
		number, err := strconv.Atoi(value)
		if err != nil || number < 1 || number > 100_000 {
			return 0
		}
		return number
	}
	return parse(attr(image, "width")), parse(attr(image, "height"))
}

func imageCaption(slot *html.Node) string {
	for parent := slot.Parent; parent != nil; parent = parent.Parent {
		if parent.Type != html.ElementNode {
			continue
		}
		if parent.Data == "figure" {
			caption := findNode(parent, func(node *html.Node) bool {
				return node.Type == html.ElementNode && node.Data == "figcaption"
			})
			if caption != nil {
				return boundedText(nodeText(caption), 256)
			}
		}
		if parent.Data == "p" && (hasClass(parent, "Figure_Title") || hasClass(parent, "figure_title")) {
			return boundedText(nodeText(parent), 256)
		}
	}
	return ""
}

func failureURLs(failures []rasterFailure) []string {
	values := make([]string, 0, len(failures))
	for _, failure := range failures {
		values = append(values, failure.requested)
	}
	slices.Sort(values)
	return values
}

func hasAncestor(node *html.Node, name string) bool {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode && parent.Data == name {
			return true
		}
	}
	return false
}

func processedPictureAncestor(node *html.Node) bool {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type != html.ElementNode || parent.Data != "picture" {
			continue
		}
		if attr(parent, "data-archive-image-processed") == "true" {
			removeAttr(parent, "data-archive-image-processed")
			return true
		}
		image := findNode(parent, func(candidate *html.Node) bool {
			return candidate.Type == html.ElementNode && candidate.Data == "img"
		})
		return image != nil && attr(image, "data-archive-image-processed") == "true"
	}
	return false
}

func markImageSlotProcessed(slot, image *html.Node) {
	setAttr(image, "data-archive-image-processed", "true")
}

func attrFound(node *html.Node, key string) (string, bool) {
	for _, item := range node.Attr {
		if strings.EqualFold(item.Key, key) {
			return item.Val, true
		}
	}
	return "", false
}

func boundedText(value string, limit int) string {
	value = strings.TrimSpace(spacePattern.ReplaceAllString(value, " "))
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-3]) + "..."
}

func cloneMissingResources(values []model.MissingResource) []model.MissingResource {
	return append([]model.MissingResource(nil), values...)
}
