package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connectors/powerschool"
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/loginbroker"
	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
	"github.com/leftathome/glovebox/internal/loginvault"
	"github.com/leftathome/glovebox/internal/loginvault/vaulttest"
	"github.com/leftathome/glovebox/internal/pslogin"
	pslib "github.com/leftathome/powerschool-go"
)

// Every secret a full run handles. None may appear in a log line, a metric
// or an error body.
const (
	testUsername     = "parent-account-7731"
	testPassword     = "correct-horse-battery-staple-9"
	portalCookie1    = "jsession-cookie-value-first-login"
	portalCookie2    = "jsession-cookie-value-second-login"
	gatewayCookie    = "mrh-gateway-cookie-value"
	connectorToken   = "projected-token-of-the-connector"
	strangerToken    = "projected-token-of-some-other-pod"
	brokerOwnSAToken = "the-brokers-own-api-token"
	pageMarker       = "Grades and Attendance: One, Student"
)

// --- configuration ----------------------------------------------------------

func goodEnv() map[string]string {
	return map[string]string{
		"LOGIN_BROKER_SITE":             "powerschool",
		"LOGIN_BROKER_AUDIENCE":         "glovebox-login-broker-powerschool",
		"LOGIN_BROKER_ALLOWED_CALLERS":  "glovebox/glovebox-powerschool",
		"LOGIN_BROKER_USERNAME":         testUsername,
		"LOGIN_BROKER_PASSWORD":         testPassword,
		"LOGIN_BROKER_BLACKOUT_WINDOWS": "02:00-03:00",
		"LOGIN_BROKER_BLACKOUT_TZ":      "America/Los_Angeles",
		"VAULT_ADDR":                    "https://vault.vault.svc.cluster.local:8200",
		"VAULT_K8S_ROLE":                "glovebox-login-broker-powerschool",
		"VAULT_SESSION_PATH":            "glovebox/powerschool/default/session",
		"POWERSCHOOL_HOSTNAME":          "ps.example.org",
	}
}

func getenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, problems := loadConfig(getenv(goodEnv()))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if cfg.Listen != ":8080" || cfg.MetricsListen != ":9090" || cfg.MinInterval != 20*time.Minute || cfg.MaxFailures != 2 {
		t.Errorf("cfg = %+v", cfg)
	}
	// The state lives beside the session, where the scheduled job keeps
	// it: one latch for one account.
	if cfg.Vault.StatePath != "glovebox/powerschool/default/login-state" || cfg.Vault.KVMount != "secret" {
		t.Errorf("vault = %+v", cfg.Vault)
	}
	if got := cfg.Blackout.String(); got != "02:00-03:00 America/Los_Angeles" {
		t.Errorf("blackout = %q", got)
	}
	if len(cfg.AllowedCallers) != 1 || cfg.Credentials.Password != testPassword {
		t.Errorf("callers %v", cfg.AllowedCallers)
	}
}

// Each fault is reported by setting name, and no report carries a value:
// any of them may be a secret typed into the wrong field.
func TestLoadConfigReportsFaultsWithoutValues(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(e map[string]string)
		want   string
	}{
		"no site":              {func(e map[string]string) { delete(e, "LOGIN_BROKER_SITE") }, "LOGIN_BROKER_SITE"},
		"unknown site":         {func(e map[string]string) { e["LOGIN_BROKER_SITE"] = testPassword }, "names no driver"},
		"no audience":          {func(e map[string]string) { delete(e, "LOGIN_BROKER_AUDIENCE") }, "LOGIN_BROKER_AUDIENCE"},
		"nobody allowed":       {func(e map[string]string) { e["LOGIN_BROKER_ALLOWED_CALLERS"] = " , " }, "LOGIN_BROKER_ALLOWED_CALLERS"},
		"no username":          {func(e map[string]string) { delete(e, "LOGIN_BROKER_USERNAME") }, "LOGIN_BROKER_USERNAME"},
		"no password":          {func(e map[string]string) { delete(e, "LOGIN_BROKER_PASSWORD") }, "LOGIN_BROKER_PASSWORD"},
		"interval not a time":  {func(e map[string]string) { e["LOGIN_BROKER_MIN_INTERVAL"] = testPassword }, "LOGIN_BROKER_MIN_INTERVAL"},
		"interval too short":   {func(e map[string]string) { e["LOGIN_BROKER_MIN_INTERVAL"] = "30s" }, "LOGIN_BROKER_MIN_INTERVAL"},
		"failure limit":        {func(e map[string]string) { e["LOGIN_BROKER_MAX_FAILURES"] = "50" }, "LOGIN_BROKER_MAX_FAILURES"},
		"windows without zone": {func(e map[string]string) { delete(e, "LOGIN_BROKER_BLACKOUT_TZ") }, "LOGIN_BROKER_BLACKOUT_TZ is required"},
		"zone Local":           {func(e map[string]string) { e["LOGIN_BROKER_BLACKOUT_TZ"] = "Local" }, "LOGIN_BROKER_BLACKOUT_TZ"},
		"zone unknown":         {func(e map[string]string) { e["LOGIN_BROKER_BLACKOUT_TZ"] = "Pacific" }, "LOGIN_BROKER_BLACKOUT_TZ"},
		"same listener twice":  {func(e map[string]string) { e["LOGIN_BROKER_METRICS_LISTEN"] = ":8080" }, "must differ"},
		"plaintext vault":      {func(e map[string]string) { e["VAULT_ADDR"] = "http://vault:8200" }, "VAULT_ADDR must be an https URL"},
		"skip verify":          {func(e map[string]string) { e["VAULT_SKIP_VERIFY"] = "true" }, "VAULT_SKIP_VERIFY"},
		"no vault role":        {func(e map[string]string) { delete(e, "VAULT_K8S_ROLE") }, "VAULT_K8S_ROLE"},
		"no session path":      {func(e map[string]string) { delete(e, "VAULT_SESSION_PATH") }, "VAULT_SESSION_PATH"},
		"state is the session": {func(e map[string]string) { e["VAULT_STATE_PATH"] = "/glovebox/powerschool/default/session/" }, "VAULT_STATE_PATH must differ"},
	} {
		env := goodEnv()
		c.mutate(env)
		_, problems := loadConfig(getenv(env))
		joined := strings.Join(problems, "; ")
		if !strings.Contains(joined, c.want) {
			t.Errorf("%s: problems = %q, want one naming %q", name, joined, c.want)
		}
		if strings.Contains(joined, testPassword) || strings.Contains(joined, testUsername) {
			t.Errorf("%s: a problem quotes a value: %q", name, joined)
		}
	}
	env := goodEnv()
	env["LOGIN_BROKER_BLACKOUT_WINDOWS"] = "4pm-9pm"
	if _, problems := loadConfig(getenv(env)); len(problems) == 0 {
		t.Error("malformed windows accepted")
	}
}

func TestPowerSchoolDriverFactory(t *testing.T) {
	d, problems := newPowerSchoolDriver(getenv(goodEnv()))
	if len(problems) != 0 || d == nil || d.Site() != "powerschool" {
		t.Fatalf("driver %v, problems %v", d, problems)
	}
	// The owner's hours come with the driver, whatever is configured.
	limits := d.(loginbroker.Constrained).Constraints()
	if got := limits.Blackout.String(); got != "16:00-21:00 America/Los_Angeles" || limits.MinInterval != 20*time.Minute {
		t.Errorf("driver limits: %q, %s", got, limits.MinInterval)
	}
	for name, mutate := range map[string]func(e map[string]string){
		"no host":        func(e map[string]string) { delete(e, "POWERSCHOOL_HOSTNAME") },
		"not a web host": func(e map[string]string) { e["POWERSCHOOL_HOSTNAME"] = "ftp://ps.example.org" },
		"short lifetime": func(e map[string]string) { e["POWERSCHOOL_SESSION_LIFETIME"] = "5m" },
	} {
		env := goodEnv()
		mutate(env)
		if d, problems := newPowerSchoolDriver(getenv(env)); d != nil || len(problems) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	if names := driverNames(); len(names) != 1 || names[0] != "powerschool" {
		t.Errorf("drivers = %v", names)
	}
}

// --- a full run -------------------------------------------------------------

// world is everything the broker talks to, as local servers: the portal,
// Vault, the Kubernetes API (TokenReview), and the broker's own two
// listeners in front of the real PowerSchool driver. Only the browser is
// replaced -- by a function that does what the browser does to the portal:
// one POST of the sign-in form.
type world struct {
	t *testing.T

	portal *httptest.Server
	vault  *vaulttest.Vault
	kube   *httptest.Server
	api    string // broker API base URL
	metric string // broker metrics base URL
	logs   *lockedBuffer
	clock  *clock

	mu             sync.Mutex
	portalRequests []string
	portalCookies  []string
	liveSession    string // the JSESSIONID the portal accepts ("" = none)
	issue          []string
	reviews        int
	rejectPassword bool
	stop           func()
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const signInPage = `<!doctype html><html><body>
<form id="LoginForm" action="/guardian/home.html" method="post">
<input type="text" name="account"/><input type="password" name="pw"/>
</form></body></html>`

const signedInHome = `<!doctype html><html><body>
<ul id="students-list"><li class="selected"><a href="javascript:switchStudent(111111);">Student One</a></li></ul>
<h1>` + pageMarker + `</h1></body></html>`

const (
	sessionKV = "glovebox/powerschool/default/session"
	stateKV   = "glovebox/powerschool/default/login-state"
)

func newWorld(t *testing.T) *world {
	t.Helper()
	vaulttest.CleanEnv(t)
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{
		t: t, logs: &lockedBuffer{},
		clock: &clock{t: time.Date(2026, 10, 12, 6, 5, 0, 0, la)}, // a Monday morning poll window
		issue: []string{portalCookie1, portalCookie2},
	}

	// The portal: a sign-in page, a home page for whoever holds the live
	// session, and the form's POST target.
	w.portal = httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.portalRequests = append(w.portalRequests, r.Method+" "+r.URL.Path)
		w.portalCookies = append(w.portalCookies, r.Header.Get("Cookie"))
		live := w.liveSession
		w.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/public/"):
			_, _ = rw.Write([]byte(signInPage))
		case r.URL.Path == "/guardian/home.html" && r.Method == http.MethodPost:
			_ = r.ParseForm()
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.rejectPassword || r.PostForm.Get("account") != testUsername || r.PostForm.Get("pw") != testPassword {
				http.Redirect(rw, r, "/public/home.html", http.StatusFound)
				return
			}
			// One live session per account: this login ends the last one.
			w.liveSession, w.issue = w.issue[0], w.issue[1:]
			http.SetCookie(rw, &http.Cookie{Name: "JSESSIONID", Value: w.liveSession, Path: "/"})
			http.SetCookie(rw, &http.Cookie{Name: "MRHSession", Value: gatewayCookie, Path: "/"})
			_, _ = rw.Write([]byte(signedInHome))
		case r.URL.Path == "/guardian/home.html":
			if c, err := r.Cookie("JSESSIONID"); err != nil || live == "" || c.Value != live {
				http.Redirect(rw, r, "/public/home.html", http.StatusFound)
				return
			}
			_, _ = rw.Write([]byte(signedInHome))
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.portal.Close)

	w.vault = vaulttest.New(t)

	// The Kubernetes API: TokenReview only.
	w.kube = httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" ||
			r.Header.Get("Authorization") != "Bearer "+brokerOwnSAToken {
			rw.WriteHeader(http.StatusForbidden)
			return
		}
		var review map[string]any
		_ = json.NewDecoder(r.Body).Decode(&review)
		spec, _ := review["spec"].(map[string]any)
		w.mu.Lock()
		w.reviews++
		w.mu.Unlock()
		status := map[string]any{"authenticated": false}
		switch spec["token"] {
		case connectorToken:
			status = map[string]any{"authenticated": true, "audiences": spec["audiences"],
				"user": map[string]any{"username": "system:serviceaccount:glovebox:glovebox-powerschool"}}
		case strangerToken:
			status = map[string]any{"authenticated": true, "audiences": spec["audiences"],
				"user": map[string]any{"username": "system:serviceaccount:glovebox:glovebox-rss"}}
		}
		review["status"] = status
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(rw).Encode(review)
	}))
	t.Cleanup(w.kube.Close)

	w.start()
	return w
}

// start builds and serves a broker from configuration, the way main does
// (with the browser replaced). Calling it again is a pod restart.
func (w *world) start() {
	w.t.Helper()
	if w.stop != nil {
		w.stop()
	}
	logger := slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	env := goodEnv()
	delete(env, "LOGIN_BROKER_BLACKOUT_WINDOWS")
	delete(env, "LOGIN_BROKER_BLACKOUT_TZ")
	cfg, problems := loadConfig(getenv(env))
	if len(problems) != 0 {
		w.t.Fatalf("config: %v", problems)
	}
	cfg.Vault = w.vault.Config(w.t, cfg.Vault.K8sRole, cfg.Vault.SessionPath, cfg.Vault.StatePath)

	host := strings.TrimPrefix(w.portal.URL, "https://")
	driver, err := pslogin.NewDriver(pslogin.Config{BaseURL: "https://" + host, Host: host}, pslogin.DriverOptions{
		Base:  w.portal.Client().Transport,
		Login: w.browser,
		Now:   w.clock.now,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	auth, err := loginbroker.NewTokenReviewer(&kubecm.Access{
		BaseURL: w.kube.URL, Transport: w.kube.Client().Transport,
		Token: func() (string, error) { return brokerOwnSAToken, nil },
	}, cfg.Audience, cfg.AllowedCallers)
	if err != nil {
		w.t.Fatal(err)
	}
	connect := func(ctx context.Context) (loginbroker.Store, error) {
		return loginvault.Connect(ctx, cfg.Vault)
	}
	a, err := build(cfg, driver, auth, connect, logger, func(c *loginbroker.Config) {
		c.Now = w.clock.now
		c.Sleep = func(context.Context, time.Duration) error { return nil }
	})
	if err != nil {
		w.t.Fatal(err)
	}
	apiL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		w.t.Fatal(err)
	}
	metricsL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		w.t.Fatal(err)
	}
	w.api, w.metric = "http://"+apiL.Addr().String(), "http://"+metricsL.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.serve(ctx, apiL, metricsL, logger) }()
	stopped := false
	w.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			w.t.Errorf("serve: %v", err)
		}
	}
	w.t.Cleanup(w.stop)
}

// browser does what the real one does to the portal: submits the form,
// once, and keeps the cookies.
func (w *world) browser(cfg pslogin.Config) func(context.Context) (*pslib.SessionExport, error) {
	return func(ctx context.Context) (*pslib.SessionExport, error) {
		form := url.Values{"account": {cfg.Username}, "pw": {cfg.Password}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/guardian/home.html", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		client := &http.Client{
			Transport:     w.portal.Client().Transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, &pslogin.LoginError{Kind: pslogin.FailedUnknown, Err: err}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			// The library's own message, password and all in the error
			// chain, as a worst case for the log scan.
			return nil, &pslogin.LoginError{Kind: pslogin.Rejected,
				Err: errors.New("login failed: invalid credentials (typed " + cfg.Password + " for " + cfg.Username + ")")}
		}
		return &pslib.SessionExport{Cookies: resp.Cookies(), ExpiresAt: time.Now().Add(4 * time.Hour)}, nil
	}
}

// connector is the caller: the real client, with the connector's token.
func (w *world) connector(token string) *brokerapi.Client {
	w.t.Helper()
	c, err := brokerapi.NewClient(w.api, func() (string, error) { return token, nil }, time.Minute, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

func (w *world) refresh(reason, fullFingerprint string) *brokerapi.Response {
	w.t.Helper()
	resp, err := w.connector(connectorToken).Refresh(context.Background(), reason, fullFingerprint)
	if err != nil {
		w.t.Fatalf("refresh: %v", err)
	}
	return resp
}

func (w *world) portalLog() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.portalRequests...)
}

func (w *world) get(url string) (int, string) {
	w.t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// fingerprintOf is what the connector computes for the session in an
// answer.
func fingerprintOf(t *testing.T, resp *brokerapi.Response) string {
	t.Helper()
	s, err := powerschool.ParseSession([]byte(resp.Session[powerschool.SessionField]), time.Time{})
	if err != nil {
		t.Fatalf("the answer's session does not parse: %v", err)
	}
	return powerschool.SessionFingerprint(s)
}

// A whole life of the service, end to end, through real HTTP on every hop:
// the connector asks, the broker authenticates it, logs in once, stores the
// session in Vault where the scheduled job stores it, and returns it; a
// second request is answered from the store; the owner signs in and the
// interval holds; a restart forgets nothing; a rejected password latches
// until a person clears it.
//
// Then every log line the run wrote, the metrics page and every refusal
// are read back: no credential, cookie, token or page content anywhere; and
// the portal's own record shows reads, plus exactly one POST per login
// attempt -- the credential submission.
func TestFullRun(t *testing.T) {
	w := newWorld(t)
	var refusals []string

	// Nothing has asked yet: the portal has heard nothing.
	if n := len(w.portalLog()); n != 0 {
		t.Fatalf("portal requests at startup: %d", n)
	}

	// 1. Strangers get nothing, and cause nothing.
	for name, token := range map[string]string{"no token": "", "made up": "made-up-token", "another pod": strangerToken} {
		_, err := w.connector(token).Refresh(context.Background(), brokerapi.ReasonNoSession, "")
		var se *brokerapi.StatusError
		if token == "" {
			// The client refuses to send an empty token only if its
			// source fails; an empty bearer is the broker's to refuse.
			if err == nil {
				t.Errorf("%s: answered", name)
			}
		} else if !errors.As(err, &se) || (se.Status != 401 && se.Status != 403) {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil {
			refusals = append(refusals, err.Error())
		}
	}
	if n := len(w.portalLog()); n != 0 {
		t.Fatalf("a stranger caused %d portal requests", n)
	}
	if logins, _, _ := w.vault.Counts(); logins != 0 {
		t.Fatalf("a stranger caused %d Vault logins", logins)
	}

	// 2. The connector has no session: one login.
	first := w.refresh(brokerapi.ReasonNoSession, "")
	if first.Outcome != brokerapi.OutcomeRefreshed || !first.HasSession() {
		t.Fatalf("first refresh = %+v", first)
	}
	if got := strings.Join(w.portalLog(), ", "); got != "GET /public/, POST /guardian/home.html, GET /guardian/home.html" {
		t.Fatalf("portal saw: %s", got)
	}
	stored, version := w.vault.Secret(sessionKV)
	if version != 1 || stored["session"] != first.Session["session"] || stored["logged_in_at"] == "" || stored["expires_at"] == "" {
		t.Fatalf("Vault session (v%d) = %v", version, keysOf(stored))
	}
	if _, ok := stored["selected_student_id"]; !ok {
		t.Error("the secret lacks a field the scheduled job writes")
	}
	firstFP := fingerprintOf(t, first)
	if first.Fingerprint != brokerapi.ShortFingerprint(firstFP) {
		t.Errorf("the answer's fingerprint is not the connector's: %q vs %q", first.Fingerprint, firstFP)
	}

	// 3. An hour on, the connector restarts with a stale file and asks
	//    about a session long gone. The stored one is alive: returned, no
	//    login.
	w.clock.add(time.Hour)
	again := w.refresh(brokerapi.ReasonSessionDead, strings.Repeat("0", 64))
	if again.Outcome != brokerapi.OutcomeCurrent || again.Session["session"] != first.Session["session"] {
		t.Fatalf("second refresh = %s", again.Outcome)
	}

	// 4. The account's owner signs in: the portal drops the session. The
	//    connector reports it dead -- an hour after the last login, so a
	//    login is made. Then the owner signs in AGAIN, five minutes later,
	//    and this time the interval holds.
	w.mu.Lock()
	w.liveSession = ""
	w.mu.Unlock()
	second := w.refresh(brokerapi.ReasonSessionDead, firstFP)
	if second.Outcome != brokerapi.OutcomeRefreshed || second.Session["session"] == first.Session["session"] {
		t.Fatalf("after the session died: %+v", second.Outcome)
	}
	secondFP := fingerprintOf(t, second)
	w.mu.Lock()
	w.liveSession = ""
	w.mu.Unlock()
	w.clock.add(5 * time.Minute)
	deferred := w.refresh(brokerapi.ReasonAuthFailure, secondFP)
	if deferred.Outcome != brokerapi.OutcomeDeferred || deferred.Code != brokerapi.CodeMinInterval || deferred.RetryAfter == nil || deferred.HasSession() {
		t.Fatalf("inside the interval: %+v", deferred)
	}

	// 5. The pod is replaced. The interval is in Vault, not in the pod.
	w.start()
	w.clock.add(5 * time.Minute)
	if resp := w.refresh(brokerapi.ReasonSessionDead, secondFP); resp.Outcome != brokerapi.OutcomeDeferred {
		t.Fatalf("after a restart, still inside the interval: %s", resp.Outcome)
	}

	// 6. Into the owner's hours (16:00-21:00 Pacific, which comes with the
	//    driver and was not configured here): deferred until 21:00.
	w.clock.add(11 * time.Hour) // 18:15
	blackout := w.refresh(brokerapi.ReasonSessionDead, secondFP)
	if blackout.Outcome != brokerapi.OutcomeDeferred || blackout.Code != brokerapi.CodeBlackout {
		t.Fatalf("at 18:15 Pacific: %+v", blackout)
	}
	if got := blackout.RetryAfter.In(w.clock.now().Location()).Format("15:04"); got != "21:00" {
		t.Errorf("deferred until %s, want 21:00", got)
	}

	// 7. 21:40: the password has been changed on the portal. One
	//    submission, rejected: latched. And latched it stays -- across
	//    time and a restart -- with nothing further sent to the portal.
	w.clock.add(3*time.Hour + 25*time.Minute)
	w.mu.Lock()
	w.rejectPassword = true
	w.mu.Unlock()
	latched := w.refresh(brokerapi.ReasonSessionDead, secondFP)
	if latched.Outcome != brokerapi.OutcomeLatched || latched.Code != brokerapi.CodeCredentialsRejected {
		t.Fatalf("a rejected password: %+v", latched)
	}
	before := len(w.portalLog())
	w.start()
	for i := 0; i < 3; i++ {
		w.clock.add(9 * time.Hour)
		if resp := w.refresh(brokerapi.ReasonSessionDead, secondFP); resp.Outcome != brokerapi.OutcomeLatched {
			t.Fatalf("latched, %d: %s", i, resp.Outcome)
		}
	}
	if n := len(w.portalLog()); n != before {
		t.Fatalf("%d requests reached the portal while latched", n-before)
	}
	state, _ := w.vault.Secret(stateKV)
	if state["login_blocked"] != "true" || state["blocked_reason"] != "credentials_rejected" || state["attempt_started_at"] != "" {
		t.Errorf("state in Vault = %v", state)
	}

	// 8. A person fixes the password and clears the latch in Vault. The
	//    session in Vault was never overwritten by the failures.
	if s, v := w.vault.Secret(sessionKV); v != 2 || s["session"] != second.Session["session"] {
		t.Errorf("Vault session after the failures: v%d", v)
	}
	w.mu.Lock()
	w.rejectPassword = false
	w.issue = []string{"jsession-cookie-value-third-login"}
	w.mu.Unlock()
	w.vault.Delete(stateKV)
	// 01:40 two days on: a clear hour.
	third := w.refresh(brokerapi.ReasonSessionDead, secondFP)
	if third.Outcome != brokerapi.OutcomeRefreshed {
		t.Fatalf("after the latch was cleared: %+v", third)
	}

	// --- what the portal saw, in total -------------------------------------
	posts := 0
	for _, r := range w.portalLog() {
		switch {
		case r == "POST /guardian/home.html":
			posts++
		case strings.HasPrefix(r, "GET "):
		default:
			t.Errorf("the portal was sent %s", r)
		}
	}
	// Four login attempts: three that worked and the rejected one. One
	// POST each -- the credential submission -- and no other.
	if posts != 4 {
		t.Errorf("credential submissions = %d, want 4 (one per login attempt)", posts)
	}

	// --- the Vault token is not kept between requests -----------------------
	if logins, revokes, roles := w.vault.Counts(); logins == 0 || logins != revokes || roles[0] != "glovebox-login-broker-powerschool" {
		t.Errorf("Vault logins %d, revokes %d, roles %v", logins, revokes, roles)
	}

	// --- nothing secret left the process sideways ---------------------------
	_, metrics := w.get(w.metric + "/metrics")
	if !strings.Contains(metrics, `login_broker_login_total{result="success",site="powerschool"}`) {
		t.Errorf("metrics lack the login counter:\n%s", metrics)
	}
	if code, _ := w.get(w.api + "/metrics"); code != http.StatusNotFound {
		t.Errorf("/metrics on the API listener: %d", code)
	}
	logs := w.logs.String()
	for _, want := range []string{`"outcome":"refreshed"`, `"outcome":"current"`, `"outcome":"deferred"`, `"outcome":"latched"`, `"refused":"forbidden"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the run's log lacks %s", want)
		}
	}
	secrets := map[string]string{
		"the username":            testUsername,
		"the password":            testPassword,
		"a session cookie":        portalCookie1,
		"a later session cookie":  portalCookie2,
		"the third cookie":        "jsession-cookie-value-third-login",
		"the gateway cookie":      gatewayCookie,
		"the connector's token":   connectorToken,
		"a stranger's token":      strangerToken,
		"the broker's API token":  brokerOwnSAToken,
		"the Vault token":         vaulttest.VaultToken,
		"the Vault login JWT":     vaulttest.SAToken,
		"page content":            pageMarker,
		"markup":                  "<html",
		"a cookie name with data": "JSESSIONID=",
	}
	for where, text := range map[string]string{"log": logs, "metrics": metrics, "refusals": strings.Join(refusals, "\n")} {
		for what, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("the %s carries %s", where, what)
			}
		}
	}
	if t.Failed() {
		t.Logf("log of the run:\n%s", logs)
	}
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
