package powerschool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	pslib "github.com/leftathome/powerschool-go"
)

// assignmentSurface is the per-kid surface for gradebook assignments.
const assignmentSurface = "assignment"

// assignmentFlags is every flag the library can report, in the order the
// boolean tags and the `flags` tag list them. Each becomes its own
// "true"/"false" tag so a summariser can count without parsing.
var assignmentFlags = []pslib.AssignmentFlag{
	pslib.FlagMissing,
	pslib.FlagLate,
	pslib.FlagIncomplete,
	pslib.FlagExempt,
	pslib.FlagCollected,
	pslib.FlagAbsent,
}

// flagSet indexes an assignment's flags.
func flagSet(a *pslib.Assignment) map[pslib.AssignmentFlag]bool {
	set := make(map[pslib.AssignmentFlag]bool, len(a.Flags))
	for _, f := range a.Flags {
		set[f] = true
	}
	return set
}

// isScored reports whether the teacher has entered a score. The library
// leaves Score nil for an entered zero, so the status is consulted too.
func isScored(a *pslib.Assignment) bool {
	return a.Score != nil || a.Status == pslib.StatusGraded
}

// isOutstanding reports whether the assignment still wants work from the
// student: flagged missing or incomplete, or neither scored nor collected.
// Exempt work is never outstanding.
//
// PowerSchool is a gradebook, not a submission system. It does not know
// that a student handed something in unless the teacher ticks "collected"
// or scores it, so work that was turned in and is waiting to be marked
// reads as outstanding here.
func isOutstanding(a *pslib.Assignment) bool {
	flags := flagSet(a)
	if flags[pslib.FlagExempt] {
		return false
	}
	if flags[pslib.FlagMissing] || flags[pslib.FlagIncomplete] {
		return true
	}
	return !isScored(a) && !flags[pslib.FlagCollected]
}

// needsAttention reports whether the assignment carries a flag that makes
// it worth staging however old it is.
func needsAttention(a *pslib.Assignment) bool {
	flags := flagSet(a)
	return flags[pslib.FlagMissing] || flags[pslib.FlagLate] || flags[pslib.FlagIncomplete]
}

// dueDay returns the due date as a calendar day ("2006-01-02") and as the
// start of that day in tz. PowerSchool due dates carry no time; the library
// parses them as UTC midnight, so the calendar fields are read back in UTC.
func dueDay(a *pslib.Assignment, tz *time.Location) (day string, start time.Time, ok bool) {
	if a.DueDate.IsZero() {
		return "", time.Time{}, false
	}
	y, m, d := a.DueDate.UTC().Date()
	start = time.Date(y, m, d, 0, 0, 0, 0, tz)
	return start.Format("2006-01-02"), start, true
}

// withinLookback reports whether the assignment is due on or after the
// first day of the lookback window. An undated assignment is always in.
func withinLookback(a *pslib.Assignment, now time.Time, tz *time.Location, lookbackDays int) bool {
	_, start, ok := dueDay(a, tz)
	if !ok {
		return true
	}
	n := now.In(tz)
	cutoff := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, tz).AddDate(0, 0, -lookbackDays)
	return !start.Before(cutoff)
}

// assignmentFingerprint hashes every field whose change should stage the
// assignment again: a new score, a cleared missing flag, a moved due date.
func assignmentFingerprint(a *pslib.Assignment) string {
	flags := make([]string, 0, len(a.Flags))
	for _, f := range a.Flags {
		flags = append(flags, string(f))
	}
	sort.Strings(flags)

	score, percent := "", ""
	if a.Score != nil {
		score = formatFloat(*a.Score)
	}
	if a.Percentage != nil {
		percent = formatFloat(*a.Percentage)
	}
	canonical := strings.Join([]string{
		a.Title,
		a.DueDate.UTC().Format("2006-01-02"),
		a.Category,
		string(a.Status),
		strings.Join(flags, ","),
		score,
		formatFloat(a.MaxScore),
		percent,
		a.LetterGrade,
	}, "\x1f")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

// courseRead is one course's assignment lookup: the grade row it was read
// for (which the library has stamped with the student id of the scores page
// it read) and what came back.
type courseRead struct {
	grade       *pslib.Grade
	assignments []*pslib.Assignment
}

// readAssignments is the read half of the assignments surface: one lookup
// per course, nothing staged. It then checks that every page it read named
// the same student (verifyOneStudent).
//
// ok is false when the kid's poll must be discarded: the session was
// reported expired (Result.AuthErr is set), the context ended, or the pages
// disagreed about whose they were. The caller then stages nothing for this
// kid -- not the grades it already holds either.
//
// Courses are taken from the grades the caller already fetched (the
// library needs each row's scores URL). A course whose lookup fails for
// another reason is logged, counted and skipped.
func readAssignments(ctx context.Context, e Env, kid Kid, grades []*pslib.Grade, cfg AssignmentsConfig) (reads []courseRead, res Result, ok bool) {
	first := true
	var verified []*pslib.Grade
	for _, g := range grades {
		if g == nil || g.ScoresURL == "" {
			// No scores link: the portal offers no assignment list for
			// this row (homeroom, advisory).
			continue
		}
		if !first && cfg.CourseDelayMS > 0 {
			if err := sleep(ctx, time.Duration(cfg.CourseDelayMS)*time.Millisecond); err != nil {
				return nil, res, false
			}
		}
		first = false

		assignments, err := getAssignmentsTraced(ctx, e, kid, g)
		if err != nil {
			class := classifyErr(err)
			e.Tel.RecordSurfaceError(assignmentSurface, class, kid.Name)
			slog.Warn("powerschool assignments read failed",
				append([]any{"kid", kid.Name}, errAttrs(err)...)...)
			res.Failed++
			if class == "auth" {
				res.AuthErr = err
				return nil, res, false
			}
			continue
		}
		verified = append(verified, g)
		reads = append(reads, courseRead{grade: g, assignments: assignments})
	}

	if err := verifyOneStudent(verified); err != nil {
		class := classifyErr(err)
		e.Tel.RecordSurfaceError(assignmentSurface, class, kid.Name)
		e.Tel.RecordKidSkipped(kid.Name, class)
		slog.Error("powerschool poll discarded for kid: the pages read in this poll do not all "+
			"belong to one student; nothing staged",
			append([]any{"kid", kid.Name, "courses_read", len(verified)}, errAttrs(err)...)...)
		res.Failed++
		return nil, res, false
	}
	return reads, res, true
}

// stageAssignments is the write half: it stages every assignment in reads
// that is new or has changed.
//
// First-sight backlog: an assignment seen for the first time that is due
// before the lookback window and carries no missing/late/incomplete flag
// is checkpointed without being staged, so enabling the connector
// mid-year does not stage the whole year's gradebook. If such an
// assignment later changes it stages like any other.
func stageAssignments(ctx context.Context, e Env, kid Kid, reads []courseRead, cfg AssignmentsConfig) Result {
	var res Result
	rule, ok := matchRule(e, kid, assignmentSurface)
	if !ok {
		return res
	}

	for _, r := range reads {
		g := r.grade
		seen := make(map[string]int, len(r.assignments))
		for _, a := range r.assignments {
			if a == nil {
				continue
			}
			// An assignment shared by several sections comes back once
			// per section under one id; keep the rows apart.
			id := a.ID
			seen[id]++
			if n := seen[id]; n > 1 {
				id = fmt.Sprintf("%s#%d", id, n)
			}
			key := courseKey(g) + ":" + id
			fp := assignmentFingerprint(a)
			change, _ := Compare(e.CP, assignmentSurface, kid.Name, key, fp)
			if change == ChangeNone {
				continue
			}
			if change == ChangeNew && !withinLookback(a, e.now(), e.tz(), cfg.LookbackDays) && !needsAttention(a) {
				if err := SaveFingerprint(e.CP, assignmentSurface, kid.Name, key, fp); err != nil {
					slog.Error("powerschool assignment baseline save failed", "kid", kid.Name, "error", err)
				}
				continue
			}

			subject := strings.TrimSpace(a.Title)
			if subject == "" {
				subject = "(untitled assignment)"
			}
			opts := itemOptions(e, rule, kid, assignmentSurface, change, g.CourseName, subject, assignmentTags(a, g, e.tz()))

			if err := stage(ctx, e, assignmentSurface, opts, buildAssignmentBody(a, g, e.tz())); err != nil {
				slog.Warn("powerschool assignment stage failed", "kid", kid.Name, "error", err)
				res.Failed++
				continue
			}
			if err := SaveFingerprint(e.CP, assignmentSurface, kid.Name, key, fp); err != nil {
				slog.Error("powerschool assignment checkpoint save failed",
					"kid", kid.Name, "duplicate_risk", true, "error", err)
			}
			e.Tel.RecordItem(assignmentSurface, kid.Name, change)
			res.Staged++
		}
	}
	return res
}

// ProcessAssignments reads each course's assignments and stages every one
// that is new or has changed: readAssignments then stageAssignments. The
// connector's poll calls the two halves itself so that grades and
// attendance are also held back until the reads have been verified.
func ProcessAssignments(ctx context.Context, e Env, kid Kid, grades []*pslib.Grade, cfg AssignmentsConfig) Result {
	if _, ok := matchRule(e, kid, assignmentSurface); !ok {
		return Result{}
	}
	reads, res, ok := readAssignments(ctx, e, kid, grades, cfg)
	if !ok {
		return res
	}
	res.add(stageAssignments(ctx, e, kid, reads, cfg))
	return res
}

// getAssignmentsTraced wraps client.GetAssignments in the
// powerschool.lib.GetAssignments span.
func getAssignmentsTraced(ctx context.Context, e Env, kid Kid, g *pslib.Grade) ([]*pslib.Assignment, error) {
	ctx, span := e.Tel.StartSpan(ctx, "powerschool.lib.GetAssignments",
		attribute.String("kid", kid.Name),
	)
	defer span.End()
	return e.Client.GetAssignments(ctx, kid.StudentID, g)
}

// assignmentTags builds the tag set of spec 16 section 7.2. Every flag is
// present as "true" or "false"; optional facts are absent when unknown.
func assignmentTags(a *pslib.Assignment, g *pslib.Grade, tz *time.Location) map[string]string {
	flags := flagSet(a)
	tags := map[string]string{
		"status":      string(a.Status),
		"outstanding": formatBool(isOutstanding(a)),
		"scored":      formatBool(isScored(a)),
	}
	var set []string
	for _, f := range assignmentFlags {
		tags[string(f)] = formatBool(flags[f])
		if flags[f] {
			set = append(set, string(f))
		}
	}
	setTag(tags, "flags", strings.Join(set, ","))
	setTag(tags, "course", g.CourseName)
	setTag(tags, "period", g.Period)
	setTag(tags, "category", a.Category)
	setTag(tags, "assignment_id", a.ID)
	setTag(tags, "letter_grade", a.LetterGrade)
	if day, start, ok := dueDay(a, tz); ok {
		tags["due_day"] = day
		tags["due_date"] = start.Format(time.RFC3339)
	}
	if a.Score != nil {
		tags["score"] = formatFloat(*a.Score)
	}
	if a.MaxScore > 0 {
		tags["max_score"] = formatFloat(a.MaxScore)
	}
	if a.Percentage != nil {
		tags["percent"] = formatFloat(*a.Percentage)
	}
	return tags
}

// buildAssignmentBody synthesizes the plaintext content.raw body. The
// teacher-authored title and description are the untrusted part the
// scanner is here for.
func buildAssignmentBody(a *pslib.Assignment, g *pslib.Grade, tz *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Assignment: %s\n", a.Title)
	if g.CourseName != "" {
		fmt.Fprintf(&b, "Course: %s\n", g.CourseName)
	}
	if a.Category != "" {
		fmt.Fprintf(&b, "Category: %s\n", a.Category)
	}
	if day, _, ok := dueDay(a, tz); ok {
		fmt.Fprintf(&b, "Due: %s\n", day)
	}
	fmt.Fprintf(&b, "Status: %s\n", a.Status)
	if len(a.Flags) > 0 {
		flags := make([]string, 0, len(a.Flags))
		for _, f := range a.Flags {
			flags = append(flags, string(f))
		}
		fmt.Fprintf(&b, "Flags: %s\n", strings.Join(flags, ", "))
	}
	switch {
	case a.Score != nil && a.MaxScore > 0:
		fmt.Fprintf(&b, "Score: %s/%s\n", formatFloat(*a.Score), formatFloat(a.MaxScore))
	case a.Score != nil:
		fmt.Fprintf(&b, "Score: %s\n", formatFloat(*a.Score))
	case a.MaxScore > 0:
		fmt.Fprintf(&b, "Points possible: %s\n", formatFloat(a.MaxScore))
	}
	if a.Percentage != nil {
		fmt.Fprintf(&b, "Percent: %s\n", formatFloat(*a.Percentage))
	}
	if a.LetterGrade != "" {
		fmt.Fprintf(&b, "Grade: %s\n", a.LetterGrade)
	}
	if d := strings.TrimSpace(a.Description); d != "" {
		fmt.Fprintf(&b, "\n%s\n", d)
	}
	return b.String()
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
