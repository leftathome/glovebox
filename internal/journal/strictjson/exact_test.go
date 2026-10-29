package strictjson

import "testing"

func TestDecodeFieldNamesAreExact(t *testing.T) {
	type note struct {
		Token string `json:"token"`
	}
	type in struct {
		Admin bool              `json:"admin"`
		Notes []note            `json:"notes"`
		Free  map[string]string `json:"free"`
	}
	for name, js := range map[string]string{
		"capitalised":  `{"Admin":true}`,
		"long s fold":  `{"admin":true,"ſnotes":[]}`,
		"nested exact": `{"notes":[{"Token":"x"}]}`,
		"upper":        `{"ADMIN":true}`,
	} {
		var v in
		if err := Decode([]byte(js), &v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var v in
	if err := Decode([]byte(`{"admin":true,"notes":[{"token":"x"}],"free":{"Any":"y"}}`), &v); err != nil || !v.Admin {
		t.Errorf("exact names refused: %v", err)
	}
}
