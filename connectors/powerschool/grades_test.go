package powerschool

import (
	"context"
	"strings"
	"testing"

	pslib "github.com/leftathome/powerschool-go"
)

func TestSplitGrade(t *testing.T) {
	cases := []struct {
		raw, letter, percent string
	}{
		{"", "", ""},
		{"B+88", "B+", "88"},
		{"A-", "A-", ""},
		{"92", "", "92"},
		{"A 93.5", "A", "93.5"},
		{"P", "P", ""},
		{"  C 71% ", "C", "71"},
		{"[ i ]", "[ i ]", ""},
		{"--", "--", ""},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			letter, percent := splitGrade(tc.raw)
			if letter != tc.letter || percent != tc.percent {
				t.Errorf("splitGrade(%q) = (%q, %q), want (%q, %q)", tc.raw, letter, percent, tc.letter, tc.percent)
			}
		})
	}
}

func TestCurrentGrade(t *testing.T) {
	cases := []struct {
		name      string
		terms     map[string]string
		term, val string
	}{
		{"none", map[string]string{}, "", ""},
		{"q1 only", map[string]string{"Q1": "B"}, "Q1", "B"},
		{"q2 beats s1 and q1", map[string]string{"Q1": "B", "S1": "B+", "Q2": "A"}, "Q2", "A"},
		{"q3 beats first semester", map[string]string{"Q2": "A", "Q3": "C"}, "Q3", "C"},
		{"q4 beats s2", map[string]string{"S2": "B", "Q4": "A-"}, "Q4", "A-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			term, val := currentGrade(tc.terms)
			if term != tc.term || val != tc.val {
				t.Errorf("currentGrade = (%q, %q), want (%q, %q)", term, val, tc.term, tc.val)
			}
		})
	}
}

func TestGradeFingerprint_RoundTrip(t *testing.T) {
	terms := map[string]string{"Q1": "B+88", "Q2": "", "S1": "", "Q3": "", "Q4": "", "S2": ""}
	fp := gradeFingerprint(terms)
	if fp != "Q1=B+88|Q2=|S1=|Q3=|Q4=|S2=" {
		t.Errorf("fingerprint = %q", fp)
	}
	got := parseGradeFingerprint(fp)
	if got["Q1"] != "B+88" || got["Q2"] != "" {
		t.Errorf("parse = %v", got)
	}
}

// TestProcessGrades is the change-detection table: each step feeds one
// poll's grades into the same checkpoint and states how many items that
// poll must stage.
func TestProcessGrades(t *testing.T) {
	steps := []struct {
		name       string
		grades     []*pslib.Grade
		wantStaged int
		wantChange string // on the staged item, when wantStaged == 1
		wantPrev   string
	}{
		{
			name:       "first sight stages a baseline",
			grades:     []*pslib.Grade{sampleGrade("00100", "Example Science 7", "B+88")},
			wantStaged: 1, wantChange: "new",
		},
		{
			name:       "unchanged grade does not re-emit",
			grades:     []*pslib.Grade{sampleGrade("00100", "Example Science 7", "B+88")},
			wantStaged: 0,
		},
		{
			name:       "changed score re-emits",
			grades:     []*pslib.Grade{sampleGrade("00100", "Example Science 7", "A-91")},
			wantStaged: 1, wantChange: "updated", wantPrev: "B+88",
		},
		{
			name:       "still unchanged after the update",
			grades:     []*pslib.Grade{sampleGrade("00100", "Example Science 7", "A-91")},
			wantStaged: 0,
		},
		{
			name: "a new term grade re-emits and becomes current",
			grades: []*pslib.Grade{func() *pslib.Grade {
				g := sampleGrade("00100", "Example Science 7", "A-91")
				g.Q2Grade = "B80"
				return g
			}()},
			wantStaged: 1, wantChange: "updated", wantPrev: "A-91",
		},
		{
			name:       "a course with no posted grade stages nothing",
			grades:     []*pslib.Grade{sampleGrade("00200", "Example Advisory", ""), nil},
			wantStaged: 0,
		},
	}

	env, _ := newTestEnv(t, &fakeClient{})
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			// A fresh staging dir per step isolates what this poll staged;
			// the checkpoint carries over.
			stepEnv, dir := newTestEnv(t, &fakeClient{})
			stepEnv.CP = env.CP

			res := ProcessGrades(context.Background(), stepEnv, testKid, step.grades)
			if res.Staged != step.wantStaged || res.Failed != 0 {
				t.Fatalf("result = %+v, want Staged=%d Failed=0", res, step.wantStaged)
			}
			items := readItems(t, dir)
			if len(items) != step.wantStaged {
				t.Fatalf("staged %d items, want %d", len(items), step.wantStaged)
			}
			if step.wantStaged == 0 {
				return
			}
			tags := items[0].Meta.Tags
			if tags["change"] != step.wantChange {
				t.Errorf("change tag = %q, want %q", tags["change"], step.wantChange)
			}
			if tags["previous_grade"] != step.wantPrev {
				t.Errorf("previous_grade tag = %q, want %q", tags["previous_grade"], step.wantPrev)
			}
		})
	}
}

func TestProcessGrades_ItemShape(t *testing.T) {
	env, dir := newTestEnv(t, &fakeClient{})
	g := sampleGrade("00100", "Example Science 7", "B+88")

	if res := ProcessGrades(context.Background(), env, testKid, []*pslib.Grade{g}); res.Staged != 1 {
		t.Fatalf("result = %+v", res)
	}
	items := readItems(t, dir)
	if len(items) != 1 {
		t.Fatalf("staged %d items, want 1", len(items))
	}
	meta := items[0].Meta
	if meta.Source != "powerschool" || meta.Sender != "Example Science 7" || meta.DestinationAgent != "school" {
		t.Errorf("meta = %+v", meta)
	}
	if meta.DataSubject != "e_333333" {
		t.Errorf("data_subject = %q, want e_333333", meta.DataSubject)
	}
	if meta.Subject != "Grade: Example Science 7 B+ (88%) (Q1)" {
		t.Errorf("subject = %q", meta.Subject)
	}
	want := map[string]string{
		"kid": "k1", "surface": "grade", "change": "new",
		"course": "Example Science 7", "period": "1(A)",
		"term": "Q1", "grade": "B+88", "grade_letter": "B+", "grade_percent": "88",
		"grade_q1": "B+88",
	}
	for k, v := range want {
		if meta.Tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, meta.Tags[k], v)
		}
	}
	for _, absent := range []string{"previous_grade", "grade_q2", "grade_s1"} {
		if _, ok := meta.Tags[absent]; ok {
			t.Errorf("tag %s present, want absent", absent)
		}
	}
	if !strings.Contains(items[0].Body, "Current grade (Q1): B+ (88%)") {
		t.Errorf("body = %q", items[0].Body)
	}
}

func TestProcessGrades_NoRuleStagesNothing(t *testing.T) {
	env, dir := newTestEnv(t, &fakeClient{})
	stranger := Kid{Name: "k9", StudentID: "999999"}

	res := ProcessGrades(context.Background(), env, stranger, []*pslib.Grade{sampleGrade("00100", "Example Science 7", "B")})
	if res.Staged != 0 {
		t.Errorf("staged = %d, want 0 without a rule", res.Staged)
	}
	if got := len(readItems(t, dir)); got != 0 {
		t.Errorf("staging dir has %d items, want 0", got)
	}
}

// TestProcessGrades_KidsDoNotShareCheckpoints guards the per-kid scope of
// the checkpoint key: the same course id under two kids is two records.
func TestProcessGrades_KidsDoNotShareCheckpoints(t *testing.T) {
	env, dir := newTestEnv(t, &fakeClient{})
	g := []*pslib.Grade{sampleGrade("00100", "Example Science 7", "B")}
	k2 := Kid{Name: "k2", StudentID: "111112"}

	ProcessGrades(context.Background(), env, testKid, g)
	if res := ProcessGrades(context.Background(), env, k2, g); res.Staged != 1 {
		t.Fatalf("k2 result = %+v, want 1 staged", res)
	}
	subjects := map[string]int{}
	for _, it := range readItems(t, dir) {
		subjects[it.Meta.DataSubject]++
	}
	if subjects["e_333333"] != 1 || subjects["e_444444"] != 1 {
		t.Errorf("data_subject counts = %v, want one each", subjects)
	}
}
