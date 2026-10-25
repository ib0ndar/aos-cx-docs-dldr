package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"aos-cx-docs-dldr/internal/model"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
	"golang.org/x/net/html/charset"
)

type dependencyKind uint8

const (
	dependencyAsset dependencyKind = iota
	dependencyCSS
	dependencySVGStyle
	// dependencyFontSource is the single URL of one entry in the src
	// descriptor of an @font-face rule. Only it may resolve to an
	// unavailableFontError instead of failing.
	dependencyFontSource
)

type cssResolver func(reference string, kind dependencyKind) (string, error)
type cssSelectorFilter func(selector []css.Token) bool

type cssDiagnostics struct {
	Notices   []model.Notice
	Warnings  []string
	FontFaces []fontFaceRecovery
}

type recoverableCSSReferenceError struct{ message string }

func (e *recoverableCSSReferenceError) Error() string { return e.message }

// fontFaceRecovery describes one @font-face rule that lost src entries
// because their hosts refused the fonts permanently.
type fontFaceRecovery struct {
	Family  string
	Dropped []model.BrokenStylesheetReference
	Removed bool
}

// fontFaceRule tracks the @font-face rule being rewritten: depth is the
// block depth of its descriptors and start the output offset of the rule.
type fontFaceRule struct {
	depth   int
	start   int
	family  string
	dropped []model.BrokenStylesheetReference
	emptied bool
}

func decodeCSS(body []byte, contentType string) ([]byte, error) {
	name := ""
	if _, params, err := mimeParse(contentType); err != nil {
		return nil, err
	} else {
		name = params
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) >= 10 && strings.EqualFold(string(trimmed[:9]), "@charset ") {
		if end := bytes.IndexByte(trimmed[9:], ';'); end >= 0 {
			value := strings.TrimSpace(string(trimmed[9 : 9+end]))
			if unquoted, err := strconv.Unquote(value); err == nil {
				name = unquoted
			}
		}
	}
	if name == "" || strings.EqualFold(name, "utf-8") || strings.EqualFold(name, "utf8") {
		body = bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})
		if !utf8.Valid(body) {
			return nil, errors.New("stylesheet is not valid UTF-8 and declares no supported charset")
		}
		return body, nil
	}
	encoding, _ := charset.Lookup(name)
	if encoding == nil {
		return nil, fmt.Errorf("unsupported stylesheet charset %q", name)
	}
	return encoding.NewDecoder().Bytes(body)
}

func mimeParse(contentType string) (string, string, error) {
	if strings.TrimSpace(contentType) == "" {
		return "", "", nil
	}
	media, params, err := mime.ParseMediaType(contentType)
	return media, params["charset"], err
}

func rewriteCSS(body []byte, inline bool, resolve cssResolver) ([]byte, cssDiagnostics, error) {
	return rewriteCSSFiltered(body, inline, resolve, nil)
}

func rewriteCSSFiltered(body []byte, inline bool, resolve cssResolver, relevant cssSelectorFilter) ([]byte, cssDiagnostics, error) {
	return rewriteCSSWithProperties(body, inline, resolve, relevant, nil)
}

func rewriteCSSWithProperties(body []byte, inline bool, resolve cssResolver, relevant cssSelectorFilter, keepProperty func(string) bool) ([]byte, cssDiagnostics, error) {
	p := css.NewParser(parse.NewInput(bytes.NewReader(body)), inline)
	var out bytes.Buffer
	diagnostics := cssDiagnostics{}
	var skipped []bool
	skippedRules := 0
	obsolete := map[string]int{}
	invalid := map[string]int{}
	var fontFace *fontFaceRule
	// The parser reports the contents of at-rules it does not know, including
	// escaped spellings of known ones, as raw tokens. They are buffered so their
	// URLs pass through the same rewriting as declaration values.
	var block []css.Token
	flushBlock := func() error {
		if len(block) == 0 {
			return nil
		}
		tokens, bad, err := rewriteCSSTokens(block, resolve, false)
		if err != nil {
			return err
		}
		if bad {
			return errors.New("CSS at-rule block contains an invalid publisher URL")
		}
		writeTokens(&out, tokens)
		block = nil
		return nil
	}
	for {
		grammar, tokenType, data := p.Next()
		if grammar != css.TokenGrammar && grammar != css.QualifiedRuleGrammar {
			if err := flushBlock(); err != nil {
				return nil, diagnostics, err
			}
		}
		if grammar == css.ErrorGrammar {
			if p.HasParseError() {
				return nil, diagnostics, fmt.Errorf("CSS parse error: %w", p.Err())
			}
			if err := p.Err(); err != nil && !errors.Is(err, io.EOF) {
				return nil, diagnostics, err
			}
			break
		}
		switch grammar {
		case css.CommentGrammar:
			if !insideSkipped(skipped) {
				out.Write(data)
			}
		case css.AtRuleGrammar, css.BeginAtRuleGrammar:
			if insideSkipped(skipped) {
				if grammar == css.BeginAtRuleGrammar {
					skipped = append(skipped, true)
				}
				continue
			}
			name, err := cssName(data)
			if err != nil {
				return nil, diagnostics, err
			}
			if name == "@charset" {
				continue
			}
			values := append([]css.Token{}, p.Values()...)
			if name == "@import" {
				values, err = rewriteImport(values, resolve)
			} else if name != "@namespace" {
				var bad bool
				values, bad, err = rewriteCSSTokens(values, resolve, false)
				if err == nil && bad {
					err = fmt.Errorf("CSS %s rule contains an invalid publisher URL", name)
				}
			}
			if err != nil {
				return nil, diagnostics, err
			}
			start := out.Len()
			out.Write(data)
			writeTokens(&out, values)
			if grammar == css.BeginAtRuleGrammar {
				out.WriteByte('{')
				skipped = append(skipped, false)
				// Only a literal @font-face, which the parser reads as a
				// descriptor list, in a stylesheet or <style> block. A style
				// attribute cannot hold one, and escaped spellings stay on
				// the ordinary path.
				if !inline && fontFace == nil && name == "@font-face" && string(data) == name {
					fontFace = &fontFaceRule{depth: len(skipped), start: start}
				}
			} else {
				out.WriteByte(';')
			}
		case css.EndAtRuleGrammar, css.EndRulesetGrammar:
			if fontFace != nil && len(skipped) == fontFace.depth {
				rule := fontFace
				fontFace = nil
				skipped = skipped[:len(skipped)-1]
				if len(rule.dropped) > 0 {
					diagnostics.FontFaces = append(diagnostics.FontFaces, fontFaceRecovery{
						Family: rule.family, Dropped: rule.dropped, Removed: rule.emptied,
					})
				}
				if rule.emptied {
					out.Truncate(rule.start)
				} else {
					out.WriteByte('}')
				}
				continue
			}
			wasSkipped := false
			if len(skipped) > 0 {
				wasSkipped = skipped[len(skipped)-1]
				skipped = skipped[:len(skipped)-1]
			}
			if !wasSkipped && !insideSkipped(skipped) {
				out.WriteByte('}')
			}
		case css.BeginRulesetGrammar:
			values := append([]css.Token{}, p.Values()...)
			filtered, removed := filterSelectorTokens(values, relevant)
			skip := insideSkipped(skipped) || len(filtered) == 0
			skipped = append(skipped, skip)
			skippedRules += removed
			if skip {
				continue
			}
			values, bad, err := rewriteCSSTokens(filtered, resolve, false)
			if err == nil && bad {
				err = errors.New("CSS selector contains an invalid publisher URL")
			}
			if err != nil {
				return nil, diagnostics, err
			}
			writeTokens(&out, values)
			out.WriteByte('{')
		case css.DeclarationGrammar:
			if insideSkipped(skipped) {
				_, _, err := rewriteCSSTokens(append([]css.Token{}, p.Values()...), func(reference string, _ dependencyKind) (string, error) {
					return reference, nil
				}, false)
				if err != nil {
					return nil, diagnostics, err
				}
				continue
			}
			property, err := cssName(data)
			if err != nil {
				return nil, diagnostics, err
			}
			if keepProperty != nil && !keepProperty(property) {
				continue
			}
			if obsoleteCSSProperty(property) {
				obsolete[property]++
				continue
			}
			if fontFace != nil && len(skipped) == fontFace.depth {
				switch property {
				case "font-family":
					fontFace.family = fontFaceFamily(p.Values())
				case "src":
					values, dropped, kept, bad, err := rewriteFontFaceSources(append([]css.Token{}, p.Values()...), resolve)
					if err != nil {
						return nil, diagnostics, err
					}
					if bad {
						invalid[property]++
						continue
					}
					fontFace.dropped = append(fontFace.dropped, dropped...)
					// Browsers use the last src descriptor. When every source
					// of that one is gone, the rule can no longer load a font
					// and is removed, so text falls back as it does online.
					fontFace.emptied = len(dropped) > 0 && kept == 0
					if fontFace.emptied {
						continue
					}
					out.Write(data)
					out.WriteByte(':')
					writeTokens(&out, values)
					out.WriteByte(';')
					continue
				}
			}
			values, bad, err := rewriteCSSTokens(append([]css.Token{}, p.Values()...), resolve, false)
			if err != nil {
				return nil, diagnostics, err
			}
			if bad {
				invalid[property]++
				continue
			}
			out.Write(data)
			out.WriteByte(':')
			writeTokens(&out, values)
			out.WriteByte(';')
		case css.CustomPropertyGrammar:
			if insideSkipped(skipped) {
				value := p.Values()
				if len(value) != 1 {
					return nil, diagnostics, errors.New("invalid parsed CSS custom property")
				}
				if _, _, err := rewriteCSSValue(value[0].Data, func(reference string, _ dependencyKind) (string, error) {
					return reference, nil
				}); err != nil {
					return nil, diagnostics, err
				}
				continue
			}
			property := strings.ToLower(string(data))
			if keepProperty != nil && !keepProperty(property) {
				continue
			}
			if obsoleteCSSProperty(property) {
				obsolete[property]++
				continue
			}
			value := p.Values()
			if len(value) != 1 {
				return nil, diagnostics, errors.New("invalid parsed CSS custom property")
			}
			rewritten, bad, err := rewriteCSSValue(value[0].Data, resolve)
			if err != nil {
				return nil, diagnostics, err
			}
			if bad {
				invalid[property]++
				continue
			}
			out.Write(data)
			out.WriteByte(':')
			out.Write(rewritten)
			out.WriteByte(';')
		case css.QualifiedRuleGrammar, css.TokenGrammar:
			if !insideSkipped(skipped) {
				block = append(block, css.Token{TokenType: tokenType, Data: bytes.Clone(data)})
			}
		default:
			return nil, diagnostics, fmt.Errorf("unsupported CSS grammar %s", grammar)
		}
	}
	if skippedRules > 0 {
		diagnostics.Notices = append(diagnostics.Notices, model.Notice{
			Kind:    model.NoticeCSSCleanup,
			Message: fmt.Sprintf("Removed %d publisher navigation/runtime or provably unused CSS rules", skippedRules),
		})
	}
	if len(obsolete) > 0 {
		diagnostics.Notices = append(diagnostics.Notices, model.Notice{
			Kind:    model.NoticeCSSCleanup,
			Message: "Removed authoring-only MadCap and obsolete IE CSS declarations: " + summarizeCSSCounts(obsolete),
		})
	}
	if len(invalid) > 0 {
		diagnostics.Warnings = append(diagnostics.Warnings,
			"Omitted recoverable CSS declarations containing invalid publisher URLs: "+summarizeCSSCounts(invalid))
	}
	return out.Bytes(), diagnostics, nil
}

func summarizeCSSCounts(values map[string]int) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s (%d)", key, values[key]))
	}
	return strings.Join(parts, ", ")
}

func insideSkipped(stack []bool) bool {
	return len(stack) > 0 && stack[len(stack)-1]
}

func publisherChromeSelector(values []css.Token) bool {
	var selector strings.Builder
	for _, value := range values {
		selector.Write(value.Data)
	}
	lower := strings.ToLower(selector.String())
	for _, marker := range []string{
		"nav.tab-bar", ".tab-bar ", ".tab-bar.", ".tab-bar-section", ".menu-icon",
		".title-bar-layout", ".search-filter", ".search-bar", ".off-canvas",
		".login-dialog", ".toolbar-button",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func splitSelectorTokens(tokens []css.Token) [][]css.Token {
	groups := [][]css.Token{{}}
	depth := 0
	for _, token := range tokens {
		switch token.TokenType {
		case css.FunctionToken, css.LeftBracketToken, css.LeftParenthesisToken:
			depth++
		case css.RightBracketToken, css.RightParenthesisToken:
			if depth > 0 {
				depth--
			}
		case css.CommaToken:
			if depth == 0 {
				groups = append(groups, []css.Token{})
				continue
			}
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], token)
	}
	return groups
}

func filterSelectorTokens(tokens []css.Token, relevant cssSelectorFilter) ([]css.Token, int) {
	groups := splitSelectorTokens(tokens)
	var result []css.Token
	removed := 0
	for _, group := range groups {
		if publisherChromeSelector(group) || relevant != nil && !relevant(group) {
			removed++
			continue
		}
		if len(result) > 0 {
			result = append(result, css.Token{TokenType: css.CommaToken, Data: []byte(",")})
		}
		result = append(result, group...)
	}
	return result, removed
}

func rewriteImport(values []css.Token, resolve cssResolver) ([]css.Token, error) {
	for i := range values {
		switch values[i].TokenType {
		case css.WhitespaceToken, css.CommentToken:
			continue
		case css.StringToken:
			ref, err := cssString(values[i].Data)
			if err != nil {
				return nil, err
			}
			local, err := resolve(ref, dependencyCSS)
			if err != nil {
				return nil, err
			}
			values[i].Data = cssQuoted(local)
			return values, nil
		case css.URLToken:
			ref, err := cssURL(values[i].Data)
			if err != nil {
				return nil, err
			}
			local, err := resolve(ref, dependencyCSS)
			if err != nil {
				return nil, err
			}
			values[i].Data = []byte("url(" + string(cssQuoted(local)) + ")")
			return values, nil
		default:
			return nil, errors.New("CSS @import is missing a URL")
		}
	}
	return nil, errors.New("empty CSS @import")
}

// rewriteFontFaceSources rewrites the entries of an @font-face src
// descriptor. An entry whose single URL resolves to an unavailableFontError
// is dropped and reported; every other entry, and every other error, is
// handled as in any declaration. kept counts the entries that remain.
func rewriteFontFaceSources(values []css.Token, resolve cssResolver) (
	[]css.Token, []model.BrokenStylesheetReference, int, bool, error,
) {
	var result []css.Token
	var dropped []model.BrokenStylesheetReference
	kept := 0
	// Source entries are split at top-level commas, as selector lists are.
	for _, entry := range splitSelectorTokens(values) {
		eligible := fontSourceEntry(entry)
		entryResolve := resolve
		if eligible {
			entryResolve = func(reference string, kind dependencyKind) (string, error) {
				if kind == dependencyAsset {
					kind = dependencyFontSource
				}
				return resolve(reference, kind)
			}
		}
		rewritten, bad, err := rewriteCSSTokens(entry, entryResolve, false)
		var unavailable *unavailableFontError
		if eligible && errors.As(err, &unavailable) {
			source := model.BrokenStylesheetReference{URL: unavailable.URL, HTTPStatus: unavailable.Status}
			if unavailable.FinalURL != unavailable.URL {
				source.FinalURL = unavailable.FinalURL
			}
			dropped = append(dropped, source)
			continue
		}
		if err != nil || bad {
			return nil, nil, 0, bad, err
		}
		if len(result) > 0 {
			result = append(result, css.Token{TokenType: css.CommaToken, Data: []byte(",")})
		}
		result = append(result, rewritten...)
		if slices.ContainsFunc(rewritten, func(token css.Token) bool {
			return token.TokenType != css.WhitespaceToken && token.TokenType != css.CommentToken
		}) {
			kept++
		}
	}
	return result, dropped, kept, false, nil
}

// fontSourceEntry reports whether a src entry loads exactly one URL, written
// as a url() token, so that dropping the entry drops only that source.
func fontSourceEntry(entry []css.Token) bool {
	urls := 0
	for _, token := range entry {
		switch token.TokenType {
		case css.URLToken:
			urls++
		case css.FunctionToken:
			name, err := cssName(token.Data)
			if err != nil || name == "image-set(" || name == "-webkit-image-set(" {
				return false
			}
		}
	}
	return urls == 1
}

// fontFaceFamily is the family name a font-family descriptor declares.
func fontFaceFamily(values []css.Token) string {
	var parts []string
	for _, token := range values {
		var value string
		var err error
		switch token.TokenType {
		case css.StringToken:
			value, err = cssString(token.Data)
		case css.IdentToken:
			value, err = cssUnescape(string(token.Data))
		default:
			continue
		}
		if err == nil {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " ")
}

func rewriteCSSValue(value []byte, resolve cssResolver) ([]byte, bool, error) {
	lexer := css.NewLexer(parse.NewInput(bytes.NewReader(value)))
	var tokens []css.Token
	for {
		kind, data := lexer.Next()
		if kind == css.ErrorToken {
			if err := lexer.Err(); err != nil && !errors.Is(err, io.EOF) {
				return nil, false, err
			}
			break
		}
		tokens = append(tokens, css.Token{TokenType: kind, Data: append([]byte{}, data...)})
	}
	tokens, bad, err := rewriteCSSTokens(tokens, resolve, false)
	var out bytes.Buffer
	writeTokens(&out, tokens)
	return out.Bytes(), bad, err
}

func rewriteCSSTokens(tokens []css.Token, resolve cssResolver, inImageSet bool) ([]css.Token, bool, error) {
	// blocks records, for each open function or parenthesis, whether it is an
	// image-set(). Only strings directly inside image-set() are image URLs.
	blocks := []bool{inImageSet}
	for i := range tokens {
		switch tokens[i].TokenType {
		case css.FunctionToken:
			name, err := cssName(tokens[i].Data)
			if err != nil {
				return nil, false, err
			}
			switch name {
			case "url(", "src(":
				// The lexer turns only a literal url( into a URL token. Browsers
				// also load the argument of an escaped spelling such as \75 rl(,
				// which therefore cannot be rewritten and is treated as invalid.
				return tokens, true, nil
			}
			blocks = append(blocks, name == "image-set(" || name == "-webkit-image-set(")
		case css.LeftParenthesisToken:
			blocks = append(blocks, false)
		case css.RightParenthesisToken:
			if len(blocks) > 1 {
				blocks = blocks[:len(blocks)-1]
			}
		case css.BadURLToken:
			return tokens, true, nil
		case css.URLToken:
			ref, err := cssURL(tokens[i].Data)
			if err != nil {
				return nil, false, err
			}
			if strings.HasPrefix(strings.TrimSpace(ref), "#") || strings.HasPrefix(strings.TrimSpace(ref), "data:") {
				continue
			}
			local, err := resolve(ref, dependencyAsset)
			if err != nil {
				var recoverable *recoverableCSSReferenceError
				if errors.As(err, &recoverable) {
					return tokens, true, nil
				}
				return nil, false, err
			}
			tokens[i].Data = []byte("url(" + string(cssQuoted(local)) + ")")
		case css.StringToken:
			if !blocks[len(blocks)-1] {
				continue
			}
			ref, err := cssString(tokens[i].Data)
			if err != nil {
				return nil, false, err
			}
			local, err := resolve(ref, dependencyAsset)
			if err != nil {
				var recoverable *recoverableCSSReferenceError
				if errors.As(err, &recoverable) {
					return tokens, true, nil
				}
				return nil, false, err
			}
			tokens[i].Data = cssQuoted(local)
		}
	}
	return tokens, false, nil
}

func obsoleteCSSProperty(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "behavior" || name == "-ms-behavior" || strings.HasPrefix(name, "-pie-") || strings.HasPrefix(name, "mc-")
}

func writeTokens(out *bytes.Buffer, values []css.Token) {
	for _, token := range values {
		out.Write(token.Data)
	}
}

func cssURL(value []byte) (string, error) {
	raw := strings.TrimSpace(string(value))
	if len(raw) < 5 || !strings.EqualFold(raw[:4], "url(") || raw[len(raw)-1] != ')' {
		return "", fmt.Errorf("invalid CSS URL token %q", raw)
	}
	raw = strings.TrimSpace(raw[4 : len(raw)-1])
	if strings.HasPrefix(raw, `"`) || strings.HasPrefix(raw, `'`) {
		return cssString([]byte(raw))
	}
	return cssUnescape(raw)
}

func cssString(value []byte) (string, error) {
	raw := string(value)
	if len(raw) < 2 || (raw[0] != '"' && raw[0] != '\'') || raw[len(raw)-1] != raw[0] {
		return "", fmt.Errorf("invalid CSS string token %q", raw)
	}
	return cssUnescape(raw[1 : len(raw)-1])
}

// cssName decodes the escapes in an identifier, function or at-keyword token
// and lowercases ASCII letters, so that escaped spellings such as \75 rl( and
// @\69mport compare equal to the names browsers resolve them to.
func cssName(data []byte) (string, error) {
	name, err := cssUnescape(string(data))
	if err != nil {
		return "", err
	}
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, name), nil
}

// cssReferenceProblem describes the first reference in archived CSS that a
// browser would resolve outside the archive, or returns "". Fragment, data
// and relative references are local. @namespace URIs are names, not loads.
func cssReferenceProblem(body []byte) string {
	lexer := css.NewLexer(parse.NewInput(bytes.NewReader(body)))
	blocks := []bool{false}
	namespace, importing := false, false
	for {
		kind, data := lexer.Next()
		switch kind {
		case css.ErrorToken:
			if err := lexer.Err(); err != nil && !errors.Is(err, io.EOF) {
				return "unreadable CSS: " + err.Error()
			}
			return ""
		case css.AtKeywordToken:
			name, err := cssName(data)
			if err != nil {
				return "invalid CSS at-rule name"
			}
			namespace, importing = name == "@namespace", name == "@import"
		case css.SemicolonToken, css.LeftBraceToken, css.RightBraceToken:
			namespace, importing = false, false
		case css.FunctionToken:
			name, err := cssName(data)
			if err != nil {
				return "invalid CSS function name"
			}
			if name == "url(" || name == "src(" {
				return "unrewritten CSS function " + string(data)
			}
			blocks = append(blocks, name == "image-set(" || name == "-webkit-image-set(")
		case css.LeftParenthesisToken:
			blocks = append(blocks, false)
		case css.RightParenthesisToken:
			if len(blocks) > 1 {
				blocks = blocks[:len(blocks)-1]
			}
		case css.BadURLToken:
			return "malformed CSS URL " + string(data)
		case css.URLToken:
			if namespace {
				continue
			}
			reference, err := cssURL(data)
			if err != nil || !localArchiveReference(reference) {
				return "CSS reference is not local " + string(data)
			}
			importing = false
		case css.StringToken:
			if !importing && !blocks[len(blocks)-1] {
				continue
			}
			reference, err := cssString(data)
			if err != nil || !localArchiveReference(reference) {
				return "CSS reference is not local " + string(data)
			}
			importing = false
		}
	}
}

// localArchiveReference reports whether a reference stays inside the archive:
// a fragment, a data URL or a relative path without scheme or authority.
func localArchiveReference(reference string) bool {
	reference = strings.TrimSpace(reference)
	lower := strings.ToLower(reference)
	if reference == "" || strings.HasPrefix(reference, "#") || strings.HasPrefix(lower, "data:") {
		return true
	}
	if strings.HasPrefix(reference, "//") || strings.HasPrefix(reference, `\\`) ||
		strings.IndexFunc(reference, unicode.IsControl) >= 0 {
		return false
	}
	parsed, err := url.Parse(reference)
	return err == nil && parsed.Scheme == "" && parsed.Host == ""
}

func cssUnescape(value string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '\\' {
			r, size := utf8.DecodeRuneInString(value[i:])
			if r == utf8.RuneError && size == 1 {
				return "", errors.New("invalid UTF-8 in CSS escape")
			}
			out.WriteRune(r)
			i += size
			continue
		}
		i++
		if i == len(value) {
			return "", errors.New("unterminated CSS escape")
		}
		if value[i] == '\n' || value[i] == '\f' {
			i++
			continue
		}
		if value[i] == '\r' {
			i++
			if i < len(value) && value[i] == '\n' {
				i++
			}
			continue
		}
		start := i
		for i < len(value) && i-start < 6 && isHex(value[i]) {
			i++
		}
		if i > start {
			code, err := strconv.ParseInt(value[start:i], 16, 32)
			if err != nil || code == 0 || code > utf8.MaxRune {
				out.WriteRune(utf8.RuneError)
			} else {
				out.WriteRune(rune(code))
			}
			if i < len(value) && strings.ContainsRune(" \t\r\n\f", rune(value[i])) {
				if value[i] == '\r' && i+1 < len(value) && value[i+1] == '\n' {
					i++
				}
				i++
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			return "", errors.New("invalid UTF-8 in CSS escape")
		}
		out.WriteRune(r)
		i += size
	}
	return out.String(), nil
}

func cssQuoted(value string) []byte {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\a `)
	value = strings.ReplaceAll(value, "\r", `\d `)
	value = strings.ReplaceAll(value, "\f", `\c `)
	return []byte(`"` + value + `"`)
}

func isHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}
