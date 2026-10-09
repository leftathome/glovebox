package powerschool

import "testing"

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in       string
		wantBase string
		wantHost string
		wantErr  bool
	}{
		{in: "ps.example.org", wantBase: "https://ps.example.org", wantHost: "ps.example.org"},
		{in: "https://ps.example.org", wantBase: "https://ps.example.org", wantHost: "ps.example.org"},
		{in: "https://ps.example.org/", wantBase: "https://ps.example.org", wantHost: "ps.example.org"},
		{in: "  https://PS.Example.ORG/public/home.html?x=1#top \n", wantBase: "https://ps.example.org", wantHost: "ps.example.org"},
		{in: "http://ps.example.org", wantBase: "https://ps.example.org", wantHost: "ps.example.org"},
		{in: "ps.example.org:8443/guardian", wantBase: "https://ps.example.org:8443", wantHost: "ps.example.org:8443"},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "ftp://ps.example.org", wantErr: true},
		{in: "https://user:pw@ps.example.org", wantErr: true},
		{in: "https://", wantErr: true},
		{in: "https://:443", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			base, host, err := NormalizeBaseURL(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if base != tc.wantBase || host != tc.wantHost {
				t.Errorf("NormalizeBaseURL(%q) = (%q, %q), want (%q, %q)", tc.in, base, host, tc.wantBase, tc.wantHost)
			}
		})
	}
}
