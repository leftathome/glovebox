package schoology

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	schoologylib "github.com/leftathome/schoology-go"
)

// recordingHarness is a detail harness whose OnVerified collects reads.
func recordingHarness(t *testing.T) (*detailHarness, *[]VerifiedDetail) {
	h := newDetailHarness(t)
	var reads []VerifiedDetail
	h.onVerified = func(v VerifiedDetail) { reads = append(reads, v) }
	return h, &reads
}

// richDescription has line breaks, a link rendered "text (href)" and a
// right-to-left override: the record carries it exactly as the library
// returned it; the journal scans it.
const richDescription = "Read pages 40-52.\n\nSee EXAMPLE notes (https://example.invalid/notes).\n\u202eEXAMPLE reversed\u202c\nAnswer all four."

func detailFields(t *testing.T, snaps []journalapi.Snapshot, key string) map[string]json.RawMessage {
	t.Helper()
	if len(snaps) != 1 {
		t.Fatalf("%d snapshots, want 1", len(snaps))
	}
	return fieldsOf(t, snaps[0], key)
}

func TestDetailRecordFromACleanRead(t *testing.T) {
	h, reads := recordingHarness(t)
	d := regularDetail(1, "A", "EXAMPLE SCIENCE 7")
	d.DescriptionText = richDescription
	h.details[1] = d
	if staged, _, _ := h.poll(k1List(listRow(1, "A", "EXAMPLE SCIENCE 7"))); staged != 1 {
		t.Fatalf("staged %d", staged)
	}
	if len(*reads) != 1 || (*reads)[0].Kid.Name != "k1" || (*reads)[0].Row.ID != 1 || !(*reads)[0].ReadAt.Equal(h.now) {
		t.Fatalf("reads %+v", *reads)
	}
	snaps := BuildAssignmentDetails(bind1, *reads, obsAt, nil)
	validateAll(t, snaps)
	s := snaps[0]
	if s.RecordType != TypeLMSAssignmentDetail || s.Complete || s.CompleteRange != nil {
		t.Fatalf("header %+v", s)
	}
	wantScope(t, s, "assignment_detail", "e_child1")
	f := detailFields(t, snaps, "1")
	var desc string
	if err := json.Unmarshal(f["description"], &desc); err != nil || desc != richDescription {
		t.Fatalf("description %q (%v), want it unchanged", desc, err)
	}
	if string(f["comments"]) != "null" {
		t.Fatalf("empty comment block: comments = %s, want null", f["comments"])
	}
	if r := s.Records[0]; r.DataSubject != "e_child1" || !reflect.DeepEqual(r.Audience, []string{"subject", "guardians"}) {
		t.Fatalf("record %+v", r)
	}
}

// A re-read whose content is unchanged stages no item, but is still a
// record (the journal skips unchanged content).
func TestDetailRecordForAnUnchangedReRead(t *testing.T) {
	h, reads := recordingHarness(t)
	h.details[1] = regularDetail(1, "A", "EXAMPLE SCIENCE 7")
	list := k1List(listRow(1, "A", "EXAMPLE SCIENCE 7"))
	if staged, _, _ := h.poll(list); staged != 1 {
		t.Fatalf("first poll staged %d", staged)
	}
	h.now = h.now.Add(21 * time.Hour) // past refresh_hours
	h.resetCalls()
	if staged, _, _ := h.poll(list); staged != 0 || len(h.fetched) != 1 {
		t.Fatalf("re-read staged %d, fetched %v", staged, h.fetched)
	}
	if len(*reads) != 2 {
		t.Fatalf("%d verified reads, want 2 (the unchanged re-read is one)", len(*reads))
	}
	if len(h.detailItems()) != 1 {
		t.Fatalf("%d detail items", len(h.detailItems()))
	}
}

// Decision T2: a degraded read is no record; its item is staged as today.
func TestDetailRecordOnlyForACleanRead(t *testing.T) {
	cases := map[string]func(h *detailHarness){
		"parse error": func(h *detailHarness) {
			h.details[1] = regularDetail(1, "A", "EXAMPLE SCIENCE 7")
			h.parseErr[1] = schoologylib.ParseErrors{schoologylib.NewParseError("GetAssignment", "EXAMPLE category not found")}
		},
		"comments error": func(h *detailHarness) {
			h.details[1] = assessmentDetail(1, "A", "EXAMPLE SCIENCE 7")
			h.commErr[1] = schoologylib.NewParseError("GetAssignmentComments", "missing .comment-container; not a comments page")
		},
		"course unverified": func(h *detailHarness) {
			d := regularDetail(1, "A", "EXAMPLE SCIENCE 7")
			d.CourseTitle = ""
			h.details[1] = d
		},
		// Comments not read: a later bad read must not blank a stored
		// teacher comment. Unknown is "no block looked at", unavailable is
		// the comments page refused (403); both come with no error.
		"comments unknown": func(h *detailHarness) {
			d := regularDetail(1, "A", "EXAMPLE SCIENCE 7")
			d.Comments = schoologylib.AssignmentComments{State: schoologylib.CommentsStateUnknown}
			h.details[1] = d
		},
		"comments unavailable (inline)": func(h *detailHarness) {
			d := regularDetail(1, "A", "EXAMPLE SCIENCE 7")
			d.Comments = schoologylib.AssignmentComments{State: schoologylib.CommentsStateUnavailable}
			h.details[1] = d
		},
		"comments page unavailable (assessment)": func(h *detailHarness) {
			h.details[1] = assessmentDetail(1, "A", "EXAMPLE SCIENCE 7")
			h.comments[1] = &schoologylib.AssignmentComments{State: schoologylib.CommentsStateUnavailable}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h, reads := recordingHarness(t)
			setup(h)
			if staged, _, _ := h.poll(k1List(listRow(1, "A", "EXAMPLE SCIENCE 7"))); staged != 1 {
				t.Fatalf("staged %d, want the degraded item as today", staged)
			}
			if len(*reads) != 0 {
				t.Fatalf("%d verified reads for a degraded read", len(*reads))
			}
		})
	}
}

// The comment states that count as read: an empty block (a regular
// assignment's inline block, or an assessment's comments page) and a
// populated one.
func TestDetailRecordWhenTheCommentsWereRead(t *testing.T) {
	for name, setup := range map[string]func(h *detailHarness){
		"inline empty":     func(h *detailHarness) { h.details[1] = regularDetail(1, "A", "EXAMPLE SCIENCE 7") },
		"assessment empty": func(h *detailHarness) { h.details[1] = assessmentDetail(1, "A", "EXAMPLE SCIENCE 7") },
		"assessment populated": func(h *detailHarness) {
			h.details[1] = assessmentDetail(1, "A", "EXAMPLE SCIENCE 7")
			h.comments[1] = &schoologylib.AssignmentComments{State: schoologylib.CommentsStatePopulated,
				Comments: []*schoologylib.AssignmentComment{{Text: "EXAMPLE comment"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, reads := recordingHarness(t)
			setup(h)
			h.poll(k1List(listRow(1, "A", "EXAMPLE SCIENCE 7")))
			if len(*reads) != 1 {
				t.Fatalf("%d verified reads, want 1", len(*reads))
			}
		})
	}
}

// An id or title that does not match the list is neither an item nor a
// record.
func TestDetailRecordNotForAMismatch(t *testing.T) {
	cases := map[string]*schoologylib.AssignmentDetail{
		"id":     regularDetail(2, "A", "EXAMPLE SCIENCE 7"),
		"title":  regularDetail(1, "B", "EXAMPLE SCIENCE 7"),
		"course": regularDetail(1, "A", "EXAMPLE HISTORY 7"),
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			h, reads := recordingHarness(t)
			h.details[1] = d
			if staged, _, _ := h.poll(k1List(listRow(1, "A", "EXAMPLE SCIENCE 7"))); staged != 0 {
				t.Fatalf("staged %d", staged)
			}
			if len(*reads) != 0 {
				t.Fatalf("%d verified reads for a mismatch", len(*reads))
			}
		})
	}
}

// Decision T3: entries joined with a blank line, in page order, as the
// library returned them; never the item's count-only placeholder.
func TestDetailRecordComments(t *testing.T) {
	h, reads := recordingHarness(t)
	h.details[1] = assessmentDetail(1, "A", "EXAMPLE SCIENCE 7")
	h.comments[1] = &schoologylib.AssignmentComments{
		State:          schoologylib.CommentsStatePopulated,
		LayoutUnmapped: true,
		Comments: []*schoologylib.AssignmentComment{
			{Text: "EXAMPLE Teacher Oct 9 Nice work on part 1."}, nil,
			{Text: "EXAMPLE Teacher Oct 10 Please redo\nquestion 3."},
		},
	}
	h.poll(k1List(listRow(1, "A", "EXAMPLE SCIENCE 7")))
	snaps := BuildAssignmentDetails(bind1, *reads, obsAt, nil)
	validateAll(t, snaps)
	f := detailFields(t, snaps, "1")
	var got string
	if err := json.Unmarshal(f["comments"], &got); err != nil {
		t.Fatal(err)
	}
	if want := "EXAMPLE Teacher Oct 9 Nice work on part 1.\n\nEXAMPLE Teacher Oct 10 Please redo\nquestion 3."; got != want {
		t.Fatalf("comments %q, want %q", got, want)
	}
	if string(f["description"]) != "null" {
		t.Fatalf("an assessment has no description: %s", f["description"])
	}
	if strings.Contains(h.detailItems()[0].content, "Nice work") {
		t.Fatal("the item body carries unmapped comment text; the item path changed")
	}
}

// With OnVerified nil (records off) the item path is exactly as without the
// hook: the same items and the same checkpoint.
func TestDetailWithoutTheHookChangesNothing(t *testing.T) {
	run := func(hook bool) ([]stagedItem, map[string]string) {
		h := newDetailHarness(t)
		cpDir := t.TempDir()
		cp, err := connector.NewCheckpoint(cpDir)
		if err != nil {
			t.Fatal(err)
		}
		h.cp = cp
		if hook {
			h.onVerified = func(VerifiedDetail) {}
		}
		h.details[1] = regularDetail(1, "A", "EXAMPLE SCIENCE 7")
		h.details[2] = assessmentDetail(2, "B", "EXAMPLE SCIENCE 7")
		h.parseErr[2] = schoologylib.ParseErrors{schoologylib.NewParseError("GetAssignment", "EXAMPLE")}
		list := k1List(listRow(1, "A", "EXAMPLE SCIENCE 7"), listRow(2, "B", "EXAMPLE SCIENCE 7"))
		h.poll(list)
		h.now = h.now.Add(21 * time.Hour)
		h.poll(list)
		return h.items(), checkpointFiles(t, cpDir)
	}
	offItems, offCP := run(false)
	onItems, onCP := run(true)
	if a, b := comparableItems(offItems), comparableItems(onItems); !reflect.DeepEqual(a, b) {
		t.Fatalf("items differ:\nwithout %v\nwith    %v", a, b)
	}
	if !reflect.DeepEqual(offCP, onCP) {
		t.Fatalf("checkpoints differ:\nwithout %v\nwith    %v", offCP, onCP)
	}
}

// comparableItems drops what differs between two runs by construction (the
// item id and staging timestamps) and sorts.
func comparableItems(items []stagedItem) []string {
	var out []string
	for _, it := range items {
		m := it.meta
		m.Timestamp = time.Time{}
		raw, _ := json.Marshal(m)
		out = append(out, string(raw)+"\n"+it.content)
	}
	sort.Strings(out)
	return out
}

// checkpointFiles reads every file under a checkpoint's directory.
func checkpointFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			raw, _ := os.ReadFile(p)
			out[strings.TrimPrefix(p, dir)] = string(raw)
		}
		return nil
	})
	if len(out) == 0 {
		t.Fatal("no checkpoint written")
	}
	return out
}

func TestBuildAssignmentDetailsKeysAndDuplicates(t *testing.T) {
	var skipped []string
	b := bind1
	b.OnSkip = func(r string) { skipped = append(skipped, r) }
	read := func(id int64, desc string) VerifiedDetail {
		d := regularDetail(id, "A", "EXAMPLE SCIENCE 7")
		d.DescriptionText = desc
		return VerifiedDetail{Kid: detailKid1, Row: listRow(id, "A", "EXAMPLE SCIENCE 7"), Detail: d, Comments: d.Comments}
	}
	snaps := BuildAssignmentDetails(b, []VerifiedDetail{read(7, "first"), read(5, "x"), read(7, "second")}, obsAt, nil)
	validateAll(t, snaps)
	if len(snaps) != 1 || !reflect.DeepEqual(keysOfSnap(snaps[0]), []string{"7", "5"}) {
		t.Fatalf("snapshots %d keys %v", len(snaps), keysOfSnap(snaps[0]))
	}
	if string(fieldsOf(t, snaps[0], "7")["description"]) != `"first"` {
		t.Fatal("a repeated id keeps the first read")
	}
	if !reflect.DeepEqual(skipped, []string{"duplicate_key"}) {
		t.Fatalf("skipped %v", skipped)
	}
	if got := BuildAssignmentDetails(b, nil, obsAt, nil); len(got) != 0 {
		t.Fatalf("no reads: %d snapshots", len(got))
	}
}

// Many long descriptions do not make one snapshot over the journal's 8 MiB
// body limit (refused whole, at every poll): the records are split over
// several complete=false snapshots of the same scope, which is what one
// would have said.
func TestBuildAssignmentDetailsSplitsAtTheSizeLimit(t *testing.T) {
	var cuts int
	long := strings.Repeat("EXAMPLE description text. ", 2600) // about 66 KiB: cut to 64 KiB
	var reads []VerifiedDetail
	for id := int64(1); id <= 200; id++ {
		d := regularDetail(id, "A", "EXAMPLE SCIENCE 7")
		d.DescriptionText = long
		reads = append(reads, VerifiedDetail{Kid: detailKid1, Row: listRow(id, "A", "EXAMPLE SCIENCE 7"), Detail: d, Comments: d.Comments})
	}
	snaps := BuildAssignmentDetails(bind1, reads, obsAt, func() { cuts++ })
	if len(snaps) < 2 {
		t.Fatalf("%d snapshots for about 13 MiB of text", len(snaps))
	}
	validateAll(t, snaps)
	total := 0
	for _, s := range snaps {
		raw, _ := json.Marshal(s)
		if len(raw) > 8<<20 {
			t.Fatalf("snapshot of %d bytes", len(raw))
		}
		if s.Complete || !reflect.DeepEqual(s.Scope, snaps[0].Scope) || !s.ObservedAt.Equal(snaps[0].ObservedAt) {
			t.Fatalf("split snapshot header %+v", s)
		}
		total += len(s.Records)
	}
	if total != 200 || cuts != 200 {
		t.Fatalf("%d records, %d cuts; want 200, 200", total, cuts)
	}
}
