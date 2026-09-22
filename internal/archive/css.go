package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"slices"
	"strconv"
	"strings"
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
)

type cssResolver func(reference string, kind dependencyKind) (string, error)
type cssSelectorFilter func(selector []css.Token) bool

type cssDiagnostics struct {
	Notices  []model.Notice
	Warnings []string
}

type recoverableCSSReferenceError struct{ message string }

func (e *recoverableCSSReferenceError) Error() string { return e.message }

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
	for {
		grammar, _, data := p.Next()
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
			name := strings.ToLower(string(data))
			if name == "@charset" {
				continue
			}
			values := append([]css.Token{}, p.Values()...)
			var err error
			if name == "@import" {
				values, err = rewriteImport(values, resolve)
			} else if name != "@namespace" {
				values, _, err = rewriteCSSTokens(values, resolve, false)
			}
			if err != nil {
				return nil, diagnostics, err
			}
			out.Write(data)
			writeTokens(&out, values)
			if grammar == css.BeginAtRuleGrammar {
				out.WriteByte('{')
				skipped = append(skipped, false)
			} else {
				out.WriteByte(';')
			}
		case css.EndAtRuleGrammar, css.EndRulesetGrammar:
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
			values, _, err := rewriteCSSTokens(filtered, resolve, false)
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
			property := strings.ToLower(string(data))
			if keepProperty != nil && !keepProperty(property) {
				continue
			}
			if obsoleteCSSProperty(property) {
				obsolete[property]++
				continue
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
				out.Write(data)
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
	imageDepth := 0
	for i := range tokens {
		switch tokens[i].TokenType {
		case css.FunctionToken:
			if strings.EqualFold(string(tokens[i].Data), "image-set(") || strings.EqualFold(string(tokens[i].Data), "-webkit-image-set(") {
				imageDepth++
			}
		case css.RightParenthesisToken:
			if imageDepth > 0 {
				imageDepth--
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
			if imageDepth == 0 && !inImageSet {
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
