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
