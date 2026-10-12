package powerschool

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

func psportalOverlay(base http.RoundTripper) http.RoundTripper {
	return psportal.NewGatewayCookies(base)
}

func pemEncode(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

const probeSignIn = `<!doctype html><html><head><title>Parent, Guardian, and Student Login</title></head><body>
<form id="LoginForm" action="/guardian/home.html" method="post">
<input type="text" name="account"/><input type="password" name="pw"/></form></body></html>`

// gatewayPortal emulates the portal behind its access gateway, as observed
// on the reference instance:
//
//   - a request without a live gateway cookie is redirected to /my.policy
//     and handed a new MRHSession on the way;
//   - /my.policy, asked with that cookie, marks it live and redirects back;
//     asked without (a client that drops cookies between redirects), it
//     redirects to /my.logout.php3, a page with no form;
//   - behind the gateway, the home page is served to a live JSESSIONID and
//     anything else is redirected to the sign-in page under /public/.
type gatewayPortal struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
	issued   int
	// refuse makes /my.policy refuse everybody.
	refuse bool
	// portalSession is the JSESSIONID the portal accepts ("" = none).
	portalSession string
	// homeStatus overrides the home page's status when not zero.
	homeStatus int
}

func newGatewayPortal(t *testing.T) *gatewayPortal {
	t.Helper()
	g := &gatewayPortal{portalSession: "portal-live"}
	live := map[string]bool{}
	handedOut := map[string]bool{}
	g.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.requests = append(g.requests, r.Method+" "+r.URL.Path)
		mrh := ""
		if c, err := r.Cookie("MRHSession"); err == nil {
			mrh = c.Value
		}
		switch r.URL.Path {
		case "/my.logout.php3":
			_, _ = w.Write([]byte("<html><body>Your session could not be established.</body></html>"))
			return
		case "/my.policy":
			if g.refuse || !handedOut[mrh] {
				http.Redirect(w, r, "/my.logout.php3?errorcode=19", http.StatusFound)
				return
			}
			live[mrh] = true
			back := "/public/"
			if c, err := r.Cookie("F5_back"); err == nil {
				back = c.Value
			}
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		if !live[mrh] {
			g.issued++
			fresh := fmt.Sprintf("mrh-%d", g.issued)
			handedOut[fresh] = true
			http.SetCookie(w, &http.Cookie{Name: "MRHSession", Value: fresh, Path: "/", Secure: true, HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "F5_back", Value: r.URL.Path, Path: "/", Secure: true})
			http.Redirect(w, r, "/my.policy", http.StatusFound)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/public/"):
			_, _ = w.Write([]byte(probeSignIn))
		case r.URL.Path == "/guardian/home.html":
			if g.homeStatus != 0 {
				w.WriteHeader(g.homeStatus)
				return
			}
			if c, err := r.Cookie("JSESSIONID"); err != nil || c.Value != g.portalSession {
				http.Redirect(w, r, "/public/home.html", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(syntheticHome("111111")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gatewayPortal) host() string { return strings.TrimPrefix(g.srv.URL, "https://") }

// client is the http.Client main.go builds, with only the innermost
// transport swapped for the test server's.
func (g *gatewayPortal) client(overlay bool) *http.Client {
	c := NewHTTPClient(g.host(), true, []string{"111111"}, 5*time.Second)
	tr := c.Transport.(*Transport)
	if overlay {
		tr.Base = psportalOverlay(g.srv.Client().Transport)
	} else {
		tr.Base = g.srv.Client().Transport
	}
	return c
}

func (g *gatewayPortal) session(jsession, mrh string) *pslib.SessionExport {
	return &pslib.SessionExport{
		Cookies:   []*http.Cookie{{Name: "JSESSIONID", Value: jsession}, {Name: "MRHSession", Value: mrh}},
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func (g *gatewayPortal) probe(overlay bool, s *pslib.SessionExport) SessionVerdict {
	return NewSessionProbe(g.client(overlay), g.srv.URL, s)(context.Background())
}

func (g *gatewayPortal) paths() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.requests, ",")
}

// A live portal session whose gateway cookie has lapsed: the gateway bounces
// the request through /my.policy, the overlay carries the renewed cookie
// back, and the home page is served. Live.
func TestProbeLiveSessionThroughAGatewayBounce(t *testing.T) {
	g := newGatewayPortal(t)
	got := g.probe(true, g.session("portal-live", "mrh-lapsed"))
	if got.State != SessionLive || got.Reason != ReasonSignedIn {
		t.Fatalf("verdict = %s/%s, want live", got.State, got.Reason)
	}
	if want := "GET /guardian/home.html,GET /my.policy,GET /guardian/home.html"; g.paths() != want {
		t.Errorf("requests = %s, want %s", g.paths(), want)
	}
}

// The live failure of 2026-10-11, reproduced and fixed: an idle-dead portal
// session behind the gateway. With the overlay the gateway is passed and
// the PORTAL answers with its sign-in page: dead, reason sign_in_page -- not
// a parse error.
func TestProbeDeadSessionBehindTheGatewayIsDeadNotAParseError(t *testing.T) {
	g := newGatewayPortal(t)
	got := g.probe(true, g.session("portal-idle-dead", "mrh-lapsed"))
	if got.State != SessionDead || got.Reason != ReasonSignInPage {
		t.Fatalf("verdict = %s/%s, want dead/sign_in_page", got.State, got.Reason)
	}
	if !strings.HasSuffix(g.paths(), "GET /public/home.html") {
		t.Errorf("requests = %s", g.paths())
	}
	for _, r := range strings.Split(g.paths(), ",") {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("non-GET request %s", r)
		}
	}
}

// Without the overlay (0.10.3's connector) the same session ends on the
// gateway's error page. The probe still calls that dead -- the gateway's
// pages are a session answer -- where the library called it a parse error.
func TestProbeEndingOnTheGatewaysPagesIsDead(t *testing.T) {
	g := newGatewayPortal(t)
	got := g.probe(false, g.session("portal-idle-dead", "mrh-lapsed"))
	if got.State != SessionDead || got.Reason != ReasonGatewayPage {
		t.Fatalf("no overlay: verdict = %s/%s, want dead/gateway_page", got.State, got.Reason)
	}
	if !strings.HasSuffix(g.paths(), "GET /my.logout.php3") {
		t.Errorf("requests = %s", g.paths())
	}

	// What the library made of that page before: a parse error, which the
	// connector logged as a kid that could not be selected.
	lib, err := pslib.NewClient(g.srv.URL, pslib.WithHTTPClient(g.client(false)), pslib.WithHTTPPageFetch(),
		pslib.WithSession(g.session("portal-idle-dead", "mrh-lapsed").Cookies, time.Now().Add(time.Hour)),
		pslib.WithLogLevel(pslib.LogLevelWarn))
	if err != nil {
		t.Fatal(err)
	}
	_, err = lib.GetHomePage(context.Background())
	if err == nil || classifyErr(err) != "parse" {
		t.Errorf("library error class = %q (%v); the emulation no longer reproduces the live failure", classifyErr(err), err)
	}

	// The gateway refusing even a cookie-keeping client: dead as well.
	g2 := newGatewayPortal(t)
	g2.refuse = true
	if got := g2.probe(true, g2.session("portal-live", "mrh-lapsed")); got.State != SessionDead || got.Reason != ReasonGatewayPage {
		t.Errorf("gateway refusal: verdict = %s/%s", got.State, got.Reason)
	}
}

func TestProbeUnknownWhenThePortalCannotAnswer(t *testing.T) {
	g := newGatewayPortal(t)
	g.homeStatus = http.StatusServiceUnavailable
	if got := g.probe(true, g.session("portal-live", "mrh-lapsed")); got.State != SessionUnknown || got.Reason != ReasonServerError {
		t.Errorf("503: verdict = %s/%s", got.State, got.Reason)
	}

	down := newGatewayPortal(t)
	client := down.client(true)
	url := down.srv.URL
	down.srv.Close()
	got := NewSessionProbe(client, url, down.session("portal-live", "x"))(context.Background())
	if got.State != SessionUnknown || got.Reason != ReasonNetwork {
		t.Errorf("unreachable: verdict = %s/%s", got.State, got.Reason)
	}
}

// The connector's reads use the same client, so they survive the gateway
// renewing its cookie too: the library reads the home page through the
// bounce.
func TestLibraryReadsSurviveTheGatewayBounce(t *testing.T) {
	g := newGatewayPortal(t)
	s := g.session("portal-live", "mrh-lapsed")
	lib, err := pslib.NewClient(g.srv.URL, pslib.WithHTTPClient(g.client(true)), pslib.WithHTTPPageFetch(),
		pslib.WithSession(s.Cookies, s.ExpiresAt), pslib.WithLogLevel(pslib.LogLevelWarn))
	if err != nil {
		t.Fatal(err)
	}
	page, err := lib.GetHomePage(context.Background())
	if err != nil {
		t.Fatalf("GetHomePage through the gateway: %v", err)
	}
	if sel := page.SelectedStudent(); sel == nil || sel.ID != "111111" {
		t.Errorf("selected = %v", sel)
	}
	// Second read on the same client: the renewed cookie is still carried,
	// so there is no second bounce.
	before := len(strings.Split(g.paths(), ","))
	if _, err := lib.GetHomePage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := len(strings.Split(g.paths(), ",")); after != before+1 {
		t.Errorf("the second read took %d requests, want 1", after-before)
	}
}

func TestNewHTTPClientCarriesTheGatewayOverlay(t *testing.T) {
	c := NewHTTPClient("ps.example.org", false, nil, time.Second)
	if _, ok := c.Transport.(*Transport).Base.(interface {
		RoundTrip(*http.Request) (*http.Response, error)
	}); !ok || c.Transport.(*Transport).Base == nil {
		t.Fatal("NewHTTPClient has no gateway-cookie overlay under its transport")
	}
	if c.Jar != nil {
		t.Error("the client has a cookie jar")
	}
}
