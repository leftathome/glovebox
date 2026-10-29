package journal

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/jmetrics"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	gloveboxmetrics "github.com/leftathome/glovebox/internal/metrics"
)

// Emission points of Task 23. A write reports to the Recorder only after it
// commits; a refusal or an error reports nothing.

func TestApplyReportsItsDurationOnlyWhenItSucceeds(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	if r.rec.durations != 1 {
		t.Fatalf("durations after an apply: %d", r.rec.durations)
	}
	// A resend is an answer too.
	five(r, 1, T0.Add(-3*time.Hour))
	if r.rec.durations != 2 {
		t.Errorf("durations after a resend: %d", r.rec.durations)
	}
	// A refusal (id reused by another writer) reports no duration.
	n, p := r.normalize(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, keep(1)...))
	out, err := r.j.Apply(ctx, "openclaw/glovebox-other-connector", n, p)
	if err != nil || out.Rejection == nil {
		t.Fatalf("expected a refusal: %+v %v", out, err)
	}
	if r.rec.durations != 2 {
		t.Errorf("a refusal reported a duration: %d", r.rec.durations)
	}
	// So does a database error.
	if _, err := r.j.db.Exec(`CREATE FUNCTION fail_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$;
		CREATE TRIGGER fail_snapshot BEFORE INSERT ON snapshot FOR EACH ROW EXECUTE FUNCTION fail_snapshot()`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.applyErr(snap(9, "e_child1", "c1", T0.Add(-time.Hour), true, keep(1)...)); err == nil {
		t.Fatal("expected a database error")
	}
	if r.rec.durations != 2 {
		t.Errorf("an error reported a duration: %d", r.rec.durations)
	}
}

func TestConfirmHeldReportsDurationAndSnapshotOnlyOnSuccess(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...)) // held
	base := r.rec.durations
	// Refused: the writer lost the right to write. Nothing is reported.
	deny := func(string, record.Type, record.Scope) *record.Rejection {
		return &record.Rejection{Status: 403, Reason: journalapi.ReasonPolicy, Detail: "no longer allowed"}
	}
	if out, err := r.j.ConfirmHeld(ctx, uid(2), deny); err != nil || out.Rejection == nil {
		t.Fatalf("expected a refusal: %+v %v", out, err)
	}
	if _, err := r.j.ConfirmHeld(ctx, uid(99), allowAll); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := r.j.ConfirmHeld(ctx, uid(1), allowAll); !errors.Is(err, ErrNotHeld) {
		t.Fatal(err)
	}
	if r.rec.durations != base || r.rec.byType["assignment/applied"] != 1 {
		t.Errorf("a failed confirm reported: durations %d (was %d), %v", r.rec.durations, base, r.rec.byType)
	}
	if _, err := r.j.ConfirmHeld(ctx, uid(2), allowAll); err != nil {
		t.Fatal(err)
	}
	if r.rec.durations != base+1 || r.rec.byType["assignment/applied"] != 2 {
		t.Errorf("a confirm reported durations %d (was %d), %v", r.rec.durations, base, r.rec.byType)
	}
}

func TestDiscardHeldCountsADiscardedSnapshotOnce(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...)) // held
	if err := r.j.DiscardHeld(ctx, uid(99)); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := r.j.DiscardHeld(ctx, uid(1)); !errors.Is(err, ErrNotHeld) {
		t.Fatal(err)
	}
	if n := r.rec.byType["assignment/discarded"]; n != 0 {
		t.Fatalf("a failed discard was counted: %d", n)
	}
	if err := r.j.DiscardHeld(ctx, uid(2)); err != nil {
		t.Fatal(err)
	}
	if err := r.j.DiscardHeld(ctx, uid(2)); !errors.Is(err, ErrNotHeld) {
		t.Fatal(err)
	}
	if n := r.rec.byType["assignment/discarded"]; n != 1 {
		t.Errorf("discarded counted %d times, want 1", n)
	}
	if r.rec.snapshots["discarded"] != 1 {
		t.Errorf("recorder %v", r.rec.snapshots)
	}
}

func TestAutomaticDiscardOfOlderHeldSnapshotsIsCounted(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-5*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-4*time.Hour), true, keep(3)...)) // held
	r.apply(snap(3, "e_child1", "c1", T0.Add(-3*time.Hour), true, keep(2)...)) // held
	if r.rec.byType["assignment/discarded"] != 0 {
		t.Fatalf("discards before any: %v", r.rec.byType)
	}
	out := r.apply(snap(4, "e_child1", "c1", T0.Add(-time.Hour), true, keep(5)...))
	if out.Status != journalapi.StatusApplied {
		t.Fatalf("%+v", out)
	}
	if n := r.rec.byType["assignment/discarded"]; n != 2 {
		t.Errorf("automatic discards counted %d, want 2: %v", n, r.rec.byType)
	}
	// Confirming a held snapshot discards the older held ones as well.
	r2 := newRig(t)
	five(r2, 1, T0.Add(-5*time.Hour))
	r2.apply(snap(2, "e_child1", "c1", T0.Add(-4*time.Hour), true, keep(3)...))
	r2.apply(snap(3, "e_child1", "c1", T0.Add(-3*time.Hour), true, keep(2)...))
	if _, err := r2.j.ConfirmHeld(ctx, uid(3), allowAll); err != nil {
		t.Fatal(err)
	}
	if n := r2.rec.byType["assignment/discarded"]; n != 1 {
		t.Errorf("discard by confirm counted %d, want 1: %v", n, r2.rec.byType)
	}
}

func TestAutomaticDiscardIsNotCountedWhenTheWriteRollsBack(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-5*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-4*time.Hour), true, keep(3)...)) // held
	before := len(r.rec.events)
	// The failure is deferred to COMMIT, after every statement of the write
	// (the discard included) has run: only an emit-after-commit survives it.
	if _, err := r.j.db.Exec(`CREATE FUNCTION fail_discard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF NEW.status = 'discarded' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$;
		CREATE CONSTRAINT TRIGGER fail_discard AFTER UPDATE ON snapshot DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION fail_discard()`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.applyErr(snap(3, "e_child1", "c1", T0.Add(-time.Hour), true, keep(5)...)); err == nil {
		t.Fatal("the injected failure did not surface")
	}
	if r.rec.byType["assignment/discarded"] != 0 || len(r.rec.events) != before || r.snapshotStatus(2) != "held" {
		t.Errorf("a rolled-back write reported: %v %v", r.rec.byType, r.rec.events)
	}
}

func TestRefusedNoteAndEraseAndRescanReportNothing(t *testing.T) {
	r := newRig(t)
	// A refused note.
	if _, rej := r.note("e_nobody", "e_child1", "2026-10-14", `{"what_happened": "a"}`); rej == nil {
		t.Fatal("expected a refusal")
	}
	if len(r.rec.events) != 0 || r.rec.withheld != 0 || len(r.rec.snapshots) != 0 || r.rec.durations != 0 {
		t.Errorf("a refused note reported: %+v", r.rec)
	}
	// A written note reports its event, once.
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	if r.rec.events["created/"+r.events("e_child1")[0].Importance] != 1 || len(r.rec.events) != 1 {
		t.Errorf("note events: %v", r.rec.events)
	}
	snaps, evs, dur := len(r.rec.snapshots), len(r.rec.events), r.rec.durations
	// Rescan and Erase have no metric (4.15 lists none).
	if _, err := r.j.Rescan(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	if len(r.rec.snapshots) != snaps || len(r.rec.events) != evs || r.rec.durations != dur || r.rec.mismatch != 0 {
		t.Errorf("rescan or erase reported: %+v", r.rec)
	}
}

// The real Metrics, through a real journal: after writes and reads, no
// scraped line mentions a data subject, a key, a field value or a scope.
func TestScrapedMetricsCarryNoDataFromARealJournal(t *testing.T) {
	core, err := gloveboxmetrics.New()
	if err != nil {
		t.Fatal(err)
	}
	defer core.Shutdown()
	jm, err := jmetrics.New(core.Provider())
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(o *Options) { o.Metrics = jm })
	const secret = "SECRET-TITLE-VALUE"
	r.apply(snap(1, "e_child1", "c2", T0.Add(-4*time.Hour), true, rec("c2:1", "e_child1", `{"title": "`+secret+`"}`)))
	five(r, 2, T0.Add(-3*time.Hour))
	r.apply(snap(3, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...)) // held
	if err := r.j.DiscardHeld(ctx, uid(3)); err != nil {
		t.Fatal(err)
	}
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "`+secret+`"}`)
	held, last, err := r.j.Gauges(ctx, []string{"source=powerschool/"})
	if err != nil {
		t.Fatal(err)
	}
	jm.SetHeld(held)
	for p, ts := range last {
		jm.SetLastSnapshot(p, ts)
	}
	rec := httptest.NewRecorder()
	core.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	body := string(b)
	if !strings.Contains(body, `glovebox_journal_snapshots_total`) || !strings.Contains(body, `status="discarded"`) {
		t.Fatalf("the journal's metrics were not exported:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "glovebox_journal_") {
			continue
		}
		for _, forbidden := range []string{"e_child", "e_parent", "c1:1", "c2:1", secret, "2026-10-14", "course=", "data_subject", "surface="} {
			if strings.Contains(line, forbidden) {
				t.Errorf("metric line carries %q: %s", forbidden, line)
			}
		}
	}
}
