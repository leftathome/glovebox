package content

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", // loopback
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1", // RFC1918
		"169.254.169.254",                            // cloud metadata
		"0.0.0.0",                                    // unspecified
		"100.64.0.1",                                 // CGNAT / overlay
		"192.0.2.1",                                  // TEST-NET-1
		"198.18.0.1",                                 // benchmarking
		"198.51.100.1",                               // TEST-NET-2
		"203.0.113.1",                                // TEST-NET-3
		"224.0.0.1",                                  // multicast
		"240.0.0.1",                                  // reserved
		"::1",                                        // IPv6 loopback
		"fe80::1",                                    // IPv6 link-local
		"fc00::1",                                    // IPv6 unique local
		"::",                                         // IPv6 unspecified
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", // IPv4-mapped bypasses
		"64:ff9b::1",  // NAT64
		"2001:db8::1", // documentation
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test bug: cannot parse %q", s)
		}
		if !IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = false, want true", s)
		}
	}

	allowed := []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", // ordinary public IPv4
		"172.32.0.1",           // just outside RFC1918
		"2606:4700:4700::1111", // public IPv6
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test bug: cannot parse %q", s)
		}
		if IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = true, want false", s)
		}
	}
}

// Fail closed on an address that cannot be classified.
func TestIsBlockedIP_NilIsBlocked(t *testing.T) {
	if !IsBlockedIP(nil) {
		t.Error("IsBlockedIP(nil) = false, want true")
	}
}

// A hostname that does not resolve must be denied, not admitted. The old
// code treated a lookup error as "found no private IPs" and returned true.
func TestLinkPolicy_SafeDeniesUnresolvableHost(t *testing.T) {
	lp := NewLinkPolicy(LinkPolicyConfig{Default: "safe"})
	allowed, reason := lp.Check("https://this-host-does-not-exist.invalid/page")
	if allowed {
		t.Errorf("unresolvable host was allowed (reason: %s); safe mode must fail closed", reason)
	}
}
