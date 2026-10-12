package main

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

// signInPath is the page the library's browser login opens first
// (powerschool-go internal/browser: BaseURL + "/public/").
const signInPath = "/public/"

// publicPrefix is the portal's signed-out area. The sign-in page lives
// under it, wherever redirects end.
const publicPrefix = "/public/"

// maxSignInBytes bounds how much of the sign-in page is read.
const maxSignInBytes = 2 << 20

// maxSignInRedirects bounds the redirects followed on the way to the
// sign-in page.
const maxSignInRedirects = 5

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

// Sentinels the redirect policy returns; realSignInCheck turns them into
// reasons.
var (
	errRedirectOffHost = errors.New("redirect leaves the configured host")
	errRedirectLoop    = errors.New("too many redirects")
)

// signInHTTPClient builds the client for ONE sign-in check.
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
//     another client; see probeHTTPClient);
//   - is never logged and is garbage once the check returns.
//
// Everything still goes through the connector's transport: pinned to the
// portal host, https only, GET and HEAD only. Redirects are followed only
// to that same host over https, at most maxSignInRedirects of them.
func signInHTTPClient(cfg config, base http.RoundTripper) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout: probeTimeout,
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
			if len(via) > maxSignInRedirects {
				return errRedirectLoop
			}
			return nil
		},
	}, nil
}

// realSignInCheck returns the production sign-in check. Before any browser
// is started it fetches the sign-in page with a plain GET and requires:
//
//   - every redirect on the way stayed on the configured host over https,
//     and there were at most maxSignInRedirects;
//   - the page finally served is under /public/ on that host. Ending at the
//     gateway's own pages instead is reported as gateway_session_refused;
//   - the page has a form holding a password field and a text field (the
//     account name), and every form with a password field posts to that
//     same host over https.
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

		client, err := signInHTTPClient(cfg, base)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+signInPath, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		resp, err := client.Do(req)
		if err != nil {
			switch {
			case errors.Is(err, errRedirectOffHost), errors.Is(err, powerschool.ErrHostBlocked):
				return &signInError{reason: reasonRedirectOffHost}
			case errors.Is(err, errRedirectLoop):
				return &signInError{reason: reasonRedirectLoop}
			}
			return fmt.Errorf("fetch sign-in page: %w", stripURL(err))
		}
		defer resp.Body.Close()

		final := resp.Request.URL
		if final.Scheme != "https" || !strings.EqualFold(final.Host, cfg.Host) {
			return &signInError{reason: reasonRedirectOffHost}
		}
		if psportal.IsGatewayPath(final.Path) {
			return &signInError{reason: reasonGatewayRefused}
		}
		if resp.StatusCode >= 500 {
			return &pslib.HTTPStatusError{Op: "GET " + signInPath, StatusCode: resp.StatusCode}
		}
		if resp.StatusCode != http.StatusOK {
			return &signInError{reason: fmt.Sprintf("http_%d", resp.StatusCode)}
		}
		if !strings.HasPrefix(final.Path, publicPrefix) {
			return &signInError{reason: reasonNotUnderPublic}
		}
		doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxSignInBytes))
		if err != nil {
			return &signInError{reason: reasonUnparseable}
		}
		return checkSignInForms(doc, final, cfg.Host)
	}
}

// textLikeInput matches an input a person types an account name into: an
// explicit text or email type, or no type at all (which is text).
const textLikeInput = `input[type="text"], input[type="email"], input:not([type])`

// checkSignInForms applies the form rule of realSignInCheck to a parsed
// page that was served from pageURL.
//
// It is deliberately about shape, not about this portal's field names: a
// form to sign in with has somewhere to type a name and somewhere to type a
// password, and sends them back to the host it came from. Hidden inputs and
// further password fields (the reference page has several) do not matter.
func checkSignInForms(doc *goquery.Document, pageURL *url.URL, host string) error {
	forms := doc.Find("form").FilterFunction(func(_ int, f *goquery.Selection) bool {
		return f.Find(`input[type="password"]`).Length() > 0
	})
	if forms.Length() == 0 {
		return &signInError{reason: reasonNoPasswordForm}
	}
	var problem error
	signInForms := 0
	forms.EachWithBreak(func(_ int, f *goquery.Selection) bool {
		// A missing or empty action posts to the page's own URL.
		target := pageURL
		if action, ok := f.Attr("action"); ok && strings.TrimSpace(action) != "" {
			ref, err := url.Parse(strings.TrimSpace(action))
			if err != nil {
				problem = &signInError{reason: reasonActionUnparseable}
				return false
			}
			target = pageURL.ResolveReference(ref)
		}
		// Every form that takes a password must send it home, whether or
		// not it is the one the browser will fill in.
		if target.Scheme != "https" || !strings.EqualFold(target.Host, host) {
			problem = &signInError{reason: reasonFormPostsOffHost}
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
		return &signInError{reason: reasonNoUsernameField}
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
