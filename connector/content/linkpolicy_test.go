package content

import (
	"strings"
	"testing"
)

func safePolicyConfig() LinkPolicyConfig {
	return LinkPolicyConfig{Default: "safe"}
}

func TestLinkPolicy_SafeDeniesPrivateIPs(t *testing.T) {
	lp := NewLinkPolicy(safePolicyConfig())
	tests := []string{
		"https://10.0.0.1/page",
		"https://172.16.0.1/page",
		"https://192.168.1.1/page",
		"https://127.0.0.1/page",
		"https://169.254.0.1/page",
	}
	for _, u := range tests {
		allowed, reason := lp.Check(u)
		if allowed {
			t.Errorf("safe mode should deny %s: %s", u, reason)
		}
	}
}

func TestLinkPolicy_SafeDeniesHTTP(t *testing.T) {
	lp := NewLinkPolicy(safePolicyConfig())
	allowed, reason := lp.Check("http://example.com/page")
	if allowed {
		t.Errorf("safe mode should deny http: %s", reason)
	}
}

func TestLinkPolicy_SafeDeniesNonHTTPS(t *testing.T) {
	lp := NewLinkPolicy(safePolicyConfig())
	for _, scheme := range []string{"ftp://example.com", "file:///etc/passwd"} {
		allowed, _ := lp.Check(scheme)
		if allowed {
			t.Errorf("safe mode should deny %s", scheme)
		}
	}
}

func TestLinkPolicy_SafeAllowsPublicHTTPS(t *testing.T) {
	lp := NewLinkPolicy(safePolicyConfig())
	// Use a known public IP to avoid DNS lookup issues in tests
	allowed, reason := lp.Check("https://8.8.8.8/page")
	if !allowed {
		t.Errorf("safe mode should allow public https: %s", reason)
	}
}

func TestLinkPolicy_RuleAllowsInternalDomain(t *testing.T) {
	lp := NewLinkPolicy(LinkPolicyConfig{
		Default: "safe",
		Rules: []LinkPolicyRule{
			{Match: "domain:wiki.home.lan", Allow: true},
		},
	})
	allowed, _ := lp.Check("http://wiki.home.lan/page")
	if !allowed {
		t.Error("rule should allow wiki.home.lan")
	}
}

func TestLinkPolicy_RuleAllowsCIDR(t *testing.T) {
	lp := NewLinkPolicy(LinkPolicyConfig{
		Default: "safe",
		Rules: []LinkPolicyRule{
			{Match: "network:10.0.0.0/8", Allow: true},
		},
	})
	allowed, _ := lp.Check("https://10.0.0.5/page")
	if !allowed {
		t.Error("rule should allow 10.x network")
	}
}

func TestLinkPolicy_RuleDeniesScheme(t *testing.T) {
	lp := NewLinkPolicy(LinkPolicyConfig{
		Default: "unrestricted",
		Rules: []LinkPolicyRule{
			{Match: "scheme:ftp", Allow: false},
		},
	})
	allowed, _ := lp.Check("ftp://files.example.com/data")
	if allowed {
		t.Error("rule should deny ftp scheme")
	}
}

func TestLinkPolicy_UnrestrictedAllowsEverything(t *testing.T) {
	lp := NewLinkPolicy(LinkPolicyConfig{Default: "unrestricted"})
	tests := []string{
		"http://10.0.0.1/internal",
		"ftp://files.local/data",
		"https://example.com/public",
	}
	for _, u := range tests {
		allowed, _ := lp.Check(u)
		if !allowed {
			t.Errorf("unrestricted should allow %s", u)
		}
	}
}

func TestLinkPolicy_FirstMatchWins(t *testing.T) {
	lp := NewLinkPolicy(LinkPolicyConfig{
		Default: "safe",
		Rules: []LinkPolicyRule{
			{Match: "domain:special.com", Allow: true},
			{Match: "domain:special.com", Allow: false},
		},
	})
	allowed, reason := lp.Check("http://special.com/page")
	if !allowed {
		t.Errorf("first rule should win (allow): %s", reason)
	}
}

func TestLinkPolicy_InvalidURL(t *testing.T) {
	lp := NewLinkPolicy(safePolicyConfig())
	allowed, reason := lp.Check("://broken")
	if allowed {
		t.Error("should deny invalid URL")
	}
	if !strings.Contains(reason, "invalid") {
		t.Errorf("reason should mention invalid: %s", reason)
	}
}
