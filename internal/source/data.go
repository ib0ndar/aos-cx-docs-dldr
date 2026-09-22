package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/titanous/json5"
)

type numberLiteral string

var integerLiteral = regexp.MustCompile(`^[+-]?(?:0[xX][0-9a-fA-F]+|[0-9]+)$`)

func integer(value any) (int, error) {
	raw, ok := value.(numberLiteral)
	if n, yes := value.(json5.Number); yes {
		raw, ok = numberLiteral(n.String()), true
	}
	if !ok || !integerLiteral.MatchString(string(raw)) {
		return 0, errors.New("expected an integer data value")
	}
	base := 10
	s := string(raw)
	unsigned := s
	if len(unsigned) > 0 && (unsigned[0] == '+' || unsigned[0] == '-') {
		unsigned = unsigned[1:]
	}
	if len(unsigned) > 2 && (unsigned[:2] == "0x" || unsigned[:2] == "0X") {
		base = 0
	}
	n, err := strconv.ParseInt(s, base, 32)
	if err != nil {
		return 0, fmt.Errorf("integer data value out of range: %w", err)
	}
	return int(n), nil
}

func stringLabel(value any, context string) (string, error) {
	s, ok := value.(string)
	if !ok || !label(s) {
		return "", fmt.Errorf("expected nonempty string for %s", context)
	}
	return s, nil
}

func jsonData(ctx context.Context, body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	count := 0
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count++
		if depth > 2*maxDepth+16 || count > 16*maxEntries {
			return nil, errors.New("JSON inventory exceeds nesting/entry limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case json.Delim:
			switch token {
			case '{':
				object := map[string]any{}
				for decoder.More() {
					keyToken, err := decoder.Token()
					if err != nil {
						return nil, err
					}
					key, ok := keyToken.(string)
					if !ok {
						return nil, errors.New("expected object key")
					}
					if _, exists := object[key]; exists {
						return nil, fmt.Errorf("duplicate JSON key %q", key)
					}
					entry, err := value(depth + 1)
					if err != nil {
						return nil, err
					}
					object[key] = entry
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim('}') {
					return nil, errors.New("invalid JSON object ending")
				}
				return object, nil
			case '[':
				array := []any{}
				for decoder.More() {
					entry, err := value(depth + 1)
					if err != nil {
						return nil, err
					}
					array = append(array, entry)
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim(']') {
					return nil, errors.New("invalid JSON array ending")
				}
				return array, nil
			default:
				return nil, errors.New("unexpected JSON delimiter")
			}
		case json.Number:
			return numberLiteral(token.String()), nil
		case string, bool, nil:
			return token, nil
		default:
			return nil, errors.New("unsupported JSON data token")
		}
	}
	root, err := value(0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON inventory data")
	}
	return root, ctx.Err()
}

func (p *planner) json(in input) (any, error) {
	media, err := mediaType(in)
	if err != nil {
		return nil, err
	}
	hpeContent := false
	if u, parseErr := url.Parse(in.url); parseErr == nil {
		hpeContent = u.Hostname() == "support.hpe.com" && strings.HasPrefix(u.Path, "/hpesc/public/api/document/") && u.Query().Get("page") == "content.json"
	}
	if media != "" && media != "application/json" && media != "text/json" && media != "text/plain" && media != "application/octet-stream" &&
		!(media == "multipage" && hpeContent) {
		return nil, fmt.Errorf("expected JSON inventory, received %s: %s", media, in.url)
	}
	body, err := inventoryText(in.body, in.headers.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	data, err := jsonData(p.ctx, body)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON inventory at %s: %w", in.url, err)
	}
	return data, nil
}
