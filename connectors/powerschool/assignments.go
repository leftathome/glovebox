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
	"unicode"
	"unicode/utf8"

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

// maxTextBlockBytes bounds each teacher-authored text block of an
// assignment item's body (the teacher's comment, the assignment description
// and the category description), each on its own. A longer block is cut at
// a UTF-8 boundary and followed by truncationMarker.
const maxTextBlockBytes = 4 << 10

// truncationMarker follows a text block that was cut, here or by the
// library (Assignment.TeacherCommentTruncated).
const truncationMarker = "[truncated]"

// textBlockIndent prefixes every line of a teacher-authored text block.
// The connector's own labels start in column one, so nothing a teacher
// types can pass for one of them.
const textBlockIndent = "  "

// assignmentText is the teacher-authored free text of one assignment, as
// the item's body shows it. All of it is untrusted third-party text: it is
// carried as plain text, bounded, and never interpreted.
type assignmentText struct {
	// comment is the note the teacher left on this student's score, from
	// the library's plain-text field. The HTML form
	// (Assignment.TeacherCommentHTML) is never read by this connector.
	comment string
	// description is the assignment's description.
	description string
	// categoryDescription is the description of the assignment's category;
	// empty when it says the same as description.
	categoryDescription string

	commentCut, descriptionCut, categoryCut bool
}

// textOf collects the assignment's free text, cleaned and bounded.
func textOf(a *pslib.Assignment) assignmentText {
	var t assignmentText
	if a.HasTeacherComment {
		t.comment, t.commentCut = boundText(a.TeacherComment)
		if t.comment != "" && a.TeacherCommentTruncated {
			t.commentCut = true
		}
	}
	t.description, t.descriptionCut = boundText(a.Description)
	if normaliseText(a.CategoryDescription) != normaliseText(a.Description) {
		t.categoryDescription, t.categoryCut = boundText(a.CategoryDescription)
	}
	return t
}

// hasComment reports whether there is a teacher comment with text to show.
func (t assignmentText) hasComment() bool { return t.comment != "" }

// cleanText prepares free text for a text/plain body: line ends become
// "\n", tabs become spaces, other control characters are dropped, trailing
// space is removed from each line, and blank lines at either end go. The
// words are not touched: no markup is parsed and nothing is decoded.
func cleanText(s string) string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", " ").Replace(s)
	s = strings.Map(func(r rune) rune {
		if r != '\n' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// boundText cleans s and cuts it to maxTextBlockBytes without splitting a
// character. cut reports whether anything was removed.
func boundText(s string) (text string, cut bool) {
	text = cleanText(strings.ToValidUTF8(s, ""))
	if len(text) <= maxTextBlockBytes {
		return text, false
	}
	n := maxTextBlockBytes
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return strings.TrimRightFunc(text[:n], unicode.IsSpace), true
}

// normaliseText is the form free text is fingerprinted in: runs of
// whitespace (line breaks and non-breaking spaces included) are one space
// and the ends are trimmed, so re-wrapping or re-indenting a text is not a
// change. Case and punctuation are kept.
func normaliseText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// recordFields is the canonical form of the gradebook facts of an
// assignment: everything but its free text.
func recordFields(a *pslib.Assignment) []string {
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
	return []string{
		a.Title,
		a.DueDate.UTC().Format("2006-01-02"),
		a.Category,
		string(a.Status),
		strings.Join(flags, ","),
		score,
		formatFloat(a.MaxScore),
		percent,
		a.LetterGrade,
	}
}

func hashFields(fields []string) string {
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// assignmentFingerprint hashes every field whose change should stage the
// assignment again: a new score, a cleared missing flag, a moved due date,
// and the free text -- the teacher's comment, the assignment description
// and the category description, each normalised (normaliseText) so that a
// whitespace-only difference is not a change.
//
// A staged item's id is minted fresh each time, and downstream notices new
// items by id; this fingerprint is therefore the whole of an assignment's
// identity: same fingerprint, no new item; different fingerprint, exactly
// one.
//
// The comment is hashed in full (as the library returns it, up to its own
// bound), not as cut for the body: an edit past maxTextBlockBytes is still
// an edit.
func assignmentFingerprint(a *pslib.Assignment) string {
	comment := ""
	if a.HasTeacherComment {
		comment = normaliseText(a.TeacherComment)
	}
	return hashFields(append(recordFields(a),
		"text/v2",
		comment,
		normaliseText(a.Description),
		normaliseText(a.CategoryDescription),
	))
}

// legacyAssignmentFingerprint is the fingerprint this connector saved
// before the free text was part of it (gradebook facts only). It exists so
// that the first poll after the upgrade does not stage every assignment
// already checkpointed: see stageAssignments.
func legacyAssignmentFingerprint(a *pslib.Assignment) string {
	return hashFields(recordFields(a))
}

// courseRead is one course's assignment lookup: the grade row it was read
// for and what came back.
type courseRead struct {
	grade       *pslib.Grade
	assignments []*pslib.Assignment
}

// readAssignments is the read half of the assignments surface: for each
// course, the scores page and then the lookup. Nothing is staged.
//
// The scores page is the check. The portal renders scores.html for the
// student the SESSION has selected, whatever course the link was for
// (observed live), and embeds that student's id in it. So before anything
// else is done with a scores page, its student id is put to the kid's
// binding (studentBinding.check): it must be this kid's bound id, or -- for
// a kid with no bound id yet -- the same id as every other scores page of
// this turn and nobody else's bound id. The first page that fails ends the
// kid's turn; the lookup for that course is not sent.
//
// ok is false when the kid's poll must be discarded: a scores page belonged
// to another student (or to nobody), the session was reported expired
// (Result.AuthErr is set), or the context ended. The caller then stages
// nothing for this kid -- not the grades it already holds either.
//
// Courses are taken from the grades the caller already fetched (the
// library needs each row's scores URL). A course whose read fails for
// another reason is logged, counted and skipped.
func readAssignments(ctx context.Context, e Env, kid Kid, grades []*pslib.Grade, cfg AssignmentsConfig, binding *studentBinding) (reads []courseRead, res Result, ok bool) {
	first := true
	failed := func(err error) (auth bool) {
		class := classifyErr(err)
		e.Tel.RecordSurfaceError(assignmentSurface, class, kid.Name)
		slog.Warn("powerschool assignments read failed",
			append([]any{"kid", kid.Name}, errAttrs(err)...)...)
		res.Failed++
		if class == "auth" {
			res.AuthErr = err
			return true
		}
		return false
	}
	discard := func(err error, coursesRead int) {
		class := classifyErr(err)
		e.Tel.RecordSurfaceError(assignmentSurface, class, kid.Name)
		e.Tel.RecordKidSkipped(kid.Name, class)
		slog.Error("powerschool poll discarded for kid: a scores page read in this poll does not "+
			"belong to this kid's student; nothing staged, no checkpoint advanced",
			append([]any{"kid", kid.Name, "courses_read", coursesRead}, errAttrs(err)...)...)
		res.Failed++
	}

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

		md, err := getScoresTraced(ctx, e, kid, g)
		if err != nil {
			if failed(err) {
				return nil, res, false
			}
			continue
		}
		apiID := ""
		if md != nil {
			apiID = md.StudentAPIID
		}
		if err := binding.check(apiID); err != nil {
			discard(err, len(reads))
			return nil, res, false
		}
		// Hand the verified ids to the library so the lookup it sends is
		// for exactly the page that was checked, and it does not fetch the
		// scores page a second time.
		g.SectionID, g.StudentAPIID = md.SectionID, md.StudentAPIID

		assignments, err := getAssignmentsTraced(ctx, e, kid, g)
		if err != nil {
			if failed(err) {
				return nil, res, false
			}
			continue
		}
		// The library must not have swapped the id underneath the check.
		if err := binding.check(g.StudentAPIID); err != nil {
			discard(err, len(reads))
			return nil, res, false
		}
		reads = append(reads, courseRead{grade: g, assignments: assignments})
	}
	return reads, res, true
}

// getScoresTraced wraps client.FetchScoresMetadata in the
// powerschool.lib.FetchScoresMetadata span.
func getScoresTraced(ctx context.Context, e Env, kid Kid, g *pslib.Grade) (*pslib.ScoresMetadata, error) {
	ctx, span := e.Tel.StartSpan(ctx, "powerschool.lib.FetchScoresMetadata",
		attribute.String("kid", kid.Name),
	)
	defer span.End()
	return e.Client.FetchScoresMetadata(ctx, g.ScoresURL)
}

// stageAssignments is the write half: it stages every assignment in reads
// that is new or has changed. A teacher's comment travels on the
// assignment's own item, built from this kid's rule like the rest of it, so
// it has that kid's data subject and audience and no other.
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
			change, prev := Compare(e.CP, assignmentSurface, kid.Name, key, fp)
			if change == ChangeNone {
				continue
			}
			text := textOf(a)
			if change == ChangeUpdated && prev == legacyAssignmentFingerprint(a) && !text.hasComment() {
				// The stored fingerprint is the pre-text form and the
				// gradebook facts it covers have not moved: nothing
				// happened to this assignment, the fingerprint just grew.
				// Re-baseline without staging. An assignment that already
				// carries a teacher comment does stage (once): the comment
				// has never been delivered.
				if err := SaveFingerprint(e.CP, assignmentSurface, kid.Name, key, fp); err != nil {
					slog.Error("powerschool assignment fingerprint upgrade save failed", "kid", kid.Name, "error", err)
				}
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
			opts := itemOptions(e, rule, kid, assignmentSurface, change, g.CourseName, subject, assignmentTags(a, g, text, e.tz()))

			if err := stage(ctx, e, assignmentSurface, opts, buildAssignmentBody(a, g, text, e.tz())); err != nil {
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
	reads, res, ok := readAssignments(ctx, e, kid, grades, cfg, newStudentBinding(nil, e.CP, kid))
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
//
// has_teacher_comment is always present; teacher_comment_truncated is
// present (as "true") only when the comment in the body is not the whole
// comment.
func assignmentTags(a *pslib.Assignment, g *pslib.Grade, text assignmentText, tz *time.Location) map[string]string {
	flags := flagSet(a)
	tags := map[string]string{
		"status":              string(a.Status),
		"outstanding":         formatBool(isOutstanding(a)),
		"scored":              formatBool(isScored(a)),
		"has_teacher_comment": formatBool(text.hasComment()),
	}
	if text.hasComment() && text.commentCut {
		tags["teacher_comment_truncated"] = "true"
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

// buildAssignmentBody synthesizes the plaintext content.raw body: the
// gradebook facts as "Label: value" lines, then up to three labelled text
// blocks -- "Teacher comment:", "Assignment description:" and "Category
// description:" -- each present only when there is text for it.
//
// The title and the three blocks are teacher-authored and untrusted; they
// are what the scanner is here for. A block's lines are indented
// (textBlockIndent) and a cut block ends with truncationMarker.
func buildAssignmentBody(a *pslib.Assignment, g *pslib.Grade, text assignmentText, tz *time.Location) string {
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
	writeTextBlock(&b, "Teacher comment", text.comment, text.commentCut)
	writeTextBlock(&b, "Assignment description", text.description, text.descriptionCut)
	writeTextBlock(&b, "Category description", text.categoryDescription, text.categoryCut)
	return b.String()
}

// writeTextBlock appends one labelled block of free text, or nothing when
// text is empty.
func writeTextBlock(b *strings.Builder, label, text string, cut bool) {
	if text == "" {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", label)
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(textBlockIndent)
		b.WriteString(line)
		b.WriteString("\n")
	}
	if cut {
		b.WriteString(truncationMarker)
		b.WriteString("\n")
	}
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
