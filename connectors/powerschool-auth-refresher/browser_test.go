package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRealLoginAgainstAnUnresolvableHost starts a real browser through the
// real library and points it at a name that cannot resolve (RFC 6761
// reserves .invalid), so it reaches no portal and submits nothing. It pins
// the one thing the fakes cannot: that the error the library actually
// returns for "the sign-in page could not be reached" is the one
// classifyLoginFailure treats as safe to retry.
//
// Skipped unless POWERSCHOOL_REFRESHER_BROWSER_TEST=1 and a browser is on
// PATH; the unit-test job's image has none.
func TestRealLoginAgainstAnUnresolvableHost(t *testing.T) {
	if os.Getenv("POWERSCHOOL_REFRESHER_BROWSER_TEST") != "1" {
		t.Skip("set POWERSCHOOL_REFRESHER_BROWSER_TEST=1 to start a real browser")
	}
	found := false
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "headless-shell", "chrome"} {
		if _, err := exec.LookPath(name); err == nil {
			found = true
			break
		}
	}
	if !found {
		t.Skip("no Chromium on PATH")
	}

	cfg := config{
		BaseURL:         "https://portal.invalid",
		Host:            "portal.invalid",
		Username:        testUsername,
		Password:        testPassword,
		ChromeNoSandbox: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	session, err := realLogin(cfg)(ctx)
	if err == nil {
		t.Fatalf("a login to an unresolvable host succeeded with %d cookies", len(session.Cookies))
	}
	if got := failureKind(err); got != loginFailedBeforeSubmit {
		t.Errorf("failureKind = %d, want loginFailedBeforeSubmit; error: %s", got, cfg.redact(err.Error()))
	}
	if msg := err.Error(); strings.Contains(msg, testPassword) || strings.Contains(msg, testUsername) {
		t.Errorf("the library's error text carried a credential")
	}
}
