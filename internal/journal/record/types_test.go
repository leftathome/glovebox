package record

import (
	"strings"
	"testing"
)

func assignmentV2() Type {
	t := assignmentV1
	t.SchemaVersion = 2
	t.Fields = map[string]FieldDef{
		"name": {Type: String, Required: true}, "due": {Type: Date}, "score": {Type: Number},
		"missing": {Type: Bool}, "teacher_comment": {Type: Text},
	}
	t.Renames = map[string]string{"title": "name", "comment": "teacher_comment"}
	return t
}

func assignmentV3() Type {
	t := assignmentV2()
	t.SchemaVersion = 3
	t.Fields = map[string]FieldDef{
		"label": {Type: String, Required: true}, "due": {Type: Date}, "score": {Type: Number},
		"missing": {Type: Bool}, "teacher_comment": {Type: Text},
	}
	t.Renames = map[string]string{"name": "label"}
	return t
}

var noteV1 = Type{
	Name: "attendance_note", SchemaVersion: 1, Owner: OwnerHousehold,
	Fields:        map[string]FieldDef{"date": {Type: Date}, "what_happened": {Type: Text}, "heard_from": {Type: String}},
	FixedAudience: []string{"subject", "guardians"},
}

func TestRegistryActiveVersionAndRenameChain(t *testing.T) {
	r, err := NewRegistry([]Type{assignmentV3(), assignmentV1, assignmentV2(), noteV1})
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := r.Active("assignment"); !ok || a.SchemaVersion != 3 {
		t.Errorf("active = %v, %v", a.SchemaVersion, ok)
	}
	if v1, ok := r.Version("assignment", 1); !ok || v1.Fields["title"].Type != String {
		t.Error("version 1 lookup")
	}
	if got := r.ForwardName("assignment", 1, "title"); got != "label" {
		t.Errorf("title from v1 = %s, want label", got)
	}
	if got := r.ForwardName("assignment", 2, "name"); got != "label" {
		t.Errorf("name from v2 = %s", got)
	}
	if got := r.ForwardName("assignment", 1, "comment"); got != "teacher_comment" {
		t.Errorf("comment from v1 = %s", got)
	}
	if got := r.ForwardName("assignment", 3, "label"); got != "label" {
		t.Errorf("active names stay: %s", got)
	}
	f := r.Forward("assignment", 1, Fields{"title": raw(`"W3"`), "score": raw(`1`)})
	if string(f["label"]) != `"W3"` || f["title"] != nil || string(f["score"]) != "1" {
		t.Errorf("Forward = %v", f)
	}
	if got := len(r.Types()); got != 4 {
		t.Errorf("Types() has %d entries", got)
	}
}

func TestRegistryRefusals(t *testing.T) {
	gap := assignmentV3()
	otherOwner := assignmentV2()
	otherOwner.Owner = "schoology"
	badRename := assignmentV2()
	badRename.Renames = map[string]string{"title": "nosuch"}
	staleSource := assignmentV2()
	staleSource.Renames = map[string]string{"never_was": "name"}
	keepsOld := assignmentV2()
	keepsOld.Fields["title"] = FieldDef{Type: String}
	v1WithRenames := assignmentV1
	v1WithRenames.Renames = map[string]string{"a": "title"}
	noteNoAudience := noteV1
	noteNoAudience.FixedAudience = nil
	noteNoDate := noteV1
	noteNoDate.Fields = map[string]FieldDef{"what_happened": {Type: Text}}
	fixedOnSource := assignmentV1
	fixedOnSource.FixedAudience = []string{"subject"}
	badField := assignmentV1
	badField.Fields = map[string]FieldDef{"Title": {Type: String}}
	badType := assignmentV1
	badType.Fields = map[string]FieldDef{"title": {Type: "blob"}}
	badToken := assignmentV1
	badToken.DefaultAudience = []string{"Guardians"}
	keptNameNewType := assignmentV2()
	keptNameNewType.Fields["due"] = FieldDef{Type: String}
	renameNewType := assignmentV2()
	renameNewType.Fields["teacher_comment"] = FieldDef{Type: String}
	textDropped := assignmentV2()
	delete(textDropped.Fields, "teacher_comment")
	textDropped.Renames = map[string]string{"title": "name"}
	for name, types := range map[string][]Type{
		"version gap":             {assignmentV1, gap},
		"owner changes":           {assignmentV1, otherOwner},
		"rename to undeclared":    {assignmentV1, badRename},
		"rename from nonexistent": {assignmentV1, staleSource},
		"old name still declared": {assignmentV1, keepsOld},
		"renames on version 1":    {v1WithRenames},
		"household w/o audience":  {noteNoAudience},
		"household w/o date":      {noteNoDate},
		"fixed audience on src":   {fixedOnSource},
		"bad field name":          {badField},
		"bad field type":          {badType},
		"bad audience token":      {badToken},
		"duplicate version":       {assignmentV1, assignmentV1},
		"kept name, new type":     {assignmentV1, keptNameNewType},
		"rename changes type":     {assignmentV1, renameNewType},
		"text field dropped":      {assignmentV1, textDropped},
	} {
		if _, err := NewRegistry(types); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTypeCanonicalIgnoresNilVersusEmpty(t *testing.T) {
	a := assignmentV1
	b := assignmentV1
	b.Renames = map[string]string{}
	b.DefaultAudience = []string{"guardians", "subject"}
	if string(a.Canonical()) != string(b.Canonical()) {
		t.Errorf("%s\n%s", a.Canonical(), b.Canonical())
	}
	c := assignmentV1
	c.Fields = map[string]FieldDef{"title": {Type: Text}}
	if string(a.Canonical()) == string(c.Canonical()) {
		t.Error("a changed field type is not a change")
	}
	if !strings.Contains(string(noteV1.Canonical()), `"fixed_audience":["guardians","subject"]`) {
		t.Errorf("canonical note type %s", noteV1.Canonical())
	}
}
