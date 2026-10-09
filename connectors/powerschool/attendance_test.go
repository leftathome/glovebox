package powerschool

import (
	"context"
	"testing"

	pslib "github.com/leftathome/powerschool-go"
)

func attendanceGrade(absences, tardies int) *pslib.Grade {
	g := sampleGrade("00100", "Example Science 7", "B")
	g.Absences, g.Tardies = absences, tardies
	return g
}

func TestAttendanceFingerprint_RoundTrip(t *testing.T) {
	a, tardy := parseAttendanceFingerprint(attendanceFingerprint(3, 1))
	if a != 3 || tardy != 1 {
		t.Errorf("round trip = (%d, %d), want (3, 1)", a, tardy)
	}
	if a, tardy := parseAttendanceFingerprint(""); a != 0 || tardy != 0 {
		t.Errorf("empty = (%d, %d), want zeros", a, tardy)
	}
	if a, tardy := parseAttendanceFingerprint("absences=x|junk"); a != 0 || tardy != 0 {
		t.Errorf("malformed = (%d, %d), want zeros", a, tardy)
	}
}

// TestProcessAttendance walks one course's counts through a term against a
// single checkpoint.
func TestProcessAttendance(t *testing.T) {
	steps := []struct {
		name             string
		absences         int
		tardies          int
		wantStaged       int
		wantAbsDelta     string
		wantTardiesDelta string
		wantChange       string
	}{
		{name: "clean record stages nothing", absences: 0, tardies: 0, wantStaged: 0},
		{name: "still clean", absences: 0, tardies: 0, wantStaged: 0},
		{name: "first absence", absences: 1, tardies: 0, wantStaged: 1, wantAbsDelta: "1", wantTardiesDelta: "0", wantChange: "new"},
		{name: "unchanged counts do not re-emit", absences: 1, tardies: 0, wantStaged: 0},
		{name: "a tardy", absences: 1, tardies: 1, wantStaged: 1, wantAbsDelta: "0", wantTardiesDelta: "1", wantChange: "updated"},
		{name: "two more absences", absences: 3, tardies: 1, wantStaged: 1, wantAbsDelta: "2", wantTardiesDelta: "0", wantChange: "updated"},
		{name: "a corrected absence", absences: 2, tardies: 1, wantStaged: 1, wantAbsDelta: "-1", wantTardiesDelta: "0", wantChange: "updated"},
	}

	env, _ := newTestEnv(t, &fakeClient{})
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			stepEnv, dir := newTestEnv(t, &fakeClient{})
			stepEnv.CP = env.CP

			res := ProcessAttendance(context.Background(), stepEnv, testKid,
				[]*pslib.Grade{attendanceGrade(step.absences, step.tardies), nil})
			if res.Staged != step.wantStaged || res.Failed != 0 {
				t.Fatalf("result = %+v, want Staged=%d", res, step.wantStaged)
			}
			items := readItems(t, dir)
			if len(items) != step.wantStaged {
				t.Fatalf("staged %d items, want %d", len(items), step.wantStaged)
			}
			if step.wantStaged == 0 {
				return
			}
			tags := items[0].Meta.Tags
			if tags["absences_delta"] != step.wantAbsDelta || tags["tardies_delta"] != step.wantTardiesDelta {
				t.Errorf("deltas = (%q, %q), want (%q, %q)",
					tags["absences_delta"], tags["tardies_delta"], step.wantAbsDelta, step.wantTardiesDelta)
			}
			if tags["change"] != step.wantChange || tags["surface"] != "attendance" || tags["kid"] != "k1" {
				t.Errorf("tags = %v", tags)
			}
			if items[0].Meta.DataSubject != "e_333333" {
				t.Errorf("data_subject = %q", items[0].Meta.DataSubject)
			}
		})
	}
}
