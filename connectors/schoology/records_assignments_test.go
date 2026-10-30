package schoology

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	schoologylib "github.com/leftathome/schoology-go"
)

// listingRows are two listed assignments in EXAMPLE courses.
func listingRows() []*schoologylib.Assignment {
	a := sampleAssignment(9100000001, "EXAMPLE Reading Log", "EXAMPLE ENGLISH 7")
	a.Status = schoologylib.AssignmentStatusUpcoming
	a.URL = "/assignment/9100000001"
	b := sampleAssignment(9100000002, "EXAMPLE Problem Set", "EXAMPLE SCIENCE 7")
	b.URL = "/assignment/9100000002"
	return []*schoologylib.Assignment{a, b}
}

func TestAssignmentListingFieldsAndKeys(t *testing.T) {
	var skipped []string
	b := bind1
	b.OnSkip = func(r string) { skipped = append(skipped, r) }
	rows := listingRows()
	noDue := sampleAssignment(9100000003, "EXAMPLE Ungraded Survey", "")
	noDue.DueAt, noDue.URL, noDue.Status = time.Time{}, "", ""
	rows = append(rows, nil, noDue, &schoologylib.Assignment{Title: "EXAMPLE no id"},
		sampleAssignment(9100000001, "EXAMPLE repeated", "EXAMPLE ENGLISH 7"))

	s := BuildAssignmentListing(b, rows, obsAt)
	validateAll(t, []journalapi.Snapshot{s})
	if s.RecordType != TypeLMSAssignment || s.SchemaVersion != 1 || s.Complete || s.CompleteRange != nil {
		t.Fatalf("snapshot header %+v", s)
	}
	if !s.ObservedAt.Equal(obsAt) {
		t.Fatalf("observed_at %v", s.ObservedAt)
	}
	wantScope(t, s, "assignments", "e_child1")
	if got := keysOfSnap(s); !reflect.DeepEqual(got, []string{"9100000001", "9100000002", "9100000003"}) {
		t.Fatalf("keys %v", got)
	}
	wantJSON(t, fieldsOf(t, s, "9100000001"), map[string]string{
		"title": `"EXAMPLE Reading Log"`, "course": `"EXAMPLE ENGLISH 7"`, "status": `"upcoming"`,
		"link": `"/assignment/9100000001"`, "due": `"2026-05-30T23:59:00Z"`,
	})
	wantJSON(t, fieldsOf(t, s, "9100000002"), map[string]string{"status": `"overdue"`, "title": `"EXAMPLE Problem Set"`})
	wantJSON(t, fieldsOf(t, s, "9100000003"), map[string]string{
		"course": "null", "status": "null", "link": "null", "due": "null",
	})
	for _, r := range s.Records {
		if r.DataSubject != "e_child1" || !reflect.DeepEqual(r.Audience, []string{"subject", "guardians"}) {
			t.Fatalf("record %s: subject %q audience %v", r.Key, r.DataSubject, r.Audience)
		}
	}
	sort.Strings(skipped)
	if !reflect.DeepEqual(skipped, []string{"duplicate_key", "no_id"}) {
		t.Fatalf("skipped %v, want one duplicate_key and one no_id", skipped)
	}
}

// A listing that answered with nothing is still a (windowed, empty)
// snapshot: it removes nothing and lets the journal see the scope.
func TestAssignmentListingEmpty(t *testing.T) {
	s := BuildAssignmentListing(bind1, nil, obsAt)
	validateAll(t, []journalapi.Snapshot{s})
	if s.Records == nil || len(s.Records) != 0 || s.Complete {
		t.Fatalf("empty listing: %+v", s)
	}
	raw, _ := json.Marshal(s)
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if _, ok := back["records"].([]any); !ok {
		t.Fatalf("records must encode as []: %s", raw)
	}
}

// The gap spec 5.2 describes: a status change on a listed assignment stages
// nothing (the seen set suppresses it) but changes the record, which the
// journal turns into an updated event.
func TestAssignmentListingCarriesAStatusChangeTheItemsMiss(t *testing.T) {
	writer, dir := newTestWriter(t)
	cp := newTestCheckpoint(t)
	kid := detailKid1
	matcher := connector.NewRuleMatcher(recordRules())
	rows := listingRows()
	client := &fakeClient{OverdueSubmissionsFunc: func(context.Context, int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
		return rows, nil, nil
	}}
	n1, _, got1, listed := processAssignments(context.Background(), client, writer, matcher, cp, NewReceiptDedup(), nil, kid, "v0.3.0")
	if n1 != 2 || !listed {
		t.Fatalf("first poll staged %d listed %v", n1, listed)
	}
	first := BuildAssignmentListing(bind1, got1, obsAt)

	changed := listingRows()
	changed[0].Status = schoologylib.AssignmentStatusOverdue
	rows = changed
	n2, _, got2, _ := processAssignments(context.Background(), client, writer, matcher, cp, NewReceiptDedup(), nil, kid, "v0.3.0")
	if n2 != 0 || stagingDirCount(t, dir) != 2 {
		t.Fatalf("second poll staged %d (items %d); the seen set should suppress it", n2, stagingDirCount(t, dir))
	}
	second := BuildAssignmentListing(bind1, got2, obsAt.Add(time.Hour))
	validateAll(t, []journalapi.Snapshot{first, second})

	if !reflect.DeepEqual(keysOfSnap(first), keysOfSnap(second)) {
		t.Fatalf("keys %v then %v", keysOfSnap(first), keysOfSnap(second))
	}
	for i := range first.Records {
		a, b := first.Records[i].Fields, second.Records[i].Fields
		for name := range a {
			same := string(a[name]) == string(b[name])
			if want := !(first.Records[i].Key == "9100000001" && name == "status"); same != want {
				t.Errorf("record %s field %s: %s then %s", first.Records[i].Key, name, a[name], b[name])
			}
		}
	}
	if string(fieldsOf(t, second, "9100000001")["status"]) != `"overdue"` {
		t.Fatal("the second snapshot does not carry the new status")
	}

	// The record keys are the ids the item path's seen set holds.
	seen, err := seenIDs(cp, assignmentSurface, kid.Name)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for id := range seen {
		ids = append(ids, AssignmentRecordKey(id))
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, keysOfSnap(second)) {
		t.Fatalf("seen-set ids %v, record keys %v", ids, keysOfSnap(second))
	}
	if AssignmentRecordKey(9100000001) != strconv.FormatInt(9100000001, 10) {
		t.Fatal("AssignmentRecordKey is not the decimal id")
	}
}
