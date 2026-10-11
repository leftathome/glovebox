package psportal

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayCookiesOverlay(t *testing.T) {
	var seen []string
	responses := [][]string{
		{"MRHSession=new; Path=/", "F5_ST=st; Path=/", "gone=; Max-Age=0; Path=/"},
		{"epoch=x; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/"},
		nil,
	}
	call := 0
	g := NewGatewayCookies(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.Header.Get("Cookie"))
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}
		for _, sc := range responses[call] {
			resp.Header.Add("Set-Cookie", sc)
		}
		call++
		return resp, nil
	}))

	original := "JSESSIONID=keep; MRHSession=old; gone=was-here; epoch=was-here"
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodGet, "https://portal.example.org/guardian/home.html", nil)
		req.Header.Set("Cookie", original)
		if _, err := g.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Cookie") != original {
			t.Fatalf("the caller's request was modified: %q", req.Header.Get("Cookie"))
		}
	}

	if seen[0] != original {
		t.Errorf("first request = %q, want the stored cookies untouched", seen[0])
	}
	second := "; " + seen[1] + ";"
	for _, want := range []string{"; JSESSIONID=keep;", "; MRHSession=new;", "; F5_ST=st;", "; epoch=was-here;"} {
		if !strings.Contains(second, want) {
			t.Errorf("second request %q lacks %q", seen[1], want)
		}
	}
	if strings.Contains(seen[1], "MRHSession=old") || strings.Contains(seen[1], "gone=") {
		t.Errorf("second request %q still carries a replaced or deleted cookie", seen[1])
	}
	if strings.Contains(seen[2], "epoch=") {
		t.Errorf("third request %q still carries a cookie expired at the epoch", seen[2])
	}
}

func TestGatewayCookiesOverlayIsBounded(t *testing.T) {
	g := NewGatewayCookies(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}
		for i := 0; i < 500; i++ {
			resp.Header.Add("Set-Cookie", fmt.Sprintf("c%d=v; Path=/", i))
		}
		return resp, nil
	}))
	req, _ := http.NewRequest(http.MethodGet, "https://portal.example.org/", nil)
	if _, err := g.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if len(g.overlay) != maxOverlayCookies {
		t.Errorf("overlay holds %d cookies, want the cap of %d", len(g.overlay), maxOverlayCookies)
	}
}

func TestIsGatewayPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/my.policy": true, "/my.logout.php3": true, "/my.policy/x": true,
		"/public/": false, "/guardian/home.html": false, "/my.policy2": false, "": false,
	} {
		if got := IsGatewayPath(path); got != want {
			t.Errorf("IsGatewayPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// The floor and its headroom, on an ordinary winter and summer day and
// around both 2026 daylight-saving changes (8 March, 1 November).
func TestFloor(t *testing.T) {
	if LoginHeadroom != 150*time.Second {
		t.Fatalf("this table assumes 150s of headroom, got %s", LoginHeadroom)
	}
	la := FloorLocation()
	for _, day := range [][2]int{{1, 14}, {7, 15}, {3, 7}, {3, 8}, {3, 9}, {10, 31}, {11, 1}, {11, 2}} {
		for _, c := range []struct {
			h, m, s        int
			inside, blocks bool
		}{
			{15, 57, 29, false, false},
			{15, 57, 30, false, true},
			{15, 59, 59, false, true},
			{16, 0, 0, true, true},
			{18, 30, 0, true, true},
			{20, 59, 59, true, true},
			{21, 0, 0, false, false},
			{6, 0, 0, false, false},
			{15, 30, 0, false, false},
			{21, 30, 0, false, false},
		} {
			at := time.Date(2026, time.Month(day[0]), day[1], c.h, c.m, c.s, 0, la).UTC()
			if got := InFloor(at); got != c.inside {
				t.Errorf("%d/%d %02d:%02d:%02d: InFloor = %v, want %v", day[0], day[1], c.h, c.m, c.s, got, c.inside)
			}
			if got := FloorBlocksLoginAt(at); got != c.blocks {
				t.Errorf("%d/%d %02d:%02d:%02d: FloorBlocksLoginAt = %v, want %v", day[0], day[1], c.h, c.m, c.s, got, c.blocks)
			}
		}
	}
	// Hand-computed UTC instants: PST is UTC-8, PDT is UTC-7.
	if !InFloor(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)) { // 16:00 PST on the 14th
		t.Error("00:00 UTC in January is 16:00 PST and must be inside")
	}
	if InFloor(time.Date(2026, 7, 15, 22, 59, 59, 0, time.UTC)) { // 15:59:59 PDT
		t.Error("22:59:59 UTC in July is 15:59:59 PDT and must be outside")
	}
	if !InFloor(time.Date(2026, 7, 15, 23, 0, 0, 0, time.UTC)) { // 16:00 PDT
		t.Error("23:00 UTC in July is 16:00 PDT and must be inside")
	}
}
