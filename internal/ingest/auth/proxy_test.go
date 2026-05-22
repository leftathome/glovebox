package auth

import (
	"net"
	"testing"
)

func TestResolveClientIP_NoTrustedCIDRs_UsesRemoteAddr(t *testing.T) {
	r := &ProxyResolver{}
	got := r.ResolveClientIP("203.0.113.5:54321", "10.0.0.1")
	if got != "203.0.113.5" {
		t.Errorf("got %q, want 203.0.113.5", got)
	}
}

func TestResolveClientIP_TrustedPeer_UsesXFFRightmost(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.244.0.0/16")
	r := &ProxyResolver{TrustedCIDRs: []*net.IPNet{cidr}}
	got := r.ResolveClientIP("10.244.0.5:54321", "198.51.100.7, 192.0.2.3")
	if got != "192.0.2.3" {
		t.Errorf("got %q, want 192.0.2.3", got)
	}
}

func TestResolveClientIP_UntrustedPeer_IgnoresXFF(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.244.0.0/16")
	r := &ProxyResolver{TrustedCIDRs: []*net.IPNet{cidr}}
	// Untrusted peer attempting to forge XFF.
	got := r.ResolveClientIP("198.51.100.99:54321", "10.0.0.1")
	if got != "198.51.100.99" {
		t.Errorf("got %q, want 198.51.100.99 (XFF must be ignored)", got)
	}
}

func TestBucketIP_IPv4_24(t *testing.T) {
	if got := BucketIP("203.0.113.42"); got != "203.0.113.0/24" {
		t.Errorf("got %q, want 203.0.113.0/24", got)
	}
}

func TestBucketIP_IPv6_64(t *testing.T) {
	if got := BucketIP("2001:db8:1:2:3:4:5:6"); got != "2001:db8:1:2::/64" {
		t.Errorf("got %q, want 2001:db8:1:2::/64", got)
	}
}
