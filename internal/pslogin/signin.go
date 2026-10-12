package pslogin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/leftathome/glovebox/connectors/powerschool"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// SignInPath is the page the library's browser login opens first
// (powerschool-go internal/browser: BaseURL + "/public/").
const SignInPath = "/public/"

// publicPrefix is the portal's signed-out area. The sign-in page lives
// under it, wherever redirects end.
const publicPrefix = "/public/"

// MaxSignInBytes bounds how much of the sign-in page is read.
const MaxSignInBytes = 2 << 20

// MaxSignInRedirects bounds the redirects followed on the way to the
// sign-in page.
const MaxSignInRedirects = 5

// Reasons a sign-in check fails. A closed list: these are log values.
const (
	reasonRedirectOffHost   = "signin_redirect_off_host"
	reasonRedirectLoop      = "signin_redirect_loop"
	reasonGatewayRefused    = "gateway_session_refused"
	reasonNotUnderPublic    = "final_page_not_under_public"
	reasonNoPasswordForm    = "no_password_form"
	reasonNoUsernameField   = "no_username_field"
	reasonFormPostsOffHost  = "form_posts_off_host"
	reasonActionUnparseable = "form_action_unparseable"
	reasonUnparseable       = "unparseable"
)

// Sentinels the redirect policy returns; SignInCheck turns them into
// reasons.
var (
	errRedirectOffHost = errors.New("redirect leaves the configured host")
	errRedirectLoop    = errors.New("too many redirects")
)

// SignInHTTPClient builds the client for ONE sign-in check.
//
// The portal may sit behind an access gateway that hands every new client a
// session cookie and redirects it back (/public/ -> /my.policy -> /public/
// on the reference instance). A client that does not return those cookies
// is sent to the gateway's error page instead and never sees the sign-in
// form. So this client has a cookie jar -- and that jar:
//
//   - is created here, empty, and belongs to this one client; it is not
//     seeded with anything, least of all a session;
//   - is used for nothing but the sign-in check (the home-page reads use
//     another client; see ProbeHTTPClient);
//   - is never logged and is garbage once the check returns.
//
// Everything still goes through the connector's transport: pinned to the
// portal host, https only, GET and HEAD only. Redirects are followed only
// to that same host over https, at most MaxSignInRedirects of them.
func SignInHTTPClient(cfg Config, base http.RoundTripper) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout: ProbeTimeout,
		Jar:     jar,
		Transport: &powerschool.Transport{
			Host: strings.ToLower(cfg.Host),
			Base: base,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, cfg.Host) {
				return errRedirectOffHost
			}
			if req.Method != http.MethodGet && req.Method != http.MethodHead {
				return errRedirectOffHost
			}
			if len(via) > MaxSignInRedirects {
				return errRedirectLoop
			}
			return nil
		},
	}, nil
}

// SignInCheck returns the production sign-in check. Before any browser
// is started it fetches the sign-in page with a plain GET and requires:
//
//   - every redirect on the way stayed on the configured host over https,
//     and there were at most MaxSignInRedirects;
//   - the page finally served is under /public/ on that host. Ending at the
//     gateway's own pages instead is reported as gateway_session_refused;
//   - the page has a form holding a password field and a text field (the
//     account name), and every form with a password field posts to that
//     same host over https.
//
// Otherwise the password would be typed into a page that is not the
// portal's sign-in form -- a maintenance page, an identity provider the
// portal was moved behind, a hijacked redirect. The error then wraps
// ErrSignInUnrecognised and no browser is started.
//
// What this does not close: the browser fetches the page again, and what it
// gets could differ from what was checked here. Closing that needs the
// check inside the browser session, which is the library's to add.
func SignInCheck(cfg Config, base http.RoundTripper) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
		defer cancel()

		client, err := SignInHTTPClient(cfg, base)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+SignInPath, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		resp, err := client.Do(req)
		if err != nil {
			switch {
			case errors.Is(err, errRedirectOffHost), errors.Is(err, powerschool.ErrHostBlocked):
				return &SignInError{Reason: reasonRedirectOffHost}
			case errors.Is(err, errRedirectLoop):
				return &SignInError{Reason: reasonRedirectLoop}
			}
			return fmt.Errorf("fetch sign-in page: %w", stripURL(err))
		}
		defer resp.Body.Close()

		final := resp.Request.URL
		if final.Scheme != "https" || !strings.EqualFold(final.Host, cfg.Host) {
			return &SignInError{Reason: reasonRedirectOffHost}
		}
		if psportal.IsGatewayPath(final.Path) {
			return &SignInError{Reason: reasonGatewayRefused}
		}
		if resp.StatusCode >= 500 {
			return &pslib.HTTPStatusError{Op: "GET " + SignInPath, StatusCode: resp.StatusCode}
		}
		if resp.StatusCode != http.StatusOK {
			return &SignInError{Reason: fmt.Sprintf("http_%d", resp.StatusCode)}
		}
		if !strings.HasPrefix(final.Path, publicPrefix) {
			return &SignInError{Reason: reasonNotUnderPublic}
		}
		doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, MaxSignInBytes))
		if err != nil {
			return &SignInError{Reason: reasonUnparseable}
		}
		return CheckSignInForms(doc, final, cfg.Host)
	}
}

// textLikeInput matches an input a person types an account name into: an
// explicit text or email type, or no type at all (which is text).
const textLikeInput = `input[type="text"], input[type="email"], input:not([type])`

// CheckSignInForms applies the form rule of SignInCheck to a parsed
// page that was served from pageURL.
//
// It is deliberately about shape, not about this portal's field names: a
// form to sign in with has somewhere to type a name and somewhere to type a
// password, and sends them back to the host it came from. Hidden inputs and
// further password fields (the reference page has several) do not matter.
func CheckSignInForms(doc *goquery.Document, pageURL *url.URL, host string) error {
	forms := doc.Find("form").FilterFunction(func(_ int, f *goquery.Selection) bool {
		return f.Find(`input[type="password"]`).Length() > 0
	})
	if forms.Length() == 0 {
		return &SignInError{Reason: reasonNoPasswordForm}
	}
	var problem error
	signInForms := 0
	forms.EachWithBreak(func(_ int, f *goquery.Selection) bool {
		// A missing or empty action posts to the page's own URL.
		target := pageURL
		if action, ok := f.Attr("action"); ok && strings.TrimSpace(action) != "" {
			ref, err := url.Parse(strings.TrimSpace(action))
			if err != nil {
				problem = &SignInError{Reason: reasonActionUnparseable}
				return false
			}
			target = pageURL.ResolveReference(ref)
		}
		// Every form that takes a password must send it home, whether or
		// not it is the one the browser will fill in.
		if target.Scheme != "https" || !strings.EqualFold(target.Host, host) {
			problem = &SignInError{Reason: reasonFormPostsOffHost}
			return false
		}
		if f.Find(textLikeInput).Length() > 0 {
			signInForms++
		}
		return true
	})
	if problem != nil {
		return problem
	}
	if signInForms == 0 {
		return &SignInError{Reason: reasonNoUsernameField}
	}
	return nil
}

// stripURL drops the URL from an http.Client error, keeping its cause.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// ErrSignInUnrecognised is returned (wrapped) by a sign-in check when the
// page it fetched is not one to type the account's password into.
var ErrSignInUnrecognised = errors.New("the portal's sign-in page was not recognised")

// SignInError is ErrSignInUnrecognised with a reason from a fixed list.
type SignInError struct{ Reason string }

func (e *SignInError) Error() string { return ErrSignInUnrecognised.Error() + ": " + e.Reason }
func (e *SignInError) Unwrap() error { return ErrSignInUnrecognised }

// SignInReason is the short, closed-vocabulary reason carried by a sign-in
// check error ("unrecognised" when there is none).
func SignInReason(err error) string {
	var se *SignInError
	if errors.As(err, &se) {
		return se.Reason
	}
	return "unrecognised"
}
