package powerschool

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// AssignmentLookupPath is the one PowerSchool endpoint the connector may
// POST to. It is the query the portal's own scores page issues to list a
// section's assignments and scores: the JSON body is a filter (section ids,
// student ids, a date range) and the call changes nothing upstream. There
// is no GET form of it.
const AssignmentLookupPath = "/ws/xte/assignment/lookup"

// ErrHostBlocked is returned (wrapped) by Transport for any request that is
// not an https request to the pinned PowerSchool host.
var ErrHostBlocked = fmt.Errorf("powerschool: request to unexpected host blocked")

// Transport is the http.RoundTripper every post-login request goes through.
//
// The session belongs to a real parent account. Two rules keep the
// connector from ever acting as that parent:
//
//  1. Host pin. Only https requests to Host are forwarded, so the session
//     cookie cannot be carried to another origin by a redirect or a
//     library code path (the library attaches the cookie itself).
//  2. Read-only. Everything is delegated to connector.ReadOnlyTransport,
//     which refuses any method but GET/HEAD -- with one named exception:
//     when AllowAssignmentLookup is set, a POST whose path is exactly
//     AssignmentLookupPath is forwarded. That exception exists because
//     PowerSchool exposes assignment scores only through that query
//     endpoint; it is off unless the assignments surface is enabled.
//
// Login needs POST and a browser and therefore cannot happen through this
// transport; it is a separate workload (spec 16 section 4).
type Transport struct {
	// Host is the pinned host[:port], lower-case.
	Host string
	// AllowAssignmentLookup enables the single POST exception.
	AllowAssignmentLookup bool
	// Base is the wrapped transport. nil means http.DefaultTransport.
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, t.Host) {
		// Path only: a query string may carry tokens.
		slog.Error("powerschool: blocked request to unexpected host",
			"scheme", req.URL.Scheme, "host", req.URL.Host, "path", req.URL.Path)
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: %s://%s%s", ErrHostBlocked, req.URL.Scheme, req.URL.Host, req.URL.Path)
	}

	if t.AllowAssignmentLookup && req.Method == http.MethodPost && req.URL.Path == AssignmentLookupPath {
		base := t.Base
		if base == nil {
			base = http.DefaultTransport
		}
		return base.RoundTrip(req)
	}

	ro := &connector.ReadOnlyTransport{Base: t.Base, Name: "powerschool"}
	return ro.RoundTrip(req)
}

// NewHTTPClient returns the *http.Client handed to the library in place of
// its default. host is the pinned host[:port] from NormalizeBaseURL.
//
// The client has no cookie jar: the library attaches the session cookies to
// each request itself, and a jar would let a response rewrite them.
func NewHTTPClient(host string, allowAssignmentLookup bool, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &Transport{
			Host:                  strings.ToLower(host),
			AllowAssignmentLookup: allowAssignmentLookup,
		},
	}
}
