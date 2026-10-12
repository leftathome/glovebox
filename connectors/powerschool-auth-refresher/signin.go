package main

import (
	"context"
	"net/http"

	"github.com/leftathome/glovebox/internal/pslogin"
)

// The sign-in page check lives in internal/pslogin, shared with the login
// broker. These are this binary's names for it.

const (
	signInPath         = pslogin.SignInPath
	maxSignInBytes     = pslogin.MaxSignInBytes
	maxSignInRedirects = pslogin.MaxSignInRedirects
)

// errSignInUnrecognised is returned (wrapped) by a sign-in check when the
// page it fetched is not one to type the account's password into.
var errSignInUnrecognised = pslogin.ErrSignInUnrecognised

// signInError is errSignInUnrecognised with a reason from a fixed list.
type signInError = pslogin.SignInError

// signInReason is the short, closed-vocabulary reason carried by a sign-in
// check error.
func signInReason(err error) string { return pslogin.SignInReason(err) }

// signInHTTPClient builds the client for ONE sign-in check.
func signInHTTPClient(cfg config, base http.RoundTripper) (*http.Client, error) {
	return pslogin.SignInHTTPClient(cfg.portal(), base)
}

// realSignInCheck returns the production sign-in check: before any browser
// is started, the sign-in page is fetched with a plain GET and must be the
// portal's own password form on the configured host.
func realSignInCheck(cfg config, base http.RoundTripper) func(ctx context.Context) error {
	return pslogin.SignInCheck(cfg.portal(), base)
}
