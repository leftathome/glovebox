package content

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

type LinkPolicyConfig struct {
	Default string           `json:"default"` // "safe" or "unrestricted"
	Rules   []LinkPolicyRule `json:"rules"`
}

type LinkPolicyRule struct {
	Match string `json:"match"`
	Allow bool   `json:"allow"`
}

type LinkPolicy struct {
	config LinkPolicyConfig
}

func NewLinkPolicy(config LinkPolicyConfig) *LinkPolicy {
	if config.Default == "" {
		config.Default = "safe"
	}
	return &LinkPolicy{config: config}
}

func (lp *LinkPolicy) Check(rawURL string) (bool, string) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false, "invalid URL"
	}

	// Check rules first (first match wins)
	for _, rule := range lp.config.Rules {
		if lp.ruleMatches(rule, parsed) {
			if rule.Allow {
				return true, "allowed by rule: " + rule.Match
			}
			return false, "denied by rule: " + rule.Match
		}
	}

	if lp.config.Default == "unrestricted" {
		return true, "unrestricted mode"
	}

	// Safe mode checks
	if parsed.Scheme != "https" {
		return false, fmt.Sprintf("scheme %q not allowed in safe mode (https only)", parsed.Scheme)
	}

	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return false, fmt.Sprintf("private IP %s not allowed in safe mode", host)
		}
	} else {
		// DNS name -- resolve and check
		ips, err := net.LookupIP(host)
		if err == nil {
			for _, ip := range ips {
				if isPrivateIP(ip) {
					return false, fmt.Sprintf("host %s resolves to private IP, not allowed in safe mode", host)
				}
			}
		}
	}

	return true, "passed safe mode checks"
}

func (lp *LinkPolicy) ruleMatches(rule LinkPolicyRule, u *url.URL) bool {
	parts := strings.SplitN(rule.Match, ":", 2)
	if len(parts) != 2 {
		return false
	}
	matchType, matchValue := parts[0], parts[1]

	switch matchType {
	case "domain":
		return strings.EqualFold(u.Hostname(), matchValue)
	case "scheme":
		return strings.EqualFold(u.Scheme, matchValue)
	case "network":
		_, cidr, err := net.ParseCIDR(matchValue)
		if err != nil {
			return false
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		if ip == nil {
			ips, err := net.LookupIP(host)
			if err != nil || len(ips) == 0 {
				return false
			}
			ip = ips[0]
		}
		return cidr.Contains(ip)
	}

	return false
}

func isPrivateIP(ip net.IP) bool {
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}

	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
