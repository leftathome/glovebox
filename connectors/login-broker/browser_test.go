package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/pslogin"
)

// TestRealLoginAgainstAnUnresolvableHost starts a real browser through the
// real library, inside this binary's image, and points it at a name that
// cannot resolve (RFC 6761 reserves .invalid): it reaches no portal and
// submits nothing. It pins what the fakes cannot -- that Chromium starts in
// the image as the chart runs it, and that the error the library returns
// for "the sign-in page could not be reached" is the one classified as
// "nothing was submitted", the only kind the broker ever retries.
//
// Skipped unless LOGIN_BROKER_BROWSER_TEST=1 and a browser is on PATH; the
// unit-test job's image has none. scripts/test-login-broker-image.sh runs
// it inside the image.
func TestRealLoginAgainstAnUnresolvableHost(t *testing.T) {
	if os.Getenv("LOGIN_BROKER_BROWSER_TEST") != "1" {
		t.Skip("set LOGIN_BROKER_BROWSER_TEST=1 to start a real browser")
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

	cfg := pslogin.Config{
		BaseURL:         "https://portal.invalid",
		Host:            "portal.invalid",
		Username:        testUsername,
		Password:        testPassword,
		ChromeNoSandbox: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	session, err := pslogin.Login(cfg)(ctx)
	if err == nil {
		t.Fatalf("a login to an unresolvable host succeeded with %d cookies", len(session.Cookies))
	}
	if got := pslogin.KindOf(err); got != pslogin.FailedBeforeSubmit {
		t.Errorf("KindOf = %d, want FailedBeforeSubmit", got)
	}
	if msg := err.Error(); strings.Contains(msg, testPassword) || strings.Contains(msg, testUsername) {
		t.Errorf("the library's error text carried a credential")
	}
}
