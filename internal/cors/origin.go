package cors

import (
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// This parser mirrors CanonicalOrigin of synthwerk-identity (internal/apps/origin.go).
// llm looks up only origins that pass it, so a caller cannot fill the cache with
// values identity would refuse anyway. Keep both parsers the same.

// MaxOriginLen is the longest origin the parser accepts (contracts AppOrigin).
const MaxOriginLen = 256

const (
	maxHostLen    = 253
	maxLabelLen   = 63
	schemeHTTPS   = "https"
	schemeHTTP    = "http"
	defaultHTTPS  = "443"
	defaultHTTP   = "80"
	originNull    = "null"
	schemeDivider = "://"
)

// ErrOrigin: the value is not an origin llm accepts.
var ErrOrigin = errors.New("cors: not a valid origin")

// loopback hosts may use http. Every other host needs https.
var loopback = []string{"localhost", "127.0.0.1", "[::1]"}

// CanonicalOrigin checks s and returns its canonical form: scheme://host[:port],
// lower case, default port removed, IP addresses in their short form.
//
// The parser is strict on purpose. It refuses what a browser never sends in an
// Origin header: "null", a path (even "/"), a query, a fragment, user info, a
// wildcard, percent-encoding, a backslash, white space, non-ASCII bytes, a trailing
// dot and a port with leading zeros. http is accepted only for localhost and
// loopback IPs.
func CanonicalOrigin(s string) (string, error) {
	if s == "" || len(s) > MaxOriginLen || s == originNull {
		return "", ErrOrigin
	}
	for i := range len(s) {
		// ASCII only, before any case folding: strings.ToLower maps some non-ASCII
		// runes to ASCII letters (U+212A KELVIN SIGN becomes "k").
		if c := s[i]; c <= ' ' || c >= 0x7f {
			return "", ErrOrigin
		}
	}
	scheme, rest, ok := strings.Cut(s, schemeDivider)
	if !ok {
		return "", ErrOrigin
	}
	scheme = strings.ToLower(scheme)
	if scheme != schemeHTTPS && scheme != schemeHTTP {
		return "", ErrOrigin
	}
	if rest == "" || strings.ContainsAny(rest, `/?#@\%*`) {
		return "", ErrOrigin
	}
	rawHost, port, err := splitHostPort(rest)
	if err != nil {
		return "", err
	}
	host, err := canonicalHost(rawHost)
	if err != nil {
		return "", err
	}
	if scheme == schemeHTTP && !slices.Contains(loopback, host) {
		return "", ErrOrigin
	}
	if (scheme == schemeHTTPS && port == defaultHTTPS) || (scheme == schemeHTTP && port == defaultHTTP) {
		port = ""
	}
	out := scheme + schemeDivider + host
	if port != "" {
		out += ":" + port
	}
	return out, nil
}

// splitHostPort splits "host", "host:port", "[v6]" and "[v6]:port". The port is
// checked here: 1 to 65535, digits only, no leading zero.
func splitHostPort(s string) (host, port string, err error) {
	var hasPort bool
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", "", ErrOrigin
		}
		host = s[:end+1]
		after := s[end+1:]
		if after != "" {
			if after[0] != ':' {
				return "", "", ErrOrigin
			}
			port, hasPort = after[1:], true
		}
	} else {
		if strings.Count(s, ":") > 1 {
			return "", "", ErrOrigin
		}
		host, port, hasPort = strings.Cut(s, ":")
	}
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || port == "" || port[0] < '1' || port[0] > '9' || n < 1 || n > 65535 {
			return "", "", ErrOrigin
		}
	}
	return host, port, nil
}

// canonicalHost checks a DNS name, an IPv4 address or a bracketed IPv6 address and
// returns it in lower case and short form.
func canonicalHost(h string) (string, error) {
	if h == "" || len(h) > maxHostLen {
		return "", ErrOrigin
	}
	if strings.HasPrefix(h, "[") {
		a, err := netip.ParseAddr(h[1 : len(h)-1])
		if err != nil || !a.Is6() || a.Zone() != "" {
			return "", ErrOrigin
		}
		return "[" + a.String() + "]", nil
	}
	h = strings.ToLower(h)
	if strings.Trim(h, "0123456789.") == "" {
		// All digits and dots: an IPv4 address. ParseAddr refuses leading zeros, so
		// "010.0.0.1" never means an octal address.
		a, err := netip.ParseAddr(h)
		if err != nil || !a.Is4() {
			return "", ErrOrigin
		}
		return a.String(), nil
	}
	for label := range strings.SplitSeq(h, ".") {
		if !validLabel(label) {
			return "", ErrOrigin
		}
	}
	return h, nil
}

// validLabel: 1 to 63 of [a-z0-9-], no hyphen at either end.
func validLabel(l string) bool {
	if l == "" || len(l) > maxLabelLen || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := range len(l) {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
