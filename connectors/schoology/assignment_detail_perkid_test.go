package schoology

import (
	"strconv"
	"strings"
	"testing"
	"time"

	schoologylib "github.com/leftathome/schoology-go"
)

// All names, courses and text in this file are invented.

// Detail reads are made per kid, under that kid's view (glovebox-n1x1).
// Whether Schoology's assignment page -- its comment block in particular --
// depends on the child being viewed is not known, so the harness can serve
// a different page per view (viewDetails) and the tests assert that every
// read for a kid is made after a switch to that kid and that nothing read
// for one kid is used for another.

func indexOf(calls []string, call string) int {
	for i, c := range calls {
		if c == call {
			return i
		}
	}
	return -1
}

func countPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func uidOf(k Kid) string { return strconv.FormatInt(k.SchoologyUID, 10) }

// readsAreUnderTheirKidsView fails the test if any page or comments read in
// calls was made while the session viewed a child other than the one whose
// list holds the id.
func readsAreUnderTheirKidsView(t *testing.T, calls []string, owner map[int64]Kid) {
	t.Helper()
	view := ""
	for _, c := range calls {
		parts := strings.Split(c, ":")
		switch parts[0] {
		case "switch":
			view = parts[1]
		case "get", "comments":
			id, _ := strconv.ParseInt(parts[2], 10, 64)
			if parts[1] != view {
				t.Errorf("%s: the fake's view (%s) and the last switch (%s) disagree", c, parts[1], view)
			}
			if k, ok := owner[id]; ok && uidOf(k) != parts[1] {
				t.Errorf("%s: read under view %s, but the id is %s's", c, parts[1], k.Name)
			}
		}
	}
}

func TestAssignmentDetail_SharedIDIsReadOncePerKidAfterThatKidsSwitch(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000020
	h.details[id] = regularDetail(id, "Shared worksheet", "Example Course A: Section 1")

	staged, _, _ := h.poll(
		k1List(listRow(id, "Shared worksheet", "Example Course A")),
		k2List(listRow(id, "Shared worksheet", "Example Course A")),
	)
	if staged != 2 {
		t.Fatalf("staged = %d, want 2", staged)
	}
	sid := strconv.FormatInt(id, 10)
	want := []string{
		"switch:" + uidOf(detailKid1), "get:" + uidOf(detailKid1) + ":" + sid,
		"switch:" + uidOf(detailKid2), "get:" + uidOf(detailKid2) + ":" + sid,
	}
	if strings.Join(h.calls, " ") != strings.Join(want, " ") {
		t.Fatalf("calls = %v, want %v (one read per kid, each after a switch to that kid)", h.calls, want)
	}
	s1 := indexOf(h.calls, "switch:"+uidOf(detailKid1))
	g1 := indexOf(h.calls, "get:"+uidOf(detailKid1)+":"+sid)
	s2 := indexOf(h.calls, "switch:"+uidOf(detailKid2))
	g2 := indexOf(h.calls, "get:"+uidOf(detailKid2)+":"+sid)
	if !(s1 >= 0 && s1 < g1 && s2 >= 0 && s2 < g2) {
		t.Errorf("ordering: switch(k1)=%d fetch(k1)=%d switch(k2)=%d fetch(k2)=%d", s1, g1, s2, g2)
	}
}

// The page served under each kid's view differs; each kid's item and its
// record (the OnVerified read) must carry that kid's page, never the other
// kid's -- including its comments.
func TestAssignmentDetail_EachKidsItemAndRecordComeFromItsOwnRead(t *testing.T) {
	h, reads := recordingHarness(t)
	const id = 9100000021
	h.details[id] = regularDetail(id, "Shared worksheet", "Example Course A: Section 1")

	p1 := regularDetail(id, "Shared worksheet", "Example Course A: Section 1")
	p1.DescriptionText = "EXAMPLE page as seen for e_child1."
	p1.Comments = schoologylib.AssignmentComments{State: schoologylib.CommentsStatePopulated,
		Comments: []*schoologylib.AssignmentComment{{Text: "EXAMPLE comment for e_child1"}}}
	p2 := regularDetail(id, "Shared worksheet", "Example Course A: Section 1")
	p2.DescriptionText = "EXAMPLE page as seen for e_child2."
	p2.Comments = schoologylib.AssignmentComments{State: schoologylib.CommentsStateEmpty}
	h.viewDetails[detailKid1.SchoologyUID] = map[int64]*schoologylib.AssignmentDetail{id: p1}
	h.viewDetails[detailKid2.SchoologyUID] = map[int64]*schoologylib.AssignmentDetail{id: p2}

	if staged, _, _ := h.poll(
		k1List(listRow(id, "Shared worksheet", "Example Course A")),
		k2List(listRow(id, "Shared worksheet", "Example Course A")),
	); staged != 2 {
		t.Fatalf("staged = %d, want 2", staged)
	}

	bySubject := map[string]stagedItem{}
	for _, it := range h.detailItems() {
		bySubject[it.meta.DataSubject] = it
	}
	if c := bySubject["e_k1"].content; !strings.Contains(c, "seen for e_child1") || strings.Contains(c, "e_child2") {
		t.Errorf("k1's item does not carry k1's page:\n%s", c)
	}
	if c := bySubject["e_k2"].content; !strings.Contains(c, "seen for e_child2") || strings.Contains(c, "e_child1") {
		t.Errorf("k2's item does not carry k2's page:\n%s", c)
	}
	if bySubject["e_k1"].meta.Tags["comments_state"] != "populated" || bySubject["e_k2"].meta.Tags["comments_state"] != "empty" {
		t.Errorf("comments_state k1=%q k2=%q, want populated, empty",
			bySubject["e_k1"].meta.Tags["comments_state"], bySubject["e_k2"].meta.Tags["comments_state"])
	}

	if len(*reads) != 2 {
		t.Fatalf("%d verified reads, want 2", len(*reads))
	}
	for _, v := range *reads {
		want := map[string]*schoologylib.AssignmentDetail{"k1": p1, "k2": p2}[v.Kid.Name]
		if want == nil {
			t.Fatalf("verified read for unexpected kid %q", v.Kid.Name)
		}
		if v.Detail.DescriptionText != want.DescriptionText {
			t.Errorf("%s's record carries description %q, want its own %q", v.Kid.Name, v.Detail.DescriptionText, want.DescriptionText)
		}
		if v.Comments.State != want.Comments.State || len(v.Comments.Comments) != len(want.Comments.Comments) {
			t.Errorf("%s's record carries comments %+v, want its own %+v", v.Kid.Name, v.Comments, want.Comments)
		}
	}
}

// A read made for one kid is never reused for another, in the same poll or
// a later one: each kid's refresh is its own request.
func TestAssignmentDetail_ReadForOneKidNeverServesAnother(t *testing.T) {
	h := newDetailHarness(t)
	h.cfg.MaxPerPoll = 1
	const id = 9100000022
	h.details[id] = regularDetail(id, "Shared worksheet", "Example Course A: Section 1")
	lists := []KidAssignments{
		k1List(listRow(id, "Shared worksheet", "Example Course A")),
		k2List(listRow(id, "Shared worksheet", "Example Course A")),
	}

	// A cap of one: k1 (first turn) is read, k2 is deferred -- not
	// satisfied from k1's read.
	staged, _, _ := h.poll(lists...)
	if staged != 1 || len(h.fetched) != 1 {
		t.Fatalf("staged=%d fetched=%v, want one read and one item under a cap of 1", staged, h.fetched)
	}
	if got := h.detailItems(); len(got) != 1 || got[0].meta.DataSubject != "e_k1" {
		t.Fatalf("items = %d, want only k1's", len(got))
	}
	if countPrefix(h.calls, "switch:"+uidOf(detailKid2)) != 0 {
		t.Errorf("switched to k2 with none of k2's reads inside the cap: %v", h.calls)
	}

	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	if staged, _, _ := h.poll(lists...); staged != 1 {
		t.Fatalf("second poll staged %d, want k2's", staged)
	}
	sid := strconv.FormatInt(id, 10)
	if strings.Join(h.calls, " ") != "switch:"+uidOf(detailKid2)+" get:"+uidOf(detailKid2)+":"+sid {
		t.Errorf("second poll calls = %v, want k2's own switch and read", h.calls)
	}
}

// One switch per kid that has reads in the poll, however many reads it
// has; a kid with nothing due gets no switch.
func TestAssignmentDetail_SwitchesAreBoundedToOnePerKidPerPoll(t *testing.T) {
	h := newDetailHarness(t)
	owner := map[int64]Kid{}
	k1rows := simpleRows(h, 1, 5)
	h.details[3] = assessmentDetail(3, "Task 3", "Example Course A: Section 1")
	k2rows := simpleRows(h, 101, 103)
	for _, r := range k1rows {
		owner[r.ID] = detailKid1
	}
	for _, r := range k2rows {
		owner[r.ID] = detailKid2
	}

	h.poll(k1List(k1rows...), k2List(k2rows...))
	if n := countPrefix(h.calls, "switch:"); n != 2 {
		t.Errorf("%d switches in a poll with two kids, want 2: %v", n, h.calls)
	}
	if countPrefix(h.calls, "get:") != 8 || countPrefix(h.calls, "comments:") != 1 {
		t.Errorf("calls = %v, want 8 pages and 1 comments page", h.calls)
	}
	readsAreUnderTheirKidsView(t, h.calls, owner)

	// Nothing due for either kid: no switch at all.
	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	h.poll(k1List(k1rows...), k2List(k2rows...))
	if len(h.calls) != 0 {
		t.Errorf("calls with nothing due = %v, want none", h.calls)
	}

	// Only k2 has a new row: one switch, to k2.
	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	k2more := append(append([]*schoologylib.Assignment{}, k2rows...), simpleRows(h, 104, 104)...)
	owner[104] = detailKid2
	h.poll(k1List(k1rows...), k2List(k2more...))
	if strings.Join(h.calls, " ") != "switch:"+uidOf(detailKid2)+" get:"+uidOf(detailKid2)+":104" {
		t.Errorf("calls = %v, want one switch to k2 and its one read", h.calls)
	}
}

// The switch is a request like any other: spaced, and not started once the
// poll window is about to close.
func TestAssignmentDetail_SwitchIsSpacedAndRespectsTheWindow(t *testing.T) {
	h := newDetailHarness(t)
	rows := simpleRows(h, 1, 2)
	h.poll(k1List(rows...))
	// switch, page, page: two gaps.
	if len(h.sleeps) != 2 {
		t.Errorf("waited %d times around a switch and two reads, want 2", len(h.sleeps))
	}

	h2 := newDetailHarness(t)
	h2.deadline = h2.now.Add(10 * time.Second)
	h2.poll(k1List(simpleRows(h2, 1, 2)...))
	if len(h2.calls) != 0 {
		t.Errorf("calls with the window closing = %v, want none (not even the switch)", h2.calls)
	}
}

// A kid whose switch is refused for that kid alone is skipped -- none of
// its pages are read under another kid's view -- and the other kid still
// gets its reads. The skipped rows are not remembered, so they are tried
// again next poll.
func TestAssignmentDetail_FailedSwitchSkipsOnlyThatKid(t *testing.T) {
	h := newDetailHarness(t)
	const id = 9100000023
	h.details[id] = regularDetail(id, "Shared worksheet", "Example Course A: Section 1")
	h.switchErr[detailKid1.SchoologyUID] = &schoologylib.Error{Code: schoologylib.ErrCodePermission, Op: "viewAs", Message: "forbidden"}

	staged, errs, _ := h.poll(
		k1List(listRow(id, "Shared worksheet", "Example Course A")),
		k2List(listRow(id, "Shared worksheet", "Example Course A")),
	)
	sid := strconv.FormatInt(id, 10)
	want := "switch:" + uidOf(detailKid1) + " switch:" + uidOf(detailKid2) + " get:" + uidOf(detailKid2) + ":" + sid
	if strings.Join(h.calls, " ") != want {
		t.Fatalf("calls = %v, want %s", h.calls, want)
	}
	if staged != 1 || !errs {
		t.Errorf("staged=%d errs=%v, want 1,true", staged, errs)
	}
	if got := h.detailItems(); len(got) != 1 || got[0].meta.DataSubject != "e_k2" {
		t.Errorf("items %v, want only k2's", got)
	}
	if len(h.receipts("permission")) != 1 {
		t.Error("no receipt for k1's refused switch")
	}
	state, _ := loadDetailRows(h.cp, "k1")
	if state[id].FetchedAt != 0 {
		t.Error("k1's row was remembered as read although its switch failed")
	}

	delete(h.switchErr, detailKid1.SchoologyUID)
	h.resetCalls()
	h.now = h.now.Add(time.Hour)
	if staged, _, _ := h.poll(
		k1List(listRow(id, "Shared worksheet", "Example Course A")),
		k2List(listRow(id, "Shared worksheet", "Example Course A")),
	); staged != 1 {
		t.Errorf("next poll staged %d, want k1's", staged)
	}
}

// A switch that fails the way a site or network failure does ends the
// surface for the poll, as a page read would.
func TestAssignmentDetail_SwitchServerErrorStopsTheSurface(t *testing.T) {
	h := newDetailHarness(t)
	h.switchErr[detailKid1.SchoologyUID] = &schoologylib.Error{Code: schoologylib.ErrCodeServer, Op: "viewAs", Message: "server error", StatusCode: 502}
	staged, errs, _ := h.poll(k1List(simpleRows(h, 1, 2)...), k2List(simpleRows(h, 101, 102)...))
	if staged != 0 || !errs || len(h.calls) != 1 {
		t.Errorf("staged=%d errs=%v calls=%v, want 0,true and only the failed switch", staged, errs, h.calls)
	}
	if len(h.receipts("server")) != 1 {
		t.Error("no server receipt")
	}
}

// An auth answer to the switch is checked like one on a page: a refused
// session check takes the rejected-session path.
func TestAssignmentDetail_SwitchAuthWithRefusedSessionStops(t *testing.T) {
	h := newDetailHarness(t)
	h.switchErr[detailKid1.SchoologyUID] = authErr("viewAs")
	h.validateErr = schoologylib.ErrSessionExpired
	_, _, dedup := h.poll(k1List(simpleRows(h, 1, 2)...), k2List(simpleRows(h, 101, 102)...))
	if len(h.fetched) != 0 || h.validated != 1 {
		t.Errorf("fetched=%v validated=%d, want no page read and one session check", h.fetched, h.validated)
	}
	if dedup.AffectedCount(detailParser, "auth") != 1 {
		t.Error("a confirmed dead session was not recorded for the rejected-session path")
	}
}
