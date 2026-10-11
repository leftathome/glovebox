package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// The first three messages are what powerschool-go v0.4.0 actually returned
// when its login was pointed at an unresolvable name and run without a
// browser on PATH (measured 2026-10-10, no portal involved). The rest are
// the library's and chromedp's messages as written in their source.
func TestClassifyLoginFailure(t *testing.T) {
	const wrap = "[AUTH_FAILED] authentication failed: authentication failed: "
	for _, c := range []struct {
		name string
		msg  string
		want loginFailure
	}{
		{"name not resolved", wrap + "page load error net::ERR_NAME_NOT_RESOLVED", loginFailedBeforeSubmit},
		{"chrome did not start", wrap + "chrome failed to start:\n/usr/bin/google-chrome: line 8: readlink: command not found\n", loginFailedBeforeSubmit},
		{"no browser installed", wrap + `exec: "google-chrome": executable file not found in $PATH`, loginFailedBeforeSubmit},
		{"connection refused", wrap + "page load error net::ERR_CONNECTION_REFUSED", loginFailedBeforeSubmit},
		{"connection timed out", wrap + "page load error net::ERR_CONNECTION_TIMED_OUT", loginFailedBeforeSubmit},
		{"address unreachable", wrap + "page load error net::ERR_ADDRESS_UNREACHABLE", loginFailedBeforeSubmit},
		{"devtools socket never came up", wrap + "websocket url timeout reached", loginFailedBeforeSubmit},

		{"rejected", wrap + "login failed: invalid credentials", loginRejected},

		// Not retried: none of these is positively before the submit, or
		// none is transient.
		{"timeout somewhere in the flow", wrap + "context deadline exceeded", loginFailedUnknown},
		{"cancelled", wrap + "context canceled", loginFailedUnknown},
		{"sign-in form changed", wrap + "could not find username input field", loginFailedUnknown},
		{"password field missing", wrap + "could not find password input field", loginFailedUnknown},
		{"cookies unreadable after the submit", wrap + "failed to get cookies: websocket: close 1006", loginFailedUnknown},
		{"certificate error", wrap + "page load error net::ERR_CERT_AUTHORITY_INVALID", loginFailedUnknown},
		{"unsafe port", wrap + "page load error net::ERR_UNSAFE_PORT", loginFailedUnknown},
		{"aborted", wrap + "page load error net::ERR_ABORTED", loginFailedUnknown},
		{"a longer code that only starts like a listed one", wrap + "page load error net::ERR_TIMED_OUT_AFTER_SUBMIT", loginFailedUnknown},
		{"a net error not from a page load", wrap + "net::ERR_CONNECTION_RESET", loginFailedUnknown},
		{"empty", "", loginFailedUnknown},
	} {
		if got := classifyLoginFailure(c.msg); got != c.want {
			t.Errorf("%s: classifyLoginFailure = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestFailureKindOfAnUntypedErrorNeverAllowsARetry(t *testing.T) {
	if got := failureKind(errors.New("page load error net::ERR_NAME_NOT_RESOLVED")); got != loginFailedUnknown {
		t.Errorf("failureKind = %d, want loginFailedUnknown", got)
	}
	wrapped := &loginError{kind: loginFailedBeforeSubmit, err: errors.New("x")}
	if got := failureKind(errors.Join(errors.New("outer"), wrapped)); got != loginFailedBeforeSubmit {
		t.Errorf("failureKind through a wrapper = %d", got)
	}
}

// --- the home-page read, against a local TLS server ------------------------

const signedInHome = `<!doctype html><html><body>
<ul id="students-list">
<li class="selected"><a href="javascript:switchStudent(111111);">Student One</a></li>
<li><a href="javascript:switchStudent(111112);">Student Two</a></li>
</ul>
<form id="switch_student_form" action="home.html" method="post"><input type="hidden" name="selected_student_id"/></form>
<div id="content-main"><h1>Grades and Attendance: One, Student</h1></div>
</body></html>`

const signInPage = `<!doctype html><html><head><title>Parent, Guardian, and Student Login</title></head><body>
<form id="LoginForm" action="/guardian/home.html" method="post">
<input type="text" name="account" id="fieldAccount"/><input type="password" name="pw" id="fieldPassword"/>
</form></body></html>`

// portal is a stand-in for the portal that records every request.
type portal struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD path"
	cookies  []string // Cookie header of each request
}

func newPortal(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *portal {
	t.Helper()
	p := &portal{}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests = append(p.requests, r.Method+" "+r.URL.Path)
		p.cookies = append(p.cookies, r.Header.Get("Cookie"))
		p.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *portal) probe(t *testing.T) probeFunc {
	t.Helper()
	host := strings.TrimPrefix(p.srv.URL, "https://")
	cfg := config{BaseURL: "https://" + host, Host: host}
	return realProbe(cfg, p.srv.Client().Transport)
}

func probeSession() *pslib.SessionExport {
	return &pslib.SessionExport{
		Cookies:   []*http.Cookie{{Name: "JSESSIONID", Value: testCookieValue}},
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func TestProbeReadsTheSignedInHomePageWithOneGET(t *testing.T) {
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/guardian/home.html" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(signedInHome))
	})
	selected, err := p.probe(t)(context.Background(), probeSession())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if selected != "111111" {
		t.Errorf("selected student = %q, want 111111", selected)
	}
	if len(p.requests) != 1 || p.requests[0] != "GET /guardian/home.html" {
		t.Errorf("requests = %v, want exactly one GET of the home page", p.requests)
	}
	if !strings.Contains(p.cookies[0], "JSESSIONID="+testCookieValue) {
		t.Errorf("the session cookie was not presented: %q", p.cookies[0])
	}
}

func TestProbeReportsNoSelectionWhenThePageMarksNone(t *testing.T) {
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Replace(signedInHome, ` class="selected"`, "", 1)))
	})
	selected, err := p.probe(t)(context.Background(), probeSession())
	if err != nil || selected != "" {
		t.Errorf("probe = (%q, %v), want (\"\", nil)", selected, err)
	}
}

func TestProbeReportsTheSignInPageAsARejectedSession(t *testing.T) {
	t.Run("200 with the sign-in form", func(t *testing.T) {
		p := newPortal(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(signInPage)) })
		_, err := p.probe(t)(context.Background(), probeSession())
		if !pslib.IsSessionExpired(err) {
			t.Errorf("err = %v, want a session-expired error", err)
		}
	})
	t.Run("redirect to the public area", func(t *testing.T) {
		p := newPortal(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/public/") {
				_, _ = w.Write([]byte(signInPage))
				return
			}
			http.Redirect(w, r, "/public/home.html", http.StatusFound)
		})
		_, err := p.probe(t)(context.Background(), probeSession())
		if !pslib.IsSessionExpired(err) {
			t.Errorf("err = %v, want a session-expired error", err)
		}
		for _, req := range p.requests {
			if !strings.HasPrefix(req, "GET ") {
				t.Errorf("non-GET request %q", req)
			}
		}
	})
	t.Run("401", func(t *testing.T) {
		p := newPortal(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
		_, err := p.probe(t)(context.Background(), probeSession())
		if !pslib.IsSessionExpired(err) {
			t.Errorf("err = %v, want a session-expired error", err)
		}
	})
}

// A page that is neither the home page nor the sign-in page, and a server
// error, leave the question unanswered: an error, but not "rejected".
func TestProbeDoesNotCallAnUnreadablePageARejection(t *testing.T) {
	for name, handler := range map[string]func(http.ResponseWriter, *http.Request){
		"maintenance notice": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html><body>Down for maintenance</body></html>"))
		},
		"503": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
	} {
		t.Run(name, func(t *testing.T) {
			p := newPortal(t, handler)
			_, err := p.probe(t)(context.Background(), probeSession())
			if err == nil {
				t.Fatal("probe succeeded")
			}
			if pslib.IsSessionExpired(err) {
				t.Errorf("err = %v reads as a rejected session", err)
			}
		})
	}
}

// An expired session document is not presented at all, and the client holds
// no credentials with which the library could log in again.
func TestProbeDoesNotPresentAnExpiredSession(t *testing.T) {
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(signedInHome)) })
	s := probeSession()
	s.ExpiresAt = time.Now().Add(-time.Minute)
	_, err := p.probe(t)(context.Background(), s)
	if !pslib.IsSessionExpired(err) {
		t.Errorf("err = %v, want a session-expired error", err)
	}
	if len(p.requests) != 0 {
		t.Errorf("requests = %v, want none", p.requests)
	}
}

// The probe's HTTP client cannot write to the portal or reach another host,
// whatever the code holding it asks for.
func TestProbeTransportIsReadOnlyAndPinned(t *testing.T) {
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(signedInHome)) })
	host := strings.TrimPrefix(p.srv.URL, "https://")

	// Build the client exactly as realProbe does.
	cfg := config{BaseURL: "https://" + host, Host: host}
	client := probeHTTPClient(cfg, p.srv.Client().Transport)

	for _, body := range []string{"selected_student_id=111111", "account=u&pw=p"} {
		resp, err := client.Post(cfg.BaseURL+"/guardian/home.html", "application/x-www-form-urlencoded", strings.NewReader(body))
		if err == nil {
			resp.Body.Close()
			t.Fatalf("a POST of %q went through", body)
		}
		if !errors.Is(err, connector.ErrWriteBlocked) {
			t.Errorf("POST error = %v, want ErrWriteBlocked", err)
		}
	}
	if resp, err := client.Get("https://elsewhere.example.org/guardian/home.html"); err == nil {
		resp.Body.Close()
		t.Fatal("a request to another host went through")
	}
	if len(p.requests) != 0 {
		t.Errorf("requests reached the server: %v", p.requests)
	}
}
