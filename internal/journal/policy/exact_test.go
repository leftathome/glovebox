package policy

import (
	"encoding/json"
	"testing"
)

func TestPolicyFieldNamesAreExact(t *testing.T) {
	for name, js := range map[string]string{
		"capitalised admin":   `{"a/b": {"Admin": true}}`,
		"long s write_scopes": `{"a/b": {"write_types": ["assignment"], "write_subjects": "roster_subjects", "write_ſcopes": {"source": ["powerschool"]}}}`,
	} {
		if _, err := Parse([]byte(js), reg(t)); err == nil {
			t.Errorf("%s: Parse accepted", name)
		}
		var s map[string]Service
		if err := json.Unmarshal([]byte(js), &s); err == nil {
			t.Errorf("%s: json.Unmarshal accepted", name)
		}
	}
}

func TestMayCountNeedsCounts(t *testing.T) {
	p, err := Parse([]byte(`{"a/b": {"read": true, "readers": ["e_parent1"]}}`), reg(t))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := p.Service("a/b")
	if !s.MayRead("e_parent1") {
		t.Fatal("setup: read service cannot read")
	}
	if s.MayCount("e_parent1") {
		t.Error("a service without counts may count")
	}
}
