package content

import "net"

// extraBlockedNetworks covers ranges the stdlib predicates in IsBlockedIP
// do not classify: carrier-grade NAT (which Tailscale and similar overlays
// use, so it routes to real hosts), the IETF documentation and benchmarking
// ranges, NAT64, and IPv4-mapped IPv6.
//
// This list and IsBlockedIP are the single source of truth for "an address
// an attacker-supplied URL must not reach". connector/netguard.go enforces
// it at connect time; keep this package free of any dependency on the
// parent connector package so that import direction stays acyclic.
var extraBlockedNetworks = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // CGNAT / overlay networks
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved
		"::/128",          // unspecified
		"64:ff9b::/96",    // NAT64
		"2001:db8::/32",   // documentation
		"fc00::/7",        // unique local
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// IsBlockedIP reports whether ip is one an attacker-supplied URL must never
// be able to reach: loopback, private, link-local (which includes the cloud
// metadata address 169.254.169.254), multicast, unspecified, and the
// reserved ranges above.
//
// IPv4-mapped IPv6 addresses (::ffff:127.0.0.1) are normalised first, so a
// mapped form cannot slip past the IPv4 checks.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true // fail closed on an address we cannot classify
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, n := range extraBlockedNetworks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
