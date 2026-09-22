package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aos-cx-docs-dldr/internal/model"
	"github.com/temoto/robotstxt"
)

var robotsField = regexp.MustCompile(`^[A-Za-z][A-Za-z-]*$`)

func (c *Client) loadRobots(ctx context.Context, raw string) (group *robotstxt.Group, err error) {
	var body []byte
	r, err := c.request(ctx, model.Request{URL: raw, Method: http.MethodGet}, true)
	if err != nil {
		return nil, fmt.Errorf("cannot establish publisher robots rules: %w", err)
	}
	defer func() { err = errors.Join(err, r.Body.Close()) }()
	if r.Status == http.StatusOK {
		body, err = io.ReadAll(io.LimitReader(r.Body, maxRobotsBytes+1))
		if err != nil {
			return nil, fmt.Errorf("cannot establish publisher robots rules: %w", err)
		}
	}
	switch r.Status {
	case http.StatusNotFound:
		body = []byte("User-agent: *\nAllow: /\n")
	case http.StatusOK:
		if err := validateRobots(body, r.Headers.Get("Content-Type")); err != nil {
			return nil, fmt.Errorf("cannot establish publisher robots rules at %s: %w", r.URL, err)
		}
	default:
		return nil, fmt.Errorf("cannot establish publisher robots rules: %w", &StatusError{URL: r.URL, Status: r.Status})
	}
	data, err := robotstxt.FromBytes([]byte(strings.TrimPrefix(string(body), "\ufeff")))
	if err != nil {
		return nil, fmt.Errorf("invalid robots.txt at %s: %w", r.URL, err)
	}
	return data.FindGroup(userAgent), nil
}

func validateRobots(body []byte, contentType string) error {
	if len(body) > maxRobotsBytes || !utf8.Valid(body) {
		return errors.New("oversized or invalid UTF-8 robots.txt")
	}
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return fmt.Errorf("invalid robots.txt content type: %w", err)
		}
		if mediaType == "text/html" || strings.Contains(mediaType, "json") || strings.Contains(mediaType, "xml") {
			return fmt.Errorf("expected robots.txt, received %s", mediaType)
		}
	}
	nonempty, recognized, hasAgent := false, false, false
	for _, line := range strings.Split(strings.TrimPrefix(string(body), "\ufeff"), "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonempty = true
		name, value, found := strings.Cut(line, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !found || !robotsField.MatchString(name) {
			return errors.New("invalid robots.txt directive")
		}
		switch strings.ToLower(name) {
		case "user-agent":
			if value == "" {
				return errors.New("empty robots.txt user-agent")
			}
			recognized, hasAgent = true, true
		case "allow", "disallow", "crawl-delay":
			if !hasAgent {
				return errors.New("robots.txt rules have no user-agent group")
			}
			if strings.EqualFold(name, "crawl-delay") {
				delay, err := strconv.ParseFloat(value, 64)
				if err != nil || math.IsNaN(delay) || math.IsInf(delay, 0) || delay < 0 ||
					delay >= float64(math.MaxInt64)/float64(time.Second) {
					return errors.New("unsupported robots.txt crawl-delay; expected a finite non-negative representable duration")
				}
			}
		case "sitemap":
			if _, err := NormalizeURL(value); err != nil {
				return fmt.Errorf("invalid robots.txt sitemap: %w", err)
			}
			recognized = true
		case "host":
			if strings.ContainsAny(value, "/?#") {
				return errors.New("invalid robots.txt host")
			}
			if _, err := NormalizeURL("https://" + value); err != nil {
				return fmt.Errorf("invalid robots.txt host: %w", err)
			}
			recognized = true
		}
	}
	// Empty and comment-only text files explicitly mean no restrictions.
	// Unknown extension fields are ignored only alongside recognizable rules.
	if nonempty && !recognized {
		return errors.New("robots.txt contains no recognizable directives")
	}
	return nil
}
