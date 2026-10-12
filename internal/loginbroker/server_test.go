package loginbroker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
)

// fakeAuth maps tokens to callers or errors.
type fakeAuth struct {
	mu     sync.Mutex
	tokens map[string]error // nil = allowed
	seen   []string
}

func (f *fakeAuth) Authenticate(_ context.Context, token string) (Caller, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, token)
	err, ok := f.tokens[token]
	if !ok {
		return Caller{}, ErrUnauthenticated
	}
	return Caller{Namespace: "glovebox", Name: "glovebox-powerschool"}, err
}

const goodToken = "token-of-the-connector"

func newTestServer(t *testing.T, r *rig) (*httptest.Server, *fakeAuth, *Server) {
	t.Helper()
	auth := &fakeAuth{tokens: map[string]error{goodToken: nil, "other-sa": ErrForbidden, "api-down": ErrAuthUnavailable}}
	srv, err := NewServer(r.b, auth, nil, r.b.metrics)
	if err != nil {
		t.Fatal(err)
	}
	// Tests other than the one about the limiter send more requests in a
	// burst than a connector does in a week.
	srv.limiter = rate.NewLimiter(rate.Inf, 1)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, auth, srv
}

func post(t *testing.T, url, token, contentType, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+brokerapi.RefreshPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

const goodBody = `{"reason":"session_dead","fingerprint":"none"}`

func TestRefreshEndpointAnswersAnAuthenticatedCaller(t *testing.T) {
	r := newRig(t, nil)
	r.store("s1", ProbeAlive, time.Hour)
	ts, _, _ := newTestServer(t, r)

	resp, body := post(t, ts.URL, goodToken, "application/json", goodBody)
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, cache-control %q, body %s", resp.StatusCode, resp.Header.Get("Cache-Control"), body)
	}
	var out brokerapi.Response
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != brokerapi.OutcomeCurrent || out.Site != "testsite" || out.Session["session"] != cookiePrefix+"s1" {
		t.Errorf("answer = %+v", out)
	}
}

// Whoever is not an allow-listed caller with a token for this broker gets
// nothing done: no store connection, no probe, no login.
func TestRefreshEndpointRefusesEverybodyElseBeforeDoingAnything(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	ts, auth, _ := newTestServer(t, r)

	for name, c := range map[string]struct {
		token, contentType, body string
		status                   int
		code                     string
	}{
		"no token":               {"", "application/json", goodBody, 401, "unauthenticated"},
		"unknown token":          {"stolen-or-made-up", "application/json", goodBody, 401, "unauthenticated"},
		"another ServiceAccount": {"other-sa", "application/json", goodBody, 403, "forbidden"},
		"API server unreachable": {"api-down", "application/json", goodBody, 503, "auth_unavailable"},
		"not JSON":               {goodToken, "text/plain", goodBody, 400, "bad_request"},
		"no content type":        {goodToken, "", goodBody, 400, "bad_request"},
		"free-text reason":       {goodToken, "application/json", `{"reason":"please log in now","fingerprint":"none"}`, 400, "bad_request"},
		"no reason":              {goodToken, "application/json", `{"fingerprint":"none"}`, 400, "bad_request"},
		"fingerprint not hex":    {goodToken, "application/json", `{"reason":"session_dead","fingerprint":"ZZZZZZZZZZZZZZZZ"}`, 400, "bad_request"},
		"a whole fingerprint":    {goodToken, "application/json", `{"reason":"session_dead","fingerprint":"` + strings.Repeat("a", 64) + `"}`, 400, "bad_request"},
		"unknown field":          {goodToken, "application/json", `{"reason":"session_dead","fingerprint":"none","force":true}`, 400, "bad_request"},
		"two documents":          {goodToken, "application/json", goodBody + goodBody, 400, "bad_request"},
		"an oversized body":      {goodToken, "application/json", `{"reason":"` + strings.Repeat("x", 4096) + `"}`, 400, "bad_request"},
		"empty body":             {goodToken, "application/json", ``, 400, "bad_request"},
	} {
		resp, body := post(t, ts.URL, c.token, c.contentType, c.body)
		var e brokerapi.Error
		_ = json.Unmarshal([]byte(body), &e)
		if resp.StatusCode != c.status || e.Error != c.code {
			t.Errorf("%s: status %d code %q, want %d %q", name, resp.StatusCode, e.Error, c.status, c.code)
		}
		if strings.Contains(body, cookiePrefix) {
			t.Errorf("%s: the refusal carries a session", name)
		}
	}
	if r.driver.loginCalls != 0 || len(r.driver.probeCalls) != 0 || r.back.connects != 0 {
		t.Errorf("refused requests reached the policy: logins %d, probes %d, store connections %d",
			r.driver.loginCalls, len(r.driver.probeCalls), r.back.connects)
	}
	// The body of an unauthenticated request is not even read.
	for _, tok := range auth.seen {
		if tok == "" {
			t.Error("the authenticator was called without a token")
		}
	}
}

func TestOnlyPOSTIsAcceptedOnTheRefreshPath(t *testing.T) {
	r := newRig(t, nil)
	ts, auth, _ := newTestServer(t, r)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(method, ts.URL+brokerapi.RefreshPath, nil)
		req.Header.Set("Authorization", "Bearer "+goodToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d", method, resp.StatusCode)
		}
	}
	if len(auth.seen) != 0 {
		t.Error("a non-POST request was authenticated")
	}
}

// Reviews of tokens are rate limited whoever sends them: a flood of bad
// tokens cannot be turned into a flood of TokenReviews.
func TestTokenReviewsAreRateLimited(t *testing.T) {
	r := newRig(t, nil)
	ts, auth, srv := newTestServer(t, r)
	srv.limiter = rate.NewLimiter(rate.Limit(1), 10) // the production setting
	limited := 0
	for i := 0; i < 40; i++ {
		resp, _ := post(t, ts.URL, "junk", "application/json", goodBody)
		if resp.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 || len(auth.seen) > 15 {
		t.Errorf("40 requests: %d limited, %d reviewed", limited, len(auth.seen))
	}
}

// The API listener serves no metrics; the metrics listener serves no
// refresh. A NetworkPolicy can then admit a scraper to one without
// admitting it to the other.
func TestTheTwoListenersServeDifferentThings(t *testing.T) {
	r := newRig(t, nil)
	r.store("s1", ProbeAlive, time.Hour)
	api, _, srv := newTestServer(t, r)
	metrics := httptest.NewServer(srv.MetricsHandler())
	defer metrics.Close()

	get := func(url string) (int, string) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	for _, base := range []string{api.URL, metrics.URL} {
		if code, body := get(base + "/healthz"); code != 200 || body != "ok\n" {
			t.Errorf("healthz on %s: %d %q", base, code, body)
		}
	}
	if code, _ := get(api.URL + "/metrics"); code != 404 {
		t.Errorf("/metrics on the API listener: %d", code)
	}
	if resp, _ := post(t, metrics.URL, goodToken, "application/json", goodBody); resp.StatusCode != 404 {
		t.Errorf("refresh on the metrics listener: %d", resp.StatusCode)
	}

	post(t, api.URL, goodToken, "application/json", goodBody)
	post(t, api.URL, "", "application/json", goodBody)
	code, body := get(metrics.URL + "/metrics")
	if code != 200 {
		t.Fatalf("/metrics: %d", code)
	}
	for _, want := range []string{
		`login_broker_refresh_total{code="",outcome="current",site="testsite"} 1`,
		`login_broker_probe_total{result="alive",site="testsite"} 1`,
		`login_broker_request_refused_total{reason="unauthenticated",site="testsite"} 1`,
		`login_broker_latched{site="testsite"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	for _, secret := range []string{testUsername, testPassword, cookiePrefix, goodToken} {
		if strings.Contains(body, secret) {
			t.Errorf("metrics carry %q", secret)
		}
	}
}

// --- TokenReview --------------------------------------------------------------

const brokerOwnToken = "the-brokers-own-sa-token"

// fakeAPI is the one endpoint of the API server the reviewer uses.
type fakeAPI struct {
	srv *httptest.Server
	mu  sync.Mutex
	// reviews maps a presented token to the status the API server gives.
	reviews  map[string]tokenReviewStatus
	status   int
	requests []string
	specs    []tokenReviewSpec
	bearers  []string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{reviews: map[string]tokenReviewStatus{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"Forbidden"}`))
			return
		}
		var in tokenReview
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.specs = append(f.specs, in.Spec)
		in.Status = f.reviews[in.Spec.Token]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(in)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) access() *kubecm.Access {
	return &kubecm.Access{
		BaseURL:   f.srv.URL,
		Transport: f.srv.Client().Transport,
		Token:     func() (string, error) { return brokerOwnToken, nil },
		Namespace: "glovebox",
	}
}

func saStatus(username string, audiences ...string) tokenReviewStatus {
	st := tokenReviewStatus{Authenticated: true, Audiences: audiences}
	st.User.Username = username
	return st
}

const testAudience = "glovebox-login-broker-powerschool"

func TestTokenReviewerAcceptsOnlyAnAllowListedServiceAccountWithThisAudience(t *testing.T) {
	api := newFakeAPI(t)
	api.reviews["connector"] = saStatus("system:serviceaccount:glovebox:glovebox-powerschool", testAudience)
	api.reviews["other-sa"] = saStatus("system:serviceaccount:glovebox:glovebox-rss", testAudience)
	api.reviews["other-namespace"] = saStatus("system:serviceaccount:default:glovebox-powerschool", testAudience)
	api.reviews["wrong-audience"] = saStatus("system:serviceaccount:glovebox:glovebox-powerschool", "glovebox-login-broker-schoology")
	api.reviews["no-audience"] = saStatus("system:serviceaccount:glovebox:glovebox-powerschool")
	api.reviews["a-user"] = saStatus("steve", testAudience)
	api.reviews["a-node"] = saStatus("system:node:worker-1", testAudience)
	api.reviews["odd-name"] = saStatus("system:serviceaccount:glovebox:glovebox-powerschool:extra", testAudience)
	api.reviews["not-authenticated"] = tokenReviewStatus{Authenticated: false, Audiences: []string{testAudience}}

	rv, err := NewTokenReviewer(api.access(), testAudience, []string{"glovebox/glovebox-powerschool"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	caller, err := rv.Authenticate(ctx, "connector")
	if err != nil || caller.String() != "glovebox/glovebox-powerschool" {
		t.Fatalf("the connector: %v, %v", caller, err)
	}
	for token, want := range map[string]error{
		"other-sa":          ErrForbidden,
		"other-namespace":   ErrForbidden,
		"a-user":            ErrForbidden,
		"a-node":            ErrForbidden,
		"odd-name":          ErrForbidden,
		"wrong-audience":    ErrUnauthenticated,
		"no-audience":       ErrUnauthenticated,
		"not-authenticated": ErrUnauthenticated,
		"never-issued":      ErrUnauthenticated,
		"":                  ErrUnauthenticated,
	} {
		if _, err := rv.Authenticate(ctx, token); err != want {
			t.Errorf("token %q: %v, want %v", token, err, want)
		}
	}

	// What went to the API server: one POST per review, to the TokenReview
	// endpoint, as the broker's own ServiceAccount, asking about THIS
	// audience.
	api.mu.Lock()
	defer api.mu.Unlock()
	for i, req := range api.requests {
		if req != "POST /apis/authentication.k8s.io/v1/tokenreviews" || api.bearers[i] != "Bearer "+brokerOwnToken {
			t.Errorf("request %d = %s as %q", i, req, api.bearers[i])
		}
	}
	for _, spec := range api.specs {
		if len(spec.Audiences) != 1 || spec.Audiences[0] != testAudience {
			t.Errorf("review asked for audiences %v", spec.Audiences)
		}
	}
	if len(api.requests) != 10 { // the empty token never leaves the process
		t.Errorf("reviews sent = %d, want 10", len(api.requests))
	}
}

// The API server refusing or failing is "cannot authenticate", never "let
// them in" and never "they are unauthenticated".
func TestTokenReviewerFailsClosedWhenTheAPIServerDoesNotAnswer(t *testing.T) {
	api := newFakeAPI(t)
	api.reviews["connector"] = saStatus("system:serviceaccount:glovebox:glovebox-powerschool", testAudience)
	rv, err := NewTokenReviewer(api.access(), testAudience, []string{"glovebox/glovebox-powerschool"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{403, 500, 302} {
		api.status = status
		_, err := rv.Authenticate(context.Background(), "connector")
		if err == nil || !strings.Contains(err.Error(), ErrAuthUnavailable.Error()) {
			t.Errorf("API status %d: %v", status, err)
		}
		if err != nil && strings.Contains(err.Error(), "connector") {
			t.Errorf("the error quotes the token: %v", err)
		}
	}
	api.srv.Close()
	if _, err := rv.Authenticate(context.Background(), "connector"); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("API gone: %v", err)
	}
}

func TestTokenReviewerConfiguration(t *testing.T) {
	api := newFakeAPI(t)
	for name, c := range map[string]struct {
		audience string
		allowed  []string
	}{
		"no audience":         {"", []string{"glovebox/a"}},
		"two audiences":       {"a,b", []string{"glovebox/a"}},
		"nobody allowed":      {testAudience, nil},
		"only blanks":         {testAudience, []string{" ", ""}},
		"a wildcard":          {testAudience, []string{"glovebox/*"}},
		"no namespace":        {testAudience, []string{"glovebox-powerschool"}},
		"the API's long form": {testAudience, []string{"system:serviceaccount:glovebox:a"}},
		"three parts":         {testAudience, []string{"a/b/c"}},
	} {
		if _, err := NewTokenReviewer(api.access(), c.audience, c.allowed); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	plain := api.access()
	plain.BaseURL = "http://10.0.0.1:6443"
	if _, err := NewTokenReviewer(plain, testAudience, []string{"glovebox/a"}); err == nil {
		t.Error("a plaintext API address was accepted")
	}
	rv, err := NewTokenReviewer(api.access(), testAudience, []string{" glovebox/b ", "glovebox/a"})
	if err != nil || strings.Join(rv.Allowed(), ",") != "glovebox/a,glovebox/b" || rv.Audience() != testAudience {
		t.Errorf("reviewer = %v, %v", rv, err)
	}
}
