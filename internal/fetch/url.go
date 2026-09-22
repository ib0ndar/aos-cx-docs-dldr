package fetch

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func NormalizeURL(raw string) (string, error) {
	if !utf8.ValidString(raw) || strings.ContainsAny(raw, "\\") || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("control character or backslash in URL %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("malformed publisher URL %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || strings.IndexFunc(u.Host, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("unsupported publisher URL %q", raw)
	}
	if (strings.ContainsAny(u.Host, "[]") || strings.Contains(u.Hostname(), ":")) && net.ParseIP(u.Hostname()) == nil {
		return "", fmt.Errorf("invalid IP authority in URL %q", raw)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid port in URL %q", raw)
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("empty port in URL %q", raw)
	}
	if _, err := url.QueryUnescape(u.RawQuery); err != nil {
		return "", fmt.Errorf("invalid query escape in URL %q: %w", raw, err)
	}
	// Preserve query ordering, plus signs and existing escapes, rather than
	// round-tripping through url.Values and changing publisher identity.
	u.RawQuery = escapeComponent(u.RawQuery, "/?%:@!$&'()*+,;=-._~")
	if u.RawPath != "" {
		u.RawPath = escapeComponent(u.RawPath, "/%:@!$&'()*+,;=-._~")
	}
	if u.RawFragment != "" {
		u.RawFragment = escapeComponent(u.RawFragment, "/?%:@!$&'()*+,;=-._~")
	}
	return u.String(), nil
}

func escapeComponent(raw, safe string) string {
	var escaped strings.Builder
	for _, b := range []byte(raw) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
			strings.ContainsRune(safe, rune(b)) {
			escaped.WriteByte(b)
		} else {
			fmt.Fprintf(&escaped, "%%%02X", b)
		}
	}
	return escaped.String()
}

func CanonicalURL(raw string) (string, error) {
	normalized, err := NormalizeURL(raw)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(normalized)
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}

func origin(raw string) string {
	u, _ := url.Parse(raw) // Callers have already validated the URL.
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	} else {
		host = net.JoinHostPort(host, port)
	}
	return u.Scheme + "://" + host
}
