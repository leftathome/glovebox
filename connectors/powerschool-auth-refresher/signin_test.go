package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connectors/powerschool"
	pslib "github.com/leftathome/powerschool-go"
)

// referenceSignInPage has the shape of the live sign-in page as seen
// without credentials on 2026-10-11: one form#LoginForm posting to
// /guardian/home.html, a text input for the account, two password inputs
// and a set of hidden ones. Every value is invented.
const referenceSignInPage = `<!doctype html><html><head><title>Parent, Guardian, and Student Login</title></head><body>
<form id="LoginForm" action="/guardian/home.html" method="post" name="LoginForm">
<input type="hidden" name="dbpw" value=""/>
<input type="hidden" name="translator_username" value=""/>
<input type="hidden" name="translator_password" value=""/>
<input type="hidden" name="translator_ldappassword" value=""/>
<input type="hidden" name="returnUrl" value=""/>
<input type="hidden" name="serviceName" value="PS Parent Portal"/>
<input type="hidden" name="serviceTicket" value=""/>
<input type="hidden" name="pcasServerUrl" value="/"/>
<input type="hidden" name="credentialType" value="User Id and Password Credential"/>
<input type="hidden" name="ldappassword" value=""/>
<input type="text" id="fieldAccount" name="account" value=""/>
<input type="password" id="fieldPassword" name="pw" value=""/>
<input type="password" id="translatorpw" name="translatorpw" value=""/>
<button type="submit" id="btn-enter-sign-in">Sign In</button>
</form></body></html>`

func (p *portal) cfg() config {
	host := strings.TrimPrefix(p.srv.URL, "https://")
	return config{BaseURL: "https://" + host, Host: host}
}

func (p *portal) signInCheck(t *testing.T) func(context.Context) error {
	t.Helper()
	return realSignInCheck(p.cfg(), p.srv.Client().Transport)
}

func (p *portal) paths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

// --- an emulated access gateway --------------------------------------------

// gateway emulates what the F5 access gateway in front of the reference
// portal does to a client, as observed from the public sign-in page:
//
//   - a request without a live gateway cookie is redirected to /my.policy,
//     and handed a new MRHSession on the way;
//   - /my.policy, asked WITH that cookie, marks it live and redirects back
//     to where the client came from;
//   - /my.policy asked WITHOUT a cookie (a client that drops cookies between
//     redirects) redirects to /my.logout.php3?errorcode=19, a small page
//     with no form.
//
// Behind it sit the portal's sign-in page and home page.
type gateway struct {
	*portal
	issued atomic.Int64
	// refuse makes /my.policy refuse every client, cookie or not.
	refuse bool
	// portalSession is the JSESSIONID the portal accepts.
	portalSession string
}

func newGateway(t *testing.T) *gateway {
	t.Helper()
	g := &gateway{portalSession: "portal-session-live"}
	live := map[string]bool{}
	handedOut := map[string]bool{}
	g.portal = newPortal(t, func(w http.ResponseWriter, r *http.Request) {
		mrh := ""
		if c, err := r.Cookie("MRHSession"); err == nil {
			mrh = c.Value
		}
		switch r.URL.Path {
		case "/my.logout.php3":
			_, _ = w.Write([]byte("<html><body>Your session could not be established.</body></html>"))
			return
		case "/my.policy":
			// Only a cookie this gateway handed out moments ago is accepted
			// here; a lapsed one from an old session is not.
			if g.refuse || !handedOut[mrh] {
				http.Redirect(w, r, "/my.logout.php3?errorcode=19", http.StatusFound)
				return
			}
			live[mrh] = true
			http.SetCookie(w, &http.Cookie{Name: "F5_ST", Value: "st-1", Path: "/", Secure: true})
			back := "/public/"
			if c, err := r.Cookie("F5_back"); err == nil {
				back = c.Value
			}
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		if !live[mrh] {
			// No gateway session, or a lapsed one: start a new one.
			fresh := fmt.Sprintf("mrh-%d", g.issued.Add(1))
			handedOut[fresh] = true
			http.SetCookie(w, &http.Cookie{Name: "MRHSession", Value: fresh, Path: "/", Secure: true, HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "LastMRH_Session", Value: fresh, Path: "/", Secure: true})
			http.SetCookie(w, &http.Cookie{Name: "F5_back", Value: r.URL.Path, Path: "/", Secure: true})
			http.Redirect(w, r, "/my.policy", http.StatusFound)
			return
		}
		// Through the gateway: the portal itself.
		switch {
		case strings.HasPrefix(r.URL.Path, "/public/"):
			_, _ = w.Write([]byte(referenceSignInPage))
		case r.URL.Path == "/guardian/home.html":
			if c, err := r.Cookie("JSESSIONID"); err != nil || c.Value != g.portalSession {
				http.Redirect(w, r, "/public/home.html", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(signedInHome))
		default:
			http.NotFound(w, r)
		}
	})
	return g
}

// --- the sign-in check through the gateway ---------------------------------

// What 0.10.2 did, reproduced: the connector's jar-less client loses the
// gateway's cookie between redirects, is refused at /my.policy, and ends on
// the gateway's error page -- which has no form. That is the live failure
// `signin_page_unrecognised: no_password_form`.
func TestAJarlessClientIsTurnedAwayByTheGateway(t *testing.T) {
	g := newGateway(t)
	cfg := g.cfg()

	client := powerschool.NewHTTPClient(cfg.Host, false, nil, probeTimeout)
	client.Transport.(*powerschool.Transport).Base = g.srv.Client().Transport
	resp, err := client.Get(cfg.BaseURL + signInPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if got := resp.Request.URL.Path; got != "/my.logout.php3" {
		t.Fatalf("a client without a jar ended at %s, want /my.logout.php3", got)
	}
	want := []string{"GET /public/", "GET /my.policy", "GET /my.logout.php3"}
	if got := g.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if strings.Contains(string(body), "<form") {
		t.Error("the gateway's error page has a form")
	}
}

// The fix: with a jar of its own, the check returns the gateway's cookies,
// is let through, and sees the real sign-in form.
func TestSignInCheckPassesTheGatewayWithItsOwnCookieJar(t *testing.T) {
	g := newGateway(t)
	if err := g.signInCheck(t)(context.Background()); err != nil {
		t.Fatalf("sign-in check through the gateway: %v (reason %q)", err, signInReason(err))
	}
	want := []string{"GET /public/", "GET /my.policy", "GET /public/"}
	if got := g.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if g.cookies[0] != "" {
		t.Errorf("the first request carried cookies: %q", g.cookies[0])
	}
	if !strings.Contains(g.cookies[2], "MRHSession=mrh-1") {
		t.Errorf("the gateway's cookie was not returned on the way back: %q", g.cookies[2])
	}
}

// The jar lives for one check. A second check starts with nothing: it is
// handed a new gateway session, not the first one's.
func TestSignInCheckJarIsNotReused(t *testing.T) {
	g := newGateway(t)
	check := g.signInCheck(t)
	for i := 0; i < 2; i++ {
		if err := check(context.Background()); err != nil {
			t.Fatalf("check %d: %v", i+1, err)
		}
	}
	if g.cookies[3] != "" {
		t.Errorf("the second check began with the first one's cookies: %q", g.cookies[3])
	}
	if got := g.issued.Load(); got != 2 {
		t.Errorf("gateway sessions issued = %d, want one per check", got)
	}
}

// The gateway refuses even a client that returns its cookies. The reason
// names the gateway, so the log says what happened.
func TestSignInCheckReportsAGatewayRefusal(t *testing.T) {
	g := newGateway(t)
	g.refuse = true
	err := g.signInCheck(t)(context.Background())
	if !errors.Is(err, errSignInUnrecognised) || signInReason(err) != "gateway_session_refused" {
		t.Fatalf("err = %v, reason = %q, want gateway_session_refused", err, signInReason(err))
	}
	if got := g.paths(); got[len(got)-1] != "GET /my.logout.php3" {
		t.Errorf("requests = %v", got)
	}

	// Stuck at the policy endpoint itself counts the same.
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/my.policy" {
			http.Redirect(w, r, "/my.policy", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("<html><body><noscript>Enable JavaScript</noscript></body></html>"))
	})
	err = p.signInCheck(t)(context.Background())
	if signInReason(err) != "gateway_session_refused" {
		t.Errorf("ending at /my.policy: reason = %q", signInReason(err))
	}
}

func TestSignInCheckRefusesARedirectLoop(t *testing.T) {
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/public/" {
			http.Redirect(w, r, "/public/again", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/public/", http.StatusFound)
	})
	err := p.signInCheck(t)(context.Background())
	if !errors.Is(err, errSignInUnrecognised) || signInReason(err) != "signin_redirect_loop" {
		t.Fatalf("err = %v, reason = %q, want signin_redirect_loop", err, signInReason(err))
	}
	if got := len(p.paths()); got != maxSignInRedirects+1 {
		t.Errorf("requests = %d, want the first and %d redirects", got, maxSignInRedirects)
	}
}

func TestSignInCheckFollowsExactlyFiveRedirects(t *testing.T) {
	var hops atomic.Int64
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) {
		if n := hops.Add(1); n <= maxSignInRedirects {
			http.Redirect(w, r, fmt.Sprintf("/public/hop%d", n), http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(referenceSignInPage))
	})
	if err := p.signInCheck(t)(context.Background()); err != nil {
		t.Fatalf("five redirects: %v (reason %q)", err, signInReason(err))
	}
}

func TestSignInCheckRefusesAnOffHostRedirect(t *testing.T) {
	for name, target := range map[string]func(r *http.Request) string{
		"another host":             func(r *http.Request) string { return "https://sso.example.net/login" },
		"plain http, same host":    func(r *http.Request) string { return "http://" + r.Host + "/public/" },
		"another port":             func(r *http.Request) string { return "https://127.0.0.1:1/public/" },
		"another host, then back":  func(r *http.Request) string { return "https://sso.example.net/back?to=" + r.Host },
		"scheme-relative redirect": func(r *http.Request) string { return "//evil.example.net/public/" },
	} {
		t.Run(name, func(t *testing.T) {
			p := newPortal(t, func(w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "MRHSession", Value: "mrh-1", Path: "/"})
				http.Redirect(w, r, target(r), http.StatusFound)
			})
			err := p.signInCheck(t)(context.Background())
			if !errors.Is(err, errSignInUnrecognised) || signInReason(err) != "signin_redirect_off_host" {
				t.Fatalf("err = %v, reason = %q, want signin_redirect_off_host", err, signInReason(err))
			}
			if got := p.paths(); len(got) != 1 {
				t.Errorf("requests = %v, want only the first: the redirect is not followed", got)
			}
		})
	}
}

// --- what counts as the sign-in form ---------------------------------------

func TestSignInCheckAcceptsThePortalsOwnForm(t *testing.T) {
	for name, page := range map[string]string{
		"the reference page": referenceSignInPage,
		"minimal":            signInPage,
		"no action":          `<form id="LoginForm"><input type="text" name="account"><input type="password" name="pw"></form>`,
		"empty action":       `<form action=""><input name="account"><input type="password" name="pw"></form>`,
		"page-relative":      `<form action="home.html" method="post"><input type="email" name="u"><input type="password" name="pw"></form>`,
		"other forms beside": `<form action="/search"><input type="text" name="q"></form>` + signInPage,
	} {
		t.Run(name, func(t *testing.T) {
			p := newPortal(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(page)) })
			if err := p.signInCheck(t)(context.Background()); err != nil {
				t.Fatalf("sign-in check: %v (reason %q)", err, signInReason(err))
			}
			if got := p.paths(); len(got) != 1 || got[0] != "GET /public/" {
				t.Errorf("requests = %v, want one GET of /public/", got)
			}
		})
	}
}

func TestSignInCheckAcceptsAnAbsoluteActionOnTheSameHost(t *testing.T) {
	var p *portal
	p = newPortal(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<form action="` + p.srv.URL + `/guardian/home.html"><input type="text" name="account"><input type="password"></form>`))
	})
	if err := p.signInCheck(t)(context.Background()); err != nil {
		t.Fatalf("sign-in check: %v", err)
	}
}

func TestSignInCheckRefusesAPageThatIsNotThePortalsForm(t *testing.T) {
	const fields = `<input type="text" name="account"><input type="password" name="pw">`
	for name, c := range map[string]struct {
		handler func(http.ResponseWriter, *http.Request)
		reason  string
	}{
		"no form at all": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html><body>Down for maintenance</body></html>"))
		}, "no_password_form"},
		"a form without a password field": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<form action="/search"><input type="text" name="q"></form>`))
		}, "no_password_form"},
		"a password field outside any form": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<div><input type="text"><input type="password" name="pw"></div>`))
		}, "no_password_form"},
		"a password field but nowhere to type a name": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<form action="/guardian/home.html"><input type="hidden" name="account" value="x"><input type="password" name="pw"></form>`))
		}, "no_username_field"},
		"the name field is in another form": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<form action="/a"><input type="text" name="account"></form><form action="/b"><input type="password" name="pw"></form>`))
		}, "no_username_field"},
		"the form posts to another host": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<form action="https://login.example.net/sso">` + fields + `</form>`))
		}, "form_posts_off_host"},
		"the form posts over plain http": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<form action="http://` + r.Host + `/guardian/home.html">` + fields + `</form>`))
		}, "form_posts_off_host"},
		"a scheme-relative action to another host": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<form action="//evil.example.net/x">` + fields + `</form>`))
		}, "form_posts_off_host"},
		"one good form and one that posts a password elsewhere": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(referenceSignInPage + `<form action="https://evil.example.net/"><input type="password"></form>`))
		}, "form_posts_off_host"},
		"the right form, but not under /public/": {func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/public/" {
				http.Redirect(w, r, "/elsewhere/login.html", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(referenceSignInPage))
		}, "final_page_not_under_public"},
		"not found": {func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "http_404"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPortal(t, c.handler)
			err := p.signInCheck(t)(context.Background())
			if !errors.Is(err, errSignInUnrecognised) {
				t.Fatalf("err = %v, want errSignInUnrecognised", err)
			}
			if got := signInReason(err); got != c.reason {
				t.Errorf("reason = %q, want %q", got, c.reason)
			}
			for _, req := range p.paths() {
				if !strings.HasPrefix(req, "GET ") {
					t.Errorf("non-GET request %q", req)
				}
			}
		})
	}
}

// A server error or an unreachable portal is a failure, but not a verdict
// on the page.
func TestSignInCheckDoesNotCallAnOutageAnUnrecognisedPage(t *testing.T) {
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	err := p.signInCheck(t)(context.Background())
	if err == nil || errors.Is(err, errSignInUnrecognised) {
		t.Errorf("a 502: err = %v", err)
	}

	check := p.signInCheck(t)
	p.srv.Close()
	err = check(context.Background())
	if err == nil || errors.Is(err, errSignInUnrecognised) {
		t.Errorf("a closed server: err = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "https://") {
		t.Errorf("the error carries the URL: %v", err)
	}
}

// The sign-in client can only read, whatever holds it.
func TestSignInClientIsReadOnlyAndPinned(t *testing.T) {
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(referenceSignInPage)) })
	cfg := p.cfg()
	client, err := signInHTTPClient(cfg, p.srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := client.Post(cfg.BaseURL+"/guardian/home.html", "application/x-www-form-urlencoded", strings.NewReader("account=u&pw=p")); err == nil {
		resp.Body.Close()
		t.Fatal("a POST went through the sign-in client")
	}
	if resp, err := client.Get("https://elsewhere.example.org/public/"); err == nil {
		resp.Body.Close()
		t.Fatal("a request to another host went through the sign-in client")
	}
	if len(p.paths()) != 0 {
		t.Errorf("requests reached the server: %v", p.paths())
	}
}

// --- the home-page read through the gateway (the stored session) ------------

// gatewaySession is a stored session as the browser login exports it: the
// portal's cookie and the gateway's, for the same host.
func gatewaySession(portalSession, mrh string) *pslib.SessionExport {
	return &pslib.SessionExport{
		Cookies: []*http.Cookie{
			{Name: "JSESSIONID", Value: portalSession},
			{Name: "MRHSession", Value: mrh},
			{Name: "LastMRH_Session", Value: mrh},
		},
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

// The gateway's cookie in a stored session has lapsed; the portal's has
// not. The gateway renews its cookie through /my.policy in mid-read. The
// probe must carry the renewed cookie back and report the session good --
// otherwise a good session reads as "could not tell" on every run.
func TestProbeSurvivesTheGatewayRenewingItsCookie(t *testing.T) {
	g := newGateway(t)
	session := gatewaySession(g.portalSession, "mrh-lapsed")

	selected, err := g.probe(t)(context.Background(), session)
	if err != nil {
		t.Fatalf("probe with a lapsed gateway cookie and a good portal session: %v", err)
	}
	if selected != "111111" {
		t.Errorf("selected = %q", selected)
	}
	want := []string{"GET /guardian/home.html", "GET /my.policy", "GET /guardian/home.html"}
	if got := g.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", got, want)
	}
	last := g.cookies[2]
	if !strings.Contains(last, "MRHSession=mrh-1") || strings.Contains(last, "mrh-lapsed") {
		t.Errorf("the renewed gateway cookie did not replace the lapsed one: %q", last)
	}
	if !strings.Contains(last, "JSESSIONID="+g.portalSession) {
		t.Errorf("the portal's cookie was lost: %q", last)
	}
	// The stored session itself is untouched: nothing is written back.
	if session.Cookies[1].Value != "mrh-lapsed" {
		t.Error("the probe modified the session it was given")
	}
}

// Without the overlay -- the connector's client as it is -- the same read
// ends on the gateway's error page and is neither "good" nor "rejected".
func TestWithoutTheOverlayALapsedGatewayCookieMakesTheSessionUnreadable(t *testing.T) {
	g := newGateway(t)
	cfg := g.cfg()
	client := powerschool.NewHTTPClient(cfg.Host, false, nil, probeTimeout)
	client.Transport.(*powerschool.Transport).Base = g.srv.Client().Transport
	session := gatewaySession(g.portalSession, "mrh-lapsed")
	lib, err := pslib.NewClient(cfg.BaseURL, pslib.WithHTTPClient(client), pslib.WithHTTPPageFetch(),
		pslib.WithSession(session.Cookies, session.ExpiresAt), pslib.WithLogLevel(pslib.LogLevelWarn))
	if err != nil {
		t.Fatal(err)
	}
	_, err = lib.GetHomePage(context.Background())
	if err == nil {
		t.Fatal("the jar-less read succeeded; the emulation is wrong")
	}
	if pslib.IsSessionExpired(err) {
		t.Errorf("err = %v reads as a rejected session", err)
	}
	if got := g.paths(); got[len(got)-1] != "GET /my.logout.php3" {
		t.Errorf("requests = %v", got)
	}
}

// A live gateway cookie needs no renewal: one request, cookies as stored.
func TestProbeWithALiveGatewayCookieMakesOneRequest(t *testing.T) {
	g := newGateway(t)
	// Obtain a live gateway session the way a browser would.
	if err := g.signInCheck(t)(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(g.paths())
	if _, err := g.probe(t)(context.Background(), gatewaySession(g.portalSession, "mrh-1")); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := g.paths()[before:]; len(got) != 1 || got[0] != "GET /guardian/home.html" {
		t.Errorf("requests = %v, want one GET of the home page", got)
	}
}

// The portal's own session is dead. Renewing the gateway cookie must not
// hide that: the portal then answers with its sign-in page, and the probe
// says "rejected".
func TestProbeStillReportsADeadPortalSessionBehindTheGateway(t *testing.T) {
	g := newGateway(t)
	_, err := g.probe(t)(context.Background(), gatewaySession("portal-session-dead", "mrh-lapsed"))
	if !pslib.IsSessionExpired(err) {
		t.Fatalf("err = %v, want a session-expired error", err)
	}
}

// The gateway refuses outright: the question is unanswered, not "rejected",
// so the run stops instead of logging in.
func TestProbeTreatsAGatewayRefusalAsUnanswered(t *testing.T) {
	g := newGateway(t)
	g.refuse = true
	_, err := g.probe(t)(context.Background(), gatewaySession(g.portalSession, "mrh-lapsed"))
	if err == nil || pslib.IsSessionExpired(err) {
		t.Fatalf("err = %v, want an error that is not session-expired", err)
	}
}

// Each probe starts from the stored cookies alone: what one probe collected
// is not carried into the next.
func TestProbeOverlayIsNotSharedBetweenProbes(t *testing.T) {
	g := newGateway(t)
	probe := g.probe(t)
	for i := 0; i < 2; i++ {
		if _, err := probe(context.Background(), gatewaySession(g.portalSession, "mrh-lapsed")); err != nil {
			t.Fatalf("probe %d: %v", i+1, err)
		}
	}
	// The second probe's first request carries the stored (lapsed) cookie,
	// not the one the first probe was given.
	if first := g.cookies[3]; !strings.Contains(first, "MRHSession=mrh-lapsed") || strings.Contains(first, "mrh-1") {
		t.Errorf("the second probe began with %q", first)
	}
}

// --- the overlay itself ----------------------------------------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayCookiesOverlay(t *testing.T) {
	var seen []string
	responses := [][]string{
		{"MRHSession=new; Path=/", "F5_ST=st; Path=/", "gone=; Max-Age=0; Path=/"},
		{"epoch=x; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/"},
		nil,
	}
	call := 0
	g := newGatewayCookies(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.Header.Get("Cookie"))
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}
		for _, sc := range responses[call] {
			resp.Header.Add("Set-Cookie", sc)
		}
		call++
		return resp, nil
	}))

	original := "JSESSIONID=keep; MRHSession=old; gone=was-here; epoch=was-here"
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodGet, "https://portal.example.org/guardian/home.html", nil)
		req.Header.Set("Cookie", original)
		if _, err := g.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Cookie") != original {
			t.Fatalf("the caller's request was modified: %q", req.Header.Get("Cookie"))
		}
	}

	if seen[0] != original {
		t.Errorf("first request = %q, want the stored cookies untouched", seen[0])
	}
	second := "; " + seen[1] + ";"
	for _, want := range []string{"; JSESSIONID=keep;", "; MRHSession=new;", "; F5_ST=st;", "; epoch=was-here;"} {
		if !strings.Contains(second, want) {
			t.Errorf("second request %q lacks %q", seen[1], want)
		}
	}
	if strings.Contains(seen[1], "MRHSession=old") || strings.Contains(seen[1], "gone=") {
		t.Errorf("second request %q still carries a replaced or deleted cookie", seen[1])
	}
	if strings.Contains(seen[2], "epoch=") {
		t.Errorf("third request %q still carries a cookie expired at the epoch", seen[2])
	}
}

func TestGatewayCookiesOverlayIsBounded(t *testing.T) {
	g := newGatewayCookies(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}
		for i := 0; i < 500; i++ {
			resp.Header.Add("Set-Cookie", fmt.Sprintf("c%d=v; Path=/", i))
		}
		return resp, nil
	}))
	req, _ := http.NewRequest(http.MethodGet, "https://portal.example.org/", nil)
	if _, err := g.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if len(g.overlay) != maxOverlayCookies {
		t.Errorf("overlay holds %d cookies, want the cap of %d", len(g.overlay), maxOverlayCookies)
	}
}
