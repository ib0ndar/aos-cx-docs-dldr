package fetch

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// The transport applies one destination policy to every hop it sends: the
// initial request, each redirect Location, robots.txt and its redirects. Only
// HTTP or HTTPS to a DNS name or a public IP literal is accepted. The URL check
// is syntactic. With HTTPS and normal certificate verification, a public name
// that resolves to an internal host fails the TLS handshake, so HTTPS needs no
// more, and resolving names here would break split-horizon DNS. Plain HTTP has
// no such protection: a direct HTTP connection is also checked at dial time
// against the address actually connected to (see publicDialContext), which
// also covers DNS rebinding. A connection through a configured proxy is left to
// the proxy, which resolves the name.
//
// Config.AllowLoopbackFixtures adds only loopback IP literals and the name
// localhost, for local test fixtures. Private networks stay refused in that
// mode too.

// DestinationError reports a URL refused by the destination policy. It is
// final: retrying the same URL cannot change the decision.
type DestinationError struct {
	URL    string
	Reason string
}

func (e *DestinationError) Error() string {
	return fmt.Sprintf("refusing destination %s: %s", e.URL, e.Reason)
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),   // Carrier-grade NAT.
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments.
	netip.MustParsePrefix("192.0.2.0/24"),    // Documentation.
	netip.MustParsePrefix("198.18.0.0/15"),   // Benchmarking.
	netip.MustParsePrefix("198.51.100.0/24"), // Documentation.
	netip.MustParsePrefix("203.0.113.0/24"),  // Documentation.
	netip.MustParsePrefix("240.0.0.0/4"),     // Reserved, including broadcast.
	netip.MustParsePrefix("64:ff9b:1::/48"),  // Local-use NAT64.
	netip.MustParsePrefix("100::/64"),        // Discard-only.
	netip.MustParsePrefix("2001:2::/48"),     // Benchmarking.
	netip.MustParsePrefix("2001:db8::/32"),   // Documentation.
	netip.MustParsePrefix("3fff::/20"),       // Documentation.
	netip.MustParsePrefix("fec0::/10"),       // Deprecated site-local.
}

var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
var sixToFourPrefix = netip.MustParsePrefix("2002::/16")

// CheckDestination applies the transport destination policy to one URL.
func CheckDestination(raw string, allowLoopbackFixtures bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return &DestinationError{URL: raw, Reason: "malformed URL"}
	}
	loopback, reason := classifyHost(u.Hostname())
	if reason == "" && loopback {
		if allowLoopbackFixtures && (u.Scheme == "http" || u.Scheme == "https") {
			return nil
		}
		reason = "loopback and localhost destinations are reserved for local test fixtures"
	}
	if reason == "" && u.Scheme != "https" && u.Scheme != "http" {
		reason = "only HTTP and HTTPS publisher URLs are retrieved"
	}
	if reason != "" {
		return &DestinationError{URL: raw, Reason: reason}
	}
	return nil
}

// LoopbackFixtureURL reports an http or https URL whose host is a loopback IP
// literal or localhost: the only destinations fixture mode adds.
func LoopbackFixtureURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	loopback, reason := classifyHost(u.Hostname())
	return loopback && reason == ""
}

func classifyHost(host string) (loopback bool, reason string) {
	if host == "" {
		return false, "URL has no host"
	}
	name := strings.ToLower(host)
	if addr, err := netip.ParseAddr(name); err == nil {
		return classifyAddress(addr)
	}
	// HTTP backends map non-ASCII hosts through IDNA before dialling, which
	// can turn full-width digits and dots into an IPv4 literal.
	for i := 0; i < len(name); i++ {
		if name[i] >= utf8.RuneSelf {
			ascii, err := idna.Lookup.ToASCII(name)
			if err != nil {
				return false, "invalid internationalized host name"
			}
			if loopback, reason := classifyHost(ascii); loopback || reason != "" {
				return false, "internationalized host name maps to a non-public address"
			}
			return false, ""
		}
	}
	name = strings.TrimSuffix(name, ".")
	if name == "localhost" {
		return true, ""
	}
	if strings.HasSuffix(name, ".localhost") {
		return false, "localhost names are not public destinations"
	}
	if endsInNumber(name) {
		return false, "numeric host is not a canonical dotted-quad IPv4 address"
	}
	return false, ""
}

// endsInNumber reports hosts that net/url accepts as names but resolvers and
// browsers may read as IPv4, such as 2130706433, 0x7f.1 or 0177.0.0.1.
// Canonical dotted quads have already been parsed as addresses.
func endsInNumber(name string) bool {
	last := name[strings.LastIndexByte(name, '.')+1:]
	if last == "" {
		return false
	}
	digits := "0123456789"
	if len(last) >= 2 && last[0] == '0' && (last[1] == 'x' || last[1] == 'X') {
		last, digits = last[2:], "0123456789abcdefABCDEF"
	}
	for i := 0; i < len(last); i++ {
		if !strings.ContainsRune(digits, rune(last[i])) {
			return false
		}
	}
	return true
}

func classifyAddress(addr netip.Addr) (loopback bool, reason string) {
	if addr.Zone() != "" {
		return false, "scoped IPv6 addresses are not public destinations"
	}
	addr = addr.Unmap()
	if addr.Is6() && addr != netip.IPv6Loopback() && addr != netip.IPv6Unspecified() {
		if raw := addr.As16(); [12]byte(raw[:12]) == [12]byte{} {
			addr = netip.AddrFrom4([4]byte(raw[12:])) // Deprecated IPv4-compatible form.
		}
	}
	if addr.IsLoopback() {
		return true, ""
	}
	if addr.Is6() {
		raw := addr.As16()
		var embedded netip.Addr
		if nat64Prefix.Contains(addr) {
			embedded = netip.AddrFrom4([4]byte(raw[12:]))
		} else if sixToFourPrefix.Contains(addr) {
			embedded = netip.AddrFrom4([4]byte(raw[2:6]))
		}
		if embedded.IsValid() {
			if inner, reason := classifyAddress(embedded); inner || reason != "" {
				return false, "IPv6 translation address embeds a non-public IPv4 address"
			}
		}
	}
	if addr.IsUnspecified() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsMulticast() {
		return false, "host is a non-public IP address"
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return false, "host is a non-public IP address"
		}
	}
	return false, ""
}

type publicDialKey struct{}

type publicDialPolicy struct {
	url                   string
	allowLoopbackFixtures bool
}

// withPublicDial marks a plain-HTTP hop that will connect directly, so that
// publicDialContext refuses a non-public address the name resolves to. Hops
// through a configured proxy are not marked: the proxy resolves the name.
func withPublicDial(
	ctx context.Context,
	raw string,
	allowLoopbackFixtures bool,
	proxyFor func(*http.Request) (*url.URL, error),
) (context.Context, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return ctx, err
	}
	proxy, err := proxyFor(&http.Request{URL: u})
	if err != nil || proxy != nil {
		return ctx, err
	}
	return context.WithValue(ctx, publicDialKey{}, publicDialPolicy{url: raw, allowLoopbackFixtures: allowLoopbackFixtures}), nil
}

// publicDialContext dials like dialer, except that a hop marked by
// withPublicDial may connect only to a public address. The check runs on each
// resolved address just before connecting.
func publicDialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		policy, ok := ctx.Value(publicDialKey{}).(publicDialPolicy)
		if !ok {
			return dialer.DialContext(ctx, network, address)
		}
		guarded := *dialer
		guarded.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return &DestinationError{URL: policy.url, Reason: "unrecognized dial address " + address}
			}
			addr, err := netip.ParseAddr(host)
			if err != nil {
				return &DestinationError{URL: policy.url, Reason: "unrecognized dial address " + address}
			}
			loopback, reason := classifyAddress(addr)
			if loopback && !policy.allowLoopbackFixtures {
				reason = "host is a non-public IP address"
			}
			if reason != "" {
				return &DestinationError{URL: policy.url, Reason: "plain HTTP host resolved to " + host + ": " + reason}
			}
			return nil
		}
		return guarded.DialContext(ctx, network, address)
	}
}
