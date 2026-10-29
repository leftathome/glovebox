package diff

import (
	"encoding/json"
	"testing"
)

func TestFoldReplaysEventsAndRenames(t *testing.T) {
	id := Identity{RecordType: "assignment", DataSubject: "e_child1", Key: "k"}
	rename := func(rt string, v int, field string) string {
		if v == 1 && field == "title" {
			return "name"
		}
		return field
	}
	events := []FoldEvent{
		{ID: id, Kind: Created, SchemaVersion: 1, Scope: scopeA, Audience: []string{"subject"}, Fields: f("title", `"A"`, "score", `1`)},
		{ID: id, Kind: Updated, SchemaVersion: 1, Scope: scopeB, Audience: []string{"guardians"},
			Changed: map[string]Change{"title": {Old: json.RawMessage(`"A"`), New: json.RawMessage(`"B"`)}, "score": {Old: json.RawMessage(`1`)}}},
		{ID: id, Kind: Removed, SchemaVersion: 2, Scope: scopeB, Audience: []string{"guardians"}},
	}
	got := Fold(events, rename)[id]
	if !got.Removed || got.Scope != scopeB || got.Audience[0] != "guardians" || string(got.Fields["name"]) != `"B"` || got.Fields["score"] != nil || got.Fields["title"] != nil {
		t.Errorf("fold = %+v", got)
	}
	events = append(events, FoldEvent{ID: id, Kind: Restored, SchemaVersion: 2, Scope: scopeA, Audience: []string{"subject"}, Fields: f("name", `"C"`)})
	got = Fold(events, rename)[id]
	if got.Removed || got.Scope != scopeA || string(got.Fields["name"]) != `"C"` || len(got.Fields) != 1 {
		t.Errorf("after restore = %+v", got)
	}
}

func TestFoldRenamedValueBeatsUndeclaredKey(t *testing.T) {
	id := Identity{RecordType: "assignment", DataSubject: "e_child1", Key: "k"}
	rename := func(_ string, _ int, field string) string {
		if field == "title" {
			return "name"
		}
		return field
	}
	ev := []FoldEvent{{ID: id, Kind: Created, SchemaVersion: 1, Scope: scopeA, Fields: f("title", `"new"`, "name", `"stray"`)}}
	for i := 0; i < 50; i++ {
		if got := Fold(ev, rename)[id]; string(got.Fields["name"]) != `"new"` {
			t.Fatalf("name = %s", got.Fields["name"])
		}
	}
}
