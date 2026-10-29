package record

import (
	"strings"
	"testing"
)

func mk(name string, v int, fields map[string]FieldType, renames map[string]string) Type {
	f := map[string]FieldDef{}
	for k, ty := range fields {
		f[k] = FieldDef{Type: ty}
	}
	return Type{Name: name, SchemaVersion: v, Owner: "src", Fields: f, Renames: renames}
}

func TestRegistryRenameAndHistoryRefusals(t *testing.T) {
	for name, types := range map[string][]Type{
		"rename onto existing name": {
			mk("x", 1, map[string]FieldType{"a": Text, "b": Text}, nil),
			mk("x", 2, map[string]FieldType{"b": Text}, map[string]string{"a": "b"}),
		},
		"two-to-one rename": {
			mk("x", 1, map[string]FieldType{"a": String, "b": String}, nil),
			mk("x", 2, map[string]FieldType{"c": String}, map[string]string{"a": "c", "b": "c"}),
		},
		"rename onto a name dropped two versions ago": {
			mk("x", 1, map[string]FieldType{"a": String, "b": String}, nil),
			mk("x", 2, map[string]FieldType{"a": String}, nil),
			mk("x", 3, map[string]FieldType{"b": String}, map[string]string{"a": "b"}),
		},
		"swap": {
			mk("x", 1, map[string]FieldType{"a": String, "b": String}, nil),
			mk("x", 2, map[string]FieldType{"a": String, "b": String}, map[string]string{"a": "b", "b": "a"}),
		},
		"dropped name returns as another type": {
			mk("x", 1, map[string]FieldType{"a": String, "n": Text}, nil),
			mk("x", 2, map[string]FieldType{"n": Text}, nil),
			mk("x", 3, map[string]FieldType{"n": Text, "a": Date}, nil),
		},
	} {
		if _, err := NewRegistry(types); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAudienceLimits(t *testing.T) {
	ty := mk("x", 1, map[string]FieldType{"a": String}, nil)
	ty.DefaultAudience = []string{"subject", "subject"}
	if ty.Validate() == nil {
		t.Error("duplicate token accepted")
	}
	ty.DefaultAudience = nil
	for i := 0; i <= MaxAudienceTokens; i++ {
		ty.DefaultAudience = append(ty.DefaultAudience, "t"+strings.Repeat("a", i))
	}
	if ty.Validate() == nil {
		t.Error("too many tokens accepted")
	}
}

func TestRegistryIsImmutable(t *testing.T) {
	in := assignmentV2()
	in1 := assignmentV1
	in1.DefaultAudience = []string{"subject", "guardians"}
	r, err := NewRegistry([]Type{in1, in})
	if err != nil {
		t.Fatal(err)
	}
	in.Fields["score"] = FieldDef{Type: Text}
	in1.DefaultAudience[0] = "x"
	a, _ := r.Active("assignment")
	a.Fields["score"] = FieldDef{Type: Text}
	a.Renames["title"] = "zzz"
	for _, ty := range r.Types() {
		ty.Fields["due"] = FieldDef{Type: Text}
	}
	v, _ := r.Version("assignment", 1)
	v.DefaultAudience[0] = "x"
	b, _ := r.Active("assignment")
	if b.Fields["score"].Type != Number || b.Fields["due"].Type != Date || b.Renames["title"] != "name" {
		t.Errorf("registry changed through a copy: %+v", b)
	}
	if v1, _ := r.Version("assignment", 1); v1.DefaultAudience[0] != "subject" {
		t.Error("audience shared")
	}
}

func TestForwardUndeclaredCollisionIsDeterministic(t *testing.T) {
	r, err := NewRegistry([]Type{
		mk("x", 1, map[string]FieldType{"a": String}, nil),
		mk("x", 2, map[string]FieldType{"b": String}, map[string]string{"a": "b"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		f := r.Forward("x", 1, Fields{"a": raw(`"A"`), "b": raw(`"undeclared"`), "z": raw(`1`)})
		if string(f["b"]) != `"A"` || string(f["z"]) != "1" || len(f) != 2 {
			t.Fatalf("Forward = %v", f)
		}
	}
}

func TestForwardNameOutOfRange(t *testing.T) {
	r, err := NewRegistry([]Type{assignmentV1, assignmentV2()})
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []int{-3, 0, 2, 99} {
		got := r.ForwardName("assignment", from, "title")
		want := "title"
		if from <= 0 {
			want = "name"
		}
		if got != want {
			t.Errorf("from %d: %s, want %s", from, got, want)
		}
	}
	if got := r.ForwardName("nosuch", 1, "title"); got != "title" {
		t.Errorf("unknown type: %s", got)
	}
}
