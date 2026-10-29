package journalapi

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// The 4.3 example (with placeholder ids) decodes strictly into Snapshot.
func TestSnapshotDecodesTheSpecExample(t *testing.T) {
	body := []byte(`{
	  "snapshot_id": "0b9f6c8e-6a64-4c49-9f55-6f4cf1b3b0a1",
	  "record_type": "assignment",
	  "schema_version": 1,
	  "scope": [["source","powerschool"],["surface","assignments"],["data_subject","e_child1"],["course","c1"]],
	  "complete": true,
	  "complete_range": null,
	  "observed_at": "2026-10-16T13:00:41Z",
	  "approximate": false,
	  "records": [
	    {"key": "c1-5:98765", "data_subject": "e_child1", "audience": ["subject", "guardians"],
	     "fields": {"title": "Worksheet 3", "due": "2026-10-20", "score": 8.5, "max_score": 10, "missing": false}}
	  ]
	}`)
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var s Snapshot
	if err := dec.Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.Scope[2] != (ScopePart{"data_subject", "e_child1"}) || !s.Complete || s.CompleteRange != nil {
		t.Errorf("scope/complete: %+v", s)
	}
	if !s.ObservedAt.Equal(time.Date(2026, 10, 16, 13, 0, 41, 0, time.UTC)) {
		t.Errorf("observed_at %v", s.ObservedAt)
	}
	if string(s.Records[0].Fields["score"]) != "8.5" {
		t.Errorf("fields keep raw JSON: %s", s.Records[0].Fields["score"])
	}
}

func TestResponseShape(t *testing.T) {
	b, _ := json.Marshal(Response{Status: StatusApplied, Stats: &Stats{Created: 2}})
	want := `{"status":"applied","stats":{"created":2,"updated":0,"removed":0,"restored":0,"unchanged":0,"stale":0}}`
	if string(b) != want {
		t.Errorf("got %s", b)
	}
	b, _ = json.Marshal(Response{Status: StatusRejected, Reason: ReasonUnknownSubject})
	if string(b) != `{"status":"rejected","reason":"unknown_subject"}` {
		t.Errorf("got %s", b)
	}
}

func TestHeldTarget(t *testing.T) {
	if got := HeldPath("abc", ActionConfirm); got != "/v1/admin/held/abc:confirm" {
		t.Errorf("HeldPath = %s", got)
	}
}

func TestGoldenJSON(t *testing.T) {
	ts := time.Date(2026, 10, 16, 13, 0, 41, 0, time.UTC)
	prev := ts.Add(-time.Hour)
	yes := true
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	fields := map[string]json.RawMessage{"title": raw(`"A"`)}
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"response full", Response{Status: StatusRejected, Reason: ReasonPolicy, Detail: "d", Stats: &Stats{Created: 1}},
			`{"status":"rejected","reason":"policy","detail":"d","stats":{"created":1,"updated":0,"removed":0,"restored":0,"unchanged":0,"stale":0}}`},
		{"response zero", Response{}, `{"status":""}`},
		{"note response full", NoteResponse{Status: NoteCreated, Key: "k", Seq: 7},
			`{"status":"created","key":"k","seq":7}`},
		{"note response zero", NoteResponse{}, `{"status":"","key":""}`},
		{"state row full", StateRow{RecordType: "assignment", DataSubject: "e_child1", Key: "k", Scope: "s",
			Audience: []string{"subject"}, Fields: fields, Removed: true, FirstSeenAt: ts, LastSeenAt: ts, ConfirmedAt: ts,
			InLatestSnapshot: &yes, Approximate: true},
			`{"record_type":"assignment","data_subject":"e_child1","key":"k","scope":"s","audience":["subject"],"fields":{"title":"A"},"removed":true,"first_seen_at":"2026-10-16T13:00:41Z","last_seen_at":"2026-10-16T13:00:41Z","confirmed_at":"2026-10-16T13:00:41Z","in_latest_snapshot":true,"approximate":true}`},
		{"state row zero", StateRow{},
			`{"record_type":"","data_subject":"","key":"","scope":"","audience":null,"fields":null,"removed":false,"first_seen_at":"0001-01-01T00:00:00Z","last_seen_at":"0001-01-01T00:00:00Z","confirmed_at":"0001-01-01T00:00:00Z","approximate":false}`},
		{"state response full", StateResponse{Rows: []StateRow{}, RosterSHA256: "ab"}, `{"rows":[],"roster_sha256":"ab"}`},
		{"state response zero", StateResponse{}, `{"rows":null,"roster_sha256":""}`},
		{"event full", Event{Seq: 3, RecordType: "assignment", DataSubject: "e_child1", Key: "k", Kind: KindUpdated,
			SchemaVersion: 1, Scope: "s", PrevScope: "p", Audience: []string{"guardians"}, Fields: fields,
			Changed:    map[string]Change{"title": {Old: raw(`"A"`), New: raw(`"B"`)}},
			LastFields: fields, ObservedAt: ts, PrevObservedAt: &prev, Approximate: true, Actor: "sa",
			Importance: ImportanceHigh, Rule: "r", GroupKey: "g",
			Notify: []Notice{{Token: "guardians", Mode: ModePrompt, Purpose: "x"}}},
			`{"seq":3,"record_type":"assignment","data_subject":"e_child1","key":"k","kind":"updated","schema_version":1,"scope":"s","prev_scope":"p","audience":["guardians"],"fields":{"title":"A"},"changed":{"title":{"old":"A","new":"B"}},"last_fields":{"title":"A"},"observed_at":"2026-10-16T13:00:41Z","prev_observed_at":"2026-10-16T12:00:41Z","approximate":true,"actor":"sa","importance":"high","rule":"r","group_key":"g","notify":[{"token":"guardians","mode":"prompt","purpose":"x"}]}`},
		{"event zero", Event{},
			`{"seq":0,"record_type":"","data_subject":"","key":"","kind":"","schema_version":0,"scope":"","audience":null,"observed_at":"0001-01-01T00:00:00Z","approximate":false,"actor":"","importance":""}`},
		{"events response full", EventsResponse{Events: []Event{}, Next: 9}, `{"events":[],"next":9}`},
		{"events response zero", EventsResponse{}, `{"events":null,"next":0}`},
		{"count full", Count{DataSubject: "e_child1", RecordType: "assignment", Live: 2, Events: map[string]int{ImportanceHigh: 1}},
			`{"data_subject":"e_child1","record_type":"assignment","live":2,"events":{"high":1}}`},
		{"count zero", Count{}, `{"data_subject":"","record_type":"","live":0,"events":null}`},
		{"counts response full", CountsResponse{Since: ts, Counts: []Count{}}, `{"since":"2026-10-16T13:00:41Z","counts":[]}`},
		{"counts response zero", CountsResponse{}, `{"since":"0001-01-01T00:00:00Z","counts":null}`},
		{"healthz full", Healthz{Status: HealthOK, RosterSHA256: "ab", SchemaVersion: 2},
			`{"status":"ok","roster_sha256":"ab","schema_version":2}`},
		{"healthz zero", Healthz{}, `{"status":"","roster_sha256":"","schema_version":0}`},
		{"erase full", EraseResponse{DataSubject: "e_child1", ErasedAt: ts, Snapshots: 1, Current: 2, Events: 3,
			NotesDeletedAsAuthor: 4, NotesPseudonymised: 5, SubjectUnknown: true},
			`{"data_subject":"e_child1","erased_at":"2026-10-16T13:00:41Z","snapshots":1,"record_current":2,"record_events":3,` +
				`"notes_deleted_as_author":4,"notes_pseudonymised":5,"subject_unknown":true}`},
		{"erase zero", EraseResponse{},
			`{"data_subject":"","erased_at":"0001-01-01T00:00:00Z","snapshots":0,"record_current":0,"record_events":0,` +
				`"notes_deleted_as_author":0,"notes_pseudonymised":0,"subject_unknown":false}`},
		{"rescan full", RescanResult{CurrentRows: 1, EventRows: 2, Released: 3, Withheld: 4},
			`{"current_rows":1,"event_rows":2,"released":3,"withheld":4}`},
		{"rescan zero", RescanResult{}, `{"current_rows":0,"event_rows":0,"released":0,"withheld":0}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
	}
}
