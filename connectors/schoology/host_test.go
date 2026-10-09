package schoology

import "testing"

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"example.schoology.com":                 "example.schoology.com",
		"https://example.schoology.com":         "example.schoology.com",
		"https://example.schoology.com/":        "example.schoology.com",
		"http://example.schoology.com/home?x=1": "example.schoology.com",
		"  HTTPS://Example.Schoology.com/ \n":   "example.schoology.com",
		"example.schoology.com:8443/login":      "example.schoology.com:8443",
		"":                                      "",
		"https://":                              "",
	}
	for in, want := range cases {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
