// Package auth implements the bearer-token authentication layer described in
// docs/specs/10-external-ingest-auth-design.md. This file provides the
// trusted-proxy IP extraction and the low-cardinality CIDR bucketing helper
// used by metric labels (§6.4) and the rate limiter (§5.3).
package auth

import (
	"net"
	"strings"
)

// ProxyResolver derives the client IP from r.RemoteAddr + X-Forwarded-For
// honoring the spec 10 §5.3 trusted-proxy rule: X-Forwarded-For is honored
// only when the immediate TCP peer falls within a configured trusted-proxy
// CIDR. Otherwise the forwarded header is ignored and r.RemoteAddr is used,
// which prevents a client speaking directly to the cluster IP from forging
// its own client identity.
type ProxyResolver struct {
	// TrustedCIDRs lists the operator-configured trusted-proxy networks.
	// Nil/empty means: never trust X-Forwarded-For.
	TrustedCIDRs []*net.IPNet
}

// ResolveClientIP returns the client IP for rate-limit and audit purposes.
// remoteAddr is r.RemoteAddr (host:port); xff is r.Header.Get("X-Forwarded-For")
// (may be empty or contain a comma-separated chain).
//
// The result is always a non-empty string; on parse failure the function
// returns remoteAddr verbatim minus its port (best-effort) so downstream
// rate-limit keying never sees "".
func (p *ProxyResolver) ResolveClientIP(remoteAddr, xff string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if xff == "" {
		return host
	}
	peerIP := net.ParseIP(host)
	if peerIP == nil {
		return host
	}
	trusted := false
	for _, c := range p.TrustedCIDRs {
		if c.Contains(peerIP) {
			trusted = true
			break
		}
	}
	if !trusted {
		return host
	}
	// Use the right-most entry per RFC 7239: the last hop the chain
	// appended, i.e. the closest trusted proxy's view of its peer.
	parts := strings.Split(xff, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if net.ParseIP(last) == nil {
		return host
	}
	return last
}

// BucketIP returns a low-cardinality CIDR-string form of an IP for metric
// label use per spec 10 §6.4: IPv4 collapses to /24, IPv6 collapses to /64.
// Unparseable input returns "unknown" so the label is still bounded.
//
// IPv4-mapped IPv6 (e.g., "::ffff:192.0.2.1") is canonicalized to IPv4 first
// via ip.To4(), so it lands in the IPv4 /24 bucket alongside the same address
// expressed as plain IPv4.
func BucketIP(s string) string {
	ip := net.ParseIP(s)
	if ip == nil {
		return "unknown"
	}
	if ip4 := ip.To4(); ip4 != nil {
		// Zero the low octet; To4() returns a length-4 slice we may mutate.
		ip4 = append(net.IP(nil), ip4...)
		ip4[3] = 0
		return ip4.String() + "/24"
	}
	ip6 := ip.To16()
	if ip6 == nil {
		return "unknown"
	}
	ip6 = append(net.IP(nil), ip6...)
	// Zero out the lower 64 bits (interface identifier).
	for i := 8; i < 16; i++ {
		ip6[i] = 0
	}
	return ip6.String() + "/64"
}
