package loginbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/leftathome/glovebox/internal/kubecm"
)

// Caller is an authenticated ServiceAccount.
type Caller struct {
	Namespace string
	Name      string
}

// String is "namespace/name", the form of the allow-list.
func (c Caller) String() string { return c.Namespace + "/" + c.Name }

// Errors of an Authenticator. The HTTP layer maps them to 401, 403 and 503.
var (
	// ErrUnauthenticated: no token, or one the API server does not vouch
	// for with this broker's audience.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden: a valid token for this audience, from a ServiceAccount
	// that is not on the allow-list.
	ErrForbidden = errors.New("forbidden")
	// ErrAuthUnavailable: the API server could not be asked.
	ErrAuthUnavailable = errors.New("authentication unavailable")
)

// Authenticator decides who sent a bearer token.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Caller, error)
}

// TokenReviewer verifies a ServiceAccount token by asking the API server
// (authentication.k8s.io/v1 TokenReview), which is the only party that can.
//
// Two checks follow the review, and both are needed:
//
//   - the AUDIENCE. The review is made for this broker's own audience and
//     the answer must name it. A pod's ordinary API token, or a token minted
//     for another broker instance, fails here: only a token the caller's pod
//     had projected for this audience passes. A token stolen from the wire
//     is therefore usable against this broker only, and only until it
//     expires.
//   - the ALLOW-LIST. The authenticated identity must be one of the named
//     ServiceAccounts. Any pod in the cluster can ask the API server for a
//     token with any audience for its own ServiceAccount; what it cannot do
//     is be a ServiceAccount it is not.
//
// It can do nothing else with the API: the client makes this one POST, to
// the API server only.
type TokenReviewer struct {
	endpoint string
	http     *http.Client
	token    func() (string, error)
	audience string
	allowed  map[string]bool
}

// maxTokenBytes bounds a bearer token. A projected ServiceAccount token is
// about a kilobyte.
const maxTokenBytes = 8 << 10

// NewTokenReviewer builds the reviewer. access is how the API server is
// reached (kubecm.InCluster); audience is this broker's; allowed lists the
// ServiceAccounts that may call it, each "namespace/name".
func NewTokenReviewer(access *kubecm.Access, audience string, allowed []string) (*TokenReviewer, error) {
	if access == nil || access.Token == nil {
		return nil, errors.New("loginbroker: no access to the Kubernetes API")
	}
	u, err := url.Parse(access.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("loginbroker: the Kubernetes API address must be an https URL")
	}
	audience = strings.TrimSpace(audience)
	if audience == "" || strings.ContainsAny(audience, " \t\n,") {
		return nil, errors.New("loginbroker: an audience of its own is required")
	}
	set := map[string]bool{}
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		ns, name, ok := strings.Cut(a, "/")
		if !ok || ns == "" || name == "" || strings.ContainsAny(a, " \t\n:*") || strings.Contains(name, "/") {
			return nil, errors.New("loginbroker: each allowed caller must be namespace/serviceaccount, without wildcards")
		}
		set[a] = true
	}
	if len(set) == 0 {
		return nil, errors.New("loginbroker: the caller allow-list is empty; nobody could ask for a login")
	}
	return &TokenReviewer{
		endpoint: "https://" + u.Host + "/apis/authentication.k8s.io/v1/tokenreviews",
		token:    access.Token,
		audience: audience,
		allowed:  set,
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &kubecm.Transport{Host: u.Host, Base: access.Transport},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Audience returns the audience tokens must be minted for.
func (r *TokenReviewer) Audience() string { return r.audience }

// Allowed returns the allow-list, sorted.
func (r *TokenReviewer) Allowed() []string {
	out := make([]string, 0, len(r.allowed))
	for a := range r.allowed {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

type tokenReview struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Spec       tokenReviewSpec   `json:"spec"`
	Status     tokenReviewStatus `json:"status,omitempty"`
}

type tokenReviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences"`
}

type tokenReviewStatus struct {
	Authenticated bool     `json:"authenticated"`
	Audiences     []string `json:"audiences"`
	User          struct {
		Username string `json:"username"`
	} `json:"user"`
}

// serviceAccountPrefix is how the API server names a ServiceAccount.
const serviceAccountPrefix = "system:serviceaccount:"

// Authenticate implements Authenticator. No error it returns carries the
// token or anything the API server said about it.
func (r *TokenReviewer) Authenticate(ctx context.Context, token string) (Caller, error) {
	if token == "" || len(token) > maxTokenBytes {
		return Caller{}, ErrUnauthenticated
	}
	body, err := json.Marshal(tokenReview{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       tokenReviewSpec{Token: token, Audiences: []string{r.audience}},
	})
	if err != nil {
		return Caller{}, ErrAuthUnavailable
	}
	own, err := r.token()
	if err != nil || own == "" {
		return Caller{}, fmt.Errorf("%w: the broker's own ServiceAccount token could not be read", ErrAuthUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return Caller{}, ErrAuthUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+own)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return Caller{}, fmt.Errorf("%w: the API server could not be reached", ErrAuthUnavailable)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// 403 here is the broker's own RBAC (no create on tokenreviews),
		// not the caller's fault.
		return Caller{}, fmt.Errorf("%w: TokenReview answered HTTP %d", ErrAuthUnavailable, resp.StatusCode)
	}
	var review tokenReview
	if err := json.Unmarshal(raw, &review); err != nil {
		return Caller{}, fmt.Errorf("%w: undecodable TokenReview answer", ErrAuthUnavailable)
	}
	if !review.Status.Authenticated {
		return Caller{}, ErrUnauthenticated
	}
	// The API server lists the audiences, among those asked for, that the
	// token is valid for. An API server too old to know about audiences
	// leaves the list empty; that is not accepted as "any".
	audienceOK := false
	for _, a := range review.Status.Audiences {
		if a == r.audience {
			audienceOK = true
		}
	}
	if !audienceOK {
		return Caller{}, ErrUnauthenticated
	}
	rest, ok := strings.CutPrefix(review.Status.User.Username, serviceAccountPrefix)
	if !ok {
		return Caller{}, ErrForbidden
	}
	ns, name, ok := strings.Cut(rest, ":")
	if !ok || ns == "" || name == "" || strings.Contains(name, ":") {
		return Caller{}, ErrForbidden
	}
	caller := Caller{Namespace: ns, Name: name}
	if !r.allowed[caller.String()] {
		return caller, ErrForbidden
	}
	return caller, nil
}
