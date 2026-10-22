package strictjson

import "testing"

func TestDecodeRefusals(t *testing.T) {
	type in struct {
		A map[string]int `json:"a"`
		B string         `json:"b"`
	}
	for name, js := range map[string]string{
		"duplicate":        `{"b":"x","b":"y"}`,
		"case duplicate":   `{"a":{"k":1},"A":null}`,
		"nested duplicate": `{"a":{"k":1,"K":2}}`,
		"unknown":          `{"c":1}`,
		"trailing":         `{"b":"x"} {}`,
	} {
		var v in
		if err := Decode([]byte(js), &v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var v []in
	if err := Decode([]byte(`[{"b":"x"},{"b":"y","b":"z"}]`), &v); err == nil {
		t.Error("duplicate inside an array element accepted")
	}
	var ok in
	if err := Decode([]byte(`{"a":{"k":1,"l":2},"b":"x"}`), &ok); err != nil || ok.B != "x" {
		t.Errorf("valid document: %v", err)
	}
}
