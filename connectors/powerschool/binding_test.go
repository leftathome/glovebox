package powerschool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

func TestStudentBinding_Check(t *testing.T) {
	t.Run("bound: only the bound id passes", func(t *testing.T) {
		b := &studentBinding{want: "999001", others: map[string]bool{"999002": true}}
		for id, wantErr := range map[string]bool{"999001": false, "999002": true, "999003": true, "": true} {
			if err := b.check(id); (err != nil) != wantErr {
				t.Errorf("check(%q) = %v, wantErr %v", id, err, wantErr)
			} else if err != nil && !errors.Is(err, ErrSelectionChanged) {
				t.Errorf("check(%q) = %v, want ErrSelectionChanged", id, err)
			}
		}
		if b.seen != "" {
			t.Errorf("a bound kid recorded a candidate id %q", b.seen)
		}
	})

	t.Run("unbound: pages must agree and must not be another kid's", func(t *testing.T) {
		b := &studentBinding{others: map[string]bool{"999002": true}}
		if err := b.check("999002"); !errors.Is(err, ErrSelectionChanged) {
			t.Fatalf("another kid's id accepted: %v", err)
		}
		if err := b.check(""); !errors.Is(err, ErrSelectionChanged) {
			t.Fatalf("a page with no id accepted: %v", err)
		}
		if b.seen != "" {
			t.Fatalf("a refused id became the candidate: %q", b.seen)
		}
		if err := b.check("999001"); err != nil {
			t.Fatalf("first page refused: %v", err)
		}
		if err := b.check("999001"); err != nil {
			t.Fatalf("agreeing page refused: %v", err)
		}
		if err := b.check("999003"); !errors.Is(err, ErrSelectionChanged) {
			t.Fatalf("disagreeing page accepted: %v", err)
		}
	})
}

func TestStudentBinding_Learn(t *testing.T) {
	newCP := func(t *testing.T) connector.Checkpoint {
		t.Helper()
		cp, err := connector.NewCheckpoint(t.TempDir())
		if err != nil {
			t.Fatalf("NewCheckpoint: %v", err)
		}
		return cp
	}
	k1, k2 := Kid{Name: "k1", StudentID: "111111"}, Kid{Name: "k2", StudentID: "111112"}
	kids := []Kid{k1, k2}

	t.Run("learns once, then enforces from the checkpoint", func(t *testing.T) {
		cp := newCP(t)
		b := newStudentBinding(kids, cp, k1)
		if err := b.check("999001"); err != nil {
			t.Fatal(err)
		}
		learned, err := b.learn(cp, k1)
		if err != nil || !learned {
			t.Fatalf("learn = (%v, %v), want (true, nil)", learned, err)
		}
		if got, _ := cp.Load(apiIDKey("k1")); got != "999001" {
			t.Fatalf("checkpoint holds %q, want 999001", got)
		}
		// A later turn, a new binding object (a restart, say).
		later := newStudentBinding(kids, cp, k1)
		if later.want != "999001" {
			t.Fatalf("later binding wants %q", later.want)
		}
		if err := later.check("999002"); !errors.Is(err, ErrSelectionChanged) {
			t.Errorf("other id accepted after learning: %v", err)
		}
		if learned, err := later.learn(cp, k1); learned || err != nil {
			t.Errorf("learn on a bound kid = (%v, %v), want (false, nil)", learned, err)
		}
		// And the other kid now refuses that id.
		if err := newStudentBinding(kids, cp, k2).check("999001"); !errors.Is(err, ErrSelectionChanged) {
			t.Errorf("k2 accepted the id bound to k1: %v", err)
		}
	})

	t.Run("nothing seen, nothing learned", func(t *testing.T) {
		cp := newCP(t)
		if learned, err := newStudentBinding(kids, cp, k1).learn(cp, k1); learned || err != nil {
			t.Errorf("learn = (%v, %v), want (false, nil)", learned, err)
		}
		if _, ok := cp.Load(apiIDKey("k1")); ok {
			t.Error("checkpoint written with nothing to learn")
		}
	})

	t.Run("an id that became another kid's during the turn is a conflict", func(t *testing.T) {
		cp := newCP(t)
		b := newStudentBinding(kids, cp, k1)
		if err := b.check("999001"); err != nil {
			t.Fatal(err)
		}
		b.others["999001"] = true // bound to k2 in the meantime
		if learned, err := b.learn(cp, k1); learned || !errors.Is(err, ErrStudentAPIIDConflict) {
			t.Errorf("learn = (%v, %v), want ErrStudentAPIIDConflict", learned, err)
		}
		if _, ok := cp.Load(apiIDKey("k1")); ok {
			t.Error("a conflicting id was saved")
		}
	})

	t.Run("config wins over the checkpoint", func(t *testing.T) {
		cp := newCP(t)
		if err := cp.Save(apiIDKey("k1"), "999009"); err != nil {
			t.Fatal(err)
		}
		configured := Kid{Name: "k1", StudentID: "111111", StudentAPIID: "999001"}
		if b := newStudentBinding([]Kid{configured, k2}, cp, configured); b.want != "999001" {
			t.Errorf("want = %q, want the configured 999001", b.want)
		}
		if b := newStudentBinding([]Kid{configured, k2}, cp, k2); !b.others["999001"] || b.others["999009"] {
			t.Errorf("k2's others = %v, want only the configured id of k1", b.others)
		}
	})
}

// flipAwayAndBack makes the portal's selection flip to `other` just before
// the scores page of the kid whose student is `victim` is served, and flip
// back right after. Both home-page marker checks -- at the start and at the
// end of the kid's turn -- see the right student. Only the scores page
// knows.
func flipAwayAndBack(f *fakeClient, victim, other string) {
	f.ScoresFunc = func(ctx context.Context, scoresURL string) (*pslib.ScoresMetadata, error) {
		flip := f.Selected == victim
		if flip {
			f.Selected = other
		}
		md := &pslib.ScoresMetadata{SectionID: "654321", StudentAPIID: studentAPIIDs[f.Selected]}
		if flip {
			f.Selected = victim
		}
		return md, nil
	}
}

func boundConfig() Config {
	cfg := testConfig()
	cfg.Kids[0].StudentAPIID = studentAPIIDs["111111"]
	cfg.Kids[1].StudentAPIID = studentAPIIDs["111112"]
	return cfg
}

// assertFlipCaught checks the outcome of a poll in which k1's scores page
// was served while the other child was selected: nothing staged for k1, no
// lookup sent for k1, k1's checkpoints untouched, and k2 unaffected.
func assertFlipCaught(t *testing.T, client *fakeClient, cp connector.Checkpoint, items []stagedItem, rec *metricRecorder, k1Before int) {
	t.Helper()
	k1Items := 0
	for _, it := range items {
		if it.Meta.Tags["kid"] == "k1" {
			k1Items++
		}
	}
	if k1Items != k1Before {
		t.Errorf("k1 has %d staged items, want %d: a poll whose scores page was another child's staged something", k1Items, k1Before)
	}
	for _, call := range client.AssignmentsCalls {
		if strings.HasPrefix(call, "111111/") {
			t.Errorf("an assignment lookup was sent for k1 after its scores page failed the check: %v", client.AssignmentsCalls)
		}
	}
	if k1Before == 0 {
		assertNoFingerprints(t, cp, "k1")
	}
	counters := rec.counters()
	if counters["powerschool_kids_skipped_total{kid=k1,reason=selection_changed}"] != 1 ||
		counters["powerschool_surface_errors_total{error_class=selection_changed,kid=k1,surface=assignment}"] != 1 {
		t.Errorf("selection_changed not recorded for k1: %v", counters)
	}
}

// TestPoll_FlipAwayAndBack_ConfiguredID is the case the marker checks miss:
// the selection is right when k1's turn starts, wrong while its scores page
// is read, and right again when the turn ends. With k1 bound to its student
// id, the scores page gives it away.
func TestPoll_FlipAwayAndBack_ConfiguredID(t *testing.T) {
	client := pollFixture("111111")
	flipAwayAndBack(client, "111111", "111112")
	c, cp, dir := newWiredConnector(t, client, boundConfig())
	rec := recordMetrics(t, c)
	logs := captureLogs(t)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	items := readItems(t, dir)
	assertFlipCaught(t, client, cp, items, rec, 0)
	assertKidItems(t, items, "k2")
	if !strings.Contains(logs.String(), "error_class=selection_changed") {
		t.Errorf("no selection_changed log line:\n%s", logs.String())
	}
	for _, id := range studentAPIIDs {
		if strings.Contains(logs.String(), id) {
			t.Errorf("log contains a student id (%s):\n%s", id, logs.String())
		}
	}
}

// TestPoll_LearnThenEnforce: no ids configured. The first clean poll binds
// each kid to the id on its scores pages; the second poll, with the same
// flip, is caught -- by the same connector, and by a new one on the same
// state after a restart.
func TestPoll_LearnThenEnforce(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same process"
		if restart {
			name = "after a restart"
		}
		t.Run(name, func(t *testing.T) {
			client := pollFixture("111111")
			cfg := testConfig()
			stagingDir := t.TempDir()
			w, err := connector.NewStagingWriter(stagingDir, "powerschool")
			if err != nil {
				t.Fatalf("NewStagingWriter: %v", err)
			}
			cp, err := connector.NewCheckpoint(t.TempDir())
			if err != nil {
				t.Fatalf("NewCheckpoint: %v", err)
			}
			c := wireConnector(t, client, cfg, w)
			captureLogs(t)
			ctx := context.Background()

			// Poll 1: clean. Everything stages and both kids are bound.
			if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
				t.Fatalf("first poll: %v", err)
			}
			assertKidItems(t, readItems(t, stagingDir), "k1", "k2")
			for studentID, kid := range kidOf {
				if got, _ := cp.Load(apiIDKey(kid)); got != studentAPIIDs[studentID] {
					t.Fatalf("%s bound to %q after a clean poll, want %q", kid, got, studentAPIIDs[studentID])
				}
			}

			if restart {
				c = wireConnector(t, client, cfg, w)
			}
			rec := recordMetrics(t, c)

			// Poll 2: k1's grade has changed (so there is something to
			// stage) and its scores page is served for the other child.
			studentGrades["111111"] = "A95"
			t.Cleanup(func() { studentGrades["111111"] = "B+88" })
			flipAwayAndBack(client, "111111", "111112")
			client.AssignmentsCalls = nil
			if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
				t.Fatalf("second poll: %v", err)
			}
			items := readItems(t, stagingDir)
			assertFlipCaught(t, client, cp, items, rec, 3)
			for _, it := range items {
				if it.Meta.Tags["kid"] == "k1" && it.Meta.Tags["change"] == "updated" {
					t.Errorf("k1's changed grade was staged from a discarded poll: %v", it.Meta.Tags)
				}
			}
			// The discarded poll did not advance k1's grade checkpoint.
			if fp, _ := cp.Load(CheckpointKey(gradeSurface, "k1", "00100")); !strings.Contains(fp, "B+88") {
				t.Errorf("k1 grade checkpoint = %q, want it still at the last staged grade", fp)
			}
			// And the binding itself was not disturbed.
			if got, _ := cp.Load(apiIDKey("k1")); got != studentAPIIDs["111111"] {
				t.Errorf("k1 binding changed to %q", got)
			}

			// Poll 3: no interference. The change stages now.
			client.ScoresFunc = nil
			if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
				t.Fatalf("third poll: %v", err)
			}
			updated := 0
			for _, it := range readItems(t, stagingDir) {
				if it.Meta.Tags["kid"] == "k1" && it.Meta.Tags["change"] == "updated" && it.Meta.Tags["grade"] == "A95" {
					updated++
				}
			}
			if updated != 1 {
				t.Errorf("%d updated grade items for k1 after the clean third poll, want 1", updated)
			}
		})
	}
}

// TestPoll_TwoKidsNeverShareAStudentID: at learn time. Both children's
// scores pages carry the same id (which cannot be right). The first kid is
// bound to it; the second is refused and stages nothing.
func TestPoll_TwoKidsNeverShareAStudentID(t *testing.T) {
	client := pollFixture("111111")
	client.APIIDFunc = func(string) string { return "999001" }
	c, cp, dir := newWiredConnector(t, client, testConfig())
	rec := recordMetrics(t, c)
	captureLogs(t)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	assertKidItems(t, readItems(t, dir), "k1")
	assertNoFingerprints(t, cp, "k2")
	if got, _ := cp.Load(apiIDKey("k1")); got != "999001" {
		t.Errorf("k1 bound to %q, want 999001", got)
	}
	if got, ok := cp.Load(apiIDKey("k2")); ok {
		t.Errorf("k2 bound to %q, the id already bound to k1", got)
	}
	if rec.counters()["powerschool_kids_skipped_total{kid=k2,reason=selection_changed}"] != 1 {
		t.Errorf("k2 not recorded as skipped: %v", rec.counters())
	}
}

// TestPoll_NothingLearnedFromADiscardedPoll: a turn that fails its
// end-of-turn marker check must not bind the kid to what it saw.
func TestPoll_NothingLearnedFromADiscardedPoll(t *testing.T) {
	client := pollFixture("111111")
	inner := client.AssignmentsFunc
	client.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
		out, err := inner(ctx, studentID, g)
		client.Selected = "111112" // moves and stays moved
		return out, err
	}
	c, cp, dir := newWiredConnector(t, client, oneKidConfig())
	captureLogs(t)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := len(readItems(t, dir)); got != 0 {
		t.Errorf("staged %d items", got)
	}
	if got, ok := cp.Load(apiIDKey("k1")); ok {
		t.Errorf("k1 bound to %q by a poll that failed its end-of-turn check", got)
	}
}
