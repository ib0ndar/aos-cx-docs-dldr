package archive

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
)

func TestArchiveStylesPreservePublisherTablePresentation(t *testing.T) {
	parser := css.NewParser(parse.NewInput(strings.NewReader(archiveStyles)), false)
	tableRule := false
	for {
		grammar, _, data := parser.Next()
		switch grammar {
		case css.ErrorGrammar:
			if parser.HasParseError() || parser.Err() != io.EOF {
				t.Fatalf("invalid archive stylesheet: %v", parser.Err())
			}
			return
		case css.BeginRulesetGrammar:
			tableRule = false
			for _, token := range parser.Values() {
				if token.TokenType == css.IdentToken &&
					(bytes.Equal(token.Data, []byte("table")) || bytes.Equal(token.Data, []byte("th")) ||
						bytes.Equal(token.Data, []byte("td"))) {
					tableRule = true
				}
			}
		case css.EndRulesetGrammar:
			tableRule = false
		case css.DeclarationGrammar:
			property := strings.ToLower(string(data))
			if tableRule && (strings.HasPrefix(property, "border") ||
				strings.HasPrefix(property, "padding") || property == "vertical-align") {
				t.Errorf("generated table rule overrides publisher/default %s", property)
			}
		}
	}
}
