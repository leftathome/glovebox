package powerschool

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

const (
	switchURL  = "https://" + pinnedHost + SwitchStudentPath
	formType   = "application/x-www-form-urlencoded"
	switchBody = "selected_student_id=111112"
)

// switchTrip sends one student-switch-shaped request through a Transport
// pinned to pinnedHost with the fixture kids configured, and returns what
// was forwarded.
func switchTrip(t *testing.T, tr *Transport, method, rawURL, contentType, body string, mutate func(*http.Request)) (forwardedBody string, forwarded bool, err error) {
	t.Helper()
	var reader io.Reader
	if body != "\x00nobody" {
		reader = strings.NewReader(body)
	}
	req, reqErr := http.NewRequest(method, rawURL, reader)
	if reqErr != nil {
		t.Fatalf("NewRequest(%s %s): %v", method, rawURL, reqErr)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if mutate != nil {
		mutate(req)
	}
	base := &countingBase{}
	tr.Base = base
	if tr.Host == "" {
		tr.Host = pinnedHost
	}
	resp, err := tr.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	n := len(base.forwarded())
	if (err == nil) != (n == 1) {
		t.Fatalf("%s %s: err=%v but %d request(s) forwarded", method, rawURL, err, n)
	}
	if n == 1 {
		return base.bodies[0], true, nil
	}
	return "", false, err
}

func kidsTransport() *Transport {
	return &Transport{SwitchStudentIDs: []string{"111111", "111112"}}
}

// TestTransport_StudentSwitchIsForwarded: the second approved POST -- the
// portal's switcher form, for a configured student -- goes through, with
// the assignments exception on or off, and the bytes forwarded are the
// bytes that were checked.
func TestTransport_StudentSwitchIsForwarded(t *testing.T) {
	for _, allowLookup := range []bool{false, true} {
		for _, id := range []string{"111111", "111112"} {
			tr := kidsTransport()
			tr.AllowAssignmentLookup = allowLookup
			body := "selected_student_id=" + id
			got, ok, err := switchTrip(t, tr, http.MethodPost, switchURL, formType, body, nil)
			if !ok {
				t.Fatalf("allowLookup=%v: switch to %s refused: %v", allowLookup, id, err)
			}
			if got != body {
				t.Errorf("forwarded body %q, want %q", got, body)
			}
		}
	}
}

// TestTransport_StudentSwitchBodyRejections: the body must be exactly the
// one field with a configured id. Everything else is refused.
func TestTransport_StudentSwitchBodyRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"no body at all", "\x00nobody"},
		{"field without a value", "selected_student_id="},
		{"field name only", "selected_student_id"},
		{"a student that is not configured", "selected_student_id=222222"},
		{"a configured id with a digit appended", "selected_student_id=1111121"},
		{"a configured id with a leading zero", "selected_student_id=0111112"},
		{"non-numeric id", "selected_student_id=abc"},
		{"negative id", "selected_student_id=-111112"},
		{"another field", "student_id=111112"},
		{"another field with a configured id", "account=111112"},
		{"an extra field after", "selected_student_id=111112&action=delete"},
		{"an extra field before", "action=delete&selected_student_id=111112"},
		{"an extra empty field", "selected_student_id=111112&"},
		{"the field repeated", "selected_student_id=111112&selected_student_id=111111"},
		{"the field repeated with an unconfigured id", "selected_student_id=111112&selected_student_id=222222"},
		{"credentials alongside", "selected_student_id=111112&account=x&pw=y"},
		{"percent-encoded field name", "selected%5Fstudent%5Fid=111112"},
		{"percent-encoded value", "selected_student_id=%31%31%31%31%31%32"},
		{"percent-encoded separator in the value", "selected_student_id=111112%26action%3Ddelete"},
		{"plus in the value", "selected_student_id=111112+1"},
		{"semicolon separator", "selected_student_id=111112;action=delete"},
		{"leading whitespace", " selected_student_id=111112"},
		{"trailing newline", "selected_student_id=111112\n"},
		{"trailing CRLF and a second line", "selected_student_id=111112\r\naction=delete"},
		{"upper-case field name", "SELECTED_STUDENT_ID=111112"},
		{"over-long id", "selected_student_id=" + strings.Repeat("1", 21)},
		{"over-long body", "selected_student_id=111112&pad=" + strings.Repeat("x", 4096)},
		{"JSON", `{"selected_student_id":"111112"}`},
		{"multipart", "--b\r\nContent-Disposition: form-data; name=\"selected_student_id\"\r\n\r\n111112\r\n--b--\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok, err := switchTrip(t, kidsTransport(), http.MethodPost, switchURL, formType, tc.body, nil)
			if ok || !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("body %q: forwarded=%v err=%v, want ErrWriteBlocked", tc.body, ok, err)
			}
		})
	}
}

// TestTransport_StudentSwitchRequestRejections: a correct body does not
// help a request that is wrong anywhere else.
func TestTransport_StudentSwitchRequestRejections(t *testing.T) {
	base := "https://" + pinnedHost

	t.Run("content types", func(t *testing.T) {
		for _, ct := range []string{
			"", "application/json", "text/plain", "multipart/form-data; boundary=b",
			"application/x-www-form-urlencoded; charset=UTF-8", "APPLICATION/X-WWW-FORM-URLENCODED",
		} {
			_, ok, err := switchTrip(t, kidsTransport(), http.MethodPost, switchURL, ct, switchBody, nil)
			if ok || !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("Content-Type %q: forwarded=%v err=%v, want ErrWriteBlocked", ct, ok, err)
			}
		}
		_, ok, err := switchTrip(t, kidsTransport(), http.MethodPost, switchURL, formType, switchBody,
			func(r *http.Request) { r.Header.Add("Content-Type", "application/json") })
		if ok || !errors.Is(err, connector.ErrWriteBlocked) {
			t.Errorf("two Content-Type headers: forwarded=%v err=%v", ok, err)
		}
	})

	t.Run("paths", func(t *testing.T) {
		for _, u := range []string{
			base + "/guardian/home.html/",
			base + "/guardian/home.html?selected_student_id=111111",
			base + "/guardian/home.html?",
			base + "/guardian/home.html#x",
			base + "/guardian/home%2Ehtml",
			base + "/guardian%2Fhome.html",
			base + "//guardian/home.html",
			base + "/guardian//home.html",
			base + "/guardian/./home.html",
			base + "/guardian/x/../home.html",
			base + "/GUARDIAN/HOME.HTML",
			base + "/guardian/Home.html",
			base + "/guardian/home.html;jsessionid=x",
			base + "/guardian/home.htm",
			base + "/guardian/home.html.bak",
			base + "/home.html",
			base + "/public/home.html",
			base + "/guardian/changepassword.html",
			base + "/guardian/forms.html",
			base + "/guardian/scores.html",
			base + "/guardian/attendance.html",
			base + "/ws/xte/assignment/lookup",
			base + "/",
		} {
			tr := kidsTransport() // lookup exception off
			_, ok, err := switchTrip(t, tr, http.MethodPost, u, formType, switchBody, nil)
			if ok || !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("POST %s: forwarded=%v err=%v, want ErrWriteBlocked", u, ok, err)
			}
		}
		_, ok, err := switchTrip(t, kidsTransport(), http.MethodPost, switchURL, formType, switchBody,
			func(r *http.Request) { r.URL.RawPath = "/guardian/home%2Ehtml" })
		if ok || !errors.Is(err, connector.ErrWriteBlocked) {
			t.Errorf("RawPath set by hand: forwarded=%v err=%v", ok, err)
		}
	})

	t.Run("methods", func(t *testing.T) {
		for _, m := range []string{
			http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace,
			http.MethodConnect, "PROPFIND", "post", "Post", "POSTX",
		} {
			_, ok, err := switchTrip(t, kidsTransport(), m, switchURL, formType, switchBody, nil)
			if ok || !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("%s: forwarded=%v err=%v, want ErrWriteBlocked", m, ok, err)
			}
		}
	})

	t.Run("origin", func(t *testing.T) {
		for _, u := range []string{
			"https://other.example.org" + SwitchStudentPath,
			"https://evil-" + pinnedHost + SwitchStudentPath,
			"https://" + pinnedHost + ".evil.example" + SwitchStudentPath,
			"https://" + pinnedHost + ":8443" + SwitchStudentPath,
			"https://user:pw@" + pinnedHost + SwitchStudentPath,
			"http://" + pinnedHost + SwitchStudentPath,
		} {
			_, ok, err := switchTrip(t, kidsTransport(), http.MethodPost, u, formType, switchBody, nil)
			if ok || !errors.Is(err, ErrHostBlocked) {
				t.Errorf("POST %s: forwarded=%v err=%v, want ErrHostBlocked", u, ok, err)
			}
		}
		_, ok, err := switchTrip(t, kidsTransport(), http.MethodPost, switchURL, formType, switchBody,
			func(r *http.Request) { r.Host = "other.example.org" })
		if ok || !errors.Is(err, ErrHostBlocked) {
			t.Errorf("Host header override: forwarded=%v err=%v", ok, err)
		}
	})

	t.Run("method-override header", func(t *testing.T) {
		_, ok, err := switchTrip(t, kidsTransport(), http.MethodPost, switchURL, formType, switchBody,
			func(r *http.Request) { r.Header.Set("X-HTTP-Method-Override", "DELETE") })
		if ok || !errors.Is(err, connector.ErrWriteBlocked) {
			t.Errorf("forwarded=%v err=%v, want ErrWriteBlocked", ok, err)
		}
	})

	t.Run("body length that is not known or not true", func(t *testing.T) {
		for name, mutate := range map[string]func(*http.Request){
			"unknown length":          func(r *http.Request) { r.ContentLength = -1 },
			"zero length":             func(r *http.Request) { r.ContentLength = 0 },
			"declared shorter":        func(r *http.Request) { r.ContentLength = int64(len(switchBody) - 1) },
			"declared longer":         func(r *http.Request) { r.ContentLength = int64(len(switchBody) + 1) },
			"chunked":                 func(r *http.Request) { r.TransferEncoding = []string{"chunked"} },
			"trailer":                 func(r *http.Request) { r.Trailer = http.Header{"X-T": nil} },
			"body larger than stated": func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(switchBody + "&action=delete")) },
		} {
			_, ok, err := switchTrip(t, kidsTransport(), http.MethodPost, switchURL, formType, switchBody, mutate)
			if ok || !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("%s: forwarded=%v err=%v, want ErrWriteBlocked", name, ok, err)
			}
		}
	})

	t.Run("no kids configured: the exception is off", func(t *testing.T) {
		for _, ids := range [][]string{nil, {}, {""}} {
			tr := &Transport{SwitchStudentIDs: ids, AllowAssignmentLookup: true}
			for _, body := range []string{switchBody, "selected_student_id="} {
				_, ok, err := switchTrip(t, tr, http.MethodPost, switchURL, formType, body, nil)
				if ok || !errors.Is(err, connector.ErrWriteBlocked) {
					t.Errorf("ids=%q body=%q: forwarded=%v err=%v, want ErrWriteBlocked", ids, body, ok, err)
				}
			}
		}
	})

	t.Run("the two exceptions do not combine", func(t *testing.T) {
		tr := kidsTransport()
		tr.AllowAssignmentLookup = true
		// The switch body at the lookup path is just a lookup POST (allowed
		// as such); the lookup's JSON at the switch path is refused.
		_, ok, err := switchTrip(t, tr, http.MethodPost, switchURL, "application/json;charset=UTF-8", `{"section_ids":[1]}`, nil)
		if ok || !errors.Is(err, connector.ErrWriteBlocked) {
			t.Errorf("lookup payload at the switch path: forwarded=%v err=%v", ok, err)
		}
	})
}

// TestTransport_RedirectOfTheSwitch: the form post is normally answered
// with a 302, which the client follows with a GET. A 307/308, which would
// replay the form at a location the server chose, is not followed.
func TestTransport_RedirectOfTheSwitch(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rs := newRecordingServer(t, "ok")
			rs.handler = func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == SwitchStudentPath {
					http.Redirect(w, r, "/guardian/changepassword.html", status)
					return
				}
				_, _ = w.Write([]byte("ok"))
			}
			resp, err := rs.client(t, false).Post(rs.URL+SwitchStudentPath, formType, strings.NewReader(switchBody))
			if resp != nil {
				resp.Body.Close()
			}
			for _, r := range rs.requests() {
				if strings.HasPrefix(r, "POST ") && r != "POST "+SwitchStudentPath {
					t.Errorf("the form was replayed at another path: %s", r)
				}
			}
			replays := status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect
			if replays && !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("err = %v, want ErrWriteBlocked", err)
			}
			if !replays && err != nil {
				t.Errorf("redirect followed with a GET was refused: %v", err)
			}
		})
	}
}
