package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
)

type assetState struct {
	record         model.FileRecord
	processing     bool
	body           []byte
	sourceBody     []byte
	svgStyles      bool
	needsSVGStyles bool
	reported       bool
}

type assetManager struct {
	ctx           context.Context
	fetcher       model.Fetcher
	root          *os.Root
	refresh       bool
	maxBytes      int64
	budget        *sourceBudget
	states        map[string]*assetState
	probeFailures map[string]error
	imageFailures map[string]rasterFailure
	order         []string
	notices       *[]model.Notice
	warnings      *[]string
	errors        *[]string
	relevant      cssSelectorFilter
	inlineSVGs    int
	progress      func(discovered, completed int)
	completed     int
}

func newAssetManager(ctx context.Context, fetcher model.Fetcher, root *os.Root, refresh bool, maxBytes int64, budget *sourceBudget, notices *[]model.Notice, warnings, errs *[]string, progress func(discovered, completed int)) *assetManager {
	return &assetManager{ctx: ctx, fetcher: fetcher, root: root, refresh: refresh, maxBytes: maxBytes, budget: budget,
		states: map[string]*assetState{}, probeFailures: map[string]error{}, imageFailures: map[string]rasterFailure{},
		notices: notices, warnings: warnings, errors: errs, progress: progress}
}

func (m *assetManager) local(reference, base, owner string, kind dependencyKind) (string, error) {
	return m.localDepth(reference, base, owner, kind, 0)
}

func (m *assetManager) localDepth(reference, base, owner string, kind dependencyKind, depth int) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" || strings.HasPrefix(reference, "#") || strings.HasPrefix(strings.ToLower(reference), "data:") {
		return reference, nil
	}
	if strings.HasPrefix(strings.ToLower(reference), "blob:") {
		return "", errors.New("blob URL cannot be archived for offline use")
	}
	resolved, err := resolveReference(base, reference)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(resolved)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("unsupported required resource URL %q", resolved)
	}
	if strings.HasPrefix(strings.ToLower(parsed.Path), "/users/") {
		return "", &recoverableCSSReferenceError{message: "publisher CSS contains an authoring-machine resource path: " + resolved}
	}
	fragment := parsed.Fragment
	parsed.Fragment = ""
	state, err := m.load(parsed.String(), kind, depth)
	if err != nil {
		return "", err
	}
	local := localReference(owner, state.record.Path)
	if fragment != "" {
		local += "#" + url.PathEscape(fragment)
	}
	return local, nil
}

func (m *assetManager) load(raw string, kind dependencyKind, depth int) (*assetState, error) {
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	if depth > maxHTMLDepth {
		return nil, errors.New("asset dependency depth exceeds limit")
	}
	key, err := fetch.CanonicalURL(raw)
	if err != nil {
		return nil, err
	}
	if existing := m.states[key]; existing != nil {
		if existing.record.Status == "failed" {
			return existing, errors.New(existing.record.Error)
		}
		if existing.record.Status == "substituted" {
			return m.fail(existing, errors.New(existing.record.Error))
		}
		if kind == dependencySVGStyle && !existing.svgStyles &&
			classifyAsset(key, existing.record.ContentType, existing.sourceBody, kind) == "css" {
			if existing.processing {
				existing.needsSVGStyles = true
				return existing, nil
			}
			existing.processing = true
			if err := m.rewriteStylesheet(existing, dependencySVGStyle, depth); err != nil {
				return m.fail(existing, err)
			}
			existing.processing = false
			existing.needsSVGStyles = false
		}
		return existing, nil
	}
	if probeErr := m.probeFailures[key]; probeErr != nil {
		delete(m.probeFailures, key)
		return m.fail(m.install(key), probeErr)
	}
	if len(m.states) >= maxHTMLAssets {
		return nil, errors.New("asset inventory exceeds limit")
	}
	state := m.install(key)
	resource, body, err := readResource(m.ctx, m.fetcher, key, m.refresh, m.maxBytes)
	if err != nil {
		return m.fail(state, err)
	}
	return m.processLoaded(state, resource, body, kind, depth)
}

func (m *assetManager) probe(raw string, kind dependencyKind, depth int) (*assetState, error) {
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	if depth > maxHTMLDepth {
		return nil, errors.New("asset dependency depth exceeds limit")
	}
	key, err := fetch.CanonicalURL(raw)
	if err != nil {
		return nil, err
	}
	if existing := m.states[key]; existing != nil {
		if existing.record.Status == "failed" {
			return existing, errors.New(existing.record.Error)
		}
		return existing, nil
	}
	if len(m.states) >= maxHTMLAssets {
		return nil, errors.New("asset inventory exceeds limit")
	}
	resource, body, err := readResource(m.ctx, m.fetcher, key, m.refresh, m.maxBytes)
	if err != nil {
		m.probeFailures[key] = err
		return nil, err
	}
	return m.installLoaded(key, resource, body, kind, depth)
}

func (m *assetManager) recordFailure(raw string, err error) (*assetState, error) {
	key, canonicalErr := fetch.CanonicalURL(raw)
	if canonicalErr != nil {
		return nil, canonicalErr
	}
	if existing := m.states[key]; existing != nil {
		return existing, errors.New(existing.record.Error)
	}
	return m.fail(m.install(key), err)
}

func (m *assetManager) install(key string) *assetState {
	state := &assetState{record: model.FileRecord{URL: key, Status: "pending"}, processing: true}
	m.states[key] = state
	m.order = append(m.order, key)
	m.report()
	return state
}

func (m *assetManager) installLoaded(key string, resource model.Resource, body []byte, kind dependencyKind, depth int) (*assetState, error) {
	if len(m.states) >= maxHTMLAssets {
		return nil, errors.New("asset inventory exceeds limit")
	}
	return m.processLoaded(m.install(key), resource, body, kind, depth)
}

func (m *assetManager) processLoaded(state *assetState, resource model.Resource, body []byte, kind dependencyKind, depth int) (*assetState, error) {
	state.record.FinalURL = resource.URL
	state.record.ContentType = strings.TrimSpace(resource.Headers.Get("Content-Type"))
	state.record.SourceSHA256 = sourceHash(body)
	state.record.SourceSize = int64(len(body))
	state.sourceBody = append([]byte{}, body...)
	if err := m.budget.add(int64(len(body))); err != nil {
		return m.fail(state, errors.New("archive aggregate source-byte limit exceeded"))
	}
	assetType := classifyAsset(state.record.URL, state.record.ContentType, body, kind)
	ext := assetExtension(state.record.URL, assetType, state.record.ContentType)
	name := fileName(state.record.URL, "", ext)
	if current := path.Ext(name); current != ext {
		name = strings.TrimSuffix(name, current) + ext
	}
	state.record.Path = path.Join("assets", name)
	if err := storage.Check(m.root, state.record.Path); err != nil {
		return m.fail(state, err)
	}
	output := body
	var err error
	switch assetType {
	case "css":
		if err := m.rewriteStylesheet(state, kind, depth); err != nil {
			return m.fail(state, err)
		}
		if state.needsSVGStyles && !state.svgStyles {
			state.needsSVGStyles = false
			if err := m.rewriteStylesheet(state, dependencySVGStyle, depth); err != nil {
				return m.fail(state, err)
			}
		}
		state.needsSVGStyles = false
		state.processing = false
		return state, nil

	case "svg":
		output, err = m.rewriteSVG(body, resource.URL, state.record.Path, depth+1)
		if err != nil {
			return m.fail(state, err)
		}
	case "invalid":
		return m.fail(state, fmt.Errorf("required asset returned HTML/error content: %s", resource.URL))
	default:
		if err := validateBinaryAsset(assetType, body); err != nil {
			return m.fail(state, fmt.Errorf("%s: %w", resource.URL, err))
		}
	}
	if err := storage.Atomic(m.root, state.record.Path, output); err != nil {
		return m.fail(state, err)
	}
	state.body = append([]byte{}, output...)
	state.record.SHA256 = sourceHash(output)
	state.record.Size = int64(len(output))
	state.record.Status = "complete"
	state.processing = false
	m.complete(state)
	return state, nil
}

func (m *assetManager) rewriteStylesheet(state *assetState, kind dependencyKind, depth int) error {
	decoded, err := decodeCSS(state.sourceBody, state.record.ContentType)
	if err != nil {
		return err
	}
	var relevant cssSelectorFilter
	if kind != dependencySVGStyle {
		relevant = m.relevant
	}
	output, diagnostics, err := rewriteCSSFiltered(decoded, false, func(reference string, dependency dependencyKind) (string, error) {
		if kind == dependencySVGStyle && dependency == dependencyCSS {
			dependency = dependencySVGStyle
		}
		return m.localDepth(reference, state.record.FinalURL, state.record.Path, dependency, depth+1)
	}, relevant)
	*m.notices = append(*m.notices, diagnostics.Notices...)
	*m.warnings = append(*m.warnings, diagnostics.Warnings...)
	if err != nil {
		return err
	}
	if err := storage.Atomic(m.root, state.record.Path, output); err != nil {
		return err
	}
	state.body = append([]byte{}, output...)
	state.record.SHA256 = sourceHash(output)
	state.record.Size = int64(len(output))
	state.record.Status = "complete"
	state.svgStyles = state.svgStyles || kind == dependencySVGStyle
	m.complete(state)
	return nil
}

func (m *assetManager) fail(state *assetState, err error) (*assetState, error) {
	state.processing = false
	state.record.Status = "failed"
	state.record.Error = err.Error()
	*m.errors = append(*m.errors, state.record.URL+": "+err.Error())
	m.complete(state)
	return state, err
}

func (m *assetManager) complete(state *assetState) {
	if state.reported {
		return
	}
	state.reported = true
	m.completed++
	m.report()
}

func (m *assetManager) report() {
	if m.progress != nil {
		m.progress(len(m.order), m.completed)
	}
}

func (m *assetManager) records() []model.FileRecord {
	records := make([]model.FileRecord, 0, len(m.order))
	for _, key := range m.order {
		record := m.states[key].record
		if record.Status == "substituted" {
			continue
		}
		records = append(records, record)
	}
	return records
}

func classifyAsset(raw, contentType string, body []byte, hint dependencyKind) string {
	kind := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	ext := strings.ToLower(path.Ext(mustURL(raw).Path))
	switch {
	case hint == dependencyCSS || hint == dependencySVGStyle || kind == "text/css" || ext == ".css":
		return "css"
	case kind == "image/svg+xml" || ext == ".svg":
		return "svg"
	case kind == "text/html" || kind == "application/xhtml+xml" || bytes.HasPrefix(bytes.ToLower(bytes.TrimSpace(body)), []byte("<!doctype html")):
		return "invalid"
	case kind == "font/ttf" || kind == "application/x-font-ttf":
		return "ttf"
	case kind == "image/png" || ext == ".png":
		return "png"
	case kind == "image/jpeg" || ext == ".jpg" || ext == ".jpeg":
		return "jpeg"
	case kind == "image/gif" || ext == ".gif":
		return "gif"
	case kind == "image/webp" || ext == ".webp":
		return "webp"
	case kind == "image/avif" || ext == ".avif":
		return "avif"
	case kind == "font/woff" || ext == ".woff":
		return "woff"
	case kind == "font/woff2" || ext == ".woff2":
		return "woff2"
	default:
		return "binary"
	}
}

func assetExtension(raw, kind, contentType string) string {
	switch kind {
	case "css":
		return ".css"
	case "svg":
		return ".svg"
	case "png":
		return ".png"
	case "jpeg":
		return ".jpg"
	case "gif":
		return ".gif"
	case "webp":
		return ".webp"
	case "avif":
		return ".avif"
	case "woff":
		return ".woff"
	case "woff2":
		return ".woff2"
	case "ttf":
		return ".ttf"
	}
	ext := strings.ToLower(path.Ext(mustURL(raw).Path))
	if len(ext) > 1 && len(ext) <= 10 {
		return ext
	}
	if value, _, err := mime.ParseMediaType(contentType); err == nil {
		if extensions, _ := mime.ExtensionsByType(value); len(extensions) > 0 && len(extensions[0]) <= 10 {
			return extensions[0]
		}
	}
	return ".bin"
}

func validateBinaryAsset(kind string, body []byte) error {
	valid := true
	switch kind {
	case "png":
		valid = len(body) >= 8 && bytes.Equal(body[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10})
	case "jpeg":
		valid = len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[len(body)-2] == 0xff && body[len(body)-1] == 0xd9
	case "gif":
		valid = bytes.HasPrefix(body, []byte("GIF87a")) || bytes.HasPrefix(body, []byte("GIF89a"))
	case "webp":
		valid = len(body) >= 12 && bytes.Equal(body[:4], []byte("RIFF")) && bytes.Equal(body[8:12], []byte("WEBP"))
	case "avif":
		valid = len(body) >= 16 && bytes.Equal(body[4:8], []byte("ftyp")) &&
			(bytes.Equal(body[8:12], []byte("avif")) || bytes.Equal(body[8:12], []byte("avis")))
	case "woff":
		valid = bytes.HasPrefix(body, []byte("wOFF"))
	case "woff2":
		valid = bytes.HasPrefix(body, []byte("wOF2"))
	case "ttf":
		valid = bytes.HasPrefix(body, []byte{0x00, 0x01, 0x00, 0x00}) || bytes.HasPrefix(body, []byte("true"))
	}
	if !valid {
		return fmt.Errorf("invalid %s bytes", kind)
	}
	return nil
}

func mustURL(raw string) *url.URL {
	parsed, _ := url.Parse(raw)
	if parsed == nil {
		return &url.URL{}
	}
	return parsed
}
