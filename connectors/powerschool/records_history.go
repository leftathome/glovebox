package powerschool

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	pslib "github.com/leftathome/powerschool-go"
)

// term_grade and term_comment come from the two throttled pages: the grade
// history (most polls do not read it, and then send nothing) and the
// teacher comments, one page per reporting term.

// TermGradeRecordKey is the record key of the n-th row (1-based) of a
// stored grade's key on the history page (termGradeKey, decision S5).
// Exported for the seed (S15).
func TermGradeRecordKey(g *pslib.HistoricalGrade, n int) string {
	return records.SafeKey(repeatKey(termGradeKey(g, map[string]int{}), n))
}

// TermCommentRecordKey is the record key of the n-th comment (1-based)
// with a comment's key on a term's page (teacherCommentKey, decision S6).
// Exported for the seed (S15).
func TermCommentRecordKey(cm *pslib.TeacherComment, term string, n int) string {
	return records.SafeKey(repeatKey(teacherCommentKey(cm, term, map[string]int{}), n))
}

func repeatKey(key string, n int) string {
	if n > 1 {
		return fmt.Sprintf("%s#%d", key, n)
	}
	return key
}

// BuildTermGrades builds the term_grade snapshot of a history page read in
// this turn: one complete scope per subject, one record per row with a
// letter grade. No page, no snapshot.
func BuildTermGrades(b Binding, page *pslib.GradeHistoryPage, observedAt time.Time) []journalapi.Snapshot {
	if page == nil {
		return nil
	}
	recs := []journalapi.Record{}
	seen := map[string]int{}
	for _, g := range page.Grades {
		if g == nil || strings.TrimSpace(g.LetterGrade) == "" {
			continue
		}
		key := records.SafeKey(termGradeKey(g, seen))
		f := records.NewFields(nil)
		f.String("course", g.CourseName)
		f.String("term", termGradeTerm(g))
		f.String("grade", g.LetterGrade)
		earned, attempted := g.CreditEarned, g.CreditAttempted
		f.Number("earned", &earned)
		f.Number("attempted", &attempted)
		recs = append(recs, b.record(key, f))
	}
	s := records.NewSnapshot(TypeTermGrade, recordSchemaVersion, b.scope("term_grades"), true, nil, observedAt, recs)
	if !b.fits(s) {
		return nil
	}
	return []journalapi.Snapshot{s}
}

// BuildTermComments builds one term_comment snapshot per comments page
// accepted in this turn: complete for its reporting term only (the scope's
// term part, decision S4), since each term's page is read on its own
// schedule. A page that does not confirm its term is never used. One
// record per comment with visible text; the comment is the library's text
// (Fields.Text), and the teacher's name is not a field.
//
// The binding's audience must be exactly [guardians] (ValidateConfig
// guarantees it); otherwise nothing is built, as stage refuses the item.
func BuildTermComments(b Binding, pages []*pslib.TeacherCommentsPage, observedAt time.Time, onCut func()) []journalapi.Snapshot {
	if !guardiansOnly(b.Audience) {
		b.skip("audience_not_guardians_only")
		slog.Error("powerschool term_comment records not built: the audience is not exactly [guardians]",
			"audience_tokens", len(b.Audience))
		return nil
	}
	var out []journalapi.Snapshot
	for _, page := range pages {
		if page == nil || !page.TermConfirmed || strings.TrimSpace(page.Term) == "" {
			continue
		}
		term := strings.TrimSpace(page.Term)
		recs := []journalapi.Record{}
		seen := map[string]int{}
		for _, cm := range page.Comments {
			if cm == nil {
				continue
			}
			// The item path's test for "a comment": visible text once
			// cleaned. The record carries the text as received.
			if visible, _ := boundText(cm.Comment); visible == "" {
				continue
			}
			key := records.SafeKey(teacherCommentKey(cm, term, seen))
			f := records.NewFields(onCut)
			f.String("course", cm.CourseName)
			f.String("term", term)
			f.Text("comment", cm.Comment)
			recs = append(recs, b.record(key, f))
		}
		s := records.NewSnapshot(TypeTermComment, recordSchemaVersion,
			b.scope("term_comments", records.Part("term", strings.ToUpper(term))), true, nil, observedAt, recs)
		if b.fits(s) {
			out = append(out, s)
		}
	}
	return out
}
