package record

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

var assignmentV1 = Type{
	Name: "assignment", SchemaVersion: 1, Owner: "powerschool",
	Fields: map[string]FieldDef{
		"title": {Type: String, Required: true}, "due": {Type: Date}, "score": {Type: Number},
		"missing": {Type: Bool}, "comment": {Type: Text},
	},
	DefaultAudience: []string{"subject", "guardians"},
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestNormalizeFields(t *testing.T) {
	f, rej := NormalizeFields(assignmentV1, map[string]json.RawMessage{
		"title": raw(`"W3"`), "score": raw(`8.50`), "comment": raw(`null`),
	})
	if rej != nil {
		t.Fatal(rej)
	}
	if len(f) != 2 || string(f["score"]) != "8.5" {
		t.Errorf("fields %v: nulls are absent, values canonical", f)
	}
	if _, rej := NormalizeFields(assignmentV1, map[string]json.RawMessage{"title": raw(`"W3"`), "grade": raw(`"A"`)}); rej == nil || rej.Status != 422 {
		t.Errorf("an undeclared field: %v", rej)
	}
	if _, rej := NormalizeFields(assignmentV1, map[string]json.RawMessage{"score": raw(`1`)}); rej == nil {
		t.Error("a required field was missing and accepted")
	}
	if _, rej := NormalizeFields(assignmentV1, map[string]json.RawMessage{"title": raw(`"W3"`), "due": raw(`"x"`)}); rej == nil || !bytes.Contains([]byte(rej.Detail), []byte("due")) {
		t.Errorf("the refusal names the field: %v", rej)
	}
}

func TestHashIgnoresOrderAndDetectsChange(t *testing.T) {
	a := Fields{"title": raw(`"W3"`), "score": raw(`8.5`)}
	b := Fields{"score": raw(`8.5`), "title": raw(`"W3"`)}
	if !bytes.Equal(a.Hash(), b.Hash()) {
		t.Error("hash depends on map order")
	}
	c := Fields{"title": raw(`"W3"`), "score": raw(`9`)}
	if bytes.Equal(a.Hash(), c.Hash()) {
		t.Error("hash missed a changed value")
	}
	d := Fields{"titles": raw(`"W3"`), "score": raw(`8.5`)}
	if bytes.Equal(a.Hash(), d.Hash()) {
		t.Error("hash missed a renamed field")
	}
}

// Values read back from jsonb come in PostgreSQL's formatting; Reload
// returns them to the canonical form so they compare equal.
func TestReloadRestoresCanonicalForm(t *testing.T) {
	stored := []byte(`{"title": "W3 <b>", "score": 1000000000000000000000, "due": "2026-10-20"}`)
	f, err := Reload(assignmentV1, stored)
	if err != nil {
		t.Fatal(err)
	}
	if string(f["score"]) != "1e+21" || string(f["title"]) != `"W3 <b>"` {
		t.Errorf("reloaded %v", f)
	}
}

func TestFieldsJSONIsSortedAndEmptyIsObject(t *testing.T) {
	if got := string(Fields{"b": raw(`1`), "a": raw(`2`)}.JSON()); got != `{"a":2,"b":1}` {
		t.Errorf("JSON = %s", got)
	}
	if got := string(Fields(nil).JSON()); got != `{}` {
		t.Errorf("empty JSON = %s", got)
	}
}

func TestNormalizeFieldsBoundsUndeclaredName(t *testing.T) {
	long := strings.Repeat("x", 5000)
	_, rej := NormalizeFields(assignmentV1, map[string]json.RawMessage{"title": raw(`"a"`), long: raw(`1`)})
	if rej == nil || len(rej.Detail) > 200 {
		t.Errorf("rejection %v", rej)
	}
}
