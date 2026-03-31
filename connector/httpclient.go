package connector

import (
	"net/http"
	"time"
)

// DefaultUserAgent identifies glovebox connectors in HTTP requests.
var DefaultUserAgent = "GloveboxBot/0.2.0 (+https://github.com/leftathome/glovebox)"

// HTTPClientOptions configures the HTTP client returned by NewHTTPClient.
type HTTPClientOptions struct {
	// Timeout for the entire HTTP request. Defaults to 30s if zero.
	Timeout time.Duration
	// UserAgent overrides the default User-Agent string.
	// If empty, DefaultUserAgent is used.
	UserAgent string
}

// NewHTTPClient returns an *http.Client that always sets the User-Agent header
// on outgoing requests. The User-Agent is set via a custom RoundTripper and
// overwrites any value already present on the request.
func NewHTTPClient(opts HTTPClientOptions) *http.Client {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &userAgentTransport{
			base:      http.DefaultTransport,
			userAgent: ua,
		},
	}
}

// userAgentTransport is an http.RoundTripper that sets the User-Agent header
// on every outgoing request, overwriting any existing value.
type userAgentTransport struct {
	base      http.RoundTripper
	userAgent string
}

// RoundTrip clones the request, sets the User-Agent header, and delegates
// to the base transport. The clone avoids mutating the caller's request,
// which would violate the http.RoundTripper contract.
func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", t.userAgent)
	return t.base.RoundTrip(r)
}
