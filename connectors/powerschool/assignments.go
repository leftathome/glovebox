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

// assignmentText is the teacher-authored free text of one assignment, as
// the item's body shows it. All of it is untrusted third-party text: it is
// carried as plain text, bounded, and never interpreted (text.go).
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
	t.categoryDescription, t.categoryCut = boundText(a.CategoryDescription)
	if t.categoryDescription == t.description {
		t.categoryDescription, t.categoryCut = "", false
	}
	return t
}

// hasComment reports whether there is a teacher comment with text to show.
func (t assignmentText) hasComment() bool { return t.comment != "" }

// assignmentTitle is the title as one cleaned line, or a placeholder.
func assignmentTitle(a *pslib.Assignment) string {
	if title := oneLine(a.Title); title != "" {
		return title
	}
	return "(untitled assignment)"
}

// The parts of an assignment that are fingerprinted separately, in the
// order the `changed` tag lists them. The names are tag values and so are
// stable across releases.
const (
	partDetails        = "details"         // title, category
	partDueDate        = "due_date"        // due date
	partStatus         = "status"          // the library's status
	partFlags          = "flags"           // missing, late, ...
	partScore          = "score"           // score, points possible, percent, letter
	partTeacherComment = "teacher_comment" // the teacher's note on the score
	partDescription    = "description"     // the assignment description
)

var fingerprintParts = []string{partDetails, partDueDate, partStatus, partFlags, partScore, partTeacherComment, partDescription}

// changedRecord is the `changed` value for an assignment whose stored
// fingerprint is the old single hash and no longer matches: the gradebook
// record moved, and the old form cannot say which part.
const changedRecord = "record"

// fingerprintPrefix marks the per-part fingerprint format.
const fingerprintPrefix = "a3"

func hashFields(n int, fields ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x1f")))
	return hex.EncodeToString(sum[:n])
}

func sortedFlags(a *pslib.Assignment) string {
	flags := make([]string, 0, len(a.Flags))
	for _, f := range a.Flags {
		flags = append(flags, string(f))
	}
	sort.Strings(flags)
	return strings.Join(flags, ",")
}

func scoreFields(a *pslib.Assignment) (score, percent string) {
	if a.Score != nil {
		score = formatFloat(*a.Score)
	}
	if a.Percentage != nil {
		percent = formatFloat(*a.Percentage)
	}
	return score, percent
}

// assignmentFingerprint is the identity of an assignment as staged. A
// staged item's id is minted fresh each time and downstream notices new
// items by id, so this value alone decides whether there is a new item:
// same fingerprint, none; different fingerprint, at most one (see
// stageAssignments for the two cases that are recorded without staging).
//
// It is "a3:" followed by one short hash per part of fingerprintParts, so
// that a later comparison can say which part moved (changedParts). Every
// part is taken over what the item shows, not over the raw input:
//
//   - the gradebook facts, the title and category as their one cleaned
//     line;
//   - the teacher's comment and the assignment description as cleaned and
//     cut for the body (textOf), with whether the block ends in the
//     truncation marker. An edit that changes nothing in the staged block
//     -- invisible characters, trailing space, text beyond the cut, the
//     comment's markup -- therefore does not stage an identical item.
//
// The category description is deliberately NOT part of it: it belongs to
// the category, and one edit by a teacher would otherwise stage every
// assignment in that category again. It is still shown in the body
// whenever the assignment is staged for a reason of its own.
func assignmentFingerprint(a *pslib.Assignment, text assignmentText) string {
	score, percent := scoreFields(a)
	cut := func(c bool) string {
		if c {
			return "cut"
		}
		return ""
	}
	hashes := map[string]string{
		partDetails:        hashFields(6, oneLine(a.Title), oneLine(a.Category)),
		partDueDate:        hashFields(6, a.DueDate.UTC().Format("2006-01-02")),
		partStatus:         hashFields(6, string(a.Status)),
		partFlags:          hashFields(6, sortedFlags(a)),
		partScore:          hashFields(6, score, formatFloat(a.MaxScore), percent, oneLine(a.LetterGrade)),
		partTeacherComment: hashFields(6, text.comment, cut(text.commentCut)),
		partDescription:    hashFields(6, text.description, cut(text.descriptionCut)),
	}
	out := make([]string, 0, len(fingerprintParts)+1)
	out = append(out, fingerprintPrefix)
	for _, part := range fingerprintParts {
		out = append(out, hashes[part])
	}
	return strings.Join(out, ":")
}

// legacyAssignmentFingerprint is the fingerprint the connector saved when
// it was first merged: one hash over the gradebook facts, raw, with no
// free text. It is recomputed only to recognise such a checkpoint
// (changedParts); its fields and their order must never change, and
// TestLegacyAssignmentFingerprint_Golden holds it to literal values.
func legacyAssignmentFingerprint(a *pslib.Assignment) string {
	score, percent := scoreFields(a)
	return hashFields(8,
		a.Title,
		a.DueDate.UTC().Format("2006-01-02"),
		a.Category,
		string(a.Status),
		sortedFlags(a),
		score,
		formatFloat(a.MaxScore),
		percent,
		a.LetterGrade,
	)
}

// changedParts names the parts of an assignment that differ between the
// stored fingerprint prev and the current one cur (prev != cur).
//
// A stored value in the old single-hash form cannot be compared part by
// part. If it still equals the old hash of the current record, the
// gradebook facts have not moved and the only thing never delivered is a
// teacher comment, if there is one; nothing at all is returned otherwise
// and the caller re-baselines without staging. If it does not, the record
// changed in a way the old form cannot name: changedRecord.
func changedParts(prev, cur string, a *pslib.Assignment, text assignmentText) []string {
	was, now := strings.Split(prev, ":"), strings.Split(cur, ":")
	if len(was) == len(fingerprintParts)+1 && len(now) == len(was) && was[0] == fingerprintPrefix {
		var changed []string
		for i, part := range fingerprintParts {
			if was[i+1] != now[i+1] {
				changed = append(changed, part)
			}
		}
		return changed
	}
	var changed []string
	if prev != legacyAssignmentFingerprint(a) {
		changed = append(changed, changedRecord)
	}
	if text.hasComment() {
		changed = append(changed, partTeacherComment)
	}
	return changed
}

// descriptionOnly reports whether the description is the only part that
// changed.
func descriptionOnly(changed []string) bool {
	return len(changed) == 1 && changed[0] == partDescription
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
// assignment later changes it stages like any other -- except that an edit
// to nothing but its description is checkpointed without being staged
// too.
//
// An item staged for a change carries a `changed` tag naming the parts
// that moved (fingerprintParts, or changedRecord).
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
			text := textOf(a)
			fp := assignmentFingerprint(a, text)
			prev, seenBefore := e.CP.Load(CheckpointKey(assignmentSurface, kid.Name, key))
			if seenBefore && prev == fp {
				continue
			}
			baseline := func(why string) {
				if err := SaveFingerprint(e.CP, assignmentSurface, kid.Name, key, fp); err != nil {
					slog.Error("powerschool assignment baseline save failed", "kid", kid.Name, "reason", why, "error", err)
				}
			}
			change := ChangeNew
			var changed []string
			if seenBefore {
				change = ChangeUpdated
				changed = changedParts(prev, fp, a, text)
				if len(changed) == 0 {
					// An old-format checkpoint of a record that has not
					// moved and has no comment: the fingerprint changed
					// shape, the assignment did not.
					baseline("fingerprint_format")
					continue
				}
			}
			// Outside the lookback window, with no flag that makes it
			// worth attention: not staged on first sight, and not staged
			// again for an edit to nothing but its description. Anything
			// else -- a score, a flag, a date, a teacher comment above all
			// -- stages however old the assignment is.
			if (change == ChangeNew || descriptionOnly(changed)) &&
				!withinLookback(a, e.now(), e.tz(), cfg.LookbackDays) && !needsAttention(a) {
				baseline("outside_lookback")
				continue
			}

			tags := assignmentTags(a, g, text, e.tz())
			setTag(tags, "changed", strings.Join(changed, ","))
			opts := itemOptions(e, rule, kid, assignmentSurface, change, g.CourseName, assignmentTitle(a), tags)

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
// comment. stageAssignments adds `changed`; itemOptions makes every value
// one cleaned line.
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
// Every value is untrusted text from the portal; it is what the scanner is
// here for. A fact is one cleaned line (oneLine) however many line breaks
// its source carries, so the only lines that start in column one are the
// connector's own labels. A block's lines are indented (textBlockIndent)
// and a cut block ends with truncationMarker.
func buildAssignmentBody(a *pslib.Assignment, g *pslib.Grade, text assignmentText, tz *time.Location) string {
	var b strings.Builder
	// fact writes one "Label: value" line. The value is made one line
	// first, whatever it is, so it cannot start a line of its own.
	fact := func(label, value string) {
		if v := oneLine(value); v != "" {
			b.WriteString(label + ": " + v + "\n")
		}
	}
	fact("Assignment", assignmentTitle(a))
	fact("Course", g.CourseName)
	fact("Category", a.Category)
	if day, _, ok := dueDay(a, tz); ok {
		fact("Due", day)
	}
	fact("Status", string(a.Status))
	if len(a.Flags) > 0 {
		flags := make([]string, 0, len(a.Flags))
		for _, f := range a.Flags {
			flags = append(flags, string(f))
		}
		fact("Flags", strings.Join(flags, ", "))
	}
	switch {
	case a.Score != nil && a.MaxScore > 0:
		fact("Score", formatFloat(*a.Score)+"/"+formatFloat(a.MaxScore))
	case a.Score != nil:
		fact("Score", formatFloat(*a.Score))
	case a.MaxScore > 0:
		fact("Points possible", formatFloat(a.MaxScore))
	}
	if a.Percentage != nil {
		fact("Percent", formatFloat(*a.Percentage))
	}
	fact("Grade", a.LetterGrade)
	writeTextBlock(&b, "Teacher comment", text.comment, text.commentCut)
	writeTextBlock(&b, "Assignment description", text.description, text.descriptionCut)
	writeTextBlock(&b, "Category description", text.categoryDescription, text.categoryCut)
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
