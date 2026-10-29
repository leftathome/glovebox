package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/satoken"
)

func TestAuthentication(t *testing.T) {
	r := newAuthRig(t)
	s := assignments(1, "e_child1", "powerschool", T0, one("k", "e_child1", `{"title": "A"}`))
	for name, c := range map[string]struct {
		token  string
		status int
		reason string
	}{
		"no token":           {"", 401, journalapi.ReasonUnauthenticated},
		"unknown token":      {"never-issued", 401, journalapi.ReasonUnauthenticated},
		"denied token":       {"denied", 401, journalapi.ReasonUnauthenticated},
		"other audience":     {"wrong-audience", 401, journalapi.ReasonUnauthenticated},
		"not an SA":          {"not-a-serviceaccount", 403, journalapi.ReasonForbidden},
		"no SA prefix":       {"no-prefix", 403, journalapi.ReasonForbidden},
		"rss connector":      {"rss", 403, journalapi.ReasonForbidden},
		"the core itself":    {"core", 403, journalapi.ReasonForbidden},
		"reader writes":      {"dash", 403, journalapi.ReasonPolicy},
		"gateway writes":     {"gw", 403, journalapi.ReasonPolicy},
		"operator writes":    {"admin", 403, journalapi.ReasonPolicy},
		"oversized token":    {strings.Repeat("t", 9<<10), 401, journalapi.ReasonUnauthenticated},
		"token with a space": {"ps extra", 401, journalapi.ReasonUnauthenticated},
	} {
		var resp journalapi.Response
		got := r.do("POST", journalapi.PathRecords, c.token, s, &resp)
		if got != c.status || resp.Reason != c.reason || resp.Status != journalapi.StatusRejected || resp.Stats != nil {
			t.Errorf("%s: %d %+v", name, got, resp)
		}
	}
}

// A header that is not "Bearer <token>" never reaches the API server.
func TestMalformedAuthorizationIsNotReviewed(t *testing.T) {
	r := newAuthRig(t)
	for name, h := range map[string]string{
		"basic":       "Basic cHM6cHM=",
		"no scheme":   "ps",
		"empty token": "Bearer ",
		"two tokens":  "Bearer ps gw",
	} {
		req, _ := http.NewRequest("GET", r.srv.URL+journalapi.PathHealthz, nil)
		req.Header.Set("Authorization", h)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body journalapi.Response
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != 401 || body.Reason != journalapi.ReasonUnauthenticated || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: %d %+v %q", name, resp.StatusCode, body, resp.Header.Get("WWW-Authenticate"))
		}
	}
	if reqs, _, _ := r.api.Requests(); len(reqs) != 0 {
		t.Errorf("malformed headers were sent to TokenReview: %v", reqs)
	}
}

// Every review asks about the journal's audience, as the core itself.
func TestAuthAsksForTheJournalAudience(t *testing.T) {
	r := newAuthRig(t)
	if got := r.do("GET", journalapi.PathHealthz, "gw", nil, nil); got != 200 {
		t.Fatalf("healthz as the gateway: %d", got)
	}
	reqs, bearers, auds := r.api.Requests()
	if len(reqs) != 1 || reqs[0] != "POST /apis/authentication.k8s.io/v1/tokenreviews" ||
		len(auds[0]) != 1 || auds[0][0] != journalapi.Audience || strings.Contains(bearers[0], "gw") {
		t.Errorf("reviews %v bearers %v audiences %v", reqs, bearers, auds)
	}
}

func TestAuthFailsClosedAndNotReadyIs503(t *testing.T) {
	r := newAuthRig(t)
	for _, status := range []int{500, 403, 401} {
		r.api.FailWith(status)
		var resp journalapi.Response
		if got := r.do("GET", journalapi.PathHealthz, "gw", nil, &resp); got != 503 || resp.Reason != journalapi.ReasonAuthUnavailable {
			t.Errorf("TokenReview answers %d: %d %+v", status, got, resp)
		}
		if got := r.do("POST", journalapi.PathRecords, "ps", assignments(1, "e_child1", "powerschool", T0), &resp); got != 503 || resp.Reason != journalapi.ReasonAuthUnavailable {
			t.Errorf("TokenReview answers %d, records: %d %+v", status, got, resp)
		}
	}
	r.api.FailWith(0)
	r.ready.Store(false)
	before, _, _ := r.api.Requests()
	var resp journalapi.Response
	if got := r.do("GET", journalapi.PathHealthz, "gw", nil, &resp); got != 503 || resp.Reason != journalapi.ReasonUnavailable {
		t.Errorf("not ready: %d %+v", got, resp)
	}
	if after, _, _ := r.api.Requests(); len(after) != len(before) {
		t.Errorf("a request to a journal that is not ready asked TokenReview")
	}
	r.ready.Store(true)
	r.api.Close()
	if got := r.do("GET", journalapi.PathHealthz, "gw", nil, &resp); got != 503 || resp.Reason != journalapi.ReasonAuthUnavailable {
		t.Errorf("API server gone: %d %+v", got, resp)
	}
}

// A valid token is accepted (the other side of every refusal above).
func TestEveryPolicyCallerIsAuthenticated(t *testing.T) {
	r := newAuthRig(t)
	for _, tok := range []string{"gw", "ps", "sg", "dash", "admin", "shared", "scoped"} {
		var h map[string]string
		if got := r.do("GET", journalapi.PathHealthz, tok, nil, &h); got != 200 {
			t.Errorf("%s: %d", tok, got)
		}
	}
}

// No token reaches a log line or an answer; the forbidden caller's
// identity (the API server's word) does reach the log.
func TestTokensAreNeverLoggedOrEchoed(t *testing.T) {
	r := newAuthRig(t)
	for _, tok := range []string{"never-issued", "rss", "wrong-audience", "dash"} {
		_, raw := r.send("POST", journalapi.PathRecords, tok, "application/json", "{}")
		if strings.Contains(string(raw), tok) {
			t.Errorf("answer to %s echoes the token: %s", tok, raw)
		}
	}
	r.api.FailWith(500)
	r.send("POST", journalapi.PathRecords, "ps", "application/json", "{}")
	logs := r.logs.String()
	for _, tok := range []string{"never-issued", "wrong-audience", "Bearer"} {
		if strings.Contains(logs, tok) {
			t.Errorf("log carries %q: %s", tok, logs)
		}
	}
	if !strings.Contains(logs, "glovebox-glovebox-connector-rss") {
		t.Errorf("the refused caller's identity is not logged: %s", logs)
	}
}

// blockingAuth waits for the request's context to end.
type blockingAuth struct{ deadline chan time.Time }

func (b blockingAuth) Authenticate(ctx context.Context, _ string) (satoken.Caller, error) {
	d, _ := ctx.Deadline()
	b.deadline <- d
	<-ctx.Done()
	return satoken.Caller{}, errors.Join(satoken.ErrAuthUnavailable, ctx.Err())
}

func TestRequestTimeoutBoundsTheRequest(t *testing.T) {
	ba := blockingAuth{deadline: make(chan time.Time, 1)}
	r := newAuthRig(t, func(d *Deps) { d.Auth = ba; d.RequestTimeout = 200 * time.Millisecond })
	start := time.Now()
	req, _ := http.NewRequest("GET", r.srv.URL+journalapi.PathHealthz, nil)
	req.Header.Set("Authorization", "Bearer gw")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("the request was not bounded: %v", err)
	}
	var body journalapi.Response
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 503 || body.Reason != journalapi.ReasonAuthUnavailable {
		t.Errorf("timed out review: %d %+v", resp.StatusCode, body)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("the request took %s", el)
	}
	if d := <-ba.deadline; d.IsZero() || d.Sub(start) > time.Second {
		t.Errorf("deadline %v, start %v", d, start)
	}
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	r := newAuthRig(t)
	full := r.s.d
	for name, mod := range map[string]func(*Deps){
		"journal":  func(d *Deps) { d.Journal = nil },
		"registry": func(d *Deps) { d.Registry = nil },
		"auth":     func(d *Deps) { d.Auth = nil },
		"policy":   func(d *Deps) { d.Policy = nil },
		"roster":   func(d *Deps) { d.Roster = nil },
		"timeout":  func(d *Deps) { d.RequestTimeout = -time.Second },
		"inflight": func(d *Deps) { d.MaxInFlight = -1 },
	} {
		d := full
		mod(&d)
		if _, err := New(d); err == nil {
			t.Errorf("New without %s succeeded", name)
		}
	}
}

// A panic in a handler is a 500 with no detail, and is counted.
func TestPanicIs500WithoutInternals(t *testing.T) {
	r := newAuthRig(t)
	h := r.s.route("records", func(http.ResponseWriter, *http.Request, call) { panic("secret internal state") })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", journalapi.PathRecords, nil)
	req.Header.Set("Authorization", "Bearer ps")
	h.ServeHTTP(rec, req)
	var resp journalapi.Response
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 500 || resp.Reason != journalapi.ReasonInternal || resp.Detail != "" || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("panic: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(r.scrape(), `code="500",endpoint="records"`) {
		t.Errorf("the 500 was not counted")
	}
}

// Anything unrouted is a 404 in the refusal shape, before authentication.
func TestUnroutedIs404(t *testing.T) {
	r := newAuthRig(t)
	for _, c := range [][2]string{{"GET", "/v1/nothing"}, {"GET", journalapi.PathRecords}, {"DELETE", journalapi.PathRecords}, {"GET", "/"}} {
		var resp journalapi.Response
		if got := r.do(c[0], c[1], "", nil, &resp); got != 404 || resp.Reason != journalapi.ReasonNotFound || resp.Status != journalapi.StatusRejected {
			t.Errorf("%s %s: %d %+v", c[0], c[1], got, resp)
		}
	}
	if reqs, _, _ := r.api.Requests(); len(reqs) != 0 {
		t.Errorf("an unrouted request was reviewed")
	}
}

// api_requests_total carries the route name, never the path.
func TestRequestsAreCountedByRouteName(t *testing.T) {
	r := newAuthRig(t)
	r.do("POST", journalapi.HeldPath(uid(77), journalapi.ActionConfirm), "", nil, nil)
	r.do("GET", journalapi.PathHealthz, "gw", nil, nil)
	r.do("GET", "/v1/held/"+uid(78), "", nil, nil)
	m := r.scrape()
	for _, want := range []string{`code="401",endpoint="admin_held"`, `code="200",endpoint="healthz"`, `code="404",endpoint="unrouted"`} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(m, uid(77)) || strings.Contains(m, uid(78)) {
		t.Errorf("a path reached a label")
	}
}

// countingAuth counts the tokens it is asked about.
type countingAuth struct{ n *atomic.Int32 }

func (c countingAuth) Authenticate(context.Context, string) (satoken.Caller, error) {
	c.n.Add(1)
	return satoken.Caller{}, satoken.ErrUnauthenticated
}

// A header that is not a bearer token is refused before the authenticator.
func TestMalformedAuthorizationNeverReachesTheAuthenticator(t *testing.T) {
	var n atomic.Int32
	r := newAuthRig(t, func(d *Deps) { d.Auth = countingAuth{&n} })
	for _, h := range []string{"", "Basic cHM6cHM=", "ps", "Bearer ", "Bearer ps gw"} {
		req, _ := http.NewRequest("GET", r.srv.URL+journalapi.PathHealthz, nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%q: %d", h, resp.StatusCode)
		}
	}
	if got := n.Load(); got != 0 {
		t.Errorf("the authenticator was asked %d times", got)
	}
}

// fixedAuth vouches for one caller, whatever the token.
type fixedAuth struct{ c satoken.Caller }

func (f fixedAuth) Authenticate(context.Context, string) (satoken.Caller, error) { return f.c, nil }

// An authenticator whose allow-list is wider than the policy still cannot
// let a caller without a policy through.
func TestCallerWithoutPolicyIsForbidden(t *testing.T) {
	r := newAuthRig(t, func(d *Deps) {
		d.Auth = fixedAuth{satoken.Caller{Namespace: "openclaw", Name: "glovebox-glovebox-connector-rss"}}
	})
	var resp journalapi.Response
	if got := r.do("GET", journalapi.PathHealthz, "x", nil, &resp); got != 403 || resp.Reason != journalapi.ReasonForbidden {
		t.Errorf("no policy: %d %+v", got, resp)
	}
}

// A panic after the answer started cuts the connection instead of
// appending a second answer.
func TestPanicAfterWriteAbortsTheAnswer(t *testing.T) {
	r := newAuthRig(t)
	srv := httptest.NewServer(r.s.route("records", func(w http.ResponseWriter, _ *http.Request, _ call) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("late")
	}))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("POST", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer ps")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr == nil {
			t.Errorf("a complete answer after a panic: %d %q", resp.StatusCode, raw)
		}
	}
}
