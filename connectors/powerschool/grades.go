package powerschool

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	pslib "github.com/leftathome/powerschool-go"
)

// gradeSurface is the per-kid surface for course grades.
const gradeSurface = "grade"

// termOrder is the reporting-term order used in fingerprints, tags and
// bodies. currentTermPriority is the order in which the "current" grade is
// picked: the latest term that has a grade posted.
var (
	termOrder           = []string{"Q1", "Q2", "S1", "Q3", "Q4", "S2"}
	currentTermPriority = []string{"Q4", "S2", "Q3", "Q2", "S1", "Q1"}
)

// termGrades returns a course's posted grades keyed by term.
func termGrades(g *pslib.Grade) map[string]string {
	return map[string]string{
		"Q1": strings.TrimSpace(g.Q1Grade),
		"Q2": strings.TrimSpace(g.Q2Grade),
		"S1": strings.TrimSpace(g.S1Grade),
		"Q3": strings.TrimSpace(g.Q3Grade),
		"Q4": strings.TrimSpace(g.Q4Grade),
		"S2": strings.TrimSpace(g.S2Grade),
	}
}

// gradeFingerprint is the checkpoint value for a course: every term grade
// in termOrder, "Q1=B+88|Q2=|...". It is kept readable (not hashed) so the
// previous current grade can be recovered from it on a change.
func gradeFingerprint(terms map[string]string) string {
	parts := make([]string, 0, len(termOrder))
	for _, t := range termOrder {
		parts = append(parts, t+"="+strings.ReplaceAll(terms[t], "|", "/"))
	}
	return strings.Join(parts, "|")
}

// parseGradeFingerprint is the inverse of gradeFingerprint. Unknown or
// malformed parts are ignored.
func parseGradeFingerprint(fp string) map[string]string {
	out := make(map[string]string, len(termOrder))
	for _, part := range strings.Split(fp, "|") {
		if k, v, ok := strings.Cut(part, "="); ok {
			out[k] = v
		}
	}
	return out
}

// currentGrade returns the latest term with a posted grade.
func currentGrade(terms map[string]string) (term, grade string) {
	for _, t := range currentTermPriority {
		if terms[t] != "" {
			return t, terms[t]
		}
	}
	return "", ""
}

// gradeCellRE splits a grade cell into its letter and percentage. The
// portal renders "B+<br>88"; the library flattens that to "B+88".
var gradeCellRE = regexp.MustCompile(`^([A-Za-z]{1,2}[+-]?)?\s*(\d{1,3}(?:\.\d+)?)?\s*%?$`)

// splitGrade separates a grade cell into letter and percent. A cell that
// fits neither shape is returned whole as the letter so nothing is lost.
func splitGrade(raw string) (letter, percent string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	m := gradeCellRE.FindStringSubmatch(raw)
	if m == nil {
		return raw, ""
	}
	return m[1], m[2]
}

// displayGrade renders a grade cell for a subject line or body.
func displayGrade(raw string) string {
	letter, percent := splitGrade(raw)
	switch {
	case letter != "" && percent != "":
		return letter + " (" + percent + "%)"
	case percent != "":
		return percent + "%"
	}
	return letter
}

// ProcessGrades stages one item per course whose posted grades are new or
// have changed since the last poll. A course with no grade posted in any
// term stages nothing and is not checkpointed.
func ProcessGrades(ctx context.Context, e Env, kid Kid, grades []*pslib.Grade) Result {
	var res Result
	rule, ok := matchRule(e, kid, gradeSurface)
	if !ok {
		return res
	}

	for _, g := range grades {
		if g == nil {
			continue
		}
		terms := termGrades(g)
		term, current := currentGrade(terms)
		if current == "" {
			continue
		}
		key := courseKey(g)
		fp := gradeFingerprint(terms)
		change, prev := Compare(e.CP, gradeSurface, kid.Name, key, fp)
		if change == ChangeNone {
			continue
		}
		_, previous := currentGrade(parseGradeFingerprint(prev))

		tags := map[string]string{"term": term, "grade": current}
		setTag(tags, "course", g.CourseName)
		setTag(tags, "period", g.Period)
		letter, percent := splitGrade(current)
		setTag(tags, "grade_letter", letter)
		setTag(tags, "grade_percent", percent)
		setTag(tags, "previous_grade", previous)
		for _, t := range termOrder {
			setTag(tags, "grade_"+strings.ToLower(t), terms[t])
		}

		subject := fmt.Sprintf("Grade: %s %s (%s)", g.CourseName, displayGrade(current), term)
		opts := itemOptions(e, rule, kid, gradeSurface, change, g.CourseName, subject, tags)

		if err := stage(ctx, e, gradeSurface, opts, buildGradeBody(g, terms, term, current, previous)); err != nil {
			slog.Warn("powerschool grade stage failed", "kid", kid.Name, "error", err)
			res.Failed++
			continue
		}
		if err := SaveFingerprint(e.CP, gradeSurface, kid.Name, key, fp); err != nil {
			// Item already committed; the next poll re-stages it.
			slog.Error("powerschool grade checkpoint save failed",
				"kid", kid.Name, "duplicate_risk", true, "error", err)
		}
		e.Tel.RecordItem(gradeSurface, kid.Name, change)
		res.Staged++
	}
	return res
}

// buildGradeBody synthesizes the plaintext content.raw body for a course
// grade. Everything a summariser needs is in the tags; the body is the
// same facts for a reader and for the scanner.
func buildGradeBody(g *pslib.Grade, terms map[string]string, term, current, previous string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Course: %s\n", oneLine(g.CourseName))
	if g.Period != "" {
		fmt.Fprintf(&b, "Period: %s\n", oneLine(g.Period))
	}
	if g.Teacher != "" {
		fmt.Fprintf(&b, "Teacher: %s\n", oneLine(g.Teacher))
	}
	fmt.Fprintf(&b, "Current grade (%s): %s\n", term, displayGrade(current))
	if previous != "" && previous != current {
		fmt.Fprintf(&b, "Previous grade: %s\n", displayGrade(previous))
	}
	for _, t := range termOrder {
		if terms[t] != "" {
			fmt.Fprintf(&b, "%s: %s\n", t, displayGrade(terms[t]))
		}
	}
	return b.String()
}
