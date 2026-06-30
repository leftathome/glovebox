package integrationtest

import (
	"os"
	"strings"
	"testing"
)

// RequireIntegration skips unless GLOVEBOX_INTEGRATION=1, so the
// build-tagged suite makes no live calls during an ordinary
// `go test -tags integration ./...` run.
func RequireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("GLOVEBOX_INTEGRATION") != "1" {
		t.Skip("integration disabled\n" +
			"  CHECK: env GLOVEBOX_INTEGRATION\n" +
			"  FIX:   run with GLOVEBOX_INTEGRATION=1 (nightly/manual CI does this)")
	}
}

// RequireCreds skips when any named env var is empty.
func RequireCreds(t *testing.T, envVars ...string) {
	t.Helper()
	for _, k := range envVars {
		if os.Getenv(k) == "" {
			t.Skipf("missing credential\n"+
				"  CHECK: env %s\n"+
				"  FIX:   provide %s (ESO-synced in the in-cluster job; see docs/connectors/integration-credentials.md)", k, k)
		}
	}
}

// SkipOnRateLimit turns an upstream throttle into a skip-with-warning so a
// nightly run does not go red on provider rate limiting (spec §9). Best
// effort: matches common 429/rate-limit error text. Call as:
//
//	if err := c.Poll(ctx, cp); err != nil { integrationtest.SkipOnRateLimit(t, err); t.Fatal(err) }
func SkipOnRateLimit(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "429") || strings.Contains(s, "rate limit") || strings.Contains(s, "too many requests") {
		t.Skipf("upstream rate-limited (skip, not fail)\n"+
			"  CHECK: %v\n"+
			"  FIX:   re-run later; the source throttled this account", err)
	}
}
