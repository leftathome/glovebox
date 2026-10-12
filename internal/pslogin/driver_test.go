package pslogin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connectors/powerschool"
	"github.com/leftathome/glovebox/internal/loginbroker"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// The login itself, the sign-in page check and the library probe are
// pinned by connectors/powerschool-auth-refresher's tests, which drive this
// package through that binary. These tests are about the Driver.

const (
	testUsername = "parent-account-7731"
	testPassword = "correct-horse-battery-staple-9"
	liveCookie   = "jsession-cookie-value-live"
)

const signInPage = `<!doctype html><html><head><title>Parent, Guardian, and Student Login</title></head><body>
<form id="LoginForm" action="/guardian/home.html" method="post">
<input type="text" name="account" id="fieldAccount"/><input type="password" name="pw" id="fieldPassword"/>
</form></body></html>`

const signedInHome = `<!doctype html><html><body>
<ul id="students-list">
<li class="selected"><a href="javascript:switchStudent(111111);">Student One</a></li>
</ul>
<div id="content-main"><h1>Grades and Attendance: One, Student</h1></div>
</body></html>`

// portal is a stand-in for the portal that records every request.
type portal struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD path"
	// signIn is what /public/ serves; status 0 = 200.
	signIn       string
	signInStatus int
	homeStatus   int
}

func newPortal(t *testing.T) *portal {
	t.Helper()
	p := &portal{signIn: signInPage}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests = append(p.requests, r.Method+" "+r.URL.Path)
		signIn, signInStatus, homeStatus := p.signIn, p.signInStatus, p.homeStatus
		p.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/public/"):
			if signInStatus != 0 {
				w.WriteHeader(signInStatus)
			}
			_, _ = w.Write([]byte(signIn))
		case r.URL.Path == "/guardian/home.html" && r.Method == http.MethodPost:
			// The credential submission, as the browser makes it.
			_ = r.ParseForm()
			if r.PostForm.Get("account") != testUsername || r.PostForm.Get("pw") != testPassword {
				http.Redirect(w, r, "/public/home.html", http.StatusFound)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: liveCookie, Path: "/"})
			_, _ = w.Write([]byte(signedInHome))
		case r.URL.Path == "/guardian/home.html":
			if homeStatus != 0 {
				w.WriteHeader(homeStatus)
				return
			}
			if c, err := r.Cookie("JSESSIONID"); err != nil || c.Value != liveCookie {
				http.Redirect(w, r, "/public/home.html", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(signedInHome))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *portal) cfg() Config {
	host := strings.TrimPrefix(p.srv.URL, "https://")
	return Config{BaseURL: "https://" + host, Host: host}
}

func (p *portal) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

func (p *portal) posts() int {
	n := 0
	for _, r := range p.seen() {
		if strings.HasPrefix(r, "POST ") {
			n++
		}
	}
	return n
}

// browserLogin stands in for the browser: it submits the form once, as a
// POST, and returns the cookies it was given. calls counts invocations.
func (p *portal) browserLogin(calls *int) func(Config) func(context.Context) (*pslib.SessionExport, error) {
	return func(cfg Config) func(context.Context) (*pslib.SessionExport, error) {
		return func(ctx context.Context) (*pslib.SessionExport, error) {
			*calls++
			form := url.Values{"account": {cfg.Username}, "pw": {cfg.Password}}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/guardian/home.html", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			client := p.srv.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			resp, err := client.Do(req)
			if err != nil {
				return nil, &LoginError{Kind: FailedUnknown, Err: err}
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return nil, &LoginError{Kind: Rejected, Err: errors.New("login failed: invalid credentials")}
			}
			return &pslib.SessionExport{Cookies: resp.Cookies(), ExpiresAt: time.Now().Add(4 * time.Hour)}, nil
		}
	}
}

var driverNow = time.Date(2026, 10, 12, 13, 5, 0, 0, time.UTC)

func newTestDriver(t *testing.T, p *portal, login func(Config) func(context.Context) (*pslib.SessionExport, error)) *Driver {
	t.Helper()
	cfg := p.cfg()
	// Credentials in the driver's own configuration are dropped: only the
	// broker's reach Login.
	cfg.Username, cfg.Password = "someone-else", "not-this-password"
	d, err := NewDriver(cfg, DriverOptions{
		Base: p.srv.Client().Transport, Login: login, Now: func() time.Time { return driverNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var creds = loginbroker.Credentials{Username: testUsername, Password: testPassword}

// One login: the sign-in page is looked at with a GET, the credentials are
// submitted exactly once, and the session comes back with the lifetime this
// driver writes instead of the library's guess.
func TestDriverLoginChecksTheSignInPageThenSubmitsOnce(t *testing.T) {
	p := newPortal(t)
	calls := 0
	d := newTestDriver(t, p, p.browserLogin(&calls))

	s, err := d.Login(context.Background(), creds)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	session := s.(*pslib.SessionExport)
	if len(session.Cookies) != 1 || session.Cookies[0].Value != liveCookie {
		t.Errorf("cookies = %v", session.Cookies)
	}
	if want := driverNow.Add(DefaultSessionLifetime); !session.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %s, want login time + 36h (%s)", session.ExpiresAt, want)
	}
	if got := strings.Join(p.seen(), ", "); got != "GET /public/, POST /guardian/home.html" {
		t.Errorf("portal saw: %s", got)
	}
	if calls != 1 || p.posts() != 1 {
		t.Errorf("browser logins = %d, POSTs = %d; want one credential submission", calls, p.posts())
	}

	// The new session is confirmed with a GET, and only a GET.
	if probe := d.Probe(context.Background(), s); probe.State != loginbroker.ProbeAlive || probe.Reason != powerschool.ReasonSignedIn {
		t.Errorf("probe of the new session = %+v", probe)
	}
	if p.posts() != 1 {
		t.Error("the probe posted")
	}
}

// The password is not typed into a page that is not the portal's sign-in
// form, and such a failure is "nothing was submitted": the broker may try
// again without it costing the account anything.
func TestDriverDoesNotStartALoginOnAnUnrecognisedSignInPage(t *testing.T) {
	for name, c := range map[string]struct {
		page   string
		status int
		detail string
	}{
		"a maintenance page":      {"<html>Down for maintenance</html>", 0, "signin_page_unrecognised:no_password_form"},
		"a form posting off host": {`<form action="https://evil.example/x"><input type="text"/><input type="password"/></form>`, 0, "signin_page_unrecognised:form_posts_off_host"},
		"forbidden":               {signInPage, 403, "signin_page_unrecognised:http_403"},
		"a server error":          {"", 503, "signin_page_unreachable:"},
	} {
		p := newPortal(t)
		p.signIn, p.signInStatus = c.page, c.status
		calls := 0
		d := newTestDriver(t, p, p.browserLogin(&calls))

		_, err := d.Login(context.Background(), creds)
		var le *loginbroker.LoginError
		if !errors.As(err, &le) || le.Kind != loginbroker.LoginNotSubmitted || !strings.HasPrefix(le.Detail, c.detail) {
			t.Errorf("%s: err = %v (%+v)", name, err, le)
			continue
		}
		if calls != 0 || p.posts() != 0 {
			t.Errorf("%s: browser logins = %d, POSTs = %d", name, calls, p.posts())
		}
		if strings.ContainsAny(le.Detail, " <>/") || strings.Contains(le.Error(), "maintenance") {
			t.Errorf("%s: detail %q / error %q is not a classified token", name, le.Detail, le.Error())
		}
	}
}

// What the library's login reports is mapped onto the broker's three kinds
// by the same classification the scheduled job uses, and the detail never
// carries the library's text.
func TestDriverMapsLoginFailures(t *testing.T) {
	p := newPortal(t)
	for msg, want := range map[string]loginbroker.LoginFailure{
		"authentication failed: login failed: invalid credentials for " + testUsername: loginbroker.LoginRejected,
		"page load error net::ERR_NAME_NOT_RESOLVED":                                   loginbroker.LoginNotSubmitted,
		"chrome failed to start: no usable sandbox":                                    loginbroker.LoginNotSubmitted,
		"context deadline exceeded":                                                    loginbroker.LoginUnknown,
		"could not find #fieldAccount; page was <html>" + testPassword + "</html>":     loginbroker.LoginUnknown,
		"page load error net::ERR_CERT_AUTHORITY_INVALID":                              loginbroker.LoginUnknown,
	} {
		d := newTestDriver(t, p, func(Config) func(context.Context) (*pslib.SessionExport, error) {
			return func(context.Context) (*pslib.SessionExport, error) {
				return nil, &LoginError{Kind: ClassifyLoginFailure(msg), Err: errors.New(msg)}
			}
		})
		_, err := d.Login(context.Background(), creds)
		var le *loginbroker.LoginError
		if !errors.As(err, &le) || le.Kind != want {
			t.Errorf("%q: kind = %+v, want %d", msg, le, want)
			continue
		}
		if strings.Contains(le.Detail, testUsername) || strings.Contains(le.Detail, testPassword) ||
			strings.Contains(le.Error(), testPassword) || strings.Contains(le.Error(), testUsername) {
			t.Errorf("%q: the driver's error or detail carries a credential: %q / %q", msg, le.Detail, le.Error())
		}
	}

	// A "successful" login without a usable cookie is not a session.
	d := newTestDriver(t, p, func(Config) func(context.Context) (*pslib.SessionExport, error) {
		return func(context.Context) (*pslib.SessionExport, error) {
			return &pslib.SessionExport{Cookies: []*http.Cookie{{Name: "JSESSIONID", Value: ""}}}, nil
		}
	})
	if s, err := d.Login(context.Background(), creds); err == nil || s != nil {
		t.Errorf("a login without cookies returned %v, %v", s, err)
	}
}

// Probe is the connector's own dead-session rule: only the portal's sign-in
// page (or the gateway's refusal) is "dead"; an error answer is "unknown".
func TestDriverProbe(t *testing.T) {
	p := newPortal(t)
	d := newTestDriver(t, p, nil)
	session := func(cookie string, expires time.Time) *pslib.SessionExport {
		return &pslib.SessionExport{Cookies: []*http.Cookie{{Name: "JSESSIONID", Value: cookie}}, ExpiresAt: expires}
	}
	ctx := context.Background()

	if got := d.Probe(ctx, session(liveCookie, driverNow.Add(time.Hour))); got.State != loginbroker.ProbeAlive {
		t.Errorf("live session: %+v", got)
	}
	if got := d.Probe(ctx, session("stale", driverNow.Add(time.Hour))); got.State != loginbroker.ProbeDead || got.Reason != powerschool.ReasonSignInPage {
		t.Errorf("refused session: %+v", got)
	}
	p.mu.Lock()
	p.homeStatus = 503
	p.mu.Unlock()
	if got := d.Probe(ctx, session(liveCookie, driverNow.Add(time.Hour))); got.State != loginbroker.ProbeUnknown || got.Reason != powerschool.ReasonServerError {
		t.Errorf("server error: %+v", got)
	}
	before := len(p.seen())
	// A document past its expires_at is not presented at all.
	if got := d.Probe(ctx, session(liveCookie, driverNow.Add(-time.Minute))); got.State != loginbroker.ProbeDead || got.Reason != powerschool.ReasonExpiredDocument {
		t.Errorf("expired document: %+v", got)
	}
	if got := d.Probe(ctx, nil); got.State != loginbroker.ProbeDead {
		t.Errorf("no session: %+v", got)
	}
	if len(p.seen()) != before {
		t.Error("the portal was asked about a session that would not be presented")
	}
	for _, r := range p.seen() {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("the probe sent %s", r)
		}
	}
}

// By construction, not by intention: the clients behind the probe and the
// sign-in check cannot send anything but a read, and only to the portal.
func TestDriverReadOnlyClientsRefuseToWriteOrLeaveTheHost(t *testing.T) {
	p := newPortal(t)
	cfg := p.cfg()
	signIn, err := SignInHTTPClient(cfg, p.srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	for name, client := range map[string]*http.Client{"probe": ProbeHTTPClient(cfg, p.srv.Client().Transport), "sign-in": signIn} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req, _ := http.NewRequest(method, cfg.BaseURL+"/guardian/home.html", strings.NewReader("x=1"))
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				t.Errorf("%s client sent a %s", name, method)
			}
		}
		req, _ := http.NewRequest(http.MethodGet, "https://elsewhere.invalid/", nil)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			t.Errorf("%s client left the portal host", name)
		}
	}
	if len(p.seen()) != 0 {
		t.Errorf("refused requests reached the portal: %v", p.seen())
	}
}

// The secret the broker writes is the one the scheduled job writes, field
// for field, so ESO and the connector cannot tell which of them logged in.
func TestDriverEncodesTheSecretTheScheduledJobWrites(t *testing.T) {
	p := newPortal(t)
	d := newTestDriver(t, p, nil)
	session := &pslib.SessionExport{
		Cookies:   []*http.Cookie{{Name: "JSESSIONID", Value: liveCookie}, {Name: "MRHSession", Value: "gateway-cookie"}},
		ExpiresAt: driverNow.Add(36 * time.Hour),
	}
	fields, err := d.Encode(session, driverNow)
	if err != nil {
		t.Fatal(err)
	}
	job, err := SessionData(session, driverNow, "111111")
	if err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]string) string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	jobKeys := map[string]string{}
	for k := range job {
		jobKeys[k] = ""
	}
	if keys(fields) != keys(jobKeys) {
		t.Errorf("broker fields %s, scheduled job fields %s", keys(fields), keys(jobKeys))
	}
	if fields[FieldSession] != job[FieldSession] || fields[FieldExpiresAt] != job[FieldExpiresAt] || fields[FieldLoggedInAt] != job[FieldLoggedInAt] {
		t.Errorf("fields differ from the scheduled job's: %v", fields)
	}
	if FieldSession != powerschool.SessionField {
		t.Error("the connector reads the session from another field than the driver writes")
	}

	// What the connector does with it.
	parsed, err := powerschool.ParseSession([]byte(fields[FieldSession]), driverNow)
	if err != nil || powerschool.SessionFingerprint(parsed) != d.Fingerprint(session) {
		t.Fatalf("the connector cannot use the encoded session: %v", err)
	}

	back, err := d.Decode(fields)
	if err != nil || d.Fingerprint(back) != d.Fingerprint(session) {
		t.Fatalf("Decode(Encode(s)) is another session: %v", err)
	}
	// An expired document still decodes: the probe is what calls it dead.
	old, _ := d.Encode(&pslib.SessionExport{Cookies: session.Cookies, ExpiresAt: driverNow.Add(-48 * time.Hour)}, driverNow.Add(-84*time.Hour))
	if _, err := d.Decode(old); err != nil {
		t.Errorf("an expired document does not decode: %v", err)
	}
	for _, bad := range []map[string]string{nil, {}, {FieldSession: ""}, {FieldSession: `{"cookies":[]`}, {FieldSession: "secret-" + liveCookie}} {
		_, err := d.Decode(bad)
		if err == nil {
			t.Errorf("Decode(%v) succeeded", bad)
		} else if strings.Contains(err.Error(), liveCookie) {
			t.Errorf("the decode error quotes the document: %v", err)
		}
	}
	if _, err := d.Encode("not a session", driverNow); err == nil {
		t.Error("Encode accepted a foreign session type")
	}
}

// The owner's hours, the 20-minute floor and the login headroom come with
// the driver and are the same ones the connector and the scheduled job use.
func TestDriverConstraintsAreThePortalsFloor(t *testing.T) {
	p := newPortal(t)
	d := newTestDriver(t, p, nil)
	limits := d.Constraints()
	if limits.MinInterval != 20*time.Minute || limits.LoginHeadroom != psportal.LoginHeadroom {
		t.Errorf("limits = %+v", limits)
	}
	la := psportal.FloorLocation()
	day := time.Date(2026, 10, 12, 0, 0, 0, 0, la)
	for at := day; at.Before(day.Add(25 * time.Hour)); at = at.Add(37 * time.Second) {
		if limits.Blackout.BlocksAt(at, limits.LoginHeadroom) != psportal.FloorBlocksLoginAt(at) {
			t.Fatalf("%s: the driver's blackout disagrees with the shared floor", at.Format("15:04:05"))
		}
	}
	if d.Site() != "powerschool" {
		t.Errorf("site = %q", d.Site())
	}
	if _, err := NewDriver(Config{}, DriverOptions{}); err == nil {
		t.Error("a driver without a portal address was built")
	}
	if _, err := NewDriver(p.cfg(), DriverOptions{SessionLifetime: time.Minute}); err == nil {
		t.Error("a one-minute session lifetime was accepted")
	}
}
