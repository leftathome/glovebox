package records

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
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// maxAnswerBytes bounds how much of an answer is read. A journal answer
// is a few hundred bytes.
const maxAnswerBytes = 64 << 10

// maxRetryAfter bounds a Retry-After the client passes on.
const maxRetryAfter = 24 * time.Hour

// ErrHostBlocked is returned (wrapped) by the client's transport for any
// request that is not to the journal's own address.
var ErrHostBlocked = errors.New("records: request to a host other than the journal blocked")

// ErrNoToken is returned by Send when the token file cannot be read or is
// empty: nothing was sent. It is a deployment fault, not the journal's
// answer, so the sender alerts on it (ActRetryAlert, code "token").
var ErrNoToken = errors.New("records: no ServiceAccount token for the journal")

// pinned forwards only requests to exactly one scheme and host (as
// internal/loginbroker/brokerapi does; not imported, its errors name the
// login broker).
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

// Client posts snapshots to one journal. It carries the pod's projected
// ServiceAccount token (audience glovebox-journal) and sends it to the
// journal's address only: the transport refuses any other host, follows no
// redirect and uses no proxy.
type Client struct {
	endpoint string
	http     *http.Client
	token    func() (string, error)
}

// Answer is the journal's answer to one snapshot. Code 0 never appears in
// an Answer returned with a nil error; the sender uses it for transport
// failures. Status and Reason are empty when the body is not the
// journal's JSON.
type Answer struct {
	Code       int
	Status     string
	Reason     string
	RetryAfter time.Duration
}

// NewClient builds a client for the journal at baseURL (scheme and host;
// a trailing "/" is allowed). token returns the bearer token for each
// call; TokenFile is the usual source. timeout <= 0 is the default (90 s).
// inner replaces the innermost transport in tests (nil: a plain one
// without a proxy).
func NewClient(baseURL string, token func() (string, error), timeout time.Duration, inner http.RoundTripper) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("records: the journal address must be an http or https URL with a host")
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("records: the journal address must not carry a path, query or fragment")
	}
	if token == nil {
		return nil, errors.New("records: the journal client needs a token source")
	}
	if timeout <= 0 {
		timeout = DefaultTimeoutSeconds * time.Second
	}
	if inner == nil {
		inner = &http.Transport{
			Proxy:                 nil,
			ResponseHeaderTimeout: timeout,
			TLSHandshakeTimeout:   10 * time.Second,
			MaxIdleConnsPerHost:   1,
		}
	}
	return &Client{
		endpoint: u.Scheme + "://" + u.Host + journalapi.PathRecords,
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
// kubelet rotates a projected token in place. Its errors never carry the
// token.
func TokenFile(path string) func() (string, error) {
	return func() (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("records: could not read the ServiceAccount token for the journal")
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", errors.New("records: the ServiceAccount token for the journal is empty")
		}
		return token, nil
	}
}

// Send posts one snapshot body, verbatim (A9). A nil error means the
// journal (or something in front of it) answered; what it said is in the
// Answer. An error is a token that could not be read or a transport
// failure; it never carries the token or the body.
func (c *Client) Send(ctx context.Context, body []byte) (Answer, error) {
	token, err := c.token()
	if err != nil || token == "" {
		return Answer{}, ErrNoToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Answer{}, errors.New("records: could not build the journal request")
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
		return Answer{}, fmt.Errorf("records: journal request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerBytes))
		_ = resp.Body.Close()
	}()
	a := Answer{Code: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	if err != nil || len(raw) > maxAnswerBytes {
		return a, nil
	}
	var r journalapi.Response
	if json.Unmarshal(raw, &r) == nil {
		a.Status, a.Reason = r.Status, r.Reason
	}
	return a, nil
}

// retryAfter reads a Retry-After of delay-seconds (the journal's form);
// anything else is 0. Capped at 24 hours.
func retryAfter(h string) time.Duration {
	n, err := strconv.ParseInt(strings.TrimSpace(h), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	if n > int64(maxRetryAfter/time.Second) {
		return maxRetryAfter
	}
	return time.Duration(n) * time.Second
}
