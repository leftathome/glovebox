package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
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
	// As main does, first: the browser must still start under a parent
	// that has made itself non-dumpable.
	if err := hardenProcess(); err != nil {
		t.Fatalf("hardenProcess: %v", err)
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

// TestSameUserCannotReadAHardenedProcess shows what hardenProcess buys, in
// the place it matters: as the image's unprivileged user, a child process
// of the same user -- which is what the browser is -- cannot read the
// parent's start-up environment or its memory once the parent has made
// itself non-dumpable. It can before.
//
// Skipped unless LOGIN_BROKER_BROWSER_TEST=1 (the image test sets it) and
// the test is not running as root, which may read anything.
func TestSameUserCannotReadAHardenedProcess(t *testing.T) {
	if os.Getenv("LOGIN_BROKER_BROWSER_TEST") != "1" {
		t.Skip("set LOGIN_BROKER_BROWSER_TEST=1; meant to run inside the image")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root, which is not refused anything in /proc")
	}
	self := "/proc/" + strconv.Itoa(os.Getpid())
	read := func(name string) error {
		// A separate process of the same user, as a browser would be.
		return exec.Command("/bin/sh", "-c", "head -c 1 "+self+"/"+name+" >/dev/null").Run()
	}
	if err := read("environ"); err != nil {
		t.Fatalf("before hardening, a same-user process could not read environ either (%v): the test proves nothing here", err)
	}
	if err := hardenProcess(); err != nil {
		t.Fatalf("hardenProcess: %v", err)
	}
	for _, name := range []string{"environ", "mem", "maps"} {
		if err := read(name); err == nil {
			t.Errorf("a same-user process can still read %s/%s after hardenProcess", self, name)
		}
	}
}
