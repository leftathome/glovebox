package powerschool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// Three more surfaces, each read from its own portal page, each behind its
// own flag, each off unless switched on:
//
//	attendance_day   /guardian/attendance.html       one item per (course, day) with a code
//	term_grade       /guardian/termgrades.html       one item per stored grade (course, completion month)
//	teacher_comment  /guardian/teachercomments.html  one item per (course, term) with a comment
//
// Like everything else they describe the student the session has selected,
// so they are read only inside a kid's verified turn (connector.go pollKid),
// and each page must itself mark that kid's student (readExtraPages).
const (
	attendanceDaySurface  = "attendance_day"
	termGradeSurface      = "term_grade"
	teacherCommentSurface = "teacher_comment"
)

// extraPages is what the extra-page phase of a kid's turn read. A nil page
// means the surface is off, was not due, or was skipped for this poll.
type extraPages struct {
	history    *pslib.GradeHistoryPage
	attendance *pslib.AttendancePage
	comments   []*pslib.TeacherCommentsPage
	// readHistory records that the throttled grade history was actually
	// fetched, so its "last read" time is only advanced after a turn that
	// kept the result.
	readHistory bool
	// Teacher comments are throttled per reporting term. readCommentTerms
	// lists the terms whose page was read and accepted in this turn;
	// readCommentList says the bare page was read, and commentTermList is
	// the list of terms it offered.
	readCommentTerms []string
	readCommentList  bool
	commentTermList  []string
	// needConfirm is true when a page was read after the last verified
	// home page: the turn must end with one more selection check.
	needConfirm bool
}

// lastReadKey is the checkpoint key holding when a throttled surface was
// last read for a kid.
func lastReadKey(surface, kid string) string {
	return "meta:last_read:" + surface + ":" + kid
}

// surfaceDue reports whether a surface with a minimum interval should be read in
// this poll.
func surfaceDue(cp connector.Checkpoint, surface, kid string, minInterval time.Duration, now time.Time) bool {
	if minInterval <= 0 {
		return true
	}
	raw, ok := cp.Load(lastReadKey(surface, kid))
	if !ok {
		return true
	}
	last, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return true
	}
	return !now.Before(last.Add(minInterval))
}

// checkPageStudent puts a page's own selected-student evidence to the kid
// whose turn it is. Every guardian page carries the portal's student
// switcher with exactly one entry marked (verified live), and the library
// refuses to return a page that does not; the connector checks again here
// rather than rely on that. The comparison is on the switcher id only --
// never on a display name.
func checkPageStudent(ps pslib.PageStudent, kid Kid) error {
	switch {
	case ps.SelectedID == "":
		return ErrSelectionUnknown
	case ps.SelectedID != kid.StudentID:
		return ErrSelectionChanged
	}
	return nil
}

// extraReader carries the state of the extra-page phase of one kid's turn.
type extraReader struct {
	c   *PowerSchoolConnector
	env Env
	kid Kid
	// requests counts the turn's extra-page requests, for pacing.
	requests int
	res      Result
}

// pace spaces the extra-page requests like the per-course ones.
func (r *extraReader) pace(ctx context.Context) error {
	r.requests++
	if r.requests == 1 || r.c.cfg.Assignments.CourseDelayMS <= 0 {
		return nil
	}
	return sleep(ctx, time.Duration(r.c.cfg.Assignments.CourseDelayMS)*time.Millisecond)
}

// read fetches one extra page and checks that the page itself marks this
// kid's student as selected. surface is used for the span name only.
func (r *extraReader) read(ctx context.Context, surface string, fetch func(context.Context) (pslib.PageStudent, error)) error {
	if err := r.pace(ctx); err != nil {
		return err
	}
	ctx, span := r.env.Tel.StartSpan(ctx, "powerschool.lib."+surface, attribute.String("kid", r.kid.Name))
	ps, err := fetch(ctx)
	span.End()
	if err != nil {
		return err
	}
	return checkPageStudent(ps, r.kid)
}

// readExtraPages is the extra-page phase of a kid's turn. It runs after the
// kid's student was selected and the home page verified.
//
// Every page read here must itself mark this kid's student as selected. A
// page that marks another student, or none, ends the kid's turn with an
// error: nothing is staged for the kid. Errors that only concern one
// surface (a page that would not parse, an HTTP status, a comments page for
// the wrong term) are logged, counted and that surface is skipped.
func (c *PowerSchoolConnector) readExtraPages(ctx context.Context, env Env, kid Kid) (*extraPages, Result, error) {
	out := &extraPages{}
	r := &extraReader{c: c, env: env, kid: kid}
	now := env.now()

	// skip handles a fetch error: fatal ones (auth, another student's
	// page, a cancelled context) end the turn, the rest skip the surface.
	skip := func(surface string, err error) error {
		class := classifyErr(err)
		env.Tel.RecordSurfaceError(surface, class, kid.Name)
		r.res.Failed++
		switch class {
		case "auth":
			r.res.AuthErr = err
			return err
		case "selection_changed", "selection_mismatch", "selection_unknown", "timeout":
			return err
		}
		slog.Warn("powerschool page read failed; surface skipped for this poll",
			append([]any{"kid", kid.Name, "surface", surface}, errAttrs(err)...)...)
		return nil
	}

	if c.cfg.TermGradesEnabled() && surfaceDue(env.CP, termGradeSurface, kid.Name, hours(c.cfg.TermGrades.MinIntervalHours), now) {
		var page *pslib.GradeHistoryPage
		err := r.read(ctx, termGradeSurface, func(ctx context.Context) (pslib.PageStudent, error) {
			p, err := env.Client.GetGradeHistoryPage(ctx)
			if err != nil || p == nil {
				return pslib.PageStudent{}, orNoPage(err)
			}
			page = p
			return p.Student, nil
		})
		if err != nil {
			if err := skip(termGradeSurface, err); err != nil {
				return nil, r.res, err
			}
		} else {
			out.history, out.readHistory = page, true
		}
	}

	if c.cfg.AttendanceDaysEnabled() {
		var page *pslib.AttendancePage
		err := r.read(ctx, attendanceDaySurface, func(ctx context.Context) (pslib.PageStudent, error) {
			p, err := env.Client.GetAttendance(ctx)
			if err != nil || p == nil {
				return pslib.PageStudent{}, orNoPage(err)
			}
			page = p
			return p.Student, nil
		})
		if err != nil {
			if err := skip(attendanceDaySurface, err); err != nil {
				return nil, r.res, err
			}
		} else {
			out.attendance = page
		}
	}

	if c.cfg.TeacherCommentsEnabled() {
		interval := hours(c.cfg.TeacherComments.MinIntervalHours)
		fetchTerm := func(term string) (*pslib.TeacherCommentsPage, error) {
			var page *pslib.TeacherCommentsPage
			err := r.read(ctx, teacherCommentSurface, func(ctx context.Context) (pslib.PageStudent, error) {
				p, err := env.Client.GetTeacherComments(ctx, term)
				if err != nil || p == nil {
					return pslib.PageStudent{}, orNoPage(err)
				}
				page = p
				return p.Student, nil
			})
			return page, err
		}

		// Which terms. From config, or from the page's own selector: the
		// bare page (itself throttled) lists them, and the list is kept in
		// the checkpoint so that a term whose page was refused can be
		// asked for again without reading the bare page again.
		terms := c.cfg.TeacherComments.Terms
		if len(terms) == 0 {
			if surfaceDue(env.CP, teacherCommentSurface, kid.Name, interval, now) {
				page, err := fetchTerm("")
				if err != nil {
					if err := skip(teacherCommentSurface, err); err != nil {
						return nil, r.res, err
					}
				} else {
					out.commentTermList, out.readCommentList = page.Terms, true
					if page.TermConfirmed {
						out.comments = append(out.comments, page)
						out.readCommentTerms = append(out.readCommentTerms, page.Term)
					}
				}
			}
			terms = out.commentTermList
			if !out.readCommentList {
				terms = loadTermList(env.CP, kid.Name)
			}
		}

		// A selector with an absurd number of options must not turn into
		// that many requests against the account.
		if max := c.cfg.TeacherComments.MaxTerms; max > 0 && len(terms) > max {
			if c.termCapLogged.CompareAndSwap(false, true) {
				slog.Warn("powerschool teacher comments: more reporting terms offered than "+
					"teacher_comments.max_terms; reading only the first ones",
					"kid", kid.Name, "terms_offered", len(terms), "max_terms", max)
			}
			terms = terms[:max]
		}

		for _, term := range terms {
			if alreadyHave(out.comments, term) || !surfaceDue(env.CP, termSurface(term), kid.Name, interval, now) {
				continue
			}
			page, err := fetchTerm(term)
			if err != nil {
				// A refused page (another term's, an error status) leaves
				// this term's last-read time alone: it is asked for again
				// at the next poll, not in 24 hours.
				if err := skip(teacherCommentSurface, err); err != nil {
					return nil, r.res, err
				}
				continue
			}
			out.comments = append(out.comments, page)
			out.readCommentTerms = append(out.readCommentTerms, term)
		}
	}
	// Any page read here came after the home page that opened the turn:
	// the turn must end with one more selection check.
	out.needConfirm = r.requests > 0
	return out, r.res, nil
}

// termSurface is the name under which one reporting term's comments page
// is throttled.
func termSurface(term string) string {
	return teacherCommentSurface + ":" + strings.ToUpper(term)
}

// termListKey is the checkpoint key holding the reporting terms a kid's
// comments page offered when it was last listed.
func termListKey(kid string) string {
	return "meta:comment_terms:" + kid
}

func loadTermList(cp connector.Checkpoint, kid string) []string {
	raw, ok := cp.Load(termListKey(kid))
	if !ok || raw == "" {
		return nil
	}
	var out []string
	for _, term := range strings.Split(raw, ",") {
		if isTerm(term) {
			out = append(out, term)
		}
	}
	return out
}

// orNoPage turns "no error and no page" into an error.
func orNoPage(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("powerschool: library returned no page")
}

func alreadyHave(pages []*pslib.TeacherCommentsPage, term string) bool {
	for _, p := range pages {
		if p.TermConfirmed && strings.EqualFold(p.Term, term) {
			return true
		}
	}
	return false
}

func hours(n int) time.Duration { return time.Duration(n) * time.Hour }

// stageExtraPages stages what readExtraPages read, after the whole turn has
// been verified. It also advances the "last read" time of the throttled
// surfaces.
func (c *PowerSchoolConnector) stageExtraPages(ctx context.Context, env Env, kid Kid, pages *extraPages) Result {
	var res Result
	if pages == nil {
		return res
	}
	stamp := env.now().UTC().Format(time.RFC3339)
	if pages.history != nil {
		res.add(ProcessTermGrades(ctx, env, kid, pages.history, c.cfg.TermGrades))
	}
	if pages.readHistory {
		_ = env.CP.Save(lastReadKey(termGradeSurface, kid.Name), stamp)
	}
	if pages.attendance != nil {
		res.add(ProcessAttendanceDays(ctx, env, kid, pages.attendance, c.cfg.AttendanceDays))
	}
	for _, page := range pages.comments {
		res.add(ProcessTeacherComments(ctx, env, kid, page, c.cfg.TeacherComments))
	}
	if pages.readCommentList {
		_ = env.CP.Save(lastReadKey(teacherCommentSurface, kid.Name), stamp)
		_ = env.CP.Save(termListKey(kid.Name), strings.Join(pages.commentTermList, ","))
	}
	// Only terms whose page was accepted: a refused term stays due.
	for _, term := range pages.readCommentTerms {
		_ = env.CP.Save(lastReadKey(termSurface(term), kid.Name), stamp)
	}
	return res
}

// --- attendance_day -----------------------------------------------------

// attendanceCourseKey identifies a course row of the attendance page: the
// course, its period and the enrolment's start date. A student who drops a
// section and enrols in it again has two rows with the same name and
// period; without the date they would share checkpoint keys and re-stage
// each other's days as "updated" at every poll.
func attendanceCourseKey(c *pslib.AttendanceCourse) string {
	key := strings.ToLower(strings.TrimSpace(c.CourseName)) + "|" + strings.TrimSpace(c.Expression)
	if !c.EnrollDate.IsZero() {
		key += "|" + c.EnrollDate.Format("2006-01-02")
	}
	return key
}

// attendanceKindUnknown is the kind of a code whose legend category is not
// in the configured mapping (or that is not in the legend at all). Such a
// day is still staged.
const attendanceKindUnknown = "unknown"

// defaultAttendanceKinds maps the coarse `kind` tag to the legend
// categories it covers. The categories are the district's (the part before
// the hyphen in "CODE=CATEGORY-Description"); the kinds are the
// connector's. "authorized" is an authorized activity -- a field trip, a
// student elsewhere in the building: staged, but tagged so a summariser
// does not count it as an absence.
func defaultAttendanceKinds() map[string][]string {
	return map[string][]string{
		"absence_excused":      {"EA"},
		"absence_unexcused":    {"UA", "A"},
		"late_excused":         {"EL"},
		"late_unexcused":       {"UL", "L"},
		"left_early_excused":   {"LE"},
		"left_early_unexcused": {"LU"},
		"authorized":           {"AUTH"},
	}
}

// kindOf returns the kind a legend category maps to.
func (c AttendanceDaysConfig) kindOf(category string) string {
	if category != "" {
		for kind, categories := range c.Kinds {
			for _, cat := range categories {
				if strings.EqualFold(cat, category) {
					return kind
				}
			}
		}
	}
	return attendanceKindUnknown
}

// ProcessAttendanceDays stages one item per (course, day) on which the
// student was marked with an attendance code.
//
// The legend is read from the page each time (CODE=CATEGORY-Description).
// The `kind` tag is derived from the code's CATEGORY through
// attendance_days.kinds; a category the mapping does not know -- or a code
// the legend does not list -- gets kind "unknown" and is staged all the
// same, so a new code is seen rather than dropped.
//
// Blank cells (present) stage nothing. "-" and "." are not in the legend:
// they are non-events (no class that day / nothing marked) and are never
// staged; they are counted in powerschool_attendance_non_event_cells_total
// so that a change in how often they appear would be noticed.
//
// A day's item is staged again, as "updated", when its code changes (an
// unexcused absence that becomes excused). A code that is later removed
// altogether stages nothing: the page then shows a blank cell, which is
// indistinguishable from a day that was never marked.
//
// First-sight backlog: a coded day seen for the first time that is older
// than attendance_days.lookback_days is recorded without being staged, so
// enabling the surface mid-year does not stage the whole year's absences.
func ProcessAttendanceDays(ctx context.Context, e Env, kid Kid, page *pslib.AttendancePage, cfg AttendanceDaysConfig) Result {
	var res Result
	rule, ok := matchRule(e, kid, attendanceDaySurface)
	if !ok || page == nil {
		return res
	}
	if cfg.Kinds == nil {
		cfg.Kinds = defaultAttendanceKinds()
	}
	n := e.now().In(e.tz())
	cutoff := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -cfg.LookbackDays)

	dashes, dots := 0, 0
	for _, course := range page.Courses {
		if course == nil {
			continue
		}
		for _, day := range course.Days {
			switch day.Kind {
			case pslib.CellNoMeeting:
				dashes++
			case pslib.CellUnmarked:
				dots++
			}
			if day.Kind != pslib.CellCode || day.Code == "" {
				continue
			}
			entry, _ := page.Describe(day.Code)
			kind := cfg.kindOf(entry.Category)
			date := day.Date.Format("2006-01-02")
			key := attendanceCourseKey(course) + ":" + date
			fp := "code=" + day.Code
			change, prev := Compare(e.CP, attendanceDaySurface, kid.Name, key, fp)
			if change == ChangeNone {
				continue
			}
			if change == ChangeNew && day.Date.Before(cutoff) {
				if err := SaveFingerprint(e.CP, attendanceDaySurface, kid.Name, key, fp); err != nil {
					slog.Error("powerschool attendance day baseline save failed", "kid", kid.Name, "error", err)
				}
				continue
			}

			tags := map[string]string{"date": date, "code": day.Code, "kind": kind}
			setTag(tags, "category", entry.Category)
			setTag(tags, "code_description", entry.Description)
			setTag(tags, "course", course.CourseName)
			setTag(tags, "period", course.Expression)
			setTag(tags, "previous_code", strings.TrimPrefix(prev, "code="))

			label := entry.Description
			if label == "" {
				label = day.Code
			}
			subject := fmt.Sprintf("Attendance: %s on %s (%s)", course.CourseName, date, label)
			opts := itemOptions(e, rule, kid, attendanceDaySurface, change, course.CourseName, subject, tags)

			var b strings.Builder
			fmt.Fprintf(&b, "Course: %s\n", oneLine(course.CourseName))
			if course.Expression != "" {
				fmt.Fprintf(&b, "Period: %s\n", oneLine(course.Expression))
			}
			fmt.Fprintf(&b, "Date: %s (%s)\n", date, day.Date.Weekday())
			fmt.Fprintf(&b, "Code: %s\n", oneLine(day.Code))
			if entry.Description != "" {
				fmt.Fprintf(&b, "Meaning: %s\n", oneLine(entry.Description))
			}
			fmt.Fprintf(&b, "Kind: %s\n", kind)
			if p := strings.TrimPrefix(prev, "code="); p != "" && p != day.Code {
				fmt.Fprintf(&b, "Previously: %s\n", oneLine(p))
			}

			if err := stage(ctx, e, attendanceDaySurface, opts, b.String()); err != nil {
				slog.Warn("powerschool attendance day stage failed", "kid", kid.Name, "error", err)
				res.Failed++
				continue
			}
			if err := SaveFingerprint(e.CP, attendanceDaySurface, kid.Name, key, fp); err != nil {
				slog.Error("powerschool attendance day checkpoint save failed",
					"kid", kid.Name, "duplicate_risk", true, "error", err)
			}
			e.Tel.RecordItem(attendanceDaySurface, kid.Name, change)
			res.Staged++
		}
	}
	e.Tel.RecordAttendanceNonEvents(kid.Name, "dash", dashes)
	e.Tel.RecordAttendanceNonEvents(kid.Name, "dot", dots)
	slog.Debug("powerschool attendance non-event cells", "kid", kid.Name, "dash", dashes, "dot", dots)
	return res
}

// --- term_grade ---------------------------------------------------------

// ProcessTermGrades stages one item per stored grade on the grade history
// page: one per (course, completion month).
//
// The page is the portal's record of final grades, with credit; it has no
// percentages and names no reporting term. The `term` tag is therefore the
// completion month ("2026-01"), which is what distinguishes a first
// semester's stored grade from the second's. The in-progress quarter grades
// are the `grade` surface.
//
// First-sight backlog: a grade seen for the first time that was completed
// more than term_grades.lookback_days ago is recorded without being staged.
func ProcessTermGrades(ctx context.Context, e Env, kid Kid, page *pslib.GradeHistoryPage, cfg TermGradesConfig) Result {
	var res Result
	rule, ok := matchRule(e, kid, termGradeSurface)
	if !ok || page == nil {
		return res
	}
	n := e.now().In(e.tz())
	cutoff := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -cfg.LookbackDays)

	seen := map[string]int{}
	for _, g := range page.Grades {
		if g == nil || strings.TrimSpace(g.LetterGrade) == "" {
			continue
		}
		term := strings.TrimSpace(g.DateCompletedText)
		if !g.DateCompleted.IsZero() {
			term = g.DateCompleted.Format("2006-01")
		}
		if term == "" {
			term = "undated"
		}
		course := g.CourseNumber
		if course == "" {
			course = "name:" + strings.ToLower(strings.TrimSpace(g.CourseName))
		}
		// The same course can be stored more than once for one month (two
		// schools, a repeated course). The rows are told apart by content
		// that does not change when a grade is corrected -- credit
		// attempted, school, grade level, course name -- so the key does
		// not depend on the order the page lists them in. Only rows that
		// are identical in all of that fall back to their position.
		key := course + ":" + term + ":" + strings.Join([]string{
			formatFloat(g.CreditAttempted),
			strings.ToLower(strings.TrimSpace(g.School)),
			strconv.Itoa(g.GradeLevel),
			strings.ToLower(strings.TrimSpace(g.CourseName)),
		}, "|")
		seen[key]++
		if c := seen[key]; c > 1 {
			key = fmt.Sprintf("%s#%d", key, c)
		}
		fp := strings.Join([]string{
			"grade=" + g.LetterGrade,
			"earned=" + formatFloat(g.CreditEarned),
			"attempted=" + formatFloat(g.CreditAttempted),
			"gpa=" + formatBool(g.IncludedInGPA),
		}, "|")
		change, prev := Compare(e.CP, termGradeSurface, kid.Name, key, fp)
		if change == ChangeNone {
			continue
		}
		if change == ChangeNew && !g.DateCompleted.IsZero() && g.DateCompleted.Before(cutoff) {
			if err := SaveFingerprint(e.CP, termGradeSurface, kid.Name, key, fp); err != nil {
				slog.Error("powerschool term grade baseline save failed", "kid", kid.Name, "error", err)
			}
			continue
		}
		previous := ""
		for _, part := range strings.Split(prev, "|") {
			if v, found := strings.CutPrefix(part, "grade="); found {
				previous = v
			}
		}

		tags := map[string]string{
			"term":             term,
			"grade":            g.LetterGrade,
			"credit_earned":    formatFloat(g.CreditEarned),
			"credit_attempted": formatFloat(g.CreditAttempted),
			"included_in_gpa":  formatBool(g.IncludedInGPA),
		}
		setTag(tags, "course", g.CourseName)
		setTag(tags, "course_number", g.CourseNumber)
		setTag(tags, "school", g.School)
		setTag(tags, "date_completed", g.DateCompletedText)
		if g.GradeLevel > 0 {
			tags["grade_level"] = strconv.Itoa(g.GradeLevel)
		}
		if previous != g.LetterGrade {
			setTag(tags, "previous_grade", previous)
		}

		subject := fmt.Sprintf("Term grade: %s %s (%s)", g.CourseName, g.LetterGrade, term)
		opts := itemOptions(e, rule, kid, termGradeSurface, change, g.CourseName, subject, tags)

		var b strings.Builder
		fmt.Fprintf(&b, "Course: %s\n", oneLine(g.CourseName))
		if g.CourseNumber != "" {
			fmt.Fprintf(&b, "Course number: %s\n", oneLine(g.CourseNumber))
		}
		fmt.Fprintf(&b, "Completed: %s\n", oneLine(term))
		fmt.Fprintf(&b, "Final grade: %s\n", oneLine(g.LetterGrade))
		if previous != "" && previous != g.LetterGrade {
			fmt.Fprintf(&b, "Previously recorded: %s\n", oneLine(previous))
		}
		fmt.Fprintf(&b, "Credit: %s earned of %s attempted\n", formatFloat(g.CreditEarned), formatFloat(g.CreditAttempted))
		if !g.IncludedInGPA {
			b.WriteString("Not included in GPA\n")
		}
		if g.School != "" {
			fmt.Fprintf(&b, "School: %s\n", oneLine(g.School))
		}

		if err := stage(ctx, e, termGradeSurface, opts, b.String()); err != nil {
			slog.Warn("powerschool term grade stage failed", "kid", kid.Name, "error", err)
			res.Failed++
			continue
		}
		if err := SaveFingerprint(e.CP, termGradeSurface, kid.Name, key, fp); err != nil {
			slog.Error("powerschool term grade checkpoint save failed",
				"kid", kid.Name, "duplicate_risk", true, "error", err)
		}
		e.Tel.RecordItem(termGradeSurface, kid.Name, change)
		res.Staged++
	}
	return res
}

// --- teacher_comment ----------------------------------------------------

// ProcessTeacherComments stages one item per (course, term) that has a
// non-empty teacher comment. It is staged again when the text changes.
//
// These items are for the parents only: the rule that routes them must not
// name the child in its audience (ValidateConfig enforces it). The
// teacher's name goes in the body, never in a tag.
//
// The body is the fact lines, each one cleaned line, then the comment as a
// "Comment:" block: every line indented, cut at maxTextBlockBytes with the
// truncation marker (writeTextBlock). Nothing a teacher types starts a
// line in column one.
//
// The term is half of the item's identity. The portal marks the term a
// comments page shows (the selected option of its term selector), and the
// library refuses a page that marks a term other than the one requested.
// With teacher_comments.require_term_confirmation (the default) a page that
// marks no term at all is not staged either.
func ProcessTeacherComments(ctx context.Context, e Env, kid Kid, page *pslib.TeacherCommentsPage, cfg TeacherCommentsConfig) Result {
	var res Result
	rule, ok := matchRule(e, kid, teacherCommentSurface)
	if !ok || page == nil {
		return res
	}
	// Defence in depth. ValidateConfig refuses any rule that could route a
	// teacher comment to anyone but the guardians; if one got here anyway
	// (a matcher built from rules that were never validated), nothing is
	// staged. The log line carries no content.
	if !guardiansOnly(rule.Audience) {
		e.Tel.RecordItemDropped("teacher_comment_audience")
		e.Tel.RecordSurfaceError(teacherCommentSurface, "audience_not_guardians_only", kid.Name)
		slog.Error("powerschool teacher comments not staged: the matching rule's audience is not "+
			"exactly [guardians]", "kid", kid.Name, "audience_tokens", len(rule.Audience))
		res.Failed++
		return res
	}
	term := strings.TrimSpace(page.Term)
	if term == "" || (!page.TermConfirmed && cfg.requireTermConfirmation()) {
		e.Tel.RecordSurfaceError(teacherCommentSurface, "term_unconfirmed", kid.Name)
		slog.Warn("powerschool teacher comments not staged: the page does not confirm which "+
			"reporting term it shows", "kid", kid.Name, "term_known", term != "")
		return res
	}

	seen := map[string]int{}
	for _, cm := range page.Comments {
		if cm == nil {
			continue
		}
		// The comment as it will be shown: cleaned, bounded, untrusted
		// (text.go). A comment with nothing visible in it is no comment.
		// The fingerprint below stays over the raw text and teacher, as
		// it always was, so cleaning the body re-stages nothing.
		comment, commentCut := boundText(cm.Comment)
		if comment == "" {
			continue
		}
		course := cm.CourseNumber
		if course == "" {
			course = "name:" + strings.ToLower(strings.TrimSpace(cm.CourseName))
		}
		key := course + "|" + strings.TrimSpace(cm.Expression) + ":" + term
		seen[key]++
		if c := seen[key]; c > 1 {
			key = fmt.Sprintf("%s#%d", key, c)
		}
		sum := sha256.Sum256([]byte(cm.Comment + "\x1f" + cm.Teacher))
		fp := hex.EncodeToString(sum[:8])
		change, _ := Compare(e.CP, teacherCommentSurface, kid.Name, key, fp)
		if change == ChangeNone {
			continue
		}

		tags := map[string]string{
			"term":            term,
			"teacher_present": formatBool(strings.TrimSpace(cm.Teacher) != ""),
		}
		setTag(tags, "course", cm.CourseName)
		setTag(tags, "course_number", cm.CourseNumber)
		setTag(tags, "period", cm.Expression)

		subject := fmt.Sprintf("Teacher comment: %s (%s)", cm.CourseName, term)
		opts := itemOptions(e, rule, kid, teacherCommentSurface, change, cm.CourseName, subject, tags)

		var b strings.Builder
		fmt.Fprintf(&b, "Course: %s\n", oneLine(cm.CourseName))
		if cm.Expression != "" {
			fmt.Fprintf(&b, "Period: %s\n", oneLine(cm.Expression))
		}
		fmt.Fprintf(&b, "Reporting term: %s\n", oneLine(term))
		if cm.Teacher != "" {
			fmt.Fprintf(&b, "Teacher: %s\n", oneLine(cm.Teacher))
		}
		writeTextBlock(&b, "Comment", comment, commentCut)

		if err := stage(ctx, e, teacherCommentSurface, opts, b.String()); err != nil {
			slog.Warn("powerschool teacher comment stage failed", "kid", kid.Name, "error", err)
			res.Failed++
			continue
		}
		if err := SaveFingerprint(e.CP, teacherCommentSurface, kid.Name, key, fp); err != nil {
			slog.Error("powerschool teacher comment checkpoint save failed",
				"kid", kid.Name, "duplicate_risk", true, "error", err)
		}
		e.Tel.RecordItem(teacherCommentSurface, kid.Name, change)
		res.Staged++
	}
	return res
}
