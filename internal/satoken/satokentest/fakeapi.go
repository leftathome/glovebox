// Package satokentest is a stand-in for the one Kubernetes API endpoint a
// satoken.TokenReviewer uses, for tests in any package.
package satokentest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/leftathome/glovebox/internal/kubecm"
)

// OwnToken is the reviewer's own ServiceAccount token as the fake sees it.
const OwnToken = "the-reviewers-own-sa-token"

// Status is what the fake API server answers about a presented token.
// The wire structs below deliberately duplicate the TokenReview types: this is
// an independent fake of the API server, not a reuse of satoken internals.
type Status struct {
	Authenticated bool     `json:"authenticated"`
	Audiences     []string `json:"audiences"`
	User          struct {
		Username string `json:"username"`
	} `json:"user"`
}

type review struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Token     string   `json:"token"`
		Audiences []string `json:"audiences"`
	} `json:"spec"`
	Status Status `json:"status"`
}

// FakeAPI serves POST /apis/authentication.k8s.io/v1/tokenreviews over TLS.
type FakeAPI struct {
	srv *httptest.Server

	mu        sync.Mutex
	reviews   map[string]Status
	failWith  int
	requests  []string
	bearers   []string
	audiences [][]string
}

// New starts a fake API server that is closed when the test ends.
func New(t testing.TB) *FakeAPI {
	t.Helper()
	f := &FakeAPI{reviews: map[string]Status{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		if f.failWith != 0 {
			w.WriteHeader(f.failWith)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"Forbidden"}`))
			return
		}
		var in review
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.audiences = append(f.audiences, in.Spec.Audiences)
		in.Status = f.reviews[in.Spec.Token]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(in)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// Access is how a TokenReviewer reaches this fake.
func (f *FakeAPI) Access() *kubecm.Access {
	return &kubecm.Access{
		BaseURL:   f.srv.URL,
		Transport: f.srv.Client().Transport,
		Token:     func() (string, error) { return OwnToken, nil },
		Namespace: "glovebox",
	}
}

// Allow makes token authenticate as username ("system:serviceaccount:ns:name")
// for the given audiences.
func (f *FakeAPI) Allow(token, username string, audiences ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := Status{Authenticated: true, Audiences: audiences}
	st.User.Username = username
	f.reviews[token] = st
}

// Deny makes token come back unauthenticated.
func (f *FakeAPI) Deny(token string, audiences ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews[token] = Status{Authenticated: false, Audiences: audiences}
}

// FailWith makes every review answer this HTTP status (0 restores normal answers).
func (f *FakeAPI) FailWith(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = status
}

// Close stops the server early (the API server is gone).
func (f *FakeAPI) Close() { f.srv.Close() }

// Requests returns, per review received: "METHOD path", the Authorization
// header, and the audiences asked about.
func (f *FakeAPI) Requests() (requests, bearers []string, audiences [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), append([]string(nil), f.bearers...), append([][]string(nil), f.audiences...)
}
