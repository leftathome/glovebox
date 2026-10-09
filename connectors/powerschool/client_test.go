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

// togglingLib stands in for a powerschool-go release that has HTTP page
// fetching. stuck models a library that exposes the methods but does not
// honour them.
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

// TestNewProductionClient_FailsClosedOnBrowserReads pins the behaviour
// against whichever powerschool-go go.mod links. A library that cannot
// route page reads through the injected client must be refused; one that
// can must come back with the mode switched on. Neither outcome may leave
// a usable client whose page reads go through a browser.
func TestNewProductionClient_FailsClosedOnBrowserReads(t *testing.T) {
	lib, err := pslib.NewClient("https://ps.example.org",
		pslib.WithHTTPClient(NewHTTPClient("ps.example.org", false, time.Second)),
		pslib.WithSession([]*http.Cookie{{Name: "JSESSIONID", Value: "synthetic"}}, time.Now().Add(time.Hour)),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	client, err := NewProductionClient(lib)
	if f, capable := any(lib).(httpPageFetcher); capable {
		if err != nil {
			t.Fatalf("capable library refused: %v", err)
		}
		if !f.UsesHTTPPageFetch() {
			t.Error("capable library accepted without HTTP page fetch on")
		}
		return
	}
	if !errors.Is(err, ErrBrowserReads) || client != nil {
		t.Errorf("NewProductionClient = (%v, %v), want (nil, ErrBrowserReads)", client, err)
	}
}

func TestNewProductionClient_NilLibrary(t *testing.T) {
	if _, err := NewProductionClient(nil); err == nil {
		t.Error("nil library accepted")
	}
}

func TestSelectedStudentID(t *testing.T) {
	cases := []struct {
		name     string
		students []*pslib.Student
		want     string
		wantErr  bool
	}{
		{"none", nil, "", true},
		{"one student, enriched", []*pslib.Student{{ID: "111111", StudentNumber: "9000001"}}, "111111", false},
		{"two students, first selected", []*pslib.Student{{ID: "111111", StudentNumber: "9000001"}, {ID: "111112"}}, "111111", false},
		{"two students, second selected", []*pslib.Student{{ID: "111111"}, nil, {ID: "111112", StudentNumber: "9000002"}}, "111112", false},
		{"nobody enriched", []*pslib.Student{{ID: "111111"}, {ID: "111112"}}, "", true},
		{"both enriched is ambiguous", []*pslib.Student{{ID: "111111", StudentNumber: "9000001"}, {ID: "111112", StudentNumber: "9000002"}}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectedStudentID(tc.students)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("SelectedStudentID = (%q, %v), want (%q, wantErr %v)", got, err, tc.want, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrSelectionUnknown) {
				t.Errorf("err = %v, want ErrSelectionUnknown", err)
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
		{"rate limited", pslib.ErrRateLimited, "rate_limit"},
		{"not found", pslib.ErrNotFound, "not_found"},
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
