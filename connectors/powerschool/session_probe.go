package powerschool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// SessionState is what one look at the portal says about a session.
type SessionState int

const (
	// SessionUnknown: the question was not answered (network failure, a
	// server error, a page that is neither the home page nor a sign-in
	// page). Nothing may be concluded and nothing is changed.
	SessionUnknown SessionState = iota
	// SessionLive: the portal served the signed-in home page.
	SessionLive
	// SessionDead: the portal answered with its own sign-in page, or the
	// access gateway in front of it sent the session to its "no session"
	// page. The session is no longer accepted. This is routine -- the portal ends an idle session
	// in well under an hour -- and it is NOT a page that failed to parse.
	SessionDead
)

// String is the log form.
func (s SessionState) String() string {
	switch s {
	case SessionLive:
		return "live"
	case SessionDead:
		return "dead"
	}
	return "unknown"
}

// SessionVerdict is a SessionState with the one-word reason behind it. The
// reason comes from a closed list; it never quotes the page or the URL.
type SessionVerdict struct {
	State  SessionState
	Reason string
}

// Reasons of a SessionVerdict.
const (
	ReasonSignedIn        = "signed_in"
	ReasonSignInPage      = "sign_in_page"
	ReasonGatewayPage     = "gateway_page"
	ReasonGatewayPolicy   = "gateway_policy_page"
	ReasonNetwork         = "network"
	ReasonServerError     = "server_error"
	ReasonUnexpectedPage  = "unrecognised_page"
	ReasonNoSession       = "no_session"
	ReasonExpiredDocument = "expired_document"
)

// ProbeFunc asks the portal, with one cheap GET, whether the session a
// client was built with is still accepted.
type ProbeFunc func(ctx context.Context) SessionVerdict

// homePath is the page every signed-in guardian session can read.
const homePath = "/guardian/home.html"

// maxProbeBytes bounds how much of the answer is read.
const maxProbeBytes = 4 << 20

// NewSessionProbe returns the production ProbeFunc: one GET of the home
// page with the session's cookies, through httpClient -- which must be the
// client from NewHTTPClient, so the request is pinned to the portal host,
// cannot be anything but a read, and carries a renewed gateway cookie
// across the gateway's redirect.
//
// The answer is classified by its status, where the redirects ended and
// what kind of page came back, never by whether a parser could make sense
// of it. Dead is a strong claim -- it leads to a login request -- so only
// two answers earn it:
//
//	live     200, and the page has the student switcher (only a signed-in
//	         guardian page does)
//	dead     200 under /public/ (the portal's signed-out area) AND the page
//	         has a password form: the portal's own sign-in page, positively
//	         identified; or
//	         200 at /my.logout.php3: the access gateway refusing the
//	         session whose cookies this request carried
//	unknown  everything else: a network error; any status but 200 (a 401
//	         or 403 says "not now", it does not say why); the gateway's
//	         policy endpoint; a page under /public/ without a password
//	         form; any page that is neither
//
// The status is looked at before the path: an error answer proves nothing
// about the session wherever it was served from.
//
// It does not log, and it does not use the library: the library reports a
// gateway page as a parse error, which is how a dead session used to be
// mistaken for a broken page.
func NewSessionProbe(httpClient *http.Client, baseURL string, session *pslib.SessionExport) ProbeFunc {
	return func(ctx context.Context) SessionVerdict {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+homePath, nil)
		if err != nil {
			return SessionVerdict{SessionUnknown, ReasonNetwork}
		}
		for _, c := range session.Cookies {
			if c != nil && c.Name != "" {
				req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
			}
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		resp, err := httpClient.Do(req)
		if err != nil {
			return SessionVerdict{SessionUnknown, ReasonNetwork}
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBytes))
		if err != nil {
			return SessionVerdict{SessionUnknown, ReasonNetwork}
		}
		finalPath := ""
		if resp.Request != nil && resp.Request.URL != nil {
			finalPath = resp.Request.URL.Path
		}
		return ClassifyHomeResponse(finalPath, resp.StatusCode, string(body))
	}
}

// ClassifyHomeResponse is the rule of NewSessionProbe, on its own so it can
// be tested without a server: finalPath is the path the redirects ended at.
func ClassifyHomeResponse(finalPath string, status int, body string) SessionVerdict {
	switch {
	case status >= 500:
		return SessionVerdict{SessionUnknown, ReasonServerError}
	case status != http.StatusOK:
		return SessionVerdict{SessionUnknown, fmt.Sprintf("http_%d", status)}
	}
	switch {
	case finalPath == psportal.GatewayLogoutPath:
		return SessionVerdict{SessionDead, ReasonGatewayPage}
	case psportal.IsGatewayPath(finalPath):
		return SessionVerdict{SessionUnknown, ReasonGatewayPolicy}
	case strings.HasPrefix(finalPath, "/public/"):
		if hasPasswordForm(body) {
			return SessionVerdict{SessionDead, ReasonSignInPage}
		}
		return SessionVerdict{SessionUnknown, ReasonUnexpectedPage}
	}
	// The switcher is on every signed-in guardian page and on no other.
	if strings.Contains(body, "switchStudent(") {
		return SessionVerdict{SessionLive, ReasonSignedIn}
	}
	return SessionVerdict{SessionUnknown, ReasonUnexpectedPage}
}

// hasPasswordForm reports whether html has a form with a password input.
func hasPasswordForm(html string) bool {
	if !strings.Contains(strings.ToLower(html), "password") {
		return false
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return false
	}
	return doc.Find(`form input[type="password"]`).Length() > 0
}
