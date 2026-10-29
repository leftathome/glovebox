package loginbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

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
	srv.sources = newSourceLimits(rate.Inf, 1)
	srv.callers = rate.NewLimiter(rate.Inf, 1)
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

// A pod that can reach the port and floods it with junk tokens uses up its
// OWN allowance of token reviews. It cannot turn that into a 429 for the
// connector: before authentication the only limit is per source address,
// and the shared limit is behind authentication, where only an allow-listed
// caller can spend it.
func TestAnUnauthenticatedFloodCannotLockTheConnectorOut(t *testing.T) {
	r := newRig(t, nil)
	r.store("s1", ProbeAlive, time.Hour)
	_, auth, srv := newTestServer(t, r)
	srv.sources = newSourceLimits(rate.Limit(1), 10) // the production settings
	srv.callers = rate.NewLimiter(rate.Limit(1), 10)
	h := srv.Handler()

	send := func(remote, token string) int {
		req := httptest.NewRequest(http.MethodPost, brokerapi.RefreshPath, strings.NewReader(goodBody))
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	limited := 0
	for i := 0; i < 500; i++ {
		switch send("10.42.0.66:40000", "junk") {
		case http.StatusTooManyRequests:
			limited++
		case http.StatusUnauthorized:
		default:
			t.Fatal("a junk token got an answer other than 401 or 429")
		}
	}
	if limited < 480 || len(auth.seen) > 15 {
		t.Errorf("500 junk requests from one address: %d limited, %d sent for review", limited, len(auth.seen))
	}
	// The connector, from its own address, is served as if nothing happened.
	for i := 0; i < 3; i++ {
		if code := send("10.42.0.7:51000", goodToken); code != http.StatusOK {
			t.Fatalf("the connector's request %d during the flood: HTTP %d", i+1, code)
		}
	}
	// A flood from very many addresses does not grow the table without
	// bound, and still does not touch the shared limit.
	for i := 0; i < 3*maxSources; i++ {
		send(fmt.Sprintf("10.%d.%d.%d:4000", i>>16&255, i>>8&255, i&255), "junk")
	}
	srv.sources.mu.Lock()
	n := len(srv.sources.byIP)
	srv.sources.mu.Unlock()
	if n > maxSources {
		t.Errorf("the per-source table holds %d entries, over its bound of %d", n, maxSources)
	}
	if code := send("10.42.0.7:51000", goodToken); code != http.StatusOK {
		t.Errorf("the connector after the wide flood: HTTP %d", code)
	}
}

// An allow-listed caller is itself limited -- after authentication.
func TestAnAuthenticatedCallerIsRateLimited(t *testing.T) {
	r := newRig(t, nil)
	r.store("s1", ProbeAlive, time.Hour)
	_, _, srv := newTestServer(t, r)
	srv.callers = rate.NewLimiter(rate.Limit(1), 10)
	h := srv.Handler()
	limited := 0
	for i := 0; i < 40; i++ {
		req := httptest.NewRequest(http.MethodPost, brokerapi.RefreshPath, strings.NewReader(goodBody))
		req.Header.Set("Authorization", "Bearer "+goodToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited < 25 || len(r.driver.probeCalls) > 12 {
		t.Errorf("40 requests in a burst: %d limited, %d reached the site", limited, len(r.driver.probeCalls))
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
