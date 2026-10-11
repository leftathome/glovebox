package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connectors/powerschool"
	pslib "github.com/leftathome/powerschool-go"
)

// loginTimeout bounds one browser login, browser start included. The
// library's own default is 60s; a cold container needs some room.
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
		opts := &pslib.AuthOptions{Timeout: loginTimeout, NoSandbox: cfg.ChromeNoSandbox}
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
