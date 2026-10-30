package schoology

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

// Record tests use their own subjects (e_child1, e_child2) so that nothing
// here depends on the item fixtures' ids. All names, courses and text are
// invented.

// recordRules routes k1 and k2 by exact rules on both record surfaces, with
// subjects e_child1 / e_child2 and audience [subject, guardians].
func recordRules() []connector.Rule {
	aud := []string{"subject", "guardians"}
	return []connector.Rule{
		{Match: "schoology:k1:assignment", Destination: "school", DataSubject: "e_child1", Audience: aud},
		{Match: "schoology:k1:assignment_detail", Destination: "school", DataSubject: "e_child1", Audience: aud},
		{Match: "schoology:k2:assignment", Destination: "school", DataSubject: "e_child2", Audience: aud},
		{Match: "schoology:k2:assignment_detail", Destination: "school", DataSubject: "e_child2", Audience: aud},
		{Match: "schoology-parse-failure:*", Destination: "school", Audience: []string{"guardians"}},
	}
}

// recordsBlock is an enabled records block that passes validation.
func recordsBlock(url string) records.Config {
	return records.Config{Enabled: true, JournalURL: url, TokenFile: "/var/run/secrets/glovebox-journal/token"}
}

// recordsConfig is a two-kid config (k1, k2) with the detail surface on for
// both, exact rules for both record surfaces and records enabled, through
// ApplyDefaults (not ValidateConfig).
func recordsConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		Kids:             []Kid{detailKid1, detailKid2},
		PollSchedule:     PollSchedule{Windows: []PollWindow{{Start: "08:00", End: "09:00"}}},
		AssignmentDetail: AssignmentDetailConfig{Kids: []string{"k1", "k2"}},
		Records:          recordsBlock("http://journal.example:9095"),
	}
	cfg.Rules = recordRules()
	ApplyDefaults(&cfg)
	return cfg
}

// obsAt is the observed_at of every builder test.
var obsAt = time.Date(2026, 10, 12, 15, 30, 0, 0, time.UTC)

// bind1 is k1's binding.
var bind1 = Binding{DataSubject: "e_child1", Audience: []string{"subject", "guardians"}}

func recordsRegistry(t *testing.T) *record.Registry {
	t.Helper()
	reg, err := record.NewRegistry(RecordTypes())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// keyLimits are the journal's strict-decoding caps for this registry
// (httpapi.snapshotKeyLimits).
func keyLimits(reg *record.Registry) strictjson.Limits {
	widest := 9
	for _, ty := range reg.Types() {
		widest = max(widest, len(ty.Fields))
	}
	return strictjson.Limits{MaxObjectKeys: widest + 16, MaxTotalKeys: 9 + 3 + record.MaxRecords*(4+widest) + 1024}
}

// validateAll holds every snapshot to what the journal checks: the
// database-free validation, the strict decoding of its JSON, and every
// record carrying exactly its type's field set.
func validateAll(t *testing.T, snaps []journalapi.Snapshot) {
	t.Helper()
	reg := recordsRegistry(t)
	for i, s := range snaps {
		if _, rej := record.ValidateSnapshot(reg, s, s.ObservedAt.Add(time.Minute)); rej != nil {
			t.Fatalf("snapshot %d (%s): %d %s: %s", i, s.RecordType, rej.Status, rej.Reason, rej.Detail)
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("snapshot %d: marshal: %v", i, err)
		}
		var back journalapi.Snapshot
		if err := strictjson.DecodeLimits(b, &back, keyLimits(reg)); err != nil {
			t.Fatalf("snapshot %d (%s): strict decode: %v", i, s.RecordType, err)
		}
		ty, _ := reg.Active(s.RecordType)
		for _, rec := range s.Records {
			wantFields(t, ty, rec)
		}
	}
}

// wantFields: the record's field names are exactly the type's.
func wantFields(t *testing.T, ty record.Type, rec journalapi.Record) {
	t.Helper()
	var got, want []string
	for k := range rec.Fields {
		got = append(got, k)
	}
	for k := range ty.Fields {
		want = append(want, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s record %q fields %v, want %v", ty.Name, rec.Key, got, want)
	}
}

// fieldsOf returns the fields of the record with key in snap.
func fieldsOf(t *testing.T, snap journalapi.Snapshot, key string) map[string]json.RawMessage {
	t.Helper()
	for _, r := range snap.Records {
		if r.Key == key {
			return r.Fields
		}
	}
	t.Fatalf("no record %q in %s snapshot (keys %v)", key, snap.RecordType, keysOfSnap(snap))
	return nil
}

func keysOfSnap(snap journalapi.Snapshot) []string {
	out := []string{}
	for _, r := range snap.Records {
		out = append(out, r.Key)
	}
	return out
}

// wantJSON compares the named fields with their JSON text.
func wantJSON(t *testing.T, f map[string]json.RawMessage, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got := string(f[k]); got != v {
			t.Errorf("field %s = %s, want %s", k, got, v)
		}
	}
}

// wantScope compares a snapshot's scope with (schoology, surface, subject).
func wantScope(t *testing.T, s journalapi.Snapshot, surface, subject string) {
	t.Helper()
	want := []journalapi.ScopePart{{"source", "schoology"}, {"surface", surface}, {"data_subject", subject}}
	if !reflect.DeepEqual(s.Scope, want) {
		t.Fatalf("scope %v, want %v", s.Scope, want)
	}
}
