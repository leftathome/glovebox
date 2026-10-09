package powerschool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// togglingLib stands in for a powerschool-go client. stuck models a library
// that exposes the page-fetch switch but does not honour it.
type togglingLib struct {
	on    bool
	stuck bool
}

func (l *togglingLib) SetHTTPPageFetch(on bool) {
	if !l.stuck {
		l.on = on
	}
}
func (l *togglingLib) UsesHTTPPageFetch() bool { return l.on }

func TestRequireHTTPPageFetch(t *testing.T) {
	cases := []struct {
		name    string
		lib     any
		wantErr bool
	}{
		{"library with the capability is switched on", &togglingLib{}, false},
		{"library that ignores the switch is refused", &togglingLib{stuck: true}, true},
		{"library without the capability is refused", struct{}{}, true},
		{"nil is refused", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireHTTPPageFetch(tc.lib)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrBrowserReads) {
				t.Errorf("err = %v, want ErrBrowserReads", err)
			}
			if l, ok := tc.lib.(*togglingLib); ok && !tc.wantErr && !l.on {
				t.Error("capability present but not switched on")
			}
		})
	}
}

// TestNewProductionClient_SwitchesBrowserReadsOff: against the library this
// module actually links, the production client must come back with page
// reads routed through the injected http.Client -- even when the caller did
// not ask for that itself.
func TestNewProductionClient_SwitchesBrowserReadsOff(t *testing.T) {
	lib, err := pslib.NewClient("https://ps.example.org",
		pslib.WithHTTPClient(NewHTTPClient("ps.example.org", false, nil, time.Second)),
		pslib.WithSession([]*http.Cookie{{Name: "JSESSIONID", Value: "synthetic"}}, time.Now().Add(time.Hour)),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if lib.UsesHTTPPageFetch() {
		t.Fatal("precondition: library defaults to HTTP page fetch; the test proves nothing")
	}

	client, err := NewProductionClient(lib)
	if err != nil || client == nil {
		t.Fatalf("NewProductionClient = (%v, %v), want a client", client, err)
	}
	if !lib.UsesHTTPPageFetch() {
		t.Error("library accepted with browser page reads still on")
	}
}

func TestNewProductionClient_NilLibrary(t *testing.T) {
	if _, err := NewProductionClient(nil); err == nil {
		t.Error("nil library accepted")
	}
}

// TestCheckLibrary: the linked library must POST where, and what, the
// transport's allowlist was approved for. The literals are repeated here on
// purpose: changing either side has to be a visible edit to this test.
func TestCheckLibrary(t *testing.T) {
	if err := CheckLibrary(); err != nil {
		t.Fatalf("CheckLibrary: %v", err)
	}
	for name, pair := range map[string][2]string{
		"assignment lookup path": {pslib.AssignmentLookupPath, AssignmentLookupPath},
		"student switch path":    {pslib.SwitchStudentPath, SwitchStudentPath},
		"student switch field":   {pslib.SwitchStudentField, SwitchStudentField},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: library %q, connector %q", name, pair[0], pair[1])
		}
	}
	if AssignmentLookupPath != "/ws/xte/assignment/lookup" ||
		SwitchStudentPath != "/guardian/home.html" || SwitchStudentField != "selected_student_id" {
		t.Errorf("allowlist literals changed: %q %q %q", AssignmentLookupPath, SwitchStudentPath, SwitchStudentField)
	}
}

// page builds a HomePage of two students. selected lists the ids the
// "library" marked Selected; attributed is the id stamped on the one grade
// row.
func page(attributed string, selected ...string) *pslib.HomePage {
	students := []*pslib.Student{{ID: "111111", Name: "Student One"}, {ID: "111112", Name: "Student Two"}}
	for _, s := range students {
		for _, id := range selected {
			if s.ID == id {
				s.Selected = true
				s.StudentNumber = "9000002"
			}
		}
	}
	g := sampleGrade("00100", "Example Science 7", "B+88")
	g.StudentID = attributed
	return &pslib.HomePage{Students: students, Grades: []*pslib.Grade{g}}
}

func TestSelectedStudent(t *testing.T) {
	// The pre-fix library shape: the FIRST student carries the page's
	// details although nothing marks it selected. That must not count.
	enrichedButUnmarked := page("")
	enrichedButUnmarked.Students[0].StudentNumber = "9000002"

	nilEntries := page("111112", "111112")
	nilEntries.Students = append([]*pslib.Student{nil}, nilEntries.Students...)
	nilEntries.Grades = append(nilEntries.Grades, nil)

	cases := []struct {
		name    string
		page    *pslib.HomePage
		want    string
		wantErr error
	}{
		{"nil page", nil, "", ErrSelectionUnknown},
		{"no students", &pslib.HomePage{}, "", ErrSelectionUnknown},
		{"marker on the first student", page("111111", "111111"), "111111", nil},
		{"marker on the second student", page("111112", "111112"), "111112", nil},
		{"nil entries are skipped", nilEntries, "111112", nil},
		{"no marker", page(""), "", ErrSelectionUnknown},
		{"no marker, first student enriched anyway", enrichedButUnmarked, "", ErrSelectionUnknown},
		{"two markers", page("111111", "111111", "111112"), "", ErrSelectionUnknown},
		{"grades attributed to the other student", page("111112", "111111"), "", ErrSelectionMismatch},
		{"grades attributed to nobody", page("", "111111"), "", ErrSelectionMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectedStudent(tc.page)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if got != nil {
					t.Errorf("returned a student alongside an error: %+v", got)
				}
				return
			}
			if got == nil || got.ID != tc.want {
				t.Errorf("SelectedStudent = %+v, want id %q", got, tc.want)
			}
		})
	}
}

func TestVerifyOneStudent(t *testing.T) {
	g := func(apiID string) *pslib.Grade { return &pslib.Grade{StudentAPIID: apiID} }
	cases := []struct {
		name    string
		read    []*pslib.Grade
		wantErr bool
	}{
		{"nothing read", nil, false},
		{"one page", []*pslib.Grade{g("999999")}, false},
		{"all the same", []*pslib.Grade{g("999999"), g("999999"), g("999999")}, false},
		{"second page is another student", []*pslib.Grade{g("999999"), g("999998")}, true},
		{"last page is another student", []*pslib.Grade{g("999999"), g("999999"), g("999998")}, true},
		{"a page with no id", []*pslib.Grade{g("999999"), g("")}, true},
		{"only page has no id", []*pslib.Grade{g("")}, true},
		{"nil entry", []*pslib.Grade{g("999999"), nil}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyOneStudent(tc.read)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrSelectionMismatch) {
				t.Errorf("err = %v, want ErrSelectionMismatch", err)
			}
		})
	}
}

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "unknown"},
		{"session expired sentinel", pslib.ErrSessionExpired, "auth"},
		{"session expired code", pslib.NewError(pslib.CodeSessionExpired, "x", nil), "auth"},
		{"auth failed", pslib.WrapAuthError(errors.New("x")), "auth"},
		{"write blocked through the library wrapper", pslib.WrapNetworkError(fmt.Errorf("%w: POST h/p", connector.ErrWriteBlocked)), "write_blocked"},
		{"host blocked through the library wrapper", pslib.WrapNetworkError(fmt.Errorf("%w: https://h/p", ErrHostBlocked)), "host_blocked"},
		{"selection unknown", ErrSelectionUnknown, "selection_unknown"},
		{"selection unknown from the library", pslib.ErrSelectionUnknown, "selection_unknown"},
		{"selection mismatch", ErrSelectionMismatch, "selection_mismatch"},
		{"another student selected, from the library", pslib.ErrStudentNotSelected, "selection_mismatch"},
		{"student number mismatch", ErrStudentNumberMismatch, "student_number_mismatch"},
		{"switch not confirmed", pslib.ErrSwitchNotConfirmed, "switch_not_confirmed"},
		{"rate limited", pslib.ErrRateLimited, "rate_limit"},
		{"not found", pslib.ErrNotFound, "not_found"},
		{"http status", pslib.WrapNetworkError(&pslib.HTTPStatusError{Op: "GET /x", StatusCode: 500, BodyLen: 10}), "http_status"},
		{"parse", pslib.WrapParseError(errors.New("x"), "grades"), "parse"},
		{"network", pslib.WrapNetworkError(errors.New("dial")), "network"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"anything else", errors.New("boom"), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyErr(tc.err); got != tc.want {
				t.Errorf("classifyErr = %q, want %q", got, tc.want)
			}
		})
	}
}
