package powerschool

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	pslib "github.com/leftathome/powerschool-go"
)

// Review fixes for the assignment item's untrusted text and its identity.
// Every text is synthetic. Characters outside ASCII are built from their
// code points so this file stays ASCII.
var (
	uLS  = string(rune(0x2028)) // LINE SEPARATOR (Zl)
	uPS  = string(rune(0x2029)) // PARAGRAPH SEPARATOR (Zp)
	uNEL = string(rune(0x85))   // NEXT LINE (C1)
	uRLO = string(rune(0x202e)) // RIGHT-TO-LEFT OVERRIDE (Cf)
	uLRI = string(rune(0x2066)) // LEFT-TO-RIGHT ISOLATE (Cf)
	uZWJ = string(rune(0x200d)) // ZERO WIDTH JOINER (Cf)
	uZWS = string(rune(0x200b)) // ZERO WIDTH SPACE (Cf)
	uBOM = string(rune(0xfeff)) // BYTE ORDER MARK (Cf)
	euro = string(rune(0x20ac)) // three bytes in UTF-8
)

// stageIn stages a under course g for testKid and returns its item.
func stageIn(t *testing.T, g *pslib.Grade, a *pslib.Assignment) stagedItem {
	t.Helper()
	env, dir := newTestEnv(t, assignmentsClient(map[string][]*pslib.Assignment{g.CourseID: {a}}))
	if res := ProcessAssignments(context.Background(), env, testKid, []*pslib.Grade{g}, testAssignmentsCfg); res.Staged != 1 {
		t.Fatalf("result = %+v, want one staged item", res)
	}
	items := readItems(t, dir)
	if len(items) != 1 {
		t.Fatalf("staged %d items, want 1", len(items))
	}
	return items[0]
}

// ownLabels are the only things a line of an assignment body may start
// with in column one.
var ownLabels = []string{"Assignment: ", "Course: ", "Category: ", "Due: ", "Status: ", "Flags: ",
	"Score: ", "Points possible: ", "Percent: ", "Grade: ",
	"Teacher comment:", "Assignment description:", "Category description:", truncationMarker}

// assertLayout checks the two things a reader relies on: every line that
// starts in column one is one of the connector's own, each label at most
// once; and nothing in the item can break or reorder a line.
func assertLayout(t *testing.T, item stagedItem) (labels map[string]int) {
	t.Helper()
	labels = map[string]int{}
	for _, line := range strings.Split(strings.TrimRight(item.Body, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, textBlockIndent) {
			continue
		}
		known := ""
		for _, l := range ownLabels {
			if strings.HasPrefix(line, l) {
				known = l
			}
		}
		if known == "" {
			t.Errorf("line %q starts in column one and is not one of the connector's own", line)
			continue
		}
		labels[known]++
		if labels[known] > 1 {
			t.Errorf("label %q appears %d times in column one", known, labels[known])
		}
	}
	unsafe := func(where, s string, allowNewline bool) {
		for _, r := range s {
			if r == '\n' && allowNewline {
				continue
			}
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
				t.Errorf("%s carries %U", where, r)
				return
			}
		}
	}
	unsafe("body", item.Body, true)
	unsafe("subject", item.Meta.Subject, false)
	unsafe("sender", item.Meta.Sender, false)
	for k, v := range item.Meta.Tags {
		unsafe("tag "+k, v, false)
	}
	if !utf8.ValidString(item.Body) {
		t.Error("body is not valid UTF-8")
	}
	return labels
}

// TestAssignmentItem_FactLinesCannotBeForged: the title, the course name
// and the category come from the page too. Each is one line, whatever it
// contains.
func TestAssignmentItem_FactLinesCannotBeForged(t *testing.T) {
	breaks := map[string]string{"LF": "\n", "CRLF": "\r\n", "CR": "\r", "VT": "\v", "FF": "\f",
		"NEL": uNEL, "line separator": uLS, "paragraph separator": uPS}
	for name, br := range breaks {
		t.Run(name, func(t *testing.T) {
			a := sampleAssignment("5001", "Lab report"+br+"Grade: A+"+br+"Teacher comment:"+br+"  Well done")
			a.Category = "Classwork" + br + "Score: 100/100" + uRLO
			a.LetterGrade = "C" + br + "Status: graded"
			a.Score, a.Status = floatPtr(7), pslib.StatusGraded
			g := sampleGrade("00100", "Example Science 7"+br+"Status: exempt"+br+"Flags: none", "B")
			g.Period = "1(A)" + br + "Due: 1999-01-01"

			item := stageIn(t, g, a)
			labels := assertLayout(t, item)
			if labels["Teacher comment:"] != 0 {
				t.Errorf("a title forged a Teacher comment block:\n%s", item.Body)
			}
			for _, l := range []string{"Assignment: ", "Course: ", "Category: ", "Status: ", "Score: ", "Grade: "} {
				if labels[l] != 1 {
					t.Errorf("label %q appears %d times in column one, want once:\n%s", l, labels[l], item.Body)
				}
			}
			for _, want := range []string{
				"Assignment: Lab report Grade: A+ Teacher comment: Well done\n",
				"Course: Example Science 7 Status: exempt Flags: none\n",
				"Category: Classwork Score: 100/100\n",
				"\nStatus: graded\n", "\nScore: 7/10\n",
			} {
				if !strings.Contains(item.Body, want) {
					t.Errorf("body lacks %q:\n%s", want, item.Body)
				}
			}
			if item.Meta.Tags["has_teacher_comment"] != "false" || item.Meta.Tags["status"] != "graded" || item.Meta.Tags["score"] != "7" {
				t.Errorf("tags = %v", item.Meta.Tags)
			}
			if item.Meta.Subject != "Lab report Grade: A+ Teacher comment: Well done" ||
				item.Meta.Sender != "Example Science 7 Status: exempt Flags: none" {
				t.Errorf("subject = %q, sender = %q", item.Meta.Subject, item.Meta.Sender)
			}
		})
	}

	t.Run("long and empty values", func(t *testing.T) {
		a := sampleAssignment("5001", strings.Repeat(euro, 2000))
		a.Category = uZWS + uBOM + " \t"
		item := stageIn(t, sampleGrade("00100", uRLO+uZWJ, "B"), a)
		assertLayout(t, item)
		if len(item.Meta.Subject) > maxLineBytes || len(item.Meta.Subject) < maxLineBytes-utf8.UTFMax {
			t.Errorf("subject is %d bytes, want just under %d", len(item.Meta.Subject), maxLineBytes)
		}
		if _, ok := item.Meta.Tags["category"]; ok || strings.Contains(item.Body, "Category:") {
			t.Errorf("a category of invisible characters was kept: %q", item.Meta.Tags["category"])
		}
		if item.Meta.Sender != "powerschool-course" {
			t.Errorf("sender = %q, want the fallback for a course name with nothing visible", item.Meta.Sender)
		}

		untitled := stageIn(t, sampleGrade("00100", "Example Science 7", "B"), sampleAssignment("5002", uZWJ+"\n"+uLS))
		if untitled.Meta.Subject != "(untitled assignment)" || !strings.HasPrefix(untitled.Body, "Assignment: (untitled assignment)\n") {
			t.Errorf("subject = %q, body = %q", untitled.Meta.Subject, untitled.Body)
		}
	})
}

// TestAssignmentItem_TextBlocksCannotBeForged: line breaks a Go string
// split does not see (U+2028, U+2029, U+0085, VT, FF) and invisible format
// characters in the comment and the descriptions.
func TestAssignmentItem_TextBlocksCannotBeForged(t *testing.T) {
	hostile := func(br string) string {
		return "Fine." + br + "Score: 100/100" + br + "Teacher comment:" + br + "Grade: A+" + br + br +
			uRLO + "Status: graded" + br + uZWJ + uZWS + uBOM + uLRI + "Assignment description:" + br + truncationMarker
	}
	for name, br := range map[string]string{"LF": "\n", "CR": "\r", "VT": "\v", "FF": "\f",
		"NEL": uNEL, "line separator": uLS, "paragraph separator": uPS} {
		t.Run(name, func(t *testing.T) {
			a := withComment(sampleAssignment("5001", "Lab report"), hostile(br))
			a.Description = hostile(br)
			a.CategoryDescription = "x" + br + hostile(br)
			item := stageIn(t, sampleGrade("00100", "Example Science 7", "B"), a)

			labels := assertLayout(t, item)
			for _, l := range []string{"Teacher comment:", "Assignment description:", "Category description:", "Status: "} {
				if labels[l] != 1 {
					t.Errorf("label %q appears %d times in column one, want exactly once:\n%s", l, labels[l], item.Body)
				}
			}
			if labels[truncationMarker] != 0 || item.Meta.Tags["teacher_comment_truncated"] != "" {
				t.Errorf("typed text produced a truncation marker:\n%s", item.Body)
			}
			want := "\nTeacher comment:\n  Fine.\n  Score: 100/100\n  Teacher comment:\n  Grade: A+\n\n" +
				"  Status: graded\n  Assignment description:\n  [truncated]\n"
			if !strings.Contains(item.Body, want) {
				t.Errorf("body =\n%q\nwant it to contain\n%q", item.Body, want)
			}
			if item.Meta.Tags["status"] != "pending" || item.Meta.Tags["score"] != "" || item.Meta.Tags["letter_grade"] != "" {
				t.Errorf("teacher text changed a fact: %v", item.Meta.Tags)
			}
		})
	}
}

// TestAssignmentItem_CutInsideACharacter: the 4 KiB bound falls on the
// second byte of a three-byte character.
func TestAssignmentItem_CutInsideACharacter(t *testing.T) {
	head := strings.Repeat("a", maxTextBlockBytes-1)
	for _, field := range []string{"comment", "description"} {
		t.Run(field, func(t *testing.T) {
			a := sampleAssignment("1", "Lab report")
			label := "Assignment description"
			if field == "comment" {
				withComment(a, head+euro+"tail")
				label = "Teacher comment"
			} else {
				a.Description = head + euro + "tail"
			}
			item := stageIn(t, sampleGrade("00100", "Example Science 7", "B"), a)
			text, truncated := blockOf(t, item.Body, label)
			if text != head || !truncated {
				t.Errorf("block is %d bytes (truncated=%v), want the %d bytes before the straddling character and the marker",
					len(text), truncated, len(head))
			}
			if !utf8.ValidString(item.Body) {
				t.Error("body is not valid UTF-8: the character was split")
			}
		})
	}
}

// pollOnce stages what one poll of the given assignments produces against
// cp-sharing env, and returns the items.
func pollOnce(t *testing.T, shared Env, assignments []*pslib.Assignment) []stagedItem {
	t.Helper()
	env, dir := newTestEnv(t, assignmentsClient(map[string][]*pslib.Assignment{"00100": assignments}))
	env.CP = shared.CP
	grades := []*pslib.Grade{sampleGrade("00100", "Example Science 7", "B")}
	if res := ProcessAssignments(context.Background(), env, testKid, grades, testAssignmentsCfg); res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	return readItems(t, dir)
}

// TestProcessAssignments_CategoryDescriptionEditStagesNothing: a teacher
// editing one category's description touches every assignment in it. That
// must not deliver the whole category again.
func TestProcessAssignments_CategoryDescriptionEditStagesNothing(t *testing.T) {
	const n = 40
	build := func(categoryDescription string) []*pslib.Assignment {
		var out []*pslib.Assignment
		for i := 0; i < n; i++ {
			a := sampleAssignment(fmt.Sprint(6000+i), fmt.Sprintf("Worksheet %02d", i))
			a.Description = fmt.Sprintf("Problems on page %d.", i)
			a.CategoryDescription = categoryDescription
			if i%4 == 0 {
				withComment(a, "Checked.")
			}
			out = append(out, a)
		}
		return out
	}
	shared, _ := newTestEnv(t, &fakeClient{})

	if got := pollOnce(t, shared, build("Classwork is 40% of the grade.")); len(got) != n {
		t.Fatalf("first poll staged %d items, want %d", len(got), n)
	}
	for _, edit := range []string{"Classwork is 50% of the grade.", "", "A brand new description."} {
		if got := pollOnce(t, shared, build(edit)); len(got) != 0 {
			t.Errorf("category description edited to %q: staged %d of %d assignments, want none", edit, len(got), n)
		}
	}
	// The category description is still shown when the assignment is next
	// staged for a reason of its own.
	next := build("A brand new description.")
	next[1].Score, next[1].Status = floatPtr(9), pslib.StatusGraded
	got := pollOnce(t, shared, next)
	if len(got) != 1 || !strings.Contains(got[0].Body, "\nCategory description:\n  A brand new description.\n") {
		t.Errorf("after a score change: %d items, want the one assignment with the current category description", len(got))
	}
}

// TestProcessAssignments_ChangedTagAndTextOnlyLookback: an item staged
// again says which part moved, and an edit to nothing but the description
// of an assignment outside the lookback window is not sent.
func TestProcessAssignments_ChangedTagAndTextOnlyLookback(t *testing.T) {
	recent := func() *pslib.Assignment {
		a := sampleAssignment("1", "Recent")
		a.Description = "Read chapter 3."
		return a
	}
	old := func() *pslib.Assignment {
		a := sampleAssignment("2", "Old and graded")
		a.DueDate = due(2026, 9, 1) // 36 days before testNow
		a.Score, a.Status = floatPtr(10), pslib.StatusGraded
		a.Description = "Read chapter 1."
		return a
	}
	oldMissing := func() *pslib.Assignment {
		a := sampleAssignment("3", "Old and missing")
		a.DueDate = due(2026, 9, 1)
		a.Status, a.Flags = pslib.StatusMissing, []pslib.AssignmentFlag{pslib.FlagMissing}
		a.Description = "Read chapter 2."
		return a
	}
	shared, _ := newTestEnv(t, &fakeClient{})
	changedOf := func(items []stagedItem) string {
		var out []string
		for _, it := range items {
			out = append(out, it.Meta.Subject+"="+it.Meta.Tags["changed"]+"/"+it.Meta.Tags["change"])
		}
		return strings.Join(out, "; ")
	}
	step := func(name, want string, as ...*pslib.Assignment) {
		t.Helper()
		if got := changedOf(pollOnce(t, shared, as)); got != want {
			t.Errorf("%s: staged [%s], want [%s]", name, got, want)
		}
	}

	// First sight: the old graded one is baselined, not staged. A new item
	// has no `changed` tag.
	step("first poll", "Old and missing=/new; Recent=/new", recent(), old(), oldMissing())
	step("nothing changed", "", recent(), old(), oldMissing())

	// Description edits alone: the recent one is sent, the old graded one
	// is not, the old one that still needs attention is.
	r, o, m := recent(), old(), oldMissing()
	r.Description, o.Description, m.Description = "Read chapter 4.", "Read chapter 9.", "Read chapter 8."
	step("descriptions edited", "Old and missing=description/updated; Recent=description/updated", r, o, m)
	step("same again", "", r, o, m)

	// A teacher comment is always sent, however old the assignment.
	withComment(o, "Late, but good.")
	step("comment on the old assignment", "Old and graded=teacher_comment/updated", r, o, m)
	withComment(o, "Late, but very good.")
	step("comment edited", "Old and graded=teacher_comment/updated", r, o, m)

	// Several parts at once are all named, in a fixed order.
	o.Score, o.Percentage, o.LetterGrade = floatPtr(6), floatPtr(60), "D"
	o.Flags = []pslib.AssignmentFlag{pslib.FlagLate}
	o.Description = "Read chapter 10."
	withComment(o, "")
	step("score, flag, comment and description", "Old and graded=flags,score,teacher_comment,description/updated", r, o, m)

	r.DueDate = due(2026, 10, 9)
	r.Title = "Recent (moved)"
	step("title and due date", "Recent (moved)=details,due_date/updated", r, o, m)
	m.Status, m.Flags = pslib.StatusGraded, nil
	m.Score = floatPtr(5)
	step("missing cleared and scored", "Old and missing=status,flags,score/updated", r, o, m)
}

// TestProcessAssignments_InvisibleEditsAreNotRestaged: the fingerprint is
// taken over the text as staged, so an edit that changes nothing in the
// item does not stage an identical one.
func TestProcessAssignments_InvisibleEditsAreNotRestaged(t *testing.T) {
	long := strings.Repeat("word ", maxTextBlockBytes/5+10)
	base := func() *pslib.Assignment {
		a := withComment(sampleAssignment("1", "Lab report"), "Good work.\nShow the units next time.")
		a.Description = long + "END-ONE"
		return a
	}
	shared, _ := newTestEnv(t, &fakeClient{})
	if got := pollOnce(t, shared, []*pslib.Assignment{base()}); len(got) != 1 {
		t.Fatalf("first poll staged %d items, want 1", len(got))
	}

	same := map[string]func(a *pslib.Assignment){
		"control and format characters only": func(a *pslib.Assignment) {
			a.TeacherComment = uBOM + "Good" + uZWS + " work.\x00" + uLS + "Show the " + uRLO + "units next time.\x07"
		},
		"trailing and leading space, CRLF, blank lines at the ends": func(a *pslib.Assignment) {
			a.TeacherComment = "\n\n  Good work.  \r\n\tShow the   units next time. \n\n"
		},
		"an edit beyond the cut": func(a *pslib.Assignment) { a.Description = long + "END-TWO, and more" },
		"markup only":            func(a *pslib.Assignment) { a.TeacherCommentHTML = "<div>Good work.<br>Show the units next time.</div>" },
		"title whitespace":       func(a *pslib.Assignment) { a.Title = "  Lab   report" + uZWJ },
	}
	for name, mutate := range same {
		a := base()
		mutate(a)
		if got := pollOnce(t, shared, []*pslib.Assignment{a}); len(got) != 0 {
			t.Errorf("%s: staged %d items (changed=%q), want none", name, len(got), got[0].Meta.Tags["changed"])
		}
	}

	visible := map[string]func(a *pslib.Assignment){
		"a line break moved":            func(a *pslib.Assignment) { a.TeacherComment = "Good work. Show the units\nnext time." },
		"an edit before the cut":        func(a *pslib.Assignment) { a.Description = "Now " + long },
		"the text now fits (no marker)": func(a *pslib.Assignment) { a.Description = "word" },
		"library reports the comment was cut": func(a *pslib.Assignment) {
			a.Description = "word"
			a.TeacherCommentTruncated = true
		},
	}
	for _, name := range []string{"a line break moved", "an edit before the cut", "the text now fits (no marker)", "library reports the comment was cut"} {
		a := base()
		visible[name](a)
		if got := pollOnce(t, shared, []*pslib.Assignment{a}); len(got) != 1 {
			t.Errorf("%s: staged %d items, want 1", name, len(got))
		}
	}
}

// TestLegacyAssignmentFingerprint_Golden pins the fingerprint format of the
// connector as first merged (commit d887f7b), from values computed by
// running that commit's assignmentFingerprint on these records. The
// upgrade path recognises an old checkpoint by recomputing this hash; if
// the fields or their order drift, nothing is recognised and every
// checkpointed assignment is staged again.
func TestLegacyAssignmentFingerprint_Golden(t *testing.T) {
	plain := sampleAssignment("1", "Lab report")

	graded := sampleAssignment("2", "Unit 3 quiz")
	graded.Status, graded.Flags = pslib.StatusGraded, []pslib.AssignmentFlag{pslib.FlagLate, pslib.FlagCollected}
	graded.Score, graded.Percentage, graded.LetterGrade = floatPtr(7.5), floatPtr(75), "C"
	graded.Description = "ignored by the old fingerprint"

	missing := sampleAssignment("3", "Essay draft")
	missing.DueDate = due(2026, 9, 1)
	missing.Category = "Homework"
	missing.MaxScore = 100
	missing.Status, missing.Flags = pslib.StatusMissing, []pslib.AssignmentFlag{pslib.FlagMissing}

	for _, tc := range []struct {
		name string
		a    *pslib.Assignment
		want string
	}{
		{"plain", plain, "af4bbd80e16ff9bf"},
		{"graded late", graded, "8f6865b88a72a1e8"},
		{"missing", missing, "ccbd983cfcd2133c"},
	} {
		if got := legacyAssignmentFingerprint(tc.a); got != tc.want {
			t.Errorf("%s: legacy fingerprint = %s, want %s (as written by d887f7b)", tc.name, got, tc.want)
		}
		// Free text was never part of it.
		withComment(tc.a, "a comment")
		tc.a.Description, tc.a.CategoryDescription = "d", "c"
		if got := legacyAssignmentFingerprint(tc.a); got != tc.want {
			t.Errorf("%s: free text changed the legacy fingerprint", tc.name)
		}
	}
}

// TestProcessAssignments_UpgradeFromGoldenCheckpoint: a checkpoint holding
// the literal value the first release wrote.
func TestProcessAssignments_UpgradeFromGoldenCheckpoint(t *testing.T) {
	shared, _ := newTestEnv(t, &fakeClient{})
	for id, fp := range map[string]string{"1": "af4bbd80e16ff9bf", "2": "af4bbd80e16ff9bf"} {
		if err := SaveFingerprint(shared.CP, assignmentSurface, testKid.Name, "00100:"+id, fp); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Both records have the gradebook facts of the golden "plain" record
	// (the id is not part of the fingerprint).
	quiet := sampleAssignment("1", "Lab report")
	quiet.Description, quiet.CategoryDescription = "Write up the lab.", "Work done in class."
	commented := withComment(sampleAssignment("2", "Lab report"), "See me after class.")

	got := pollOnce(t, shared, []*pslib.Assignment{quiet, commented})
	if len(got) != 1 || got[0].Meta.Tags["assignment_id"] != "2" || got[0].Meta.Tags["changed"] != "teacher_comment" {
		t.Fatalf("first poll after the upgrade staged %d items, want only the commented assignment with changed=teacher_comment", len(got))
	}
	if again := pollOnce(t, shared, []*pslib.Assignment{quiet, commented}); len(again) != 0 {
		t.Errorf("second poll staged %d items, want none", len(again))
	}
	// An old checkpoint whose facts have also moved: staged, and the item
	// says the gradebook record changed without claiming to know how.
	if err := SaveFingerprint(shared.CP, assignmentSurface, testKid.Name, "00100:1", "0123456789abcdef"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	moved := pollOnce(t, shared, []*pslib.Assignment{quiet, commented})
	if len(moved) != 1 || moved[0].Meta.Tags["changed"] != "record" {
		t.Errorf("old checkpoint with different facts: %d items, changed=%v; want one with changed=record", len(moved), moved)
	}
}
