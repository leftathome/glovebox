// Package psportal holds what the PowerSchool connector and the PowerSchool
// login job (connectors/powerschool-auth-refresher) must agree on about the
// portal they both talk to: the access gateway in front of it, and the
// hours in which nobody may log in to the account.
//
// Both binaries import it so that neither can drift from the other. It
// holds no credentials and makes no requests of its own.
package psportal

import (
	"net/http"
	"strings"
	"sync"
)

// gatewayPaths are where the access gateway in front of the portal (an F5
// APM on the reference instance) leaves a client it will not, or cannot,
// give a session: its policy endpoint and its logout/error page.
var gatewayPaths = []string{"/my.policy", GatewayLogoutPath}

// GatewayLogoutPath is the gateway's "your session could not be
// established / has ended" page. Being sent there with the session's
// cookies attached is the gateway refusing the session; being left at the
// policy endpoint is not an answer at all.
const GatewayLogoutPath = "/my.logout.php3"

// IsGatewayPath reports whether path is one of the access gateway's own
// pages rather than a page of the portal.
func IsGatewayPath(path string) bool {
	for _, p := range gatewayPaths {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// GatewayCookies lets a read made with a stored session survive the access
// gateway renewing its own session cookie in mid-request.
//
// The stored session is the browser's whole cookie set for the portal host:
// the portal's session cookie AND the gateway's (MRHSession and friends on
// the reference instance). The two expire independently. When the gateway's
// has lapsed while the portal's is still good, the gateway answers a request
// with a redirect to its policy endpoint, sets a fresh cookie there, and
// redirects back. powerschool-go attaches the stored cookies itself, as one
// fixed Cookie header, and the HTTP client deliberately has no jar -- so the
// fresh cookie would be dropped, the request would arrive back with the
// lapsed one, and the read would end on the gateway's error page. A good
// portal session would then look unreadable, and a dead one would look like
// a page that failed to parse instead of a session that was refused.
//
// This RoundTripper is the narrowest thing that fixes that. It is meant to
// sit INSIDE the pinned, read-only transport (so it only ever sees https
// requests to the portal host), remembers the Set-Cookie values of the
// responses it passes, and overlays them on the Cookie header of the
// following requests: a renewed cookie replaces the stored one of the same
// name and a new one is added.
//
// A response can only ADD to or REPLACE what is sent. A Set-Cookie that
// deletes a cookie is ignored: honouring it would let any response -- an
// error page, a page this code does not understand -- strip the session
// from the requests that follow, and turn "could not tell" into "dead".
//
// What it holds must not outlive the work it was made for: create one per
// single read, or call Reset at the start of each poll. What it collects is
// never written anywhere and never logged.
type GatewayCookies struct {
	base http.RoundTripper

	mu      sync.Mutex
	overlay map[string]string // name -> value set by a response
}

// maxOverlayCookies bounds what a server can make a GatewayCookies hold.
const maxOverlayCookies = 64

// NewGatewayCookies wraps base (nil means http.DefaultTransport).
func NewGatewayCookies(base http.RoundTripper) *GatewayCookies {
	if base == nil {
		base = http.DefaultTransport
	}
	return &GatewayCookies{base: base, overlay: map[string]string{}}
}

// Reset forgets everything collected so far. The next request goes out
// with exactly the cookies its caller attached.
func (g *GatewayCookies) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.overlay = map[string]string{}
}

// RoundTrip implements http.RoundTripper.
func (g *GatewayCookies) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	pending := len(g.overlay) > 0
	g.mu.Unlock()
	if pending {
		// A RoundTripper must not modify the request it is given.
		req = req.Clone(req.Context())
		g.apply(req)
	}

	resp, err := g.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	g.collect(resp)
	return resp, nil
}

// apply rewrites req's Cookie header with the overlay.
func (g *GatewayCookies) apply(req *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var pairs []string
	seen := map[string]bool{}
	for _, c := range req.Cookies() {
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		if v, ok := g.overlay[c.Name]; ok {
			pairs = append(pairs, c.Name+"="+v)
			continue
		}
		pairs = append(pairs, c.Name+"="+c.Value)
	}
	for name, v := range g.overlay {
		if !seen[name] {
			pairs = append(pairs, name+"="+v)
		}
	}
	if len(pairs) == 0 {
		req.Header.Del("Cookie")
		return
	}
	req.Header.Set("Cookie", strings.Join(pairs, "; "))
}

// collect records the cookies resp sets. Deletions are ignored (see the
// type's comment).
func (g *GatewayCookies) collect(resp *http.Response) {
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		if _, known := g.overlay[c.Name]; !known && len(g.overlay) >= maxOverlayCookies {
			continue
		}
		// A deletion: Max-Age <= 0 as sent, an empty value, or the customary
		// expiry at the epoch. Not applied.
		if c.MaxAge < 0 || c.Value == "" || (!c.Expires.IsZero() && c.Expires.Unix() <= 86400) {
			continue
		}
		g.overlay[c.Name] = c.Value
	}
}
