package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/leftathome/glovebox/connectors/powerschool"
	pslib "github.com/leftathome/powerschool-go"
)

// loginTimeout bounds one browser login, browser start included. The
// library's own default is 60s; a cold container needs some room. It is
// also part of the blackout headroom (loginHeadroom).
const loginTimeout = 90 * time.Second

// probeTimeout bounds one home-page read.
const probeTimeout = 30 * time.Second

// realLogin returns the production loginFunc: one powerschool-go browser
// login (a local headless Chromium driven through chromedp; the library has
// no remote-browser option, which is why this image bundles a browser where
// schoology-auth-refresher uses Browserless).
//
// The library's debug logging is never enabled: it prints the sign-in
// form's HTML.
func realLogin(cfg config) loginFunc {
	return func(ctx context.Context) (*pslib.SessionExport, error) {
		client, err := pslib.NewClient(cfg.BaseURL,
			pslib.WithCredentials(cfg.Username, cfg.Password),
			pslib.WithLogLevel(pslib.LogLevelWarn),
		)
		if err != nil {
			return nil, &loginError{kind: loginFailedUnknown, err: errors.New("could not construct the portal client")}
		}

		// The browser profile holds the session cookies once the login
		// succeeds. It gets a directory of its own (under TMPDIR, which the
		// chart backs with memory) that is removed on every way out of this
		// function, instead of being left to the browser to clean up.
		profile, err := os.MkdirTemp("", "ps-login-profile-")
		if err != nil {
			return nil, &loginError{kind: loginFailedUnknown, err: errors.New("could not create a browser profile directory")}
		}
		defer os.RemoveAll(profile)

		opts := &pslib.AuthOptions{Timeout: loginTimeout, NoSandbox: cfg.ChromeNoSandbox, UserDataDir: profile}
		if err := client.AuthenticateWithOptions(ctx, opts); err != nil {
			return nil, &loginError{kind: classifyLoginFailure(err.Error()), err: err}
		}
		session := client.ExportSession()
		if session == nil {
			return nil, &loginError{kind: loginFailedUnknown, err: errors.New("the library reported success but exported no session")}
		}
		return session, nil
	}
}

// browserStartMarkers identify a browser that never came up. They are
// chromedp's and os/exec's own messages; no page had been requested.
var browserStartMarkers = []string{
	"chrome failed to start",
	"executable file not found",
	"fork/exec",
	"websocket url timeout reached",
}

// preSubmitNetErrors are the Chromium network errors that mean the sign-in
// page itself could not be fetched: name resolution and connection
// establishment. Certificate errors, HTTP errors and aborts are left out on
// purpose -- none is "try again in ten seconds".
var preSubmitNetErrors = []string{
	"ERR_NAME_NOT_RESOLVED",
	"ERR_NAME_RESOLUTION_FAILED",
	"ERR_DNS_TIMED_OUT",
	"ERR_CONNECTION_REFUSED",
	"ERR_CONNECTION_TIMED_OUT",
	"ERR_CONNECTION_RESET",
	"ERR_CONNECTION_CLOSED",
	"ERR_ADDRESS_UNREACHABLE",
	"ERR_INTERNET_DISCONNECTED",
	"ERR_NETWORK_CHANGED",
	"ERR_TIMED_OUT",
}

// pageLoadErrorPrefix is how chromedp reports a failed Navigate action. The
// library's login issues exactly one, for the sign-in page, as its first
// step; the navigation that follows the submit is not a Navigate action and
// cannot produce this text. So an error carrying it happened before any
// field was filled in.
const pageLoadErrorPrefix = "page load error net::"

// rejectedMarker is the library's message when the portal answers the
// submitted form with its invalid-credentials notice.
const rejectedMarker = "login failed: invalid credentials"

// classifyLoginFailure places a library login error. The library returns
// one undifferentiated error for every failure, so its text is all there is
// to go on; the match is therefore an allowlist. Whatever is not positively
// recognised as "before the submit" is loginFailedUnknown and is never
// retried -- a timeout, a missing form field, an error this code has not
// seen. Getting that wrong in the other direction is what locks an account.
func classifyLoginFailure(msg string) loginFailure {
	if strings.Contains(msg, rejectedMarker) {
		return loginRejected
	}
	for _, m := range browserStartMarkers {
		if strings.Contains(msg, m) {
			return loginFailedBeforeSubmit
		}
	}
	if i := strings.Index(msg, pageLoadErrorPrefix); i >= 0 {
		code := msg[i+len(pageLoadErrorPrefix):]
		for _, n := range preSubmitNetErrors {
			if code == n || strings.HasPrefix(code, n+" ") || strings.HasPrefix(code, n+"\n") {
				return loginFailedBeforeSubmit
			}
		}
	}
	return loginFailedUnknown
}

// probeHTTPClient is the only HTTP client this binary points at the portal.
// base replaces the wrapped transport in tests; nil is the default one.
func probeHTTPClient(cfg config, base http.RoundTripper) *http.Client {
	client := powerschool.NewHTTPClient(cfg.Host, false, nil, probeTimeout)
	if base != nil {
		if t, ok := client.Transport.(*powerschool.Transport); ok {
			t.Base = base
		}
	}
	return client
}

// realProbe returns the production probeFunc. It builds a client that holds
// no credentials (so the library cannot log in again) on the connector's
// own transport, pinned to the portal host with no POST allowed at all:
// neither of the connector's two exceptions is enabled here.
func realProbe(cfg config, base http.RoundTripper) probeFunc {
	return func(ctx context.Context, session *pslib.SessionExport) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()

		lib, err := pslib.NewClient(cfg.BaseURL,
			pslib.WithHTTPClient(probeHTTPClient(cfg, base)),
			pslib.WithHTTPPageFetch(),
			pslib.WithSession(session.Cookies, session.ExpiresAt),
			pslib.WithLogLevel(pslib.LogLevelWarn),
		)
		if err != nil {
			return "", err
		}
		// GetHomePage fails unless the page carries the student switcher,
		// which only a signed-in guardian page does, and reports the
		// sign-in page (redirect or 200) as a session error.
		page, err := lib.GetHomePage(ctx)
		if err != nil {
			return "", err
		}
		if selected := page.SelectedStudent(); selected != nil {
			return selected.ID, nil
		}
		return "", nil
	}
}

// signInPath is the page the library's browser login opens first
// (powerschool-go internal/browser: BaseURL + "/public/").
const signInPath = "/public/"

// maxSignInBytes bounds how much of the sign-in page is read.
const maxSignInBytes = 2 << 20

// realSignInCheck returns the production sign-in check. Before any browser
// is started it fetches the sign-in page with a plain GET, through the same
// pinned read-only transport as the probe, and requires:
//
//   - the response, after redirects, came from the configured host over
//     https (the transport refuses any other hop, so a redirect elsewhere
//     fails here);
//   - the page has a form with a password field, and every such form posts
//     to that same host over https.
//
// Otherwise the password would be typed into a page that is not the
// portal's sign-in form -- a maintenance page, an identity provider the
// portal was moved behind, a hijacked redirect. The error then wraps
// errSignInUnrecognised and no browser is started.
//
// What this does not close: the browser fetches the page again, and what it
// gets could differ from what was checked here. Closing that needs the
// check inside the browser session, which is the library's to add.
func realSignInCheck(cfg config, base http.RoundTripper) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+signInPath, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		resp, err := probeHTTPClient(cfg, base).Do(req)
		if err != nil {
			if errors.Is(err, powerschool.ErrHostBlocked) {
				return &signInError{reason: "redirected_off_host"}
			}
			return fmt.Errorf("fetch sign-in page: %w", stripURL(err))
		}
		defer resp.Body.Close()

		final := resp.Request.URL
		if final.Scheme != "https" || !strings.EqualFold(final.Host, cfg.Host) {
			return &signInError{reason: "redirected_off_host"}
		}
		if resp.StatusCode >= 500 {
			return &pslib.HTTPStatusError{Op: "GET " + signInPath, StatusCode: resp.StatusCode}
		}
		if resp.StatusCode != http.StatusOK {
			return &signInError{reason: fmt.Sprintf("http_%d", resp.StatusCode)}
		}
		doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxSignInBytes))
		if err != nil {
			return &signInError{reason: "unparseable"}
		}
		return checkSignInForms(doc, final, cfg.Host)
	}
}

// checkSignInForms applies the form rule of realSignInCheck to a parsed
// page that was served from pageURL.
func checkSignInForms(doc *goquery.Document, pageURL *url.URL, host string) error {
	forms := doc.Find("form").FilterFunction(func(_ int, f *goquery.Selection) bool {
		return f.Find(`input[type="password"]`).Length() > 0
	})
	if forms.Length() == 0 {
		return &signInError{reason: "no_password_form"}
	}
	var problem error
	forms.EachWithBreak(func(_ int, f *goquery.Selection) bool {
		// A missing or empty action posts to the page's own URL.
		target := pageURL
		if action, ok := f.Attr("action"); ok && strings.TrimSpace(action) != "" {
			ref, err := url.Parse(strings.TrimSpace(action))
			if err != nil {
				problem = &signInError{reason: "form_action_unparseable"}
				return false
			}
			target = pageURL.ResolveReference(ref)
		}
		if target.Scheme != "https" || !strings.EqualFold(target.Host, host) {
			problem = &signInError{reason: "form_posts_off_host"}
			return false
		}
		return true
	})
	return problem
}

// stripURL drops the URL from an http.Client error, keeping its cause.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
