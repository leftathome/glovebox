package schoology

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
	schoologylib "github.com/leftathome/schoology-go"
)

// All names, courses and text in this file are invented.

var (
	detailKid1 = Kid{Name: "k1", SchoologyUID: 1001}
	detailKid2 = Kid{Name: "k2", SchoologyUID: 1002}
)

// detailHarness drives ProcessAssignmentDetails against a fake client and
// a real staging writer and checkpoint.
type detailHarness struct {
	t          *testing.T
	writer     connector.StagingBackend
	stagingDir string
	cp         connector.Checkpoint
	matcher    *connector.RuleMatcher
	cfg        AssignmentDetailConfig
	now        time.Time
	loc        *time.Location

	details  map[int64]*schoologylib.AssignmentDetail
	parseErr map[int64]schoologylib.ParseErrors
	failWith map[int64]error
	comments map[int64]*schoologylib.AssignmentComments
	commErr  map[int64]error

	fetched         []int64
	commentsFetched []int64
	sleeps          []time.Duration

	// validateErr is what the session check answers (nil: session fine).
	validateErr error
	validated   int
	// onFetch runs at the start of every GetAssignment.
	onFetch func(id int64)
	// ctx and deadline override the poll context and window end.
	ctx      context.Context
	deadline time.Time
}

func newDetailHarness(t *testing.T) *detailHarness {
	t.Helper()
	writer, dir := newTestWriter(t)
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return &detailHarness{
		t:          t,
		writer:     writer,
		stagingDir: dir,
		cp:         newTestCheckpoint(t),
		matcher: connector.NewRuleMatcher([]connector.Rule{
			{Match: "schoology:k1:assignment_detail", Destination: "school", DataSubject: "e_k1", Audience: []string{"household"}},
			{Match: "schoology:k2:assignment_detail", Destination: "school", DataSubject: "e_k2", Audience: []string{"household"}},
			{Match: "schoology-parse-failure:*", Destination: "school", Audience: []string{"guardians"}},
		}),
		cfg:      AssignmentDetailConfig{Kids: []string{"k1", "k2"}},
		now:      time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC),
		loc:      loc,
		details:  map[int64]*schoologylib.AssignmentDetail{},
		parseErr: map[int64]schoologylib.ParseErrors{},
		failWith: map[int64]error{},
		comments: map[int64]*schoologylib.AssignmentComments{},
		commErr:  map[int64]error{},
	}
}

func (h *detailHarness) client() *fakeClient {
	return &fakeClient{
		ValidateFunc: func(context.Context) error {
			h.validated++
			return h.validateErr
		},
		AssignmentFunc: func(_ context.Context, id int64) (*schoologylib.AssignmentDetail, schoologylib.ParseErrors, error) {
			h.fetched = append(h.fetched, id)
			if h.onFetch != nil {
				h.onFetch(id)
			}
			if err := h.failWith[id]; err != nil {
				return nil, nil, err
			}
			d, ok := h.details[id]
			if !ok {
				h.t.Errorf("GetAssignment(%d): id was never listed for any kid", id)
				return nil, nil, schoologylib.ErrNotFound
			}
			cp := *d
			return &cp, h.parseErr[id], nil
		},
		CommentsFunc: func(_ context.Context, id int64) (*schoologylib.AssignmentComments, error) {
			h.commentsFetched = append(h.commentsFetched, id)
			if err := h.commErr[id]; err != nil {
				return nil, err
			}
			if c, ok := h.comments[id]; ok {
				cp := *c
				return &cp, nil
			}
			return &schoologylib.AssignmentComments{State: schoologylib.CommentsStateEmpty}, nil
		},
	}
}

// poll runs the surface once and returns (staged, errsLogged, dedup).
func (h *detailHarness) poll(lists ...KidAssignments) (int, bool, *ReceiptDedup) {
	h.t.Helper()
	dedup := NewReceiptDedup()
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	staged, errs := ProcessAssignmentDetails(ctx, DetailDeps{
		Deadline:   h.deadline,
		Client:     h.client(),
		Writer:     h.writer,
		Matcher:    h.matcher,
		Checkpoint: h.cp,
		Dedup:      dedup,
		LibVersion: "v0.3.0",
		Config:     h.cfg,
		Location:   h.loc,
		Now:        func() time.Time { return h.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			return nil
		},
	}, lists)
	return staged, errs, dedup
}

// resetCalls forgets the requests recorded so far.
func (h *detailHarness) resetCalls() {
	h.fetched, h.commentsFetched, h.sleeps = nil, nil, nil
}

// stagedItem is one committed staging item.
type stagedItem struct {
	meta    staging.ItemMetadata
	content string
}

// items returns every committed item, receipts included.
func (h *detailHarness) items() []stagedItem {
	h.t.Helper()
	return readStagedItems(h.t, h.stagingDir)
}

func readStagedItems(t *testing.T, dir string) []stagedItem {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	var out []stagedItem
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "metadata.json"))
		if err != nil {
			t.Fatalf("read metadata for %s: %v", e.Name(), err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("decode metadata for %s: %v", e.Name(), err)
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name(), "content.raw"))
		if err != nil {
			t.Fatalf("read content for %s: %v", e.Name(), err)
		}
		out = append(out, stagedItem{meta: meta, content: string(body)})
	}
	return out
}

// details returns the staged detail items (not receipts).
func (h *detailHarness) detailItems() []stagedItem {
	var out []stagedItem
	for _, it := range h.items() {
		if it.meta.Tags["kind"] == detailSurface {
			out = append(out, it)
		}
	}
	return out
}

// receipts returns the staged receipts with the given error class.
func (h *detailHarness) receipts(class string) []stagedItem {
	var out []stagedItem
	for _, it := range h.items() {
		if it.meta.Source == "schoology-parse-failure" && it.meta.Tags["error_class"] == class {
			out = append(out, it)
		}
	}
	return out
}

// listRow builds an overdue/upcoming row.
func listRow(id int64, title, course string) *schoologylib.Assignment {
	return &schoologylib.Assignment{
		ID:          id,
		Title:       title,
		CourseTitle: course,
		DueAt:       time.Date(2026, 10, 17, 6, 59, 0, 0, time.UTC),
		URL:         "/assignment/" + strconv.FormatInt(id, 10),
		Status:      schoologylib.AssignmentStatusUpcoming,
	}
}

// regularDetail builds the page of a regular assignment: description and an
// inline (empty) comment block.
func regularDetail(id int64, title, course string) *schoologylib.AssignmentDetail {
	due := time.Date(2026, 10, 16, 23, 59, 0, 0, mustLA())
	return &schoologylib.AssignmentDetail{
		ID:              id,
		Title:           title,
		Kind:            schoologylib.AssignmentKindAssignment,
		CourseID:        9200000001,
		CourseTitle:     course,
		Folder:          "Unit One",
		Category:        "Homework",
		GradingPeriod:   "Quarter 1: 2026-09-01 - 2026-11-06",
		DueAt:           due,
		DueRaw:          "Friday, October 16, 2026 at 11:59 pm",
		PostedAt:        time.Date(2026, 9, 16, 9, 15, 0, 0, mustLA()),
		PostedRaw:       "Wed Sep 16, 2026 at 9:15 am",
		DescriptionText: "Read pages 40-52.\nAnswer the four questions on the handout.",
		DescriptionHTML: "<p>Read pages 40-52.</p><p>Answer the <b>four</b> questions on the handout.</p>",
		Comments:        schoologylib.AssignmentComments{State: schoologylib.CommentsStateEmpty},
		FinalPath:       "/assignment/" + strconv.FormatInt(id, 10) + "/info",
	}
}

// assessmentDetail builds the page of an assessment: a status line, no
// description, no inline comment block.
func assessmentDetail(id int64, title, course string) *schoologylib.AssignmentDetail {
	return &schoologylib.AssignmentDetail{
		ID:               id,
		Title:            title,
		Kind:             schoologylib.AssignmentKindAssessment,
		CourseTitle:      course,
		Category:         "Tests",
		GradingPeriod:    "Quarter 1: 2026-09-01 - 2026-11-06",
		PostedAt:         time.Date(2026, 10, 1, 9, 5, 0, 0, mustLA()),
		PostedRaw:        "Thursday, October 1, 2026 at 9:05 am",
		AssessmentStatus: "This assessment is open",
		FinalPath:        "/assignment/" + strconv.FormatInt(id, 10) + "/assessment",
	}
}

func mustLA() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}

func k1List(rows ...*schoologylib.Assignment) KidAssignments {
	return KidAssignments{Kid: detailKid1, Rows: rows}
}

func k2List(rows ...*schoologylib.Assignment) KidAssignments {
	return KidAssignments{Kid: detailKid2, Rows: rows}
}

// ---------------------------------------------------------------------
// Item shape
// ---------------------------------------------------------------------

func TestAssignmentDetail_RegularAssignmentItemShape(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000001
	h.details[id] = regularDetail(id, "Chapter 3 questions", "Example Course A: Section 1")

	staged, errs, _ := h.poll(k1List(listRow(id, "Chapter 3 questions", "Example Course A")))
	if staged != 1 || errs {
		t.Fatalf("staged=%d errs=%v, want 1,false", staged, errs)
	}
	items := h.items()
	if len(items) != 1 {
		t.Fatalf("staged %d items, want exactly 1 (no receipt, no attachment)", len(items))
	}
	it := items[0]

	if it.meta.Source != "schoology" || it.meta.ContentType != "text/plain" {
		t.Errorf("source/content_type = %q/%q", it.meta.Source, it.meta.ContentType)
	}
	if it.meta.Subject != "Chapter 3 questions" {
		t.Errorf("subject = %q, want the assignment title", it.meta.Subject)
	}
	if it.meta.Sender != "Example Course A: Section 1" {
		t.Errorf("sender = %q, want the course title", it.meta.Sender)
	}
	if it.meta.DataSubject != "e_k1" || it.meta.DestinationAgent != "school" {
		t.Errorf("routing = subject %q dest %q, want the k1 rule's", it.meta.DataSubject, it.meta.DestinationAgent)
	}
	if len(it.meta.Audience) != 1 || it.meta.Audience[0] != "household" {
		t.Errorf("audience = %v, want the k1 rule's", it.meta.Audience)
	}

	wantTags := map[string]string{
		"kind":            "assignment_detail",
		"assignment_id":   "9100000001",
		"assignment_kind": "assignment",
		"course":          "Example Course A: Section 1",
		"category":        "Homework",
		"grading_period":  "Quarter 1: 2026-09-01 - 2026-11-06",
		"due_date":        "2026-10-17T06:59:00Z",
		"due_raw":         "Friday, October 16, 2026 at 11:59 pm",
		"posted":          "2026-09-16T16:15:00Z",
		"comments_state":  "empty",
		"has_description": "true",
	}
	for k, want := range wantTags {
		if got := it.meta.Tags[k]; got != want {
			t.Errorf("tag %s = %q, want %q", k, got, want)
		}
	}
	if _, ok := it.meta.Tags["assessment_status"]; ok {
		t.Error("assessment_status tag set on a regular assignment")
	}
	if _, ok := it.meta.Tags["parse_status"]; ok {
		t.Error("parse_status tag set on a cleanly parsed item")
	}

	wantBody := strings.Join([]string{
		"Assignment: Chapter 3 questions",
		"Kind: assignment",
		"Course: Example Course A: Section 1",
		"Folder: Unit One",
		"Category: Homework",
		"Period: Quarter 1: 2026-09-01 - 2026-11-06",
		"Due: Friday, October 16, 2026 at 11:59 pm (2026-10-16T23:59:00-07:00)",
		"Posted: Wed Sep 16, 2026 at 9:15 am (2026-09-16T09:15:00-07:00)",
		"Link: /assignment/9100000001",
		"",
		"Description:",
		descriptionBegin,
		"| Read pages 40-52.",
		"| Answer the four questions on the handout.",
		descriptionEnd,
		"",
	}, "\n")
	if it.content != wantBody {
		t.Errorf("content =\n%s\nwant\n%s", it.content, wantBody)
	}
	if strings.Contains(it.content, "<p>") || strings.Contains(it.content, "<b>") {
		t.Error("the description HTML was staged")
	}
	if strings.Contains(it.content, "Teacher comments") {
		t.Error("a comments section was staged for an empty comment block")
	}
	if len(h.commentsFetched) != 0 {
		t.Errorf("comments page requested for a regular assignment: %v", h.commentsFetched)
	}
}

func TestAssignmentDetail_AssessmentUsesCommentsPage(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000002
	h.details[id] = assessmentDetail(id, "Unit 2 test", "Example Course B: Section 2")
	h.comments[id] = &schoologylib.AssignmentComments{State: schoologylib.CommentsStateUnavailable}

	staged, errs, _ := h.poll(k1List(listRow(id, "Unit 2 test", "Example Course B")))
	if staged != 1 || errs {
		t.Fatalf("staged=%d errs=%v, want 1,false", staged, errs)
	}
	if len(h.commentsFetched) != 1 {
		t.Fatalf("comments page requested %d times, want 1", len(h.commentsFetched))
	}
	items := h.items()
	if len(items) != 1 {
		t.Fatalf("staged %d items, want 1: comments unavailable is normal and must not produce a receipt", len(items))
	}
	it := items[0]
	for k, want := range map[string]string{
		"assignment_kind":   "assessment",
		"assessment_status": "This assessment is open",
		"comments_state":    "unavailable",
		"has_description":   "false",
		"posted":            "2026-10-01T16:05:00Z",
	} {
		if got := it.meta.Tags[k]; got != want {
			t.Errorf("tag %s = %q, want %q", k, got, want)
		}
	}
	for _, absent := range []string{"due_date", "due_raw"} {
		if _, ok := it.meta.Tags[absent]; ok {
			t.Errorf("tag %s set for an assessment with no due date", absent)
		}
	}
	if !strings.Contains(it.content, "Due: no due date listed\n") {
		t.Errorf("content does not say there is no due date:\n%s", it.content)
	}
	if !strings.Contains(it.content, "Status: This assessment is open\n") {
		t.Errorf("content lacks the assessment status:\n%s", it.content)
	}
	if strings.Contains(it.content, "Description:") {
		t.Errorf("content has a description section for an assessment:\n%s", it.content)
	}
}

func TestAssignmentDetail_PopulatedCommentsAreStagedWithLayoutCaveat(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000003
	d := regularDetail(id, "Lab report", "Example Course A: Section 1")
	d.Comments = schoologylib.AssignmentComments{
		State:          schoologylib.CommentsStatePopulated,
		LayoutUnmapped: true,
		Comments: []*schoologylib.AssignmentComment{
			{Text: "Teacher Example Mon Oct 12 Please redo the conclusion."},
		},
	}
	h.details[id] = d

	h.poll(k1List(listRow(id, "Lab report", "Example Course A")))
	items := h.detailItems()
	if len(items) != 1 {
		t.Fatalf("staged %d detail items, want 1", len(items))
	}
	it := items[0]
	if it.meta.Tags["comments_state"] != "populated" {
		t.Errorf("comments_state = %q, want populated", it.meta.Tags["comments_state"])
	}
	if it.meta.Tags["comment_count"] != "1" {
		t.Errorf("comment_count = %q, want 1", it.meta.Tags["comment_count"])
	}
	// The layout is unmapped: the count is staged, the text is not.
	want := "\nTeacher comments: 1 present (text not captured yet)\n"
	if !strings.HasSuffix(it.content, want) {
		t.Errorf("content does not end with the fixed comments line:\n%s", it.content)
	}
	if strings.Contains(it.content, "redo the conclusion") {
		t.Errorf("unmapped comment text was staged:\n%s", it.content)
	}
}

func TestAssignmentDetail_PostedFallsBackToRawWithoutParsedTime(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000004
	d := assessmentDetail(id, "Quiz", "Example Course B: Section 2")
	d.PostedAt = time.Time{}
	h.details[id] = d

	h.poll(k1List(listRow(id, "Quiz", "Example Course B")))
	items := h.detailItems()
	if len(items) != 1 {
		t.Fatalf("staged %d detail items, want 1", len(items))
	}
	if got := items[0].meta.Tags["posted"]; got != "Thursday, October 1, 2026 at 9:05 am" {
		t.Errorf("posted = %q, want the raw text", got)
	}
}

// ---------------------------------------------------------------------
// Enabling
// ---------------------------------------------------------------------

func TestAssignmentDetail_DisabledMakesNoRequest(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.Kids = nil
	h.details[1] = regularDetail(1, "A", "C")

	staged, errs, _ := h.poll(k1List(listRow(1, "A", "C")))
	if staged != 0 || errs || len(h.fetched) != 0 {
		t.Errorf("disabled surface: staged=%d errs=%v fetched=%v", staged, errs, h.fetched)
	}
}

func TestAssignmentDetail_EnabledPerKid(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.Kids = []string{"k2"}
	h.details[1] = regularDetail(1, "A", "C")
	h.details[2] = regularDetail(2, "B", "C")

	staged, _, _ := h.poll(k1List(listRow(1, "A", "C")), k2List(listRow(2, "B", "C")))
	if staged != 1 {
		t.Fatalf("staged = %d, want 1 (k2 only)", staged)
	}
	if len(h.fetched) != 1 || h.fetched[0] != 2 {
		t.Errorf("fetched = %v, want only k2's assignment", h.fetched)
	}
	if got := h.detailItems()[0].meta.DataSubject; got != "e_k2" {
		t.Errorf("data_subject = %q, want e_k2", got)
	}
}

// ---------------------------------------------------------------------
// Attribution
// ---------------------------------------------------------------------

func TestAssignmentDetail_SameIDInBothKidsListsStagesOnePerKid(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000010
	h.details[id] = regularDetail(id, "Shared worksheet", "Example Course A: Section 1")

	staged, _, _ := h.poll(
		k1List(listRow(id, "Shared worksheet", "Example Course A")),
		k2List(listRow(id, "Shared worksheet", "Example Course A")),
	)
	if staged != 2 {
		t.Fatalf("staged = %d, want 2 (one per kid)", staged)
	}
	if len(h.fetched) != 1 {
		t.Errorf("page requested %d times, want 1 (shared within the poll)", len(h.fetched))
	}
	subjects := map[string]int{}
	for _, it := range h.detailItems() {
		subjects[it.meta.DataSubject]++
	}
	if subjects["e_k1"] != 1 || subjects["e_k2"] != 1 || len(subjects) != 2 {
		t.Errorf("data subjects = %v, want one item under each kid's own rule", subjects)
	}

	// Each kid has its own seen key.
	for _, kid := range []string{"k1", "k2"} {
		seen, err := loadDetailSeen(h.cp, kid)
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] == "" {
			t.Errorf("%s has no seen key for the shared assignment", kid)
		}
	}
	raw, _ := h.cp.Load(seenKey(detailSurface, "k1"))
	if !strings.Contains(raw, `"k1|detail|9100000010|`) {
		t.Errorf("seen key is not of the form <kid>|detail|<id>|<hash>: %s", raw)
	}
}

func TestAssignmentDetail_OnlyListedIDsAreFetched(t *testing.T) {
	h := newDetailHarness(t)
	listed := map[int64]bool{}
	var rows []*schoologylib.Assignment
	for id := int64(101); id <= 105; id++ {
		h.details[id] = regularDetail(id, "Task "+strconv.FormatInt(id, 10), "Example Course A")
		rows = append(rows, listRow(id, "Task "+strconv.FormatInt(id, 10), "Example Course A"))
		listed[id] = true
	}
	// A page that exists but that nobody's list names.
	h.details[999] = regularDetail(999, "Unlisted", "Example Course A")
	// Rows the surface must ignore.
	rows = append(rows, nil, &schoologylib.Assignment{ID: 0, Title: "no id"})

	h.poll(k1List(rows...))
	for _, id := range h.fetched {
		if !listed[id] {
			t.Errorf("fetched id %d, which no kid's list contained", id)
		}
	}
	if len(h.fetched) != 5 {
		t.Errorf("fetched %d pages, want 5", len(h.fetched))
	}
}

func TestAssignmentDetail_CourseMismatchIsNotStaged(t *testing.T) {
	h := newDetailHarness(t)
	const bad, good = 9100000020, 9100000021
	h.details[bad] = regularDetail(bad, "Essay", "Example Course Z: Section 9")
	h.details[good] = regularDetail(good, "Reading", "Example Course A: Section 1")

	staged, errs, _ := h.poll(k1List(
		listRow(bad, "Essay", "Example Course A"),
		listRow(good, "Reading", "Example Course A"),
	))
	if staged != 1 {
		t.Fatalf("staged = %d, want 1 (the matching assignment only)", staged)
	}
	if errs {
		t.Error("a course mismatch must not count as a poll error")
	}
	for _, it := range h.detailItems() {
		if it.meta.Tags["assignment_id"] == strconv.Itoa(bad) {
			t.Error("the mismatched assignment was staged")
		}
	}
	rs := h.receipts(detailClassCourseMismatch)
	if len(rs) != 1 {
		t.Fatalf("course_mismatch receipts = %d, want 1", len(rs))
	}
	r := rs[0]
	if r.meta.Tags["parser"] != "assignment_detail" || r.meta.Tags["target_kid"] != "k1" {
		t.Errorf("receipt tags = %v", r.meta.Tags)
	}
	if r.meta.Tags["affected_item_ids"] != strconv.Itoa(bad) {
		t.Errorf("affected_item_ids = %q", r.meta.Tags["affected_item_ids"])
	}
	if !strings.Contains(r.content, "Example Course Z: Section 9") || !strings.Contains(r.content, "not staged") {
		t.Errorf("receipt does not give a clear reason:\n%s", r.content)
	}

	// The refusal is remembered: no request, no second receipt next poll.
	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	h.poll(k1List(
		listRow(bad, "Essay", "Example Course A"),
		listRow(good, "Reading", "Example Course A"),
	))
	if len(h.fetched) != 0 {
		t.Errorf("refetched %v an hour after a mismatch", h.fetched)
	}
	if got := len(h.receipts(detailClassCourseMismatch)); got != 1 {
		t.Errorf("course_mismatch receipts after a quiet poll = %d, want still 1", got)
	}
}

func TestSameCourse(t *testing.T) {
	cases := []struct {
		list, detail string
		want         bool
	}{
		{"Example Course A", "Example Course A", true},
		{"Example Course A", "Example Course A: Section 1", true},
		{"Example Course A", "Example Course A : Section 01", true},
		{"example  course a", "Example Course A: Section 1", true},
		{"Example Course A: Section 1", "Example Course A", true},
		{"", "Example Course A", true},
		{"Example Course A", "", true},
		{"Example Course A", "Example Course AB: Section 1", false},
		{"Example Course A", "Example Course B: Section 1", false},
		{"Example Course A", "Honors Example Course A", false},
	}
	for _, c := range cases {
		if got := sameCourse(c.list, c.detail); got != c.want {
			t.Errorf("sameCourse(%q, %q) = %v, want %v", c.list, c.detail, got, c.want)
		}
	}
}

func TestAssignmentDetail_PageForAnotherIDIsNotStaged(t *testing.T) {
	h := newDetailHarness(t)
	d := regularDetail(555, "Other", "Example Course A")
	h.details[9100000030] = d // the page served for ...30 says it is 555

	staged, _, _ := h.poll(k1List(listRow(9100000030, "Mine", "Example Course A")))
	if staged != 0 {
		t.Fatalf("staged = %d, want 0", staged)
	}
	if len(h.receipts(detailClassIDMismatch)) != 1 {
		t.Error("no id_mismatch receipt")
	}
}

// ---------------------------------------------------------------------
// Change detection
// ---------------------------------------------------------------------

func TestAssignmentDetail_UnchangedIsNotRestaged(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000040
	h.details[id] = regularDetail(id, "Vocabulary", "Example Course A: Section 1")
	row := listRow(id, "Vocabulary", "Example Course A")

	if staged, _, _ := h.poll(k1List(row)); staged != 1 {
		t.Fatalf("first poll staged %d, want 1", staged)
	}

	// Same row, minutes later: not even a request.
	h.resetCalls()
	h.now = h.now.Add(10 * time.Minute)
	if staged, _, _ := h.poll(k1List(row)); staged != 0 || len(h.fetched) != 0 {
		t.Errorf("second poll staged=%d fetched=%v, want 0 and none", staged, h.fetched)
	}

	// A day later the page is read again and, being the same, not staged.
	h.resetCalls()
	h.now = h.now.Add(24 * time.Hour)
	staged, _, _ := h.poll(k1List(row))
	if len(h.fetched) != 1 {
		t.Fatalf("refresh poll fetched %v, want one read", h.fetched)
	}
	if staged != 0 {
		t.Errorf("refresh poll staged %d, want 0 for an unchanged page", staged)
	}
	if got := len(h.detailItems()); got != 1 {
		t.Errorf("detail items = %d, want 1", got)
	}
}

func TestAssignmentDetail_ChangedDescriptionIsRestaged(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000041
	h.details[id] = regularDetail(id, "Vocabulary", "Example Course A: Section 1")
	row := listRow(id, "Vocabulary", "Example Course A")
	h.poll(k1List(row))

	edited := regularDetail(id, "Vocabulary", "Example Course A: Section 1")
	edited.DescriptionText = "Read pages 40-60 instead."
	h.details[id] = edited

	h.now = h.now.Add(24 * time.Hour)
	if staged, _, _ := h.poll(k1List(row)); staged != 1 {
		t.Fatalf("staged %d after a description edit, want 1", staged)
	}
	items := h.detailItems()
	if len(items) != 2 {
		t.Fatalf("detail items = %d, want 2 (original and edited)", len(items))
	}
	seen, _ := loadDetailSeen(h.cp, "k1")
	if len(seen) != 1 {
		t.Errorf("seen-set holds %d keys for one assignment, want 1 (latest hash only)", len(seen))
	}
}

func TestAssignmentDetail_ChangedDueDateIsRestagedOnTheNextPoll(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000042
	h.details[id] = regularDetail(id, "Vocabulary", "Example Course A: Section 1")
	h.poll(k1List(listRow(id, "Vocabulary", "Example Course A")))

	// The teacher moves the due date: the list row and the page both change.
	moved := listRow(id, "Vocabulary", "Example Course A")
	moved.DueAt = moved.DueAt.Add(72 * time.Hour)
	d := regularDetail(id, "Vocabulary", "Example Course A: Section 1")
	d.DueAt = d.DueAt.Add(72 * time.Hour)
	d.DueRaw = "Monday, October 19, 2026 at 11:59 pm"
	h.details[id] = d

	// Minutes later, well inside the refresh interval.
	h.resetCalls()
	h.now = h.now.Add(10 * time.Minute)
	staged, _, _ := h.poll(k1List(moved))
	if len(h.fetched) != 1 {
		t.Fatalf("a changed list row did not trigger a read: fetched %v", h.fetched)
	}
	if staged != 1 {
		t.Fatalf("staged %d after a due date change, want 1", staged)
	}
	var dues []string
	for _, it := range h.detailItems() {
		dues = append(dues, it.meta.Tags["due_date"])
	}
	sort.Strings(dues)
	if len(dues) != 2 || dues[0] == dues[1] {
		t.Errorf("due_date tags = %v, want two different values", dues)
	}
}

func TestAssignmentDetail_NewTeacherCommentIsRestaged(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000043
	h.details[id] = assessmentDetail(id, "Unit 2 test", "Example Course B: Section 2")
	row := listRow(id, "Unit 2 test", "Example Course B")
	h.poll(k1List(row))

	h.comments[id] = &schoologylib.AssignmentComments{
		State:          schoologylib.CommentsStatePopulated,
		LayoutUnmapped: true,
		Comments:       []*schoologylib.AssignmentComment{{Text: "Retake available Thursday."}},
	}

	h.now = h.now.Add(24 * time.Hour)
	if staged, _, _ := h.poll(k1List(row)); staged != 1 {
		t.Fatalf("staged %d after a comment appeared, want 1", staged)
	}
	var withComment int
	for _, it := range h.detailItems() {
		if strings.Contains(it.content, "Retake available Thursday.") {
			t.Error("unmapped comment text was staged")
		}
		if strings.Contains(it.content, "Teacher comments: 1 present") {
			withComment++
			if it.meta.Tags["comments_state"] != "populated" {
				t.Errorf("comments_state = %q", it.meta.Tags["comments_state"])
			}
		}
	}
	if withComment != 1 {
		t.Errorf("items carrying the comment = %d, want 1", withComment)
	}
}

func TestAssignmentDetail_RevertedContentIsStagedAgain(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000044
	original := regularDetail(id, "Vocabulary", "Example Course A: Section 1")
	edited := regularDetail(id, "Vocabulary", "Example Course A: Section 1")
	edited.DescriptionText = "Cancelled."
	row := listRow(id, "Vocabulary", "Example Course A")

	total := 0
	for _, d := range []*schoologylib.AssignmentDetail{original, edited, original} {
		h.details[id] = d
		staged, _, _ := h.poll(k1List(row))
		total += staged
		h.now = h.now.Add(24 * time.Hour)
	}
	if total != 3 {
		t.Errorf("A -> B -> A staged %d items, want 3: downstream must learn it is A again", total)
	}
}

func TestDetailContentHash_IgnoresVolatileFields(t *testing.T) {
	row := listRow(1, "T", "Example Course A")
	d := regularDetail(1, "T", "Example Course A: Section 1")
	rule := connector.MatchResult{Destination: "school"}
	loc := mustLA()

	a := buildDetailItemOptions(row, d, d.Comments, rule)
	b := buildDetailItemOptions(row, d, d.Comments, connector.MatchResult{Destination: "elsewhere", DataSubject: "x"})
	b.Timestamp = a.Timestamp.Add(time.Hour)
	body := buildDetailBody(row, d, d.Comments, loc)

	if detailContentHash(a, body) != detailContentHash(b, body) {
		t.Error("the hash moved with the staging timestamp or the routing")
	}

	// FinalPath and the description HTML are not staged, so not hashed.
	d2 := *d
	d2.FinalPath = "/assignment/1/info/"
	d2.DescriptionHTML = "<div>Read pages 40-52.</div>"
	if detailContentHash(buildDetailItemOptions(row, &d2, d2.Comments, rule), buildDetailBody(row, &d2, d2.Comments, loc)) != detailContentHash(a, body) {
		t.Error("the hash moved with a field that is not staged")
	}

	d3 := *d
	d3.Category = "Classwork"
	if detailContentHash(buildDetailItemOptions(row, &d3, d3.Comments, rule), buildDetailBody(row, &d3, d3.Comments, loc)) == detailContentHash(a, body) {
		t.Error("the hash did not move with a staged field")
	}
}

// ---------------------------------------------------------------------
// Request budget
// ---------------------------------------------------------------------

func TestAssignmentDetail_RefreshAtMostOncePerInterval(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.RefreshHours = 24
	const id = 9100000050
	h.details[id] = regularDetail(id, "Vocabulary", "Example Course A: Section 1")
	row := listRow(id, "Vocabulary", "Example Course A")
	start := h.now
	h.poll(k1List(row))

	reads := func(at time.Duration) int {
		h.resetCalls()
		h.now = start.Add(at)
		h.poll(k1List(row))
		return len(h.fetched)
	}
	if n := reads(23*time.Hour + 59*time.Minute); n != 0 {
		t.Errorf("read %d times just under 24h, want 0", n)
	}
	if n := reads(24 * time.Hour); n != 1 {
		t.Errorf("read %d times at 24h, want 1", n)
	}
	if n := reads(30 * time.Hour); n != 0 {
		t.Errorf("read %d times 6h after a refresh, want 0", n)
	}
	if n := reads(48 * time.Hour); n != 1 {
		t.Errorf("read %d times 24h after the last refresh, want 1", n)
	}
}

func TestAssignmentDetail_ConfiguredRefreshInterval(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.RefreshHours = 6
	const id = 9100000051
	h.details[id] = regularDetail(id, "Vocabulary", "Example Course A")
	row := listRow(id, "Vocabulary", "Example Course A")
	h.poll(k1List(row))

	h.resetCalls()
	h.now = h.now.Add(6 * time.Hour)
	h.poll(k1List(row))
	if len(h.fetched) != 1 {
		t.Errorf("fetched %v at the configured 6h interval, want one read", h.fetched)
	}
}

func TestAssignmentDetail_PerPollCap(t *testing.T) {
	h := newDetailHarness(t)
	var rows []*schoologylib.Assignment
	for id := int64(1); id <= 25; id++ {
		title := "Task " + strconv.FormatInt(id, 10)
		h.details[id] = regularDetail(id, title, "Example Course A")
		rows = append(rows, listRow(id, title, "Example Course A"))
	}

	staged, _, _ := h.poll(k1List(rows...))
	if len(h.fetched) != defaultDetailMaxPerPoll || staged != defaultDetailMaxPerPoll {
		t.Fatalf("first poll fetched=%d staged=%d, want the default cap %d", len(h.fetched), staged, defaultDetailMaxPerPoll)
	}

	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	staged, _, _ = h.poll(k1List(rows...))
	if len(h.fetched) != 5 || staged != 5 {
		t.Fatalf("second poll fetched=%d staged=%d, want the 5 deferred", len(h.fetched), staged)
	}
	if got := len(h.detailItems()); got != 25 {
		t.Errorf("detail items = %d, want 25 with no duplicates", got)
	}
}

func TestAssignmentDetail_CapIsSharedAcrossKidsAndConfigurable(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.MaxPerPoll = 3
	var r1, r2 []*schoologylib.Assignment
	for id := int64(1); id <= 2; id++ {
		h.details[id] = regularDetail(id, "A", "Example Course A")
		r1 = append(r1, listRow(id, "A", "Example Course A"))
	}
	for id := int64(11); id <= 12; id++ {
		h.details[id] = regularDetail(id, "B", "Example Course A")
		r2 = append(r2, listRow(id, "B", "Example Course A"))
	}
	h.poll(k1List(r1...), k2List(r2...))
	if len(h.fetched) != 3 {
		t.Errorf("fetched %d pages across two kids with max_per_poll=3", len(h.fetched))
	}
}

func TestAssignmentDetail_NewAssignmentsGoAheadOfRefreshes(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.MaxPerPoll = 2
	old := []*schoologylib.Assignment{}
	for id := int64(1); id <= 2; id++ {
		h.details[id] = regularDetail(id, "Old", "Example Course A")
		old = append(old, listRow(id, "Old", "Example Course A"))
	}
	h.poll(k1List(old...))

	// A day later both old rows are due a refresh and one new row appears.
	h.resetCalls()
	h.now = h.now.Add(25 * time.Hour)
	h.details[3] = regularDetail(3, "New", "Example Course A")
	h.poll(k1List(append(old, listRow(3, "New", "Example Course A"))...))
	if len(h.fetched) != 2 || h.fetched[0] != 3 {
		t.Errorf("fetched %v, want the new assignment (3) first within a cap of 2", h.fetched)
	}
}

func TestAssignmentDetail_RequestsAreSpaced(t *testing.T) {
	h := newDetailHarness(t)
	h.details[1] = regularDetail(1, "A", "Example Course A")
	h.details[2] = assessmentDetail(2, "B", "Example Course A")
	h.details[3] = regularDetail(3, "C", "Example Course A")

	h.poll(k1List(
		listRow(1, "A", "Example Course A"),
		listRow(2, "B", "Example Course A"),
		listRow(3, "C", "Example Course A"),
	))
	// Four requests (three pages and one comments page): three gaps.
	if len(h.sleeps) != 3 {
		t.Fatalf("waited %d times between 4 requests, want 3", len(h.sleeps))
	}
	for _, d := range h.sleeps {
		if d != defaultDetailRequestSpacingMS*time.Millisecond {
			t.Errorf("gap = %v, want the default %dms", d, defaultDetailRequestSpacingMS)
		}
	}
}

func TestAssignmentDetail_DelistedRowsAreForgottenAndReturnWithoutRestaging(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000060
	h.details[id] = regularDetail(id, "Vocabulary", "Example Course A")
	row := listRow(id, "Vocabulary", "Example Course A")
	h.poll(k1List(row))

	// It drops off the list (handed in) ...
	h.now = h.now.Add(time.Hour)
	for i := 0; i < detailRowGracePolls; i++ {
		h.poll(k1List())
	}
	rows, _ := loadDetailRows(h.cp, "k1")
	if len(rows) != 0 {
		t.Errorf("row state holds %d rows for an empty list, want 0", len(rows))
	}

	// ... and comes back: one read, nothing staged, because the seen-set
	// still knows its content.
	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	staged, _, _ := h.poll(k1List(row))
	if len(h.fetched) != 1 || staged != 0 {
		t.Errorf("relisted: fetched=%v staged=%d, want one read and nothing staged", h.fetched, staged)
	}
}

// ---------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------

func TestAssignmentDetail_AuthStopsTheSurface(t *testing.T) {
	h := newDetailHarness(t)
	var rows []*schoologylib.Assignment
	for id := int64(1); id <= 4; id++ {
		h.details[id] = regularDetail(id, "A", "Example Course A")
		rows = append(rows, listRow(id, "A", "Example Course A"))
	}
	h.failWith[2] = &schoologylib.Error{Code: schoologylib.ErrCodeAuth, Op: "GetAssignment", Message: "session expired"}
	h.validateErr = schoologylib.ErrSessionExpired

	staged, errs, dedup := h.poll(k1List(rows...))
	if len(h.fetched) != 2 {
		t.Errorf("made %d requests, want to stop at the refused one (2)", len(h.fetched))
	}
	if staged != 1 || !errs {
		t.Errorf("staged=%d errs=%v, want 1,true", staged, errs)
	}
	if dedup.AffectedCount(detailParser, "auth") != 1 {
		t.Error("the auth failure was not recorded for the rejected-session path")
	}
	if len(h.receipts("auth")) != 1 {
		t.Error("no auth receipt")
	}
	rowsState, _ := loadDetailRows(h.cp, "k1")
	if _, ok := rowsState[2]; ok {
		t.Error("the refused assignment was remembered as read; it must be retried with a new session")
	}
}

func TestAssignmentDetail_AuthOnCommentsPageStopsTheSurface(t *testing.T) {
	h := newDetailHarness(t)
	h.details[1] = assessmentDetail(1, "A", "Example Course A")
	h.details[2] = regularDetail(2, "B", "Example Course A")
	h.commErr[1] = schoologylib.ErrSessionExpired
	h.validateErr = schoologylib.ErrSessionExpired

	staged, _, dedup := h.poll(k1List(listRow(1, "A", "Example Course A"), listRow(2, "B", "Example Course A")))
	if staged != 0 || len(h.fetched) != 1 {
		t.Errorf("staged=%d fetched=%v, want nothing staged and no request after the refusal", staged, h.fetched)
	}
	if dedup.AffectedCount(detailParser, "auth") != 1 {
		t.Error("the auth failure was not recorded")
	}
}

func TestAssignmentDetail_PermissionOnOneAssignmentIsAReceiptNotAFailure(t *testing.T) {
	h := newDetailHarness(t)
	var rows []*schoologylib.Assignment
	for id := int64(1); id <= 3; id++ {
		h.details[id] = regularDetail(id, "A", "Example Course A")
		rows = append(rows, listRow(id, "A", "Example Course A"))
	}
	h.failWith[2] = schoologylib.ErrPermissionDenied

	staged, errs, dedup := h.poll(k1List(rows...))
	if staged != 2 {
		t.Errorf("staged = %d, want the other 2", staged)
	}
	if errs {
		t.Error("a permission error on one assignment was reported as a poll error")
	}
	if dedup.AffectedCount(detailParser, "auth") != 0 {
		t.Error("a permission error was treated as an auth failure")
	}
	rs := h.receipts("permission")
	if len(rs) != 1 || rs[0].meta.Tags["affected_item_ids"] != "2" {
		t.Fatalf("permission receipts = %d (%v), want 1 naming assignment 2", len(rs), rs)
	}

	// Not retried until the refresh interval.
	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	h.poll(k1List(rows...))
	if len(h.fetched) != 0 {
		t.Errorf("refetched %v an hour after a permission error", h.fetched)
	}
	h.resetCalls()
	h.now = h.now.Add(24 * time.Hour)
	h.poll(k1List(rows...))
	if len(h.fetched) != 3 {
		t.Errorf("fetched %v at the refresh, want all three", h.fetched)
	}
}

func TestAssignmentDetail_NotFoundOnOneAssignment(t *testing.T) {
	h := newDetailHarness(t)
	h.failWith[1] = schoologylib.ErrNotFound
	h.details[2] = regularDetail(2, "B", "Example Course A")

	staged, errs, _ := h.poll(k1List(listRow(1, "A", "Example Course A"), listRow(2, "B", "Example Course A")))
	if staged != 1 || errs {
		t.Errorf("staged=%d errs=%v, want 1,false", staged, errs)
	}
	if len(h.receipts("not_found")) != 1 {
		t.Error("no not_found receipt")
	}
}

func TestAssignmentDetail_ServerErrorStopsAndIsRetriedNextPoll(t *testing.T) {
	h := newDetailHarness(t)
	var rows []*schoologylib.Assignment
	for id := int64(1); id <= 3; id++ {
		h.details[id] = regularDetail(id, "A", "Example Course A")
		rows = append(rows, listRow(id, "A", "Example Course A"))
	}
	h.failWith[1] = &schoologylib.Error{Code: schoologylib.ErrCodeServer, Op: "GetAssignment", Message: "server error", StatusCode: 502}

	staged, errs, _ := h.poll(k1List(rows...))
	if staged != 0 || !errs || len(h.fetched) != 1 {
		t.Errorf("staged=%d errs=%v fetched=%v, want 0,true and one request", staged, errs, h.fetched)
	}
	if len(h.receipts("server")) != 1 {
		t.Error("no server receipt")
	}

	delete(h.failWith, 1)
	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	if staged, _, _ := h.poll(k1List(rows...)); staged != 3 {
		t.Errorf("next poll staged %d, want all 3", staged)
	}
}

// An id that resolves to a page the library does not read (it reports that
// as a "server" error with no HTTP status) will do so on every poll. It must
// not stop the surface, or it would sit at the head of the queue and starve
// every other assignment forever.
func TestAssignmentDetail_UnexpectedPageDoesNotBlockOtherAssignments(t *testing.T) {
	h := newDetailHarness(t)
	var rows []*schoologylib.Assignment
	for id := int64(1); id <= 3; id++ {
		h.details[id] = regularDetail(id, "A", "Example Course A")
		rows = append(rows, listRow(id, "A", "Example Course A"))
	}
	h.failWith[1] = &schoologylib.Error{
		Code: schoologylib.ErrCodeServer, Op: "GetAssignment",
		Message: "redirected to a page that is not the assignment",
	}

	staged, errs, _ := h.poll(k1List(rows...))
	if staged != 2 || errs {
		t.Errorf("staged=%d errs=%v, want the other 2 and no poll error", staged, errs)
	}
	if len(h.receipts(detailClassUnexpectedPage)) != 1 {
		t.Error("no unexpected_page receipt")
	}

	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	h.poll(k1List(rows...))
	if len(h.fetched) != 0 {
		t.Errorf("refetched %v an hour later; it must wait for the refresh", h.fetched)
	}
}

func TestAssignmentDetail_ParseErrorsStageDegradedWithReceipt(t *testing.T) {
	h := newDetailHarness(t)
	d := regularDetail(1, "A", "Example Course A")
	d.DueAt = time.Time{}
	d.DueRaw = "sometime next week"
	h.details[1] = d
	h.parseErr[1] = schoologylib.ParseErrors{
		schoologylib.NewParseError("parseAssignmentDetail: .due-date", "unrecognized due date format"),
	}

	staged, errs, _ := h.poll(k1List(listRow(1, "A", "Example Course A")))
	if staged != 1 || !errs {
		t.Fatalf("staged=%d errs=%v, want 1,true", staged, errs)
	}
	it := h.detailItems()[0]
	if it.meta.Tags["parse_status"] != "degraded" {
		t.Errorf("parse_status = %q, want degraded", it.meta.Tags["parse_status"])
	}
	if it.meta.Tags["due_raw"] != "sometime next week" {
		t.Errorf("due_raw = %q", it.meta.Tags["due_raw"])
	}
	if _, ok := it.meta.Tags["due_date"]; ok {
		t.Error("due_date set for an unparsed date")
	}
	if len(h.receipts(detailClassRowParse)) != 1 {
		t.Error("no row_parse receipt")
	}
}

func TestAssignmentDetail_AbsentOptionalFieldsProduceNoReceipt(t *testing.T) {
	h := newDetailHarness(t)
	// Nothing but a title and a kind: no course, folder, category, period,
	// dates, status, description or comment block.
	h.details[1] = &schoologylib.AssignmentDetail{ID: 1, Title: "Bare", Kind: schoologylib.AssignmentKindAssignment}

	staged, errs, _ := h.poll(k1List(listRow(1, "Bare", "")))
	if staged != 1 || errs {
		t.Fatalf("staged=%d errs=%v, want 1,false", staged, errs)
	}
	items := h.items()
	if len(items) != 1 {
		t.Fatalf("staged %d items, want 1 and no receipt", len(items))
	}
	it := items[0]
	if it.meta.Sender != "schoology-course" {
		t.Errorf("sender=%q, want the placeholder when neither page nor list names a course", it.meta.Sender)
	}
	if _, ok := it.meta.Tags["parse_status"]; ok {
		t.Error("an item with only absent optional fields was marked degraded")
	}
	if it.meta.Tags["comments_state"] != "unknown" || it.meta.Tags["has_description"] != "false" {
		t.Errorf("tags = %v", it.meta.Tags)
	}
	if len(h.commentsFetched) != 0 {
		t.Error("comments page requested for a regular assignment")
	}
}

func TestAssignmentDetail_UnreadableCommentsPageStillStagesTheDetail(t *testing.T) {
	h := newDetailHarness(t)
	h.details[1] = assessmentDetail(1, "A", "Example Course A")
	h.commErr[1] = schoologylib.NewParseError("GetAssignmentComments", "missing .comment-container; not a comments page")

	staged, errs, _ := h.poll(k1List(listRow(1, "A", "Example Course A")))
	if staged != 1 || !errs {
		t.Fatalf("staged=%d errs=%v, want 1,true", staged, errs)
	}
	if got := h.detailItems()[0].meta.Tags["comments_state"]; got != "unknown" {
		t.Errorf("comments_state = %q, want unknown", got)
	}
	if len(h.receipts(detailClassCommentsParse)) != 1 {
		t.Error("no comments_parse receipt")
	}
}

func TestAssignmentDetail_CorruptStateStartsFresh(t *testing.T) {
	h := newDetailHarness(t)
	h.details[1] = regularDetail(1, "A", "Example Course A")
	if err := h.cp.Save(seenKey(detailSurface, "k1"), "not json"); err != nil {
		t.Fatal(err)
	}
	if err := h.cp.Save(detailRowsKey("k1"), "{"); err != nil {
		t.Fatal(err)
	}
	staged, _, _ := h.poll(k1List(listRow(1, "A", "Example Course A")))
	if staged != 1 {
		t.Fatalf("staged = %d with corrupt state, want 1", staged)
	}
	if seen, err := loadDetailSeen(h.cp, "k1"); err != nil || len(seen) != 1 {
		t.Errorf("seen-set after recovery: %v, %v", seen, err)
	}
}

// ---------------------------------------------------------------------
// Checkpoint bound
// ---------------------------------------------------------------------

// TestAssignmentDetail_SeenSetBoundKeepsListedAssignments fills the
// seen-set well past maxSeenIDs with assignments that are no longer listed
// and checks that (a) the set stays at the cap, (b) the keys of the
// assignments Schoology still lists survive even though they have the
// smallest IDs -- "drop the smallest", the rule the other surfaces use,
// would evict exactly those and re-stage them at every refresh -- and (c)
// the other surfaces' seen-sets are untouched.
func TestAssignmentDetail_SeenSetBoundKeepsListedAssignments(t *testing.T) {
	h := newDetailHarness(t)

	// What the other surfaces rely on.
	for id := int64(1); id <= 3; id++ {
		if err := SaveLastSeenID(h.cp, assignmentSurface, "k1", id); err != nil {
			t.Fatal(err)
		}
		if err := SaveLastSeenID(h.cp, feedSurface, "k1", 100+id); err != nil {
			t.Fatal(err)
		}
	}
	assignBefore, _ := h.cp.Load(seenKey(assignmentSurface, "k1"))
	feedBefore, _ := h.cp.Load(seenKey(feedSurface, "k1"))

	// Two listed assignments with the smallest IDs, staged for real.
	listed := []*schoologylib.Assignment{
		listRow(1, "Still listed one", "Example Course A"),
		listRow(2, "Still listed two", "Example Course A"),
	}
	h.details[1] = regularDetail(1, "Still listed one", "Example Course A")
	h.details[2] = regularDetail(2, "Still listed two", "Example Course A")
	if staged, _, _ := h.poll(k1List(listed...)); staged != 2 {
		t.Fatalf("staged %d, want 2", staged)
	}

	// Years of history: more than maxSeenIDs keys for assignments that
	// are no longer listed, all with larger IDs.
	seen, err := loadDetailSeen(h.cp, "k1")
	if err != nil {
		t.Fatal(err)
	}
	const extra = maxSeenIDs + 250
	for i := int64(0); i < extra; i++ {
		seen[1000+i] = "feedfacefeedfacefeedfacefeedface"
	}
	// Write it unbounded, as if the cap had only just been reached.
	entries := make([]string, 0, len(seen))
	for id, hash := range seen {
		entries = append(entries, detailSeenEntry("k1", id, hash))
	}
	raw, _ := json.Marshal(entries)
	if err := h.cp.Save(seenKey(detailSurface, "k1"), string(raw)); err != nil {
		t.Fatal(err)
	}

	// A third assignment appears; staging it saves (and bounds) the set.
	h.details[3] = regularDetail(3, "Brand new", "Example Course A")
	listed = append(listed, listRow(3, "Brand new", "Example Course A"))
	h.now = h.now.Add(time.Hour)
	if staged, _, _ := h.poll(k1List(listed...)); staged != 1 {
		t.Fatalf("staged %d, want only the new assignment", staged)
	}

	seen, err = loadDetailSeen(h.cp, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != maxSeenIDs {
		t.Fatalf("seen-set size = %d, want the cap %d", len(seen), maxSeenIDs)
	}
	for _, id := range []int64{1, 2, 3} {
		if seen[id] == "" {
			t.Errorf("listed assignment %d was evicted", id)
		}
	}
	if _, ok := seen[1000]; ok {
		t.Error("the oldest unlisted key was kept past the cap")
	}
	if _, ok := seen[1000+extra-1]; !ok {
		t.Error("the newest unlisted key was evicted")
	}

	// The refresh reads all three again and stages none: no flood.
	h.resetCalls()
	h.now = h.now.Add(25 * time.Hour)
	staged, _, _ := h.poll(k1List(listed...))
	if len(h.fetched) != 3 || staged != 0 {
		t.Errorf("refresh after the bound: fetched=%d staged=%d, want 3 and 0", len(h.fetched), staged)
	}

	// Detail keys never touched the other surfaces' sets.
	if got, _ := h.cp.Load(seenKey(assignmentSurface, "k1")); got != assignBefore {
		t.Errorf("assignment seen-set changed: %q -> %q", assignBefore, got)
	}
	if got, _ := h.cp.Load(seenKey(feedSurface, "k1")); got != feedBefore {
		t.Errorf("feed seen-set changed: %q -> %q", feedBefore, got)
	}
	for id := int64(1); id <= 3; id++ {
		if d, _ := ShouldStage(h.cp, assignmentSurface, "k1", id); d != StageSkipDuplicate {
			t.Errorf("assignment %d would be re-staged by the assignment surface (%s)", id, d)
		}
	}
}

func TestBoundDetailSeen_HardLimitEvenWhenEverythingIsListed(t *testing.T) {
	seen := detailSeen{}
	listed := map[int64]bool{}
	for id := int64(1); id <= 12; id++ {
		seen[id] = "h"
		listed[id] = true
	}
	boundDetailSeen(seen, listed, 10)
	if len(seen) != 10 {
		t.Fatalf("size = %d, want 10", len(seen))
	}
	if _, ok := seen[1]; ok {
		t.Error("smallest listed id kept past a hard limit")
	}
	if _, ok := seen[12]; !ok {
		t.Error("largest listed id evicted")
	}
}

func TestDetailRows_StayBoundedByTheListing(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.MaxPerPoll = 200
	var rows []*schoologylib.Assignment
	for id := int64(1); id <= 60; id++ {
		h.details[id] = regularDetail(id, "A", "Example Course A")
		rows = append(rows, listRow(id, "A", "Example Course A"))
	}
	h.poll(k1List(rows...))
	h.now = h.now.Add(time.Hour)
	for i := 0; i < detailRowGracePolls; i++ {
		h.poll(k1List(rows[:4]...))
	}
	state, _ := loadDetailRows(h.cp, "k1")
	if len(state) != 4 {
		t.Errorf("row state holds %d rows, want the 4 still listed", len(state))
	}
}

// ---------------------------------------------------------------------
// Through the connector
// ---------------------------------------------------------------------

// newDetailConnector wires a connector with the detail surface enabled for
// k1 and k2 over the harness's fake client, clock and staging.
func newDetailConnector(t *testing.T, h *detailHarness, client *fakeClient) *SchoologyConnector {
	t.Helper()
	cfg := Config{
		Kids: []Kid{detailKid1, detailKid2},
		PollSchedule: PollSchedule{
			Windows: []PollWindow{{Start: "08:00", End: "09:00"}},
		},
		AssignmentDetail: AssignmentDetailConfig{Kids: []string{"k1", "k2"}},
	}
	cfg.Rules = []connector.Rule{
		{Match: "schoology:k1:assignment", Destination: "school", DataSubject: "e_k1"},
		{Match: "schoology:k1:assignment_detail", Destination: "school", DataSubject: "e_k1"},
		{Match: "schoology:k2:assignment", Destination: "school", DataSubject: "e_k2"},
		{Match: "schoology:k2:assignment_detail", Destination: "school", DataSubject: "e_k2"},
		{Match: "schoology-parse-failure:*", Destination: "school"},
	}
	ApplyDefaults(&cfg)
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	c, err := NewConnector(client, cfg, "v0.3.0")
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	c.now = func() time.Time { return h.now }
	c.sleep = func(_ context.Context, d time.Duration) error {
		h.sleeps = append(h.sleeps, d)
		return nil
	}
	metrics, err := connector.NewMetrics("schoology-detail-" + t.Name())
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	if err := c.Wire(connector.ConnectorContext{
		Writer:  h.writer.(*connector.StagingWriter),
		Matcher: connector.NewRuleMatcher(cfg.Rules),
		Metrics: metrics,
	}); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	return c
}

func TestConnector_AssignmentDetailUsesEachKidsOwnList(t *testing.T) {
	h := newDetailHarness(t)
	h.details[11] = regularDetail(11, "K1 task", "Example Course A: Section 1")
	h.details[22] = assessmentDetail(22, "K2 test", "Example Course B: Section 2")

	listCalls := 0
	client := h.client()
	client.OverdueSubmissionsFunc = func(_ context.Context, uid int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
		listCalls++
		switch uid {
		case detailKid1.SchoologyUID:
			return []*schoologylib.Assignment{listRow(11, "K1 task", "Example Course A")}, nil, nil
		case detailKid2.SchoologyUID:
			return []*schoologylib.Assignment{listRow(22, "K2 test", "Example Course B")}, nil, nil
		}
		return nil, nil, nil
	}
	client.FeedFunc = func(context.Context, int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
		return nil, nil, nil
	}
	client.InboxFunc = func(context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
		return nil, nil, nil
	}

	c := newDetailConnector(t, h, client)
	if err := c.pollNow(context.Background(), h.cp, "scheduled", 0); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	if listCalls != 2 {
		t.Errorf("the list was requested %d times, want once per kid", listCalls)
	}

	bySubject := map[string]string{}
	for _, it := range h.detailItems() {
		bySubject[it.meta.DataSubject] = it.meta.Tags["assignment_id"]
	}
	if bySubject["e_k1"] != "11" || bySubject["e_k2"] != "22" || len(bySubject) != 2 {
		t.Errorf("detail items by data subject = %v, want k1->11 and k2->22", bySubject)
	}
	// Two assignment items and two detail items, linked by id.
	if got := len(h.items()); got != 4 {
		t.Errorf("staged %d items, want 4", got)
	}
}

func TestConnector_AssignmentDetailAuthTakesTheRejectedSessionPath(t *testing.T) {
	h := newDetailHarness(t)
	h.details[11] = regularDetail(11, "K1 task", "Example Course A")
	h.details[12] = regularDetail(12, "K1 other", "Example Course A")
	h.failWith[11] = schoologylib.ErrSessionExpired
	h.validateErr = schoologylib.ErrSessionExpired

	client := h.client()
	client.OverdueSubmissionsFunc = func(_ context.Context, uid int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
		if uid != detailKid1.SchoologyUID {
			return nil, nil, nil
		}
		return []*schoologylib.Assignment{
			listRow(11, "K1 task", "Example Course A"),
			listRow(12, "K1 other", "Example Course A"),
		}, nil, nil
	}
	client.FeedFunc = func(context.Context, int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
		return nil, nil, nil
	}
	client.InboxFunc = func(context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
		return nil, nil, nil
	}

	c := newDetailConnector(t, h, client)
	rejected := NewRejectedSession(t.TempDir())
	fp := SessionFingerprint("sess-under-test")
	c.OnSessionRejected = func() {
		if err := rejected.Mark(fp); err != nil {
			t.Errorf("Mark: %v", err)
		}
	}

	err := c.pollNow(context.Background(), h.cp, "scheduled", 0)
	if err == nil || !connector.IsPermanent(err) {
		t.Fatalf("pollNow = %v, want a permanent error", err)
	}
	if !strings.Contains(err.Error(), "session expired") || !strings.Contains(err.Error(), "AUTH-RECOVERY.md") {
		t.Errorf("error = %q", err.Error())
	}
	if !rejected.IsRejected(fp) {
		t.Error("the session was not recorded as rejected")
	}
	if len(h.fetched) != 1 {
		t.Errorf("made %d detail requests with a dead session, want 1", len(h.fetched))
	}
}

func TestConnector_AssignmentDetailSkippedWhenAnotherSurfaceSawAuthFailure(t *testing.T) {
	h := newDetailHarness(t)
	h.details[11] = regularDetail(11, "K1 task", "Example Course A")

	client := h.client()
	client.OverdueSubmissionsFunc = func(context.Context, int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
		return []*schoologylib.Assignment{listRow(11, "K1 task", "Example Course A")}, nil, nil
	}
	client.FeedFunc = func(context.Context, int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
		return nil, nil, schoologylib.ErrSessionExpired
	}
	client.InboxFunc = func(context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
		return nil, nil, nil
	}

	c := newDetailConnector(t, h, client)
	if err := c.pollNow(context.Background(), h.cp, "scheduled", 0); err == nil {
		t.Fatal("pollNow returned nil with an expired session")
	}
	if len(h.fetched) != 0 {
		t.Errorf("made %d detail requests after the feed saw the session refused", len(h.fetched))
	}
}

func TestConnector_AssignmentDetailSkipsKidWhoseListFailed(t *testing.T) {
	h := newDetailHarness(t)
	h.details[22] = regularDetail(22, "K2 task", "Example Course A")
	// k1 has remembered rows from an earlier poll.
	if err := saveDetailRows(h.cp, "k1", detailRows{11: {Fingerprint: "x", FetchedAt: h.now.Unix()}}); err != nil {
		t.Fatal(err)
	}

	client := h.client()
	client.OverdueSubmissionsFunc = func(_ context.Context, uid int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
		if uid == detailKid1.SchoologyUID {
			return nil, nil, &schoologylib.Error{Code: schoologylib.ErrCodeServer, Op: "GetOverdueSubmissions", Message: "status 502"}
		}
		return []*schoologylib.Assignment{listRow(22, "K2 task", "Example Course A")}, nil, nil
	}
	client.FeedFunc = func(context.Context, int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
		return nil, nil, nil
	}
	client.InboxFunc = func(context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
		return nil, nil, nil
	}

	c := newDetailConnector(t, h, client)
	if err := c.pollNow(context.Background(), h.cp, "scheduled", 0); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	if len(h.fetched) != 1 || h.fetched[0] != 22 {
		t.Errorf("fetched %v, want only k2's assignment", h.fetched)
	}
	rows, _ := loadDetailRows(h.cp, "k1")
	if _, ok := rows[11]; !ok {
		t.Error("k1's remembered rows were pruned although its list call failed")
	}
}

// ---------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------

func detailConfig() Config {
	cfg := Config{
		Kids:         []Kid{detailKid1, detailKid2},
		PollSchedule: PollSchedule{Windows: []PollWindow{{Start: "08:00", End: "09:00"}}},
	}
	cfg.Rules = []connector.Rule{
		{Match: "schoology:k1:assignment_detail", Destination: "school", DataSubject: "e_k1"},
		{Match: "*", Destination: "school"},
	}
	return cfg
}

func TestConfig_AssignmentDetailDefaults(t *testing.T) {
	cfg := detailConfig()
	ApplyDefaults(&cfg)
	d := cfg.AssignmentDetail
	if len(d.Kids) != 0 {
		t.Errorf("surface enabled by default for %v", d.Kids)
	}
	if d.MaxPerPoll != 20 || d.RefreshHours != 20 || d.RequestSpacingMS != 1500 {
		t.Errorf("defaults = %+v, want max_per_poll 20, refresh_hours 20, request_spacing_ms 1500", d)
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Errorf("default config invalid: %v", err)
	}
	if cfg.AssignmentDetail.Enabled("k1") {
		t.Error("Enabled(k1) with no kids listed")
	}
}

func TestConfig_AssignmentDetailValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"enabled with an exact rule", func(c *Config) { c.AssignmentDetail.Kids = []string{"k1"} }, ""},
		{"unknown kid", func(c *Config) { c.AssignmentDetail.Kids = []string{"k9"} }, "not a configured kid"},
		{"duplicate kid", func(c *Config) { c.AssignmentDetail.Kids = []string{"k1", "k1"} }, "duplicate"},
		{"only the wildcard would match", func(c *Config) { c.AssignmentDetail.Kids = []string{"k2"} }, "no rule matches"},
		{"exact rule behind the wildcard", func(c *Config) {
			c.AssignmentDetail.Kids = []string{"k2"}
			c.Rules = append(c.Rules, connector.Rule{Match: "schoology:k2:assignment_detail", Destination: "school"})
		}, "no rule matches"},
		{"cap too large", func(c *Config) { c.AssignmentDetail.MaxPerPoll = 201 }, "max_per_poll"},
		{"negative cap", func(c *Config) { c.AssignmentDetail.MaxPerPoll = -1 }, "max_per_poll"},
		{"negative refresh", func(c *Config) { c.AssignmentDetail.RefreshHours = -1 }, "refresh_hours"},
		{"spacing too tight", func(c *Config) { c.AssignmentDetail.RequestSpacingMS = 100 }, "request_spacing_ms"},
		{"spacing too long", func(c *Config) { c.AssignmentDetail.RequestSpacingMS = 120000 }, "request_spacing_ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := detailConfig()
			tc.mutate(&cfg)
			ApplyDefaults(&cfg)
			err := ValidateConfig(&cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("no error, want one mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfig_AssignmentDetailParsesFromJSON(t *testing.T) {
	var cfg Config
	raw := `{
		"kids": [{"name": "k1", "schoology_uid": 1}],
		"poll_schedule": {"windows": [{"start": "07:00", "end": "09:00"}]},
		"assignment_detail": {"kids": ["k1"], "max_per_poll": 8},
		"rules": [{"match": "schoology:k1:assignment_detail", "destination": "school"}]
	}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	ApplyDefaults(&cfg)
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.AssignmentDetail.Enabled("k1") || cfg.AssignmentDetail.MaxPerPoll != 8 {
		t.Errorf("parsed = %+v", cfg.AssignmentDetail)
	}
}

func TestTimezone_FromEnv(t *testing.T) {
	t.Setenv("SCHOOLOGY_TIMEZONE", "America/New_York")
	tz, err := Timezone()
	if err != nil || tz.String() != "America/New_York" {
		t.Errorf("Timezone() = %v, %v", tz, err)
	}
	t.Setenv("SCHOOLOGY_TIMEZONE", "")
	tz, err = Timezone()
	if err != nil || tz.String() != "America/Los_Angeles" {
		t.Errorf("default Timezone() = %v, %v", tz, err)
	}
}
