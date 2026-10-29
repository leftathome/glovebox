package powerschool

import (
	"fmt"
	"time"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	pslib "github.com/leftathome/powerschool-go"
)

// AssignmentRecordKey is the record key of the n-th row (1-based) with
// assignment id in a course's read: the item path's checkpoint id
// (assignmentKey) made safe for the journal. Exported for the seed (S15).
func AssignmentRecordKey(g *pslib.Grade, id string, n int) string {
	if n > 1 {
		id = fmt.Sprintf("%s#%d", id, n)
	}
	return records.SafeKey(courseKey(g) + ":" + id)
}

// BuildAssignments builds one assignment snapshot per course read in this
// turn: a complete view of the course, every assignment in it whatever its
// age (the lookback is an item rule). A course whose read failed is not in
// reads and gets no snapshot, so nothing is removed on a partial view.
//
// Text fields carry what the library returned (Fields.Text), not the
// item's cleaned and cut blocks. onCut counts a text value cut to 64 KiB.
func BuildAssignments(b Binding, reads []courseRead, tz *time.Location, observedAt time.Time, onCut func()) []journalapi.Snapshot {
	var out []journalapi.Snapshot
	for _, r := range reads {
		g := r.grade
		if g == nil {
			continue
		}
		recs := []journalapi.Record{}
		seen := map[string]int{}
		for _, a := range r.assignments {
			if a == nil {
				continue
			}
			key := records.SafeKey(assignmentKey(g, a.ID, seen))
			recs = append(recs, b.record(key, assignmentFields(a, g, tz, onCut)))
		}
		s := records.NewSnapshot(TypeAssignment, recordSchemaVersion, b.scope("assignments", records.Part("course", courseKey(g))),
			true, nil, observedAt, recs)
		if b.fits(s) {
			out = append(out, s)
		}
	}
	return out
}

func assignmentFields(a *pslib.Assignment, g *pslib.Grade, tz *time.Location, onCut func()) *records.Fields {
	f := records.NewFields(onCut)
	f.String("course", g.CourseName)
	f.String("category", a.Category)
	f.String("title", a.Title)
	f.String("letter", a.LetterGrade)
	if _, start, ok := dueDay(a, tz); ok {
		f.Date("due", start)
	} else {
		f.Null("due")
	}
	// The library leaves Score nil for an entered zero; a graded status
	// says it was entered (isScored).
	score := a.Score
	if score == nil && a.Status == pslib.StatusGraded {
		zero := 0.0
		score = &zero
	}
	f.Number("score", score)
	if a.MaxScore == 0 && !isScored(a) {
		f.Null("max_score")
	} else {
		ms := a.MaxScore
		f.Number("max_score", &ms)
	}
	f.Number("percent", a.Percentage)
	flags := flagSet(a)
	for _, flag := range assignmentFlags {
		f.Bool(string(flag), flags[flag])
	}
	if a.HasTeacherComment {
		f.Text("teacher_comment", a.TeacherComment)
	} else {
		f.Null("teacher_comment")
	}
	f.Text("description", a.Description)
	// As the item does (textOf): a category description that says what
	// the description says is not repeated.
	desc, _ := boundText(a.Description)
	cat, _ := boundText(a.CategoryDescription)
	if cat == desc {
		f.Null("category_description")
	} else {
		f.Text("category_description", a.CategoryDescription)
	}
	return f
}
