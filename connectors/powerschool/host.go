package powerschool

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeBaseURL reduces an operator-supplied PowerSchool location to the
// two forms the connector needs: the base URL the library is constructed
// with ("https://ps.example.org") and the bare host[:port] the transport
// pins every request to.
//
// Operators store either a bare host or a full URL with a path (the Vault
// property is a full https URL). Any path, query or fragment is dropped;
// the scheme is always https -- a parent session cookie must never travel
// in the clear, so an http:// value is upgraded rather than honoured.
func NormalizeBaseURL(raw string) (baseURL, host string, err error) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", "", fmt.Errorf("empty host")
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil {
		return "", "", fmt.Errorf("parse host: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("host must not carry userinfo")
	}
	host = strings.ToLower(u.Host)
	if host == "" || strings.HasPrefix(host, ":") {
		return "", "", fmt.Errorf("no host in %q", strings.TrimSpace(raw))
	}
	return "https://" + host, host, nil
}
