package schoology

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	schoologylib "github.com/leftathome/schoology-go"
)

// Tests for the behaviour added after the first review of the
// assignment_detail surface. All names and text are invented.

// gatedBackend is a staging backend whose NewItem can be made to fail.
type gatedBackend struct {
	connector.StagingBackend
	fail func(opts connector.ItemOptions) error
}

func (b *gatedBackend) NewItem(opts connector.ItemOptions) (*connector.StagingItem, error) {
	if b.fail != nil {
		if err := b.fail(opts); err != nil {
			return nil, err
		}
	}
	return b.StagingBackend.NewItem(opts)
}

func simpleRows(h *detailHarness, from, to int64) []*schoologylib.Assignment {
	var rows []*schoologylib.Assignment
	for id := from; id <= to; id++ {
		title := "Task " + strconv.FormatInt(id, 10)
		h.details[id] = regularDetail(id, title, "Example Course A: Section 1")
		rows = append(rows, listRow(id, title, "Example Course A"))
	}
	return rows
}

func authErr(op string) error {
	return &schoologylib.Error{Code: schoologylib.ErrCodeAuth, Op: op,
		Message: "redirected to a login or home page instead of the assignment (session expired?)"}
}

// ---------------------------------------------------------------------
// Title cross-check
// ---------------------------------------------------------------------

func TestAssignmentDetail_TitleMismatchIsNotStaged(t *testing.T) {
	h := newDetailHarness(t)
	h.details[1] = regularDetail(1, "Somebody else's essay", "Example Course A: Section 1")
	h.details[2] = regularDetail(2, "reading   LOG", "Example Course A: Section 1")

	staged, errs, _ := h.poll(k1List(
		listRow(1, "Chapter 3 questions", "Example Course A"),
		listRow(2, "Reading log", "Example Course A"),
	))
	if staged != 1 || errs {
		t.Fatalf("staged=%d errs=%v, want 1 (case and spacing are folded) and no poll error", staged, errs)
	}
	if got := h.detailItems()[0].meta.Tags["assignment_id"]; got != "2" {
		t.Errorf("staged assignment %s, want 2", got)
	}
	rs := h.receipts(detailClassTitleMismatch)
	if len(rs) != 1 || rs[0].meta.Tags["affected_item_ids"] != "1" {
		t.Fatalf("title_mismatch receipts = %v, want one naming assignment 1", rs)
	}

	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	h.poll(k1List(listRow(1, "Chapter 3 questions", "Example Course A")))
	if len(h.fetched) != 0 {
		t.Errorf("refetched %v an hour after a title mismatch", h.fetched)
	}
}

func TestSameTitle(t *testing.T) {
	cases := []struct {
		list, detail string
		want         bool
	}{
		{"Unit 2 test", "Unit 2 test", true},
		{"Unit 2 test", "  unit 2   TEST ", true},
		{"", "Unit 2 test", true},
		{"Unit 2 test", "", true},
		{"Unit 2 test", "Unit 2 test (retake)", false},
		{"Unit 2 test", "Unit 3 test", false},
	}
	for _, c := range cases {
		if got := sameTitle(c.list, c.detail); got != c.want {
			t.Errorf("sameTitle(%q, %q) = %v, want %v", c.list, c.detail, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------
// Auth answers are confirmed before they are believed
// ---------------------------------------------------------------------

// One assignment that redirects to the home page looks like an expired
// session. With the session check passing it must be a receipt for that
// assignment and nothing more.
func TestAssignmentDetail_AuthAnswerOnLiveSessionIsPerItem(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 3)
	h.failWith[1] = authErr("GetAssignment")
	h.validateErr = nil // the session is fine

	staged, errs, dedup := h.poll(k1List(rows...))
	if staged != 2 {
		t.Errorf("staged = %d, want the other 2", staged)
	}
	if errs {
		t.Error("a per-assignment redirect was reported as a poll error")
	}
	if dedup.AffectedCount(detailParser, "auth") != 0 {
		t.Fatal("the session was reported as refused; that would exit the process and mark a live cookie rejected")
	}
	if h.validated != 1 {
		t.Errorf("session checked %d times, want 1", h.validated)
	}
	if len(h.receipts("auth")) != 0 || len(h.receipts(detailClassUnexpectedPage)) != 1 {
		t.Errorf("receipts: auth=%d unexpected_page=%d, want 0 and 1",
			len(h.receipts("auth")), len(h.receipts(detailClassUnexpectedPage)))
	}

	// Remembered: not asked again until the refresh.
	h.resetCalls()
	h.validated = 0
	h.now = h.now.Add(time.Hour)
	h.poll(k1List(rows...))
	if len(h.fetched) != 0 || h.validated != 0 {
		t.Errorf("an hour later: fetched=%v validated=%d, want nothing", h.fetched, h.validated)
	}
}

func TestAssignmentDetail_AuthAnswerConfirmedByRefusedSessionCheck(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 3)
	h.failWith[1] = authErr("GetAssignment")
	h.validateErr = schoologylib.ErrSessionExpired

	staged, errs, dedup := h.poll(k1List(rows...))
	if staged != 0 || !errs || len(h.fetched) != 1 {
		t.Errorf("staged=%d errs=%v fetched=%v, want 0,true and no request after the refusal", staged, errs, h.fetched)
	}
	if dedup.AffectedCount(detailParser, "auth") != 1 {
		t.Error("a confirmed dead session was not recorded for the rejected-session path")
	}
	state, _ := loadDetailRows(h.cp, "k1")
	if state[1].FetchedAt != 0 {
		t.Error("the assignment was remembered as read under a dead session")
	}
}

func TestAssignmentDetail_AuthAnswerWithUnreachableSessionCheckStopsWithoutBlame(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 3)
	h.failWith[1] = authErr("GetAssignment")
	h.validateErr = &schoologylib.Error{Code: schoologylib.ErrCodeNetwork, Op: "ValidateSession", Message: "dial timeout"}

	staged, errs, dedup := h.poll(k1List(rows...))
	if staged != 0 || !errs || len(h.fetched) != 1 {
		t.Errorf("staged=%d errs=%v fetched=%v, want the surface stopped", staged, errs, h.fetched)
	}
	if dedup.AffectedCount(detailParser, "auth") != 0 {
		t.Error("the session was called dead on the strength of an unconfirmed answer")
	}
	state, _ := loadDetailRows(h.cp, "k1")
	if state[1].FetchedAt != 0 {
		t.Error("the assignment was remembered although nothing was established")
	}
}

func TestAssignmentDetail_AuthAnswerOnCommentsPageOfLiveSession(t *testing.T) {
	h := newDetailHarness(t)
	h.details[1] = assessmentDetail(1, "Unit 2 test", "Example Course B: Section 2")
	h.commErr[1] = authErr("GetAssignmentComments")

	staged, _, dedup := h.poll(k1List(listRow(1, "Unit 2 test", "Example Course B")))
	if staged != 1 {
		t.Fatalf("staged = %d, want the detail staged without comments", staged)
	}
	if dedup.AffectedCount(detailParser, "auth") != 0 {
		t.Error("the session was reported as refused")
	}
	if got := h.detailItems()[0].meta.Tags["comments_state"]; got != "unknown" {
		t.Errorf("comments_state = %q, want unknown", got)
	}
	if len(h.receipts(detailClassUnexpectedPage)) != 1 {
		t.Error("no unexpected_page receipt for the comments page")
	}
}

// ---------------------------------------------------------------------
// Staging failures
// ---------------------------------------------------------------------

func TestAssignmentDetail_TransientStageFailureStopsTheSurface(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 4)
	real := h.writer
	down := true
	h.writer = &gatedBackend{StagingBackend: real, fail: func(connector.ItemOptions) error {
		if down {
			return errors.New("ingest failed after 3 retries: ingest returned status 503")
		}
		return nil
	}}

	staged, errs, _ := h.poll(k1List(rows...))
	if staged != 0 || !errs {
		t.Errorf("staged=%d errs=%v, want 0,true", staged, errs)
	}
	if len(h.fetched) != 1 {
		t.Errorf("made %d page requests with staging down, want to stop after the first", len(h.fetched))
	}
	if seen, _ := loadDetailSeen(h.cp, "k1"); len(seen) != 0 {
		t.Errorf("seen-set = %v, want nothing marked seen", seen)
	}
	if state, _ := loadDetailRows(h.cp, "k1"); state[1].FetchedAt != 0 {
		t.Error("the unstaged assignment was remembered as read")
	}

	// Staging comes back: everything is staged, once.
	down = false
	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	if staged, _, _ := h.poll(k1List(rows...)); staged != 4 {
		t.Errorf("next poll staged %d, want all 4", staged)
	}
}

func TestAssignmentDetail_PermanentStageRejectionIsRememberedWithReceipt(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 3)
	real := h.writer
	h.writer = &gatedBackend{StagingBackend: real, fail: func(opts connector.ItemOptions) error {
		if opts.Subject == "Task 2" {
			return connector.PermanentError(errors.New("ingest rejected with status 400"))
		}
		return nil
	}}

	staged, errs, _ := h.poll(k1List(rows...))
	if staged != 2 || errs {
		t.Errorf("staged=%d errs=%v, want the other 2 and no poll error", staged, errs)
	}
	rs := h.receipts(detailClassStageRejected)
	if len(rs) != 1 || rs[0].meta.Tags["affected_item_ids"] != "2" {
		t.Fatalf("stage_rejected receipts = %v, want one naming assignment 2", rs)
	}

	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	h.poll(k1List(rows...))
	if len(h.fetched) != 0 {
		t.Errorf("refetched %v an hour after a permanent rejection", h.fetched)
	}
}

func TestStagePermanent(t *testing.T) {
	if !stagePermanent(connector.PermanentError(errors.New("ingest rejected with status 413"))) {
		t.Error("a PermanentError is not permanent")
	}
	if !stagePermanent(errors.New("metadata validation: [subject: exceeds 1024 characters]")) {
		t.Error("a metadata validation failure is not permanent")
	}
	if stagePermanent(errors.New("ingest failed after 3 retries: http request failed")) {
		t.Error("an unreachable ingest is permanent")
	}
}

// Staging refuses a subject, sender or tag value over 1024 bytes or with a
// control character. A long title must cost a marker, not the item.
func TestAssignmentDetail_OversizeAndUncleanMetadataIsFittedNotRejected(t *testing.T) {
	h := newDetailHarness(t)
	long := strings.Repeat("very long title ", 200) // 3200 bytes
	d := regularDetail(1, long, "Example Course A: Section 1")
	d.Category = strings.Repeat("c", 3000)
	d.GradingPeriod = "Quarter\x07 1\nStatus: graded"
	d.CourseTitle = "Example Course A: " + strings.Repeat("s", 2000)
	h.details[1] = d

	staged, errs, _ := h.poll(k1List(listRow(1, long, "Example Course A")))
	if staged != 1 || errs {
		t.Fatalf("staged=%d errs=%v, want 1,false", staged, errs)
	}
	if len(h.receipts(detailClassStageRejected)) != 0 {
		t.Fatal("staging rejected the item")
	}
	it := h.detailItems()[0]
	for name, v := range map[string]string{
		"subject":      it.meta.Subject,
		"sender":       it.meta.Sender,
		"tag category": it.meta.Tags["category"],
		"tag course":   it.meta.Tags["course"],
	} {
		if len(v) > maxDetailMetaBytes {
			t.Errorf("%s is %d bytes, over the staging limit", name, len(v))
		}
		if !strings.HasSuffix(v, detailTruncationMark) {
			t.Errorf("%s does not end with the truncation marker: ...%q", name, v[len(v)-20:])
		}
	}
	if it.meta.Tags["metadata_truncated"] != "true" {
		t.Error("no metadata_truncated tag")
	}
	if got := it.meta.Tags["grading_period"]; got != "Quarter 1 Status: graded" {
		t.Errorf("grading_period = %q, want control characters dropped and the line break folded", got)
	}
}

// ---------------------------------------------------------------------
// Fairness
// ---------------------------------------------------------------------

func TestAssignmentDetail_KidsTakeTurnsWithinTheCap(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.MaxPerPoll = 4
	k1rows := simpleRows(h, 1, 6)
	k2rows := simpleRows(h, 101, 102)

	h.poll(k1List(k1rows...), k2List(k2rows...))
	got := map[int64]bool{}
	for _, id := range h.fetched {
		got[id] = true
	}
	if len(h.fetched) != 4 || !got[101] || !got[102] {
		t.Errorf("fetched %v, want both of k2's rows inside a cap of 4 despite k1's six", h.fetched)
	}
}

func TestAssignmentDetail_FirstTurnRotatesBetweenPolls(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.MaxPerPoll = 1
	k1rows := simpleRows(h, 1, 3)
	k2rows := simpleRows(h, 101, 103)

	var firsts []int64
	for i := 0; i < 4; i++ {
		h.resetCalls()
		h.poll(k1List(k1rows...), k2List(k2rows...))
		if len(h.fetched) != 1 {
			t.Fatalf("poll %d fetched %v, want exactly one", i, h.fetched)
		}
		firsts = append(firsts, h.fetched[0])
		h.now = h.now.Add(time.Hour)
	}
	k1n, k2n := 0, 0
	for _, id := range firsts {
		if id < 100 {
			k1n++
		} else {
			k2n++
		}
	}
	if k1n != 2 || k2n != 2 {
		t.Errorf("with a cap of 1 over four polls the reads went %v; want two for each kid", firsts)
	}
}

// A row the cap deferred must go ahead of rows that appear later, wherever
// Schoology puts them in the list.
func TestAssignmentDetail_DeferredRowsGoAheadOfLaterArrivals(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.MaxPerPoll = 1
	rows := simpleRows(h, 1, 2)
	h.poll(k1List(rows...)) // reads 1, defers 2

	var read []int64
	next := int64(50)
	for i := 0; i < 3; i++ {
		// Every poll a brand-new row is listed first.
		newcomer := simpleRows(h, next, next)
		next++
		rows = append(newcomer, rows...)
		h.resetCalls()
		h.now = h.now.Add(time.Hour)
		h.poll(k1List(rows...))
		read = append(read, h.fetched...)
	}
	if len(read) == 0 || read[0] != 2 {
		t.Errorf("reads after the deferral = %v; want the deferred row (2) first, not starved by newcomers", read)
	}
}

func TestAssignmentDetail_LongOverdueRefreshIsPromoted(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.MaxPerPoll = 1
	old := simpleRows(h, 1, 1)
	h.poll(k1List(old...))

	// Three refresh intervals later a new row appears alongside it.
	h.resetCalls()
	h.now = h.now.Add(detailStaleFactor * h.cfg.refreshInterval())
	rows := append(simpleRows(h, 2, 2), old...)
	h.poll(k1List(rows...))
	if len(h.fetched) != 1 || h.fetched[0] != 1 {
		t.Errorf("fetched %v, want the long-overdue refresh (1) ahead of the new row", h.fetched)
	}
}

// ---------------------------------------------------------------------
// Comments: count, not text, while the layout is unmapped
// ---------------------------------------------------------------------

func TestAssignmentDetail_UnmappedCommentTextIsNotPartOfIdentity(t *testing.T) {
	h := newDetailHarness(t)
	row := listRow(1, "Lab report", "Example Course A")
	set := func(texts ...string) {
		d := regularDetail(1, "Lab report", "Example Course A: Section 1")
		c := schoologylib.AssignmentComments{State: schoologylib.CommentsStatePopulated, LayoutUnmapped: true}
		for _, s := range texts {
			c.Comments = append(c.Comments, &schoologylib.AssignmentComment{Text: s})
		}
		if len(texts) == 0 {
			c = schoologylib.AssignmentComments{State: schoologylib.CommentsStateEmpty}
		}
		d.Comments = c
		h.details[1] = d
	}
	step := func() int {
		staged, _, _ := h.poll(k1List(row))
		h.now = h.now.Add(25 * time.Hour)
		return staged
	}

	set()
	if step() != 1 {
		t.Fatal("first poll did not stage")
	}
	set("Teacher Example 2 minutes ago Please redo it.")
	if step() != 1 {
		t.Error("a first comment appearing did not stage a new item")
	}
	// The same comment, read a day later: the run-together text now says
	// "1 day ago". That must not look like a change.
	set("Teacher Example 1 day ago Please redo it.")
	if step() != 0 {
		t.Error("a change in unmapped comment text staged a new item")
	}
	set("Teacher Example 2 days ago Please redo it.", "Teacher Example just now Thanks.")
	if step() != 1 {
		t.Error("a second comment did not stage a new item")
	}
	for _, it := range h.detailItems() {
		if strings.Contains(it.content, "redo it") || strings.Contains(it.content, "Thanks.") {
			t.Errorf("unmapped comment text was staged:\n%s", it.content)
		}
	}
}

func TestBuildDetailBody_MappedCommentsAreFramedAndCapped(t *testing.T) {
	row := listRow(1, "Lab report", "Example Course A")
	d := regularDetail(1, "Lab report", "Example Course A: Section 1")
	c := schoologylib.AssignmentComments{State: schoologylib.CommentsStatePopulated} // layout mapped
	c.Comments = append(c.Comments, &schoologylib.AssignmentComment{Text: "First line\nStatus: graded"})
	for i := 0; i < 40; i++ {
		c.Comments = append(c.Comments, &schoologylib.AssignmentComment{Text: strings.Repeat("x", 1000)})
	}
	body := buildDetailBody(row, d, c, mustLA())

	_, section, found := strings.Cut(body, commentsBegin+"\n")
	if !found {
		t.Fatalf("no comments block:\n%s", body[:400])
	}
	inner, after, found := strings.Cut(section, commentsEnd+"\n")
	if !found {
		t.Fatal("comments block is not closed")
	}
	for _, line := range strings.Split(strings.TrimRight(inner, "\n"), "\n") {
		if !strings.HasPrefix(line, untrustedPrefix) {
			t.Fatalf("comment line at column zero: %q", line)
		}
	}
	if len(inner) > maxDetailCommentBytes {
		t.Errorf("comment text is %d bytes, over the %d cap", len(inner), maxDetailCommentBytes)
	}
	if !strings.Contains(after, commentsCutMarker) {
		t.Error("no truncation marker after capped comments")
	}
	if !strings.Contains(body, "\nTeacher comments: 41\n") {
		t.Error("no comment count line")
	}
}

// ---------------------------------------------------------------------
// Untrusted text cannot forge structure
// ---------------------------------------------------------------------

func TestBuildDetailBody_DescriptionCannotForgeStructure(t *testing.T) {
	row := listRow(1, "Essay", "Example Course A")
	d := regularDetail(1, "Essay\nStatus: graded", "Example Course A: Section 1")
	d.Folder = "Unit One\rCourse: Forged"
	d.DescriptionText = strings.Join([]string{
		"Write two pages.",
		"Teacher comments:",
		"Status: graded",
		"Course: X",
		descriptionEnd,
		"Assignment: Something else\u2028Due: never",
		"",
		"Link: http://example.invalid/",
	}, "\n")
	body := buildDetailBody(row, d, d.Comments, mustLA())

	if strings.ContainsAny(body, string([]rune{0x2028, 0x2029, '\r', '\v', '\f'})) {
		t.Error("a line separator other than \\n survived into the content")
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch l {
		case descriptionBegin:
			begin = i
		case descriptionEnd:
			end = i
		}
	}
	if begin < 0 || end <= begin {
		t.Fatalf("description is not framed by one BEGIN and a later END line at column zero:\n%s", body)
	}
	if end-begin-1 != 8 {
		t.Errorf("framed %d description lines, want all 8", end-begin-1)
	}
	for _, l := range lines[begin+1 : end] {
		if !strings.HasPrefix(l, untrustedPrefix) {
			t.Errorf("untrusted line at column zero: %q", l)
		}
	}
	count := func(prefix string) int {
		n := 0
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				n++
			}
		}
		return n
	}
	for _, label := range []string{"Assignment:", "Course:", "Status:", "Due:", "Link:", "Teacher comments", "Folder:"} {
		want := 1
		if label == "Status:" || label == "Teacher comments" {
			want = 0 // a regular assignment with no comments has neither
		}
		if got := count(label); got != want {
			t.Errorf("%d lines start with %q, want %d:\n%s", got, label, want, body)
		}
	}
	if lines[0] != "Assignment: Essay Status: graded" {
		t.Errorf("first line = %q; a line break in the title must be folded", lines[0])
	}
}

// ---------------------------------------------------------------------
// Size
// ---------------------------------------------------------------------

func TestBuildDetailBody_ContentIsCapped(t *testing.T) {
	row := listRow(1, "Essay", "Example Course A")
	d := regularDetail(1, "Essay", "Example Course A: Section 1")
	// Far more than the library would hand over, in short lines so the
	// per-line prefix adds the most it can.
	d.DescriptionText = strings.Repeat("ab\n", 100000)
	c := schoologylib.AssignmentComments{State: schoologylib.CommentsStatePopulated}
	for i := 0; i < 100; i++ {
		c.Comments = append(c.Comments, &schoologylib.AssignmentComment{Text: strings.Repeat("y", 2000)})
	}
	body := buildDetailBody(row, d, c, mustLA())

	if len(body) > maxDetailContentBytes {
		t.Fatalf("content is %d bytes, over the %d cap", len(body), maxDetailContentBytes)
	}
	if len(body) < maxDetailContentBytes/2 {
		t.Errorf("content is only %d bytes; the cap cut far more than it needed to", len(body))
	}
	for _, want := range []string{descriptionEnd + "\n", descriptionCutMarker, commentsBegin, commentsEnd + "\n", commentsCutMarker} {
		if !strings.Contains(body, want) {
			t.Errorf("capped content lost %q", want)
		}
	}
}

// ---------------------------------------------------------------------
// Course cross-check that could not be made
// ---------------------------------------------------------------------

func TestAssignmentDetail_PageWithoutCourseIsStagedAsUnverified(t *testing.T) {
	h := newDetailHarness(t)
	d := regularDetail(1, "Essay", "")
	d.CourseID = 0
	h.details[1] = d

	staged, errs, _ := h.poll(k1List(listRow(1, "Essay", "Example Course A")))
	if staged != 1 || errs {
		t.Fatalf("staged=%d errs=%v, want staged and no poll error", staged, errs)
	}
	it := h.detailItems()[0]
	if it.meta.Tags["course_unverified"] != "true" || it.meta.Tags["parse_status"] != "degraded" {
		t.Errorf("tags = %v, want course_unverified=true and parse_status=degraded", it.meta.Tags)
	}
	if it.meta.Sender != "Example Course A" {
		t.Errorf("sender = %q, want the list row's course", it.meta.Sender)
	}
	if len(h.receipts(detailClassCourseUnverified)) != 1 {
		t.Error("no course_unverified receipt")
	}
}

// ---------------------------------------------------------------------
// Cancellation and the poll window
// ---------------------------------------------------------------------

func TestAssignmentDetail_CancelledBeforeStartMakesNoRequest(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.ctx = ctx

	staged, errs, _ := h.poll(k1List(rows...))
	if staged != 0 || errs || len(h.fetched) != 0 {
		t.Errorf("staged=%d errs=%v fetched=%v, want nothing at all", staged, errs, h.fetched)
	}
}

func TestAssignmentDetail_CancellationMidRequestIsNotAFailure(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 4)
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	h.onFetch = func(id int64) {
		if id == 2 {
			cancel()
			h.failWith[2] = &schoologylib.Error{Code: schoologylib.ErrCodeNetwork, Op: "GetAssignment",
				Message: "request failed", Err: context.Canceled}
		}
	}

	staged, errs, dedup := h.poll(k1List(rows...))
	if staged != 1 || len(h.fetched) != 2 {
		t.Errorf("staged=%d fetched=%v, want 1 staged and no request after the cancellation", staged, h.fetched)
	}
	if errs {
		t.Error("a cancelled request was reported as a poll error")
	}
	if n := dedup.AffectedCount(detailParser, "network"); n != 0 {
		t.Errorf("a cancelled request was recorded as %d network failure(s)", n)
	}
	for _, it := range h.items() {
		if it.meta.Source == "schoology-parse-failure" {
			t.Errorf("a receipt was staged for a cancelled request: %v", it.meta.Tags)
		}
	}
	if state, _ := loadDetailRows(h.cp, "k1"); state[2].FetchedAt != 0 {
		t.Error("the cancelled assignment was remembered as read")
	}
}

func TestAssignmentDetail_CancellationDuringSpacingStops(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 3)
	dedup := NewReceiptDedup()
	staged, errs := ProcessAssignmentDetails(context.Background(), DetailDeps{
		Client: h.client(), Writer: h.writer, Matcher: h.matcher, Checkpoint: h.cp,
		Dedup: dedup, LibVersion: "v0.3.0", Config: h.cfg, Location: h.loc,
		Now:   func() time.Time { return h.now },
		Sleep: func(context.Context, time.Duration) error { return context.Canceled },
	}, []KidAssignments{k1List(rows...)})
	if staged != 1 || errs || len(h.fetched) != 1 {
		t.Errorf("staged=%d errs=%v fetched=%v, want one read, then a clean stop", staged, errs, h.fetched)
	}
}

func TestAssignmentDetail_NoRequestThatCannotFinishInThePollWindow(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 3)

	h.deadline = h.now.Add(30 * time.Second) // less than one request's allowance
	staged, errs, _ := h.poll(k1List(rows...))
	if staged != 0 || errs || len(h.fetched) != 0 {
		t.Errorf("staged=%d errs=%v fetched=%v with the window closing, want no request and no error", staged, errs, h.fetched)
	}

	h.deadline = h.now.Add(10 * time.Minute)
	if staged, _, _ := h.poll(k1List(rows...)); staged != 3 {
		t.Errorf("staged %d with ten minutes left, want 3", staged)
	}
}

func TestConnector_WindowDeadline(t *testing.T) {
	t.Setenv("SCHOOLOGY_TIMEZONE", "America/Los_Angeles")
	c := newTestConnector(t, &fakeClient{}) // window 08:00-09:00
	la := mustLA()

	c.now = func() time.Time { return time.Date(2026, 10, 12, 8, 20, 0, 0, la) }
	if got, want := c.windowDeadline(), time.Date(2026, 10, 12, 9, 0, 0, 0, la); !got.Equal(want) {
		t.Errorf("inside the window: deadline = %v, want %v", got, want)
	}
	c.now = func() time.Time { return time.Date(2026, 10, 12, 11, 0, 0, 0, la) }
	if got := c.windowDeadline(); !got.IsZero() {
		t.Errorf("outside any window: deadline = %v, want none", got)
	}
}

// ---------------------------------------------------------------------
// Wholesale breakage
// ---------------------------------------------------------------------

func TestAssignmentDetail_EveryDetailFailingFeedsTheDriftCounterOnce(t *testing.T) {
	h := newDetailHarness(t)
	var rows []*schoologylib.Assignment
	for id := int64(1); id <= 4; id++ {
		// Every page is filed under a course the list does not show: what
		// a change to how Schoology prints courses would look like.
		h.details[id] = regularDetail(id, "Task", "Renamed Thing: Section 1")
		rows = append(rows, listRow(id, "Task", "Example Course A"))
	}

	staged, errs, _ := h.poll(k1List(rows...))
	if staged != 0 {
		t.Fatalf("staged = %d, want 0", staged)
	}
	if !errs {
		t.Error("every detail was rejected and the poll reported no error; the drift counter would never see it")
	}
	rs := h.receipts(detailClassSurfaceDegraded)
	if len(rs) != 1 {
		t.Fatalf("surface_degraded receipts = %d, want exactly 1", len(rs))
	}
	if rs[0].meta.Tags["affected_count"] != "4" {
		t.Errorf("affected_count = %q, want 4", rs[0].meta.Tags["affected_count"])
	}
}

func TestAssignmentDetail_SomeDetailsFailingIsNotWholesale(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 4)
	h.details[1] = regularDetail(1, "Task 1", "Renamed Thing: Section 1")
	h.failWith[2] = schoologylib.ErrPermissionDenied

	staged, errs, _ := h.poll(k1List(rows...))
	if staged != 2 || errs {
		t.Errorf("staged=%d errs=%v, want 2,false", staged, errs)
	}
	if len(h.receipts(detailClassSurfaceDegraded)) != 0 {
		t.Error("surface_degraded receipt with half the details fine")
	}
}

func TestAssignmentDetail_TwoFailuresAreTooFewToCallWholesale(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 2)
	h.failWith[1] = schoologylib.ErrPermissionDenied
	h.failWith[2] = schoologylib.ErrNotFound

	if _, errs, _ := h.poll(k1List(rows...)); errs {
		t.Error("two per-assignment refusals were reported as a poll error")
	}
}

// ---------------------------------------------------------------------
// A transiently short list
// ---------------------------------------------------------------------

func TestAssignmentDetail_OneEmptyListingDoesNotForgetRows(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 3)
	h.poll(k1List(rows...))

	// Schoology answers with an empty list once, then the real one again.
	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	h.poll(k1List())
	h.now = h.now.Add(time.Hour)
	staged, _, _ := h.poll(k1List(rows...))
	if len(h.fetched) != 0 || staged != 0 {
		t.Errorf("after one empty listing: fetched=%v staged=%d, want no re-read", h.fetched, staged)
	}
	state, _ := loadDetailRows(h.cp, "k1")
	if len(state) != 3 || state[1].Missing != 0 {
		t.Errorf("row state = %v, want 3 rows with the absence count cleared", state)
	}
}
