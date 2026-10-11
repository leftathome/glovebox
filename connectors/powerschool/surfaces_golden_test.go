package powerschool

import (
	"context"
	"testing"

	pslib "github.com/leftathome/powerschool-go"
)

// TestSurfaceFingerprints_Golden holds the checkpoint key and value of one
// fixed record per surface to the literals the connector as first merged
// (commit d887f7b) wrote for the same records; they were obtained by
// running this test's fixtures on that commit.
//
// Cleaning untrusted text applies to what is rendered -- bodies, tags,
// subject, sender -- and must never reach a key or a fingerprint: if it
// did, every record already checkpointed would be staged again on upgrade.
// The fixtures therefore carry exactly the text cleaning would alter (line
// breaks, tabs, doubled and trailing spaces).
func TestSurfaceFingerprints_Golden(t *testing.T) {
	env, _ := newTestEnv(t, &fakeClient{})
	ctx := context.Background()

	g := sampleGrade("00100", "Example  Science 7\t", "B+88")
	g.Q2Grade, g.S1Grade = "A-\n91", " B 85 "
	g.Absences, g.Tardies = 2, 1
	ProcessGrades(ctx, env, testKid, []*pslib.Grade{g})
	ProcessAttendance(ctx, env, testKid, []*pslib.Grade{g})

	att := attendanceFixture("111111", codeDay(day(2026, 10, 6), "V U\n"))
	att.Courses[0].CourseName = "EXAMPLE  SCIENCE 7A\t"
	ProcessAttendanceDays(ctx, env, testKid, att, AttendanceDaysConfig{LookbackDays: 30})

	hist := historyFixture("111111", "A-\r\n(honors)")
	hist.Grades[0].School = " Example  ES\n"
	ProcessTermGrades(ctx, env, testKid, hist, TermGradesConfig{LookbackDays: 3650})

	comments := commentsFixture("111111", "Q1", true, "  Line one.\r\n\tLine  two. \n\n")
	comments.Comments[1].Teacher = "Lindqvist,  Marta\t"
	ProcessTeacherComments(ctx, env, testKid, comments, TeacherCommentsConfig{})

	want := []struct{ surface, id, fingerprint string }{
		{gradeSurface, "00100", "Q1=B+88|Q2=A-\n91|S1=B 85|Q3=|Q4=|S2="},
		{attendanceSurface, "00100", "absences=2|tardies=1"},
		{attendanceDaySurface, "example  science 7a|1(A):2026-10-06", "code=V U\n"},
		{termGradeSurface, "SCI6001:2026-06:1|example  es|6|example science 6", "grade=A-\r\n(honors)|earned=1|attempted=1|gpa=true"},
		{teacherCommentSurface, "SCI7001|1(A):Q1", "9c1ff7393ce68a3d"},
	}
	for _, w := range want {
		got, ok := env.CP.Load(CheckpointKey(w.surface, testKid.Name, w.id))
		if !ok {
			t.Errorf("%s: no checkpoint under id %q; the record's key changed", w.surface, w.id)
			continue
		}
		if got != w.fingerprint {
			t.Errorf("%s: fingerprint = %q, want %q (as written by d887f7b)", w.surface, got, w.fingerprint)
		}
	}
}
