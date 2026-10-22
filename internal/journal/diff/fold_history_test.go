package diff

import (
	"encoding/json"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// chainRegistry has title -> name at v2 and name -> label at v3.
func chainRegistry(t testing.TB) *record.Registry {
	t.Helper()
	mk := func(v int, renames map[string]string, names ...string) record.Type {
		fields := map[string]record.FieldDef{}
		for _, n := range names {
			fields[n] = record.FieldDef{Type: record.String}
		}
		return record.Type{Name: "t", SchemaVersion: v, Owner: "src", Fields: fields, Renames: renames}
	}
	reg, err := record.NewRegistry([]record.Type{
		mk(1, nil, "title", "date"),
		mk(2, map[string]string{"title": "name"}, "name", "date"),
		mk(3, map[string]string{"name": "label"}, "label", "date"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestFoldRenamesAcrossSchemaVersions(t *testing.T) {
	reg := chainRegistry(t)
	id := Identity{RecordType: "t", DataSubject: "s", Key: "k"}
	cases := []struct {
		name    string
		version int
		field   string
		want    string
	}{
		{"v1 title goes through both renames", 1, "title", "label"},
		{"v2 name goes through the last rename", 2, "name", "label"},
		{"v3 label is already active", 3, "label", "label"},
		{"unrenamed field keeps its name", 1, "date", "date"},
		{"v1 name is an undeclared key, renamed once at v3", 1, "name", "label"},
		{"v2 title is undeclared there and untouched", 2, "title", "title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created := FoldEvent{ID: id, Kind: Created, SchemaVersion: tc.version, Scope: scopeA, Fields: f(tc.field, `"x"`)}
			got := Fold([]FoldEvent{created}, reg.ForwardName)[id]
			if string(got.Fields[tc.want]) != `"x"` || len(got.Fields) != 1 {
				t.Errorf("created: fields = %v, want only %s", got.Fields, tc.want)
			}
			updated := FoldEvent{ID: id, Kind: Updated, SchemaVersion: tc.version, Scope: scopeA,
				Changed: map[string]Change{tc.field: {New: json.RawMessage(`"y"`)}}}
			got = Fold([]FoldEvent{created, updated}, reg.ForwardName)[id]
			if string(got.Fields[tc.want]) != `"y"` || len(got.Fields) != 1 {
				t.Errorf("updated: fields = %v, want only %s", got.Fields, tc.want)
			}
		})
	}
}

func TestFoldMixedVersionHistory(t *testing.T) {
	reg := chainRegistry(t)
	id := Identity{RecordType: "t", DataSubject: "s", Key: "k"}
	events := []FoldEvent{
		{ID: id, Kind: Created, SchemaVersion: 1, Scope: scopeA, Fields: f("title", `"a"`, "date", `"d"`)},
		{ID: id, Kind: Updated, SchemaVersion: 2, Scope: scopeA, Changed: map[string]Change{"name": {Old: json.RawMessage(`"a"`), New: json.RawMessage(`"b"`)}}},
		{ID: id, Kind: Updated, SchemaVersion: 3, Scope: scopeA, Changed: map[string]Change{"date": {Old: json.RawMessage(`"d"`)}}},
	}
	got := Fold(events, reg.ForwardName)[id]
	if len(got.Fields) != 1 || string(got.Fields["label"]) != `"b"` {
		t.Errorf("fields = %v", got.Fields)
	}
}

func TestFoldDeletionOfRenamedKeyKeepsUndeclaredValue(t *testing.T) {
	reg := chainRegistry(t)
	id := Identity{RecordType: "t", DataSubject: "s", Key: "k"}
	events := []FoldEvent{
		{ID: id, Kind: Created, SchemaVersion: 3, Scope: scopeA, Fields: f("label", `"a"`)},
		// At v1, "title" forwards to label (deleted) while "label" is
		// undeclared there and set: the deletion must not drop the set.
		{ID: id, Kind: Updated, SchemaVersion: 1, Scope: scopeA, Changed: map[string]Change{
			"title": {Old: json.RawMessage(`"a"`)}, "label": {New: json.RawMessage(`"u"`)}}},
	}
	for i := 0; i < 50; i++ {
		if got := Fold(events, reg.ForwardName)[id]; string(got.Fields["label"]) != `"u"` {
			t.Fatalf("fields = %v", got.Fields)
		}
	}
}

// TestFoldImpossibleHistories pins the behaviour Fold's doc comment states.
func TestFoldImpossibleHistories(t *testing.T) {
	same := func(_ string, _ int, field string) string { return field }
	id := Identity{RecordType: "t", DataSubject: "s", Key: "k"}
	created := FoldEvent{ID: id, Kind: Created, SchemaVersion: 1, Scope: scopeA, Audience: []string{"subject"}, Fields: f("a", `"1"`)}
	removed := FoldEvent{ID: id, Kind: Removed, SchemaVersion: 1, Scope: scopeB, Audience: []string{"guardians"}}
	updated := FoldEvent{ID: id, Kind: Updated, SchemaVersion: 1, Scope: scopeB, Audience: []string{"guardians"},
		Changed: map[string]Change{"b": {New: json.RawMessage(`"2"`)}}}
	restored := FoldEvent{ID: id, Kind: Restored, SchemaVersion: 1, Scope: scopeB, Audience: []string{"guardians"}, Fields: f("c", `"3"`)}
	fold := func(es ...FoldEvent) FoldRow { return Fold(es, same)[id] }

	if r := fold(updated); r.Removed || len(r.Fields) != 1 || string(r.Fields["b"]) != `"2"` {
		t.Errorf("update before create = %+v", r)
	}
	if r := fold(removed); !r.Removed || len(r.Fields) != 0 || r.Scope != scopeB {
		t.Errorf("remove before create = %+v", r)
	}
	if r := fold(created, restored); r.Removed || len(r.Fields) != 1 || string(r.Fields["c"]) != `"3"` || r.Scope != scopeB {
		t.Errorf("restore while live = %+v", r)
	}
	if r := fold(created, created); r.Removed || len(r.Fields) != 1 {
		t.Errorf("create twice = %+v", r)
	}
	if r := fold(created, removed, removed); !r.Removed || len(r.Fields) != 1 || string(r.Fields["a"]) != `"1"` || r.Scope != scopeB {
		t.Errorf("remove twice = %+v", r)
	}
	if r := fold(created, removed, updated); r.Removed || len(r.Fields) != 2 {
		t.Errorf("update of a removed row = %+v", r)
	}
}

func TestFoldDoesNotShareAudienceWithEvents(t *testing.T) {
	id := Identity{RecordType: "t", DataSubject: "s", Key: "k"}
	aud := []string{"subject", "guardians"}
	same := func(_ string, _ int, field string) string { return field }
	row := Fold([]FoldEvent{{ID: id, Kind: Created, SchemaVersion: 1, Scope: scopeA, Audience: aud, Fields: f("a", `"1"`)}}, same)[id]
	row.Audience[0] = "changed"
	if aud[0] != "subject" {
		t.Error("row aliases the event's audience")
	}
}
