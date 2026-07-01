//go:build integration

package schoology

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/integrationtest"
	schoologylib "github.com/leftathome/schoology-go"
	schoologyauth "github.com/leftathome/schoology-go/auth"
)

// TestLive_Schoology is the reference credentialed live integration test: it
// drives the schoology connector against the real Schoology tenant using a
// saved session, exactly as cmd/schoology/main.go wires it in production, and
// asserts a full stage round-trip.
//
// Credential boundary: schoology is real-readonly (household session). The
// session credentials only exist inside the cluster (synced from Vault via
// ESO), so this test runs green ONLY in the in-cluster nightly/manual GitLab
// integration stage. Everywhere else -- local `go test -tags integration`, the
// merge gate -- it SKIPs cleanly via the guards below and makes no live call.
//
// Required env (bound by the private homelab/ci-templates ExternalSecret; see
// docs/connectors/integration-credentials.md):
//   - SCHOOLOGY_HOST             tenant subdomain (e.g. yourschool.schoology.com)
//   - SCHOOLOGY_CREDENTIALS_FILE path to the JSON session written by
//     schoology-go auth.SaveCredentials (SessID/CSRFToken/CSRFKey/UID)
//   - SCHOOLOGY_KID_UID          a real child UID whose feed/assignments have
//     content; kept in env (not committed) since it is account-specific PII.
//
// Because a child's Schoology surfaces can legitimately be empty at any given
// moment, the CI job is marked allow_failure until a min-content fixture/window
// is agreed (registry note). The assertions here are intentionally minimal for
// now (staged >= 1 + non-empty content); richer per-surface routing assertions
// are a follow-up.
func TestLive_Schoology(t *testing.T) {
	integrationtest.RequireIntegration(t)
	integrationtest.RequireCreds(t, "SCHOOLOGY_HOST", "SCHOOLOGY_CREDENTIALS_FILE", "SCHOOLOGY_KID_UID")

	kidUID, err := strconv.ParseInt(os.Getenv("SCHOOLOGY_KID_UID"), 10, 64)
	if err != nil {
		t.Fatalf("SCHOOLOGY_KID_UID must be a numeric Schoology UID: %v", err)
	}

	// Effective config: the shipped sample config through the connector's
	// normal ApplyDefaults + ValidateConfig path, with the single kid's UID
	// supplied from env (the committed sample uses placeholder UIDs). Routing
	// rules come from the sample config, so the matcher is production-shaped.
	cfg := Config{
		Kids: []Kid{{Name: "k1", SchoologyUID: kidUID}},
		PollSchedule: PollSchedule{
			Windows: []PollWindow{{Start: "08:00", End: "09:00"}},
		},
		BaseConfig: connector.BaseConfig{
			Rules: []connector.Rule{
				{Match: "schoology:k1:assignment", Destination: "school", DataSubject: "k1", Audience: []string{"household"}},
				{Match: "schoology:k1:feed", Destination: "school", DataSubject: "k1", Audience: []string{"household"}},
				{Match: "schoology:k1:attachment", Destination: "school", DataSubject: "k1", Audience: []string{"household"}},
				{Match: "schoology:message", Destination: "school", Audience: []string{"guardians"}},
				{Match: "schoology-parse-failure:*", Destination: "school", Audience: []string{"guardians"}},
			},
		},
	}
	ApplyDefaults(&cfg)
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}

	// Real library client, wired exactly like cmd/schoology/main.go.
	creds, err := schoologyauth.LoadCredentials(os.Getenv("SCHOOLOGY_CREDENTIALS_FILE"))
	if err != nil {
		t.Fatalf("load credentials: %v (see docs/AUTH-RECOVERY.md)", err)
	}
	lib, err := schoologylib.NewClient(os.Getenv("SCHOOLOGY_HOST"), schoologylib.WithSession(
		creds.SessID, creds.CSRFToken, creds.CSRFKey, creds.UID,
	))
	if err != nil {
		t.Fatalf("construct library client: %v", err)
	}

	c, err := NewConnector(NewProductionClient(lib), cfg, "v0.1.0")
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}

	w, readback := integrationtest.StageToTempDir(t, "schoology")
	matcher := connector.NewRuleMatcher(cfg.Rules)
	metrics, err := connector.NewMetrics("schoology-live-" + t.Name())
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })

	if err := c.Wire(connector.ConnectorContext{Writer: w, Matcher: matcher, Metrics: metrics}); err != nil {
		t.Fatalf("Wire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Poll(ctx, cp); err != nil {
		integrationtest.SkipOnRateLimit(t, err)
		t.Fatalf("Poll: %v", err)
	}

	items := readback()
	integrationtest.AssertStagedAtLeast(t, items, 1)
	if len(items) == 0 {
		t.FailNow()
	}
	integrationtest.AssertContentNonEmpty(t, items[0])
}
