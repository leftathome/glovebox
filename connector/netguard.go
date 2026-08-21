package connector

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/leftathome/glovebox/connector/content"
)

// ErrBlockedAddress is returned when a connection is refused because the
// resolved address is one attacker-supplied URLs must not reach.
type ErrBlockedAddress struct {
	Host string
	IP   net.IP
}

func (e *ErrBlockedAddress) Error() string {
	return fmt.Sprintf("blocked address: %s resolves to %s (private, loopback, link-local or reserved)", e.Host, e.IP)
}

// GuardedClientOptions configures NewGuardedHTTPClient.
type GuardedClientOptions struct {
	// Timeout for the entire request. Defaults to 30s.
	Timeout time.Duration
	// UserAgent overrides DefaultUserAgent.
	UserAgent string
	// MaxRedirects caps the redirect chain. Defaults to 5.
	MaxRedirects int
	// AllowPrivateNetworks disables the address guard entirely. Set this
	// only for destinations the operator configured, never for a URL that
	// came out of fetched content.
	AllowPrivateNetworks bool
	// ValidateURL, when set, is called for every redirect hop before it is
	// followed. Returning an error aborts the chain. Wire this to the same
	// policy that admitted the original URL, so a redirect cannot reach a
	// destination the policy would have refused.
	ValidateURL func(rawURL string) error
}

// NewGuardedHTTPClient returns a client hardened for fetching
// attacker-influenced URLs.
//
// Checking a URL against a policy and then handing it to a stock
// http.Client is not sufficient, for two reasons this client closes:
//
//   - The policy resolves the hostname, but http.Transport resolves it
//     again when it dials. A DNS answer that is public at check time and
//     private a moment later (rebinding) passes the check and then connects
//     to the private address. This client resolves once and dials the
//     validated IP literal, so the address that was checked is the address
//     that is connected to.
//   - Redirects were followed with no re-check at all, so a public URL
//     could 302 straight to 169.254.169.254. Every hop is now re-validated
//     and the chain is capped.
//
// TLS verification is unaffected: http.Transport sets the SNI and verifies
// the certificate against the hostname from the request URL, not against
// the dial address.
func NewGuardedHTTPClient(opts GuardedClientOptions) *http.Client {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects == 0 {
		maxRedirects = 5
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &guardedDialer{dialer: dialer, allowPrivate: opts.AllowPrivateNetworks}

	transport := &http.Transport{
		DialContext:           guarded.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: &userAgentTransport{base: transport, userAgent: ua},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if opts.ValidateURL != nil {
				if err := opts.ValidateURL(req.URL.String()); err != nil {
					return fmt.Errorf("redirect to %s refused: %w", req.URL.Redacted(), err)
				}
			}
			return nil
		},
	}
}

type guardedDialer struct {
	dialer       *net.Dialer
	allowPrivate bool
}

// DialContext resolves the host once, rejects addresses the guard blocks,
// and dials the surviving IP literal directly so no second resolution can
// substitute a different answer.
func (g *guardedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses for %s", host)
	}

	var lastErr error
	for _, ipa := range ips {
		if !g.allowPrivate && content.IsBlockedIP(ipa.IP) {
			lastErr = &ErrBlockedAddress{Host: host, IP: ipa.IP}
			continue
		}
		conn, err := g.dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
