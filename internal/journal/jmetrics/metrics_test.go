package jmetrics

import (
	"io"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	gloveboxmetrics "github.com/leftathome/glovebox/internal/metrics"
)

func scrape(t *testing.T, m *gloveboxmetrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

// exercise calls every instrument once.
func exercise(m *Metrics) {
	m.Snapshot("assignment", "applied")
	m.Event("attendance_day", "created", "high")
	m.ScanWithheld("assignment", 2)
	m.ApplyDuration(15 * time.Millisecond)
	m.VisibilityMismatch()
	m.Request("records", 200)
	m.RosterReloadError()
	m.SetHeld(1)
	m.SetReady(true)
	m.SetRosterSHA("abc123")
	m.SetLastSnapshot("source=powerschool/", time.Unix(1_800_000_000, 0))
}

func newPair(t *testing.T) (*gloveboxmetrics.Metrics, *Metrics) {
	t.Helper()
	core, err := gloveboxmetrics.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.Shutdown() })
	m, err := New(core.Provider())
	if err != nil {
		t.Fatal(err)
	}
	return core, m
}

func TestMetricsAreExported(t *testing.T) {
	core, m := newPair(t)
	exercise(m)
	body := scrape(t, core)
	for _, want := range []string{
		`glovebox_journal_snapshots_total{`, `record_type="assignment"`, `status="applied"`,
		`glovebox_journal_events_total{`, `importance="high"`,
		`glovebox_journal_scan_withheld_total{`,
		`glovebox_journal_apply_duration_seconds_bucket{`,
		`glovebox_journal_visibility_mismatch_total`,
		`glovebox_journal_api_requests_total{`, `endpoint="records"`, `code="200"`,
		`glovebox_journal_roster_reload_errors_total`,
		`glovebox_journal_held_snapshots`,
		`glovebox_journal_ready`,
		`glovebox_journal_roster_info{`, `sha256="abc123"`,
		`glovebox_journal_last_snapshot_timestamp_seconds{`, `prefix="source=powerschool/"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	var nilMetrics *Metrics
	nilMetrics.Request("x", 500) // a nil *Metrics is a no-op sink
	nilMetrics.Snapshot("a", "b")
	nilMetrics.Event("a", "b", "c")
	nilMetrics.ScanWithheld("a", 1)
	nilMetrics.ApplyDuration(time.Second)
	nilMetrics.VisibilityMismatch()
	nilMetrics.RosterReloadError()
	nilMetrics.SetHeld(1)
	nilMetrics.SetReady(true)
	nilMetrics.SetRosterSHA("x")
	nilMetrics.SetLastSnapshot("p", time.Now())
}

func TestScanWithheldAddsTheCount(t *testing.T) {
	core, m := newPair(t)
	m.ScanWithheld("assignment", 2)
	m.ScanWithheld("assignment", 3)
	if body := scrape(t, core); !strings.Contains(body, `record_type="assignment"} 5`) {
		t.Errorf("withheld total is not 5:\n%s", body)
	}
}

func TestReadyAndHeldGaugesFollowTheirSetters(t *testing.T) {
	core, m := newPair(t)
	m.SetHeld(3)
	m.SetReady(true)
	m.SetReady(false)
	body := scrape(t, core)
	if !regexp.MustCompile(`(?m)^glovebox_journal_held_snapshots(\{[^}]*\})? 3$`).MatchString(body) {
		t.Errorf("held gauge is not 3:\n%s", body)
	}
	if !regexp.MustCompile(`(?m)^glovebox_journal_ready(\{[^}]*\})? 0$`).MatchString(body) {
		t.Errorf("ready gauge is not 0:\n%s", body)
	}
}

func TestRosterInfoDropsTheOldHash(t *testing.T) {
	core, m := newPair(t)
	m.SetRosterSHA("old")
	m.SetRosterSHA("new")
	body := scrape(t, core)
	if strings.Contains(body, `sha256="old"`) || !strings.Contains(body, `sha256="new"`) {
		t.Errorf("roster info keeps a stale hash:\n%s", body)
	}
}

// labelAllowlist is every label a journal metric may carry. A label here is a
// closed or configured enum: never a data subject, roster member, key, scope
// or value (spec 4.15). Adding a label to a metric means adding it here and
// arguing in review that it cannot carry data.
var labelAllowlist = map[string][]string{
	"glovebox_journal_snapshots_total":                 {"record_type", "status"},
	"glovebox_journal_events_total":                    {"record_type", "kind", "importance"},
	"glovebox_journal_scan_withheld_total":             {"record_type"},
	"glovebox_journal_api_requests_total":              {"endpoint", "code"},
	"glovebox_journal_apply_duration_seconds_bucket":   {"le"},
	"glovebox_journal_apply_duration_seconds_sum":      {},
	"glovebox_journal_apply_duration_seconds_count":    {},
	"glovebox_journal_roster_reload_errors_total":      {},
	"glovebox_journal_visibility_mismatch_total":       {},
	"glovebox_journal_held_snapshots":                  {},
	"glovebox_journal_last_snapshot_timestamp_seconds": {"prefix"},
	"glovebox_journal_roster_info":                     {"sha256"},
	"glovebox_journal_ready":                           {},
}

var sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})? `)
var labelName = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="`)

// journalLabelSets returns, per journal sample name, the sorted label names
// it carries (the exporter's own otel_scope_* labels excluded).
func journalLabelSets(body string) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(body, "\n") {
		m := sampleLine.FindStringSubmatch(line)
		if m == nil || !strings.HasPrefix(m[1], "glovebox_journal_") {
			continue
		}
		set := map[string]bool{}
		for _, l := range labelName.FindAllStringSubmatch(m[2], -1) {
			if !strings.HasPrefix(l[1], "otel_scope_") {
				set[l[1]] = true
			}
		}
		for _, prev := range out[m[1]] {
			set[prev] = true
		}
		names := []string{}
		for n := range set {
			names = append(names, n)
		}
		sort.Strings(names)
		out[m[1]] = names
	}
	return out
}

func TestEveryRegisteredMetricCarriesOnlyAllowedLabels(t *testing.T) {
	core, m := newPair(t)
	exercise(m)
	got := journalLabelSets(scrape(t, core))
	if len(got) == 0 {
		t.Fatal("no journal metric was scraped")
	}
	for name, labels := range got {
		want, ok := labelAllowlist[name]
		if !ok {
			t.Errorf("metric %s is not in the label allowlist: add it, with its labels reviewed for privacy", name)
			continue
		}
		want = append([]string{}, want...)
		sort.Strings(want)
		if strings.Join(labels, ",") != strings.Join(want, ",") {
			t.Errorf("metric %s has labels %v, allowlist says %v", name, labels, want)
		}
	}
	// Every allowlisted metric is really exported (a typo in the list would
	// otherwise make the check vacuous).
	for name := range labelAllowlist {
		if _, ok := got[name]; !ok {
			t.Errorf("allowlisted metric %s was not scraped", name)
		}
	}
}

func TestLabelAllowlistCatchesAForbiddenLabel(t *testing.T) {
	body := `glovebox_journal_events_total{data_subject="e_child1",kind="created"} 1` + "\n"
	got := journalLabelSets(body)["glovebox_journal_events_total"]
	if strings.Join(got, ",") != "data_subject,kind" {
		t.Fatalf("parser lost a label: %v", got)
	}
	want := append([]string{}, labelAllowlist["glovebox_journal_events_total"]...)
	sort.Strings(want)
	if strings.Join(got, ",") == strings.Join(want, ",") {
		t.Error("a data_subject label would pass the allowlist")
	}
}

// sampleValue finds the sample of a metric with exactly these labels
// (otel_scope_* ignored) and returns its value.
func sampleValue(t *testing.T, body, name string, labels map[string]string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		m := sampleLine.FindStringSubmatch(line)
		if m == nil || m[1] != name {
			continue
		}
		got := map[string]string{}
		for _, l := range regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="([^"]*)"`).FindAllStringSubmatch(m[2], -1) {
			if !strings.HasPrefix(l[1], "otel_scope_") {
				got[l[1]] = l[2]
			}
		}
		if len(got) != len(labels) {
			continue
		}
		match := true
		for k, v := range labels {
			if got[k] != v {
				match = false
			}
		}
		if !match {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[len(m[0]):]), 64)
		if err != nil {
			t.Fatalf("sample %q: %v", line, err)
		}
		return v
	}
	t.Fatalf("no sample %s%v in:\n%s", name, labels, body)
	return 0
}

func TestExactSampleValues(t *testing.T) {
	core, m := newPair(t)
	exercise(m)
	m.SetHeld(1)
	body := scrape(t, core)
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"glovebox_journal_snapshots_total", map[string]string{"record_type": "assignment", "status": "applied"}, 1},
		{"glovebox_journal_events_total", map[string]string{"record_type": "attendance_day", "kind": "created", "importance": "high"}, 1},
		{"glovebox_journal_scan_withheld_total", map[string]string{"record_type": "assignment"}, 2},
		{"glovebox_journal_api_requests_total", map[string]string{"endpoint": "records", "code": "200"}, 1},
		{"glovebox_journal_roster_reload_errors_total", map[string]string{}, 1},
		{"glovebox_journal_visibility_mismatch_total", map[string]string{}, 1},
		{"glovebox_journal_held_snapshots", map[string]string{}, 1},
		{"glovebox_journal_ready", map[string]string{}, 1},
		{"glovebox_journal_roster_info", map[string]string{"sha256": "abc123"}, 1},
		{"glovebox_journal_last_snapshot_timestamp_seconds", map[string]string{"prefix": "source=powerschool/"}, 1.8e9},
		{"glovebox_journal_apply_duration_seconds_count", map[string]string{}, 1},
	} {
		if got := sampleValue(t, body, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	if got := sampleValue(t, body, "glovebox_journal_apply_duration_seconds_sum", map[string]string{}); got < 0.0149 || got > 0.0151 {
		t.Errorf("apply duration sum = %v, want 0.015 (seconds)", got)
	}
}

func TestSetLastSnapshotRefusesAPrefixNamingASubject(t *testing.T) {
	core, m := newPair(t)
	m.SetLastSnapshot("source=powerschool/surface=assignments/data_subject=e_child1/", time.Unix(1_800_000_000, 0))
	m.SetLastSnapshot("source=powerschool/", time.Unix(1_800_000_000, 0))
	body := scrape(t, core)
	if strings.Contains(body, "e_child1") || strings.Contains(body, "data_subject") {
		t.Errorf("a subject reached a label:\n%s", body)
	}
	if got := sampleValue(t, body, "glovebox_journal_last_snapshot_timestamp_seconds", map[string]string{"prefix": "source=powerschool/"}); got != 1.8e9 {
		t.Errorf("the safe prefix was dropped: %v", got)
	}
}
