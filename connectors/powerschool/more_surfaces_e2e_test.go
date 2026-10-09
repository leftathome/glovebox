package powerschool

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	pslib "github.com/leftathome/powerschool-go"
)

// Synthetic renderings of the three extra pages, in the shape the library
// parses (see powerschool-go docs/PAGES_STRUCTURE.md). Each is rendered for
// whichever student the portal has selected, as the real pages are. Dates
// are computed from today so they fall inside the default lookbacks.

var (
	storedGrades   = map[string]string{"111111": "A-", "111112": "B+"}
	attendanceCode = map[string]string{"111111": "VU", "111112": "L"}
)

// e2eSwitcher is the student switcher every guardian page carries, in the
// live markup's shape, marking `selected`.
func e2eSwitcher(selected string) string {
	li := func(id, name string) string {
		attr := ""
		if id == selected {
			attr = `class="selected"`
		}
		return `<li ` + attr + `><a href="javascript:switchStudent(` + id + `);">` + name + `</a></li>`
	}
	return `<ul id="students-list">` + li("111111", "Student One") + li("111112", "Student Two") + `</ul>
<form id="switch_student_form" action="home.html" method="post"><input type="hidden" name="selected_student_id"/></form>`
}

// thisMonday is the Monday of the current week, UTC.
func thisMonday() time.Time {
	now := time.Now().UTC()
	d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

func e2eAttendance(selected, _ string) string {
	monday := thisMonday()
	friday := monday.AddDate(0, 0, 4)
	startYear := monday.Year()
	if monday.Month() < time.July {
		startYear--
	}
	enroll := fmt.Sprintf("E: 07/15/%d L: 06/28/%d", startYear, startYear+1)
	return `<!doctype html><html><body>` + e2eSwitcher(selected) + `<div id="content-main" role="main">
<h1>Meeting Attendance History: ` + studentHeadings[selected] + `</h1>
<div class="box-round"><table class="grid"><tbody>
<tr><th rowspan="2">Course</th><th rowspan="2">Expression</th>
<th class="center" colspan="5">` + fmt.Sprintf("%d/%d-&rlm;%d/%d", int(monday.Month()), monday.Day(), int(friday.Month()), friday.Day()) + `</th></tr>
<tr><th>M</th><th>T</th><th>W</th><th>H</th><th>F</th></tr>
<tr><td class="table-element-text-align-start">EXAMPLE SCIENCE 7A<br>Lindqvist, Marta 12<br>` + enroll + `</td><td>1(A)</td>
<td>` + attendanceCode[selected] + `</td><td></td><td>-</td><td>.</td><td></td></tr>
</tbody></table></div>
<div id="legend"><h2>Legend</h2><p><strong>Attendance Codes:</strong> Blank=Present <span class="disclaimer">|</span>
FT=AUTH-Field Trip <span class="disclaimer">|</span> ME=EA-Medical/Sick <span class="disclaimer">|</span>
VU=UA-Unexcused Travel <span class="disclaimer">|</span> A=A-Absent <span class="disclaimer">|</span> L=L-Late <span class="disclaimer">|</span></p></div>
</div></body></html>`
}

func e2eTermGrades(selected, _ string) string {
	completed := time.Now().UTC().AddDate(0, -1, 0).Format("01/2006")
	return `<!doctype html><html><body>` + e2eSwitcher(selected) + `<div id="content-main" role="main"><div class="container">
<h1>Grade History and GPA Report</h1>
<table class="cell-border" id="grades-tbl"><thead><tr>
<th scope="col">Date Compl</th><th scope="col">Grade Level</th><th scope="col">School</th><th scope="col">Course #</th>
<th scope="col">Course</th><th scope="col">Credit Earned</th><th scope="col">Credit Attempted</th><th scope="col">Grade</th></tr></thead>
<tbody><tr><td data-sort="1">` + completed + `</td><td>7</td><td>Example MS</td><td>SCI7001</td><td>EXAMPLE SCIENCE 7A</td>
<td>0.500</td><td>0.500</td><td>` + storedGrades[selected] + `<b></b></td></tr></tbody></table>
</div></div></body></html>`
}

func e2eComments(selected, rawQuery string) string {
	q, _ := url.ParseQuery(rawQuery)
	// The term parameter is fg; a bare request shows the current term, S1.
	// As on the live page: option values are URLs, the shown term's option
	// is marked selected, and when a term is requested the first ("show
	// dropped") option's URL carries that term too.
	requested := q.Get("fg")
	term := requested
	if term == "" {
		term = "S1"
	}
	options := `<option value="teachercomments.html?fg=` + requested + `&showdropped="></option>`
	for _, t := range []string{"Q1", "Q2", "Q3", "Q4", "S1", "S2"} {
		mark := ""
		if t == term {
			mark = ` selected`
		}
		options += `<option value="teachercomments.html?fg=` + t + `"` + mark + `>` + t + `</option>`
	}
	comment := ""
	if term == "Q1" || term == "S1" {
		comment = "Note about " + kidOf[selected] + " for " + term + "."
	}
	return `<!doctype html><html><body>` + e2eSwitcher(selected) + `<div id="content-main" role="main">
<h1>Teacher Comments: ` + studentHeadings[selected] + `</h1>
<div class="box-round"><h2><form class="noSubmitLoading" name="navigation">
<select id="reportingTermHeadingSelect" name="storecode">` + options + `</select></form></h2>
<table class="grid linkDescList"><tbody>
<tr><th>Exp.</th><th>Course #</th><th>Course</th><th>Teacher</th><th>Comment</th></tr>
<tr><td>HR(A)</td><td>ADV7000</td><td>ADVISORY</td><td><a href="mailto:t1@example.org">Email Okafor, Daniel R</a></td><td><pre></pre></td></tr>
<tr><td>1(A)</td><td>SCI7001</td><td>EXAMPLE SCIENCE 7A</td><td><a href="mailto:t2@example.org">Email Lindqvist, Marta</a></td><td><pre>` + comment + `</pre></td></tr>
</tbody></table></div></div></body></html>`
}

func extraPortal(t *testing.T, selected string) *fakePortal {
	t.Helper()
	portal := newFakePortal(t, selected)
	portal.extra = map[string]func(selected, rawQuery string) string{
		pslib.AttendancePath:      e2eAttendance,
		pslib.GradeHistoryPath:    e2eTermGrades,
		pslib.TeacherCommentsPath: e2eComments,
	}
	return portal
}

// TestEndToEnd_ExtraSurfaces_RealLibrary: all six surfaces, both kids, the
// real powerschool-go on the production transport against the loopback
// portal. The pages are fetched and parsed by the library; nothing is
// handed over by a fake client.
func TestEndToEnd_ExtraSurfaces_RealLibrary(t *testing.T) {
	portal := extraPortal(t, "111111")
	captureLogs(t)
	client, err := NewProductionClient(libraryOn(t, portal.recordingServer, true))
	if err != nil {
		t.Fatalf("NewProductionClient: %v", err)
	}
	cfg := allSurfacesConfig()
	cfg.TeacherComments.Terms = []string{"Q1", "Q2", "S1"}
	c, cp, dir := newWiredConnector(t, client, cfg)
	rec := recordMetrics(t, c)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	items := readItems(t, dir)

	monday := thisMonday().Format("2006-01-02")
	for studentID, kid := range kidOf {
		mine := func(surface string) []stagedItem {
			var out []stagedItem
			for _, it := range itemsOf(items, surface) {
				if it.Meta.Tags["kid"] == kid {
					out = append(out, it)
				}
			}
			return out
		}

		days := mine(attendanceDaySurface)
		if len(days) != 1 {
			t.Fatalf("%s: %d attendance_day items, want 1", kid, len(days))
		}
		wantKind := map[string]string{"VU": "absence_unexcused", "L": "late_unexcused"}[attendanceCode[studentID]]
		if d := days[0].Meta.Tags; d["code"] != attendanceCode[studentID] || d["date"] != monday || d["kind"] != wantKind ||
			d["course"] != "EXAMPLE SCIENCE 7A" || d["code_description"] == "" {
			t.Errorf("%s attendance_day tags = %v, want code %s on %s (%s)", kid, d, attendanceCode[studentID], monday, wantKind)
		}

		grades := mine(termGradeSurface)
		if len(grades) != 1 || grades[0].Meta.Tags["grade"] != storedGrades[studentID] || grades[0].Meta.Tags["course_number"] != "SCI7001" {
			t.Errorf("%s term_grade items = %+v, want one with grade %s", kid, grades, storedGrades[studentID])
		}

		comments := mine(teacherCommentSurface)
		terms := map[string]bool{}
		for _, cm := range comments {
			terms[cm.Meta.Tags["term"]] = true
			if got := strings.Join(cm.Meta.Audience, ","); got != "guardians" {
				t.Errorf("%s teacher_comment audience = %s, want guardians only", kid, got)
			}
			if cm.Meta.DataSubject != subjectOf[kid] {
				t.Errorf("%s teacher_comment data_subject = %q", kid, cm.Meta.DataSubject)
			}
			if !strings.Contains(cm.Body, "Note about "+kid+" for "+cm.Meta.Tags["term"]) || !strings.Contains(cm.Body, "Teacher: Lindqvist, Marta") {
				t.Errorf("%s teacher_comment body = %q", kid, cm.Body)
			}
			for k, v := range cm.Meta.Tags {
				if strings.Contains(v, "Lindqvist") {
					t.Errorf("tag %s carries the teacher's name", k)
				}
			}
		}
		// Q2 was read and has no comment: no item.
		if len(comments) != 2 || !terms["Q1"] || !terms["S1"] {
			t.Errorf("%s teacher_comment terms = %v, want Q1 and S1", kid, terms)
		}

		for _, surface := range []string{gradeSurface, attendanceSurface, assignmentSurface, attendanceDaySurface, termGradeSurface} {
			for _, it := range mine(surface) {
				if strings.Join(it.Meta.Audience, ",") != "subject,guardians" || it.Meta.DataSubject != subjectOf[kid] {
					t.Errorf("%s %s routing = %q %v", kid, surface, it.Meta.DataSubject, it.Meta.Audience)
				}
			}
			if len(mine(surface)) == 0 {
				t.Errorf("%s: no %s item", kid, surface)
			}
		}
	}

	// Read-only: every request is a GET, except the two allowlisted POSTs.
	gets := map[string]int{}
	for _, r := range portal.requests() {
		method, path, _ := strings.Cut(r, " ")
		switch {
		case method == "GET":
			gets[path]++
		case r == "POST "+AssignmentLookupPath, r == "POST "+SwitchStudentPath:
		default:
			t.Errorf("unexpected request %q", r)
		}
	}
	for path, want := range map[string]int{
		pslib.AttendancePath:      2, // one per kid
		pslib.GradeHistoryPath:    2,
		pslib.TeacherCommentsPath: 6, // three terms per kid
	} {
		if gets[path] != want {
			t.Errorf("GET %s: %d requests, want %d", path, gets[path], want)
		}
	}
	for k := range rec.counters() {
		if strings.HasPrefix(k, "powerschool_kids_skipped_total") || strings.HasPrefix(k, "powerschool_surface_errors_total") {
			t.Errorf("failure metric on a clean poll: %s", k)
		}
	}

	// A second poll: nothing has changed, nothing is staged again.
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := len(readItems(t, dir)); got != len(items) {
		t.Errorf("second poll staged %d more items", got-len(items))
	}
}

// TestEndToEnd_ExtraPageOfTheOtherChild_RealLibrary: the portal serves the
// attendance page for the other child during k1's turn. That page's own
// switcher marks the other child; the library reports the id it marks and
// the connector discards k1.
func TestEndToEnd_ExtraPageOfTheOtherChild_RealLibrary(t *testing.T) {
	portal := extraPortal(t, "111111")
	served := false
	portal.extra[pslib.AttendancePath] = func(selected, q string) string {
		if selected == "111111" && !served {
			served = true
			return e2eAttendance("111112", q)
		}
		return e2eAttendance(selected, q)
	}
	captureLogs(t)
	client, err := NewProductionClient(libraryOn(t, portal.recordingServer, true))
	if err != nil {
		t.Fatalf("NewProductionClient: %v", err)
	}
	cfg := allSurfacesConfig()
	cfg.TeacherComments.Terms = []string{"Q1"}
	c, cp, dir := newWiredConnector(t, client, cfg)
	rec := recordMetrics(t, c)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	for _, it := range readItems(t, dir) {
		if it.Meta.Tags["kid"] == "k1" {
			t.Errorf("k1 item staged from a turn that read the other child's attendance page: %s", it.Meta.Tags["surface"])
		}
	}
	assertNoFingerprints(t, cp, "k1")
	if rec.counters()["powerschool_kids_skipped_total{kid=k1,reason=selection_changed}"] != 1 {
		t.Errorf("selection_changed not recorded for k1: %v", rec.counters())
	}
	k2 := 0
	for _, it := range readItems(t, dir) {
		if it.Meta.Tags["kid"] == "k2" {
			k2++
		}
	}
	if k2 == 0 {
		t.Error("k2's own turn staged nothing")
	}
}
