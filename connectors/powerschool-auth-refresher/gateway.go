package main

import (
	"net/http"
	"strings"
	"sync"
)

// gatewayCookies lets one home-page read survive the access gateway
// renewing its own session cookie in mid-request.
//
// The stored session is the browser's whole cookie set for the portal host:
// the portal's session cookie AND the gateway's (MRHSession and friends on
// the reference instance). The two expire independently. When the gateway's
// has lapsed while the portal's is still good, the gateway answers a request
// with a redirect to its policy endpoint, sets a fresh cookie there, and
// redirects back. The library attaches the stored cookies itself, as one
// fixed Cookie header, and the HTTP client deliberately has no jar -- so the
// fresh cookie would be dropped, the request would arrive back with the
// lapsed one, and the read would end on the gateway's error page. A good
// portal session would then look unreadable (the run stops without logging
// in) for as long as it stays young.
//
// This RoundTripper is the narrowest thing that fixes that. It sits inside
// the pinned, read-only transport (so it only ever sees https requests to
// the portal host), remembers the Set-Cookie values of the responses it
// passes, and overlays them on the Cookie header of the following requests:
// a renewed cookie replaces the stored one of the same name, a new one is
// added, a deleted one is dropped.
//
// One is created per probe and thrown away with it. What it collects is
// never written to Vault, never handed to the connector and never logged.
type gatewayCookies struct {
	base http.RoundTripper

	mu      sync.Mutex
	overlay map[string]*string // name -> value; nil = deleted by the server
}

// maxOverlayCookies bounds what a server can make this hold.
const maxOverlayCookies = 64

func newGatewayCookies(base http.RoundTripper) *gatewayCookies {
	if base == nil {
		base = http.DefaultTransport
	}
	return &gatewayCookies{base: base, overlay: map[string]*string{}}
}

// RoundTrip implements http.RoundTripper.
func (g *gatewayCookies) RoundTrip(req *http.Request) (*http.Response, error) {
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
func (g *gatewayCookies) apply(req *http.Request) {
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
			if v == nil {
				continue // deleted
			}
			pairs = append(pairs, c.Name+"="+*v)
			continue
		}
		pairs = append(pairs, c.Name+"="+c.Value)
	}
	for name, v := range g.overlay {
		if v != nil && !seen[name] {
			pairs = append(pairs, name+"="+*v)
		}
	}
	if len(pairs) == 0 {
		req.Header.Del("Cookie")
		return
	}
	req.Header.Set("Cookie", strings.Join(pairs, "; "))
}

// collect records the cookies resp sets or deletes.
func (g *gatewayCookies) collect(resp *http.Response) {
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
		// Deleted: Max-Age <= 0 as sent, an empty value, or the customary
		// expiry at the epoch.
		if c.MaxAge < 0 || c.Value == "" || (!c.Expires.IsZero() && c.Expires.Unix() <= 86400) {
			g.overlay[c.Name] = nil
			continue
		}
		value := c.Value
		g.overlay[c.Name] = &value
	}
}
