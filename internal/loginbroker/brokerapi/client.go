package brokerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultTimeout bounds one refresh call. A refresh that logs in takes as
// long as the login: for a browser-driven site that is minutes, not
// seconds. The broker bounds its own work below this.
const DefaultTimeout = 7 * time.Minute

// maxAnswerBytes bounds how much of an answer is read. A session is a few
// kilobytes of cookies.
const maxAnswerBytes = 1 << 20

// ErrHostBlocked is returned (wrapped) by the client's transport for any
// request that is not to the broker's own address.
var ErrHostBlocked = errors.New("login broker client: request to another host blocked")

// StatusError is a non-200 answer from the broker: the HTTP status and the
// broker's classified error code, never a response body.
type StatusError struct {
	Status int
	Code   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("login broker: HTTP %d %s", e.Status, e.Code)
}

// pinned forwards only requests to exactly one scheme and host.
type pinned struct {
	scheme, host string
	base         http.RoundTripper
}

func (p *pinned) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	if u == nil || u.Scheme != p.scheme || u.Host != p.host || u.User != nil || u.Opaque != "" ||
		(req.Host != "" && req.Host != p.host) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrHostBlocked
	}
	return p.base.RoundTrip(req)
}

// Client asks one login broker for a session. It carries the pod's
// projected ServiceAccount token -- minted for the broker's audience and
// useless anywhere else -- and sends it to the broker's address only: the
// transport refuses any other host, follows no redirect and uses no proxy.
type Client struct {
	endpoint string
	http     *http.Client
	token    func() (string, error)
}

// NewClient builds a client for the broker at baseURL
// ("http://name.namespace.svc:8080"; no path). token returns the bearer
// token for each call; TokenFile is the usual source. inner replaces the
// innermost transport in tests (nil is a plain one without a proxy).
func NewClient(baseURL string, token func() (string, error), timeout time.Duration, inner http.RoundTripper) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("the login broker address must be an http or https URL with a host")
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("the login broker address must not carry a path, query or fragment")
	}
	if token == nil {
		return nil, errors.New("the login broker client needs a token source")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if inner == nil {
		inner = &http.Transport{
			Proxy:                 nil,
			ResponseHeaderTimeout: timeout,
			TLSHandshakeTimeout:   10 * time.Second,
		}
	}
	return &Client{
		endpoint: u.Scheme + "://" + u.Host + RefreshPath,
		token:    token,
		http: &http.Client{
			Timeout:   timeout,
			Transport: &pinned{scheme: u.Scheme, host: u.Host, base: inner},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// TokenFile returns a token source that reads path on every call: the
// kubelet rotates a projected token in place.
func TokenFile(path string) func() (string, error) {
	return func() (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("could not read the ServiceAccount token for the login broker")
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", errors.New("the ServiceAccount token for the login broker is empty")
		}
		return token, nil
	}
}

// Refresh asks the broker for a session. fullFingerprint is the caller's
// fingerprint of the session it found dead ("" when it has none); only a
// prefix of it is sent.
//
// A nil error means the broker answered with a Response; what it decided is
// in Response.Outcome. An error is a transport failure, a *StatusError
// (the broker refused the caller) or ErrUnexpectedAnswer. No error carries
// the token or any part of an answer's body.
func (c *Client) Refresh(ctx context.Context, reason, fullFingerprint string) (*Response, error) {
	body, err := json.Marshal(Request{Reason: reason, Fingerprint: ShortFingerprint(fullFingerprint)})
	if err != nil {
		return nil, err
	}
	token, err := c.token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("login broker: could not build the request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error would quote the address; the cause is enough.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("login broker: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return nil, errors.New("login broker: the answer could not be read")
	}
	if resp.StatusCode != http.StatusOK {
		var e Error
		_ = json.Unmarshal(raw, &e)
		return nil, &StatusError{Status: resp.StatusCode, Code: safeCode(e.Error)}
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil || out.Outcome == "" {
		return nil, ErrUnexpectedAnswer
	}
	out.Outcome, out.Code, out.Detail = safeCode(out.Outcome), safeCode(out.Code), safeCode(out.Detail)
	return &out, nil
}

// safeCode keeps a short identifier-like prefix of s: codes end up in log
// lines and metric labels on the caller's side.
func safeCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == ':' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}
