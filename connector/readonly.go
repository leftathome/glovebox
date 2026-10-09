package connector

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"time"
)

// ErrWriteBlocked is returned (wrapped) by ReadOnlyTransport for any request
// that is not a safe, read-only HTTP method.
var ErrWriteBlocked = fmt.Errorf("connector: non-read-only HTTP request blocked")

// ReadOnlyTransport is an http.RoundTripper that refuses every request whose
// method is not GET or HEAD. It never forwards a blocked request.
//
// It exists for connectors whose credential class is real-readonly but whose
// credential is a real person's account (a parent's school login, say). The
// upstream account can post, message and submit; the connector must not be
// able to, whatever a library upgrade or a future code path asks for. A
// connector that genuinely needs to write must be a separate, explicitly
// labelled workload -- it does not get to loosen this one.
type ReadOnlyTransport struct {
	// Base is the wrapped transport. nil means http.DefaultTransport.
	Base http.RoundTripper
	// Name identifies the connector in the log line for a blocked request.
	Name string
}

// RoundTrip implements http.RoundTripper.
func (t *ReadOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case http.MethodGet, http.MethodHead, "":
		// "" is GET per net/http.
	default:
		// Path only: a query string may carry tokens.
		slog.Error("connector: blocked non-read-only request",
			"connector", t.Name, "method", req.Method,
			"host", req.URL.Host, "path", req.URL.Path)
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: %s %s%s", ErrWriteBlocked, req.Method, req.URL.Host, req.URL.Path)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// NewReadOnlyHTTPClient returns an *http.Client with a cookie jar whose
// transport is a ReadOnlyTransport. Hand it to a source library in place of
// that library's default client.
func NewReadOnlyHTTPClient(name string, timeout time.Duration) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("connector: cookie jar: %w", err)
	}
	return &http.Client{
		Jar:       jar,
		Timeout:   timeout,
		Transport: &ReadOnlyTransport{Name: name},
	}, nil
}
