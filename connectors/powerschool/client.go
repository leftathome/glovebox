// Package powerschool is the Glovebox connector for the PowerSchool SIS
// parent portal (grades, assignments, attendance).
//
// All library access flows through the PowerSchoolClient interface; the
// production code wraps powerschool-go's *powerschool.Client, and tests
// pass a hand-rolled fake. This keeps connector-level tests offline and
// allows the library to evolve without churning connector tests.
//
// The connector is read-only by construction. See transport.go for the
// transport every request goes through and docs/specs/16-powerschool-
// connector-design.md for the whole design.
package powerschool

import (
	"context"
	"errors"
	"fmt"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// PowerSchoolClient is the connector's narrowed view of the powerschool-go
// library surface. Methods correspond 1:1 to library functions used by the
// connector; new library surfaces require widening this interface.
//
// Notes on the library:
//   - The portal shows one student per session. SelectStudent makes the
//     given student the selected one -- submitting the portal's own
//     switcher form if another is selected -- and returns the home page
//     that confirmed it. It posts nothing when the page carries no
//     selection marker, and fails when the page read after the switch does
//     not mark the requested student (spec 16 section 6.1).
//   - GetHomePage reads the account's students and the grades table of the
//     student the session has selected from ONE fetch of the home page, so
//     "whose grades are these" is answered by the same response the grades
//     came from.
//   - FetchScoresMetadata reads a course's scores page and returns the
//     section id and the student id that page carries. The portal renders
//     that page for the session's selected student, so the id says whose
//     page it is; the connector checks it against the kid's bound id
//     before using the page (binding.go).
//   - GetAssignments accepts a studentID but does not act on it. When the
//     *Grade it is given already carries SectionID and StudentAPIID -- the
//     connector fills them in from the scores page it just checked -- it
//     sends the lookup for exactly those ids without reading the page
//     again.
type PowerSchoolClient interface {
	SelectStudent(ctx context.Context, studentID string) (*pslib.HomePage, error)
	GetHomePage(ctx context.Context) (*pslib.HomePage, error)
	FetchScoresMetadata(ctx context.Context, scoresURL string) (*pslib.ScoresMetadata, error)
	GetAssignments(ctx context.Context, studentID string, grade *pslib.Grade) ([]*pslib.Assignment, error)
}

// ErrBrowserReads is returned by NewProductionClient when the linked
// powerschool-go cannot route its page reads through the injected
// http.Client.
var ErrBrowserReads = errors.New("powerschool: linked powerschool-go reads pages through a headless browser, " +
	"outside the read-only transport (spec 16 section 5.3)")

// httpPageFetcher is the part of *powerschool.Client that routes page reads
// through the injected http.Client instead of a local headless Chrome,
// whose traffic would never pass through Transport.
type httpPageFetcher interface {
	SetHTTPPageFetch(on bool)
	UsesHTTPPageFetch() bool
}

// requireHTTPPageFetch turns HTTP page fetching on and confirms it took.
// lib is `any` so both outcomes can be tested with a stand-in.
func requireHTTPPageFetch(lib any) error {
	f, ok := lib.(httpPageFetcher)
	if !ok {
		return ErrBrowserReads
	}
	f.SetHTTPPageFetch(true)
	if !f.UsesHTTPPageFetch() {
		return ErrBrowserReads
	}
	return nil
}

// productionClient is the real PowerSchoolClient backed by powerschool-go.
type productionClient struct {
	lib *pslib.Client
}

// NewProductionClient wraps a powerschool-go client so it satisfies
// PowerSchoolClient. It refuses (ErrBrowserReads) a library whose page
// reads would bypass the http.Client the caller injected.
func NewProductionClient(lib *pslib.Client) (PowerSchoolClient, error) {
	if lib == nil {
		return nil, fmt.Errorf("powerschool: nil library client")
	}
	if err := requireHTTPPageFetch(lib); err != nil {
		return nil, err
	}
	return &productionClient{lib: lib}, nil
}

func (p *productionClient) SelectStudent(ctx context.Context, studentID string) (*pslib.HomePage, error) {
	return p.lib.SelectStudent(ctx, studentID)
}

func (p *productionClient) GetHomePage(ctx context.Context) (*pslib.HomePage, error) {
	return p.lib.GetHomePage(ctx)
}

func (p *productionClient) FetchScoresMetadata(ctx context.Context, scoresURL string) (*pslib.ScoresMetadata, error) {
	return p.lib.FetchScoresMetadata(ctx, scoresURL)
}

func (p *productionClient) GetAssignments(ctx context.Context, studentID string, grade *pslib.Grade) ([]*pslib.Assignment, error) {
	return p.lib.GetAssignments(ctx, studentID, grade)
}

// Selection errors. Each maps to its own error_class (see classifyErr) and
// each means "stage nothing".
var (
	// ErrSelectionUnknown: the home page does not explicitly mark which
	// student it is showing.
	ErrSelectionUnknown = errors.New("powerschool: the portal page does not mark which student is selected")
	// ErrSelectionMismatch: two parts of one poll disagree about which
	// student they belong to.
	ErrSelectionMismatch = errors.New("powerschool: pages read in one poll belong to different students")
	// ErrSelectionChanged: a scores page read during a kid's turn carries
	// a student id other than the one that kid is bound to. The session's
	// selected student was not this kid's when that page was served.
	ErrSelectionChanged = errors.New("powerschool: a scores page read for this kid belongs to another student")
	// ErrStudentAPIIDConflict: the id a kid's pages agreed on is already
	// bound to another kid.
	ErrStudentAPIIDConflict = errors.New("powerschool: the scores-page student id observed for this kid is bound to another kid")
	// ErrStudentNumberMismatch: the selected student's number on the page
	// is not the student_number configured for that kid.
	ErrStudentNumberMismatch = errors.New("powerschool: the selected student's number does not match the configured student_number")
)

// SelectedStudent returns the student the home page explicitly marks as
// selected, having checked that every grade row on the page is attributed
// to that same student.
//
// The portal shows one student at a time and the library returns that
// student's records whatever it is asked for, so this is what stands
// between one child's label and another child's records. The only accepted
// evidence is the library's Student.Selected, which it sets from the page's
// explicit marker and from nothing else. A populated StudentNumber is NOT
// evidence: before powerschool-go fixed it, the first listed student was
// given the page's details whenever the marker was missing. No marker, or
// more than one student marked, is ErrSelectionUnknown and the caller
// stages nothing for anybody.
func SelectedStudent(page *pslib.HomePage) (*pslib.Student, error) {
	if page == nil {
		return nil, ErrSelectionUnknown
	}
	var selected *pslib.Student
	for _, s := range page.Students {
		if s == nil || !s.Selected {
			continue
		}
		if selected != nil {
			return nil, ErrSelectionUnknown
		}
		selected = s
	}
	if selected == nil || selected.ID == "" {
		return nil, ErrSelectionUnknown
	}
	for _, g := range page.Grades {
		if g != nil && g.StudentID != selected.ID {
			return nil, ErrSelectionMismatch
		}
	}
	return selected, nil
}

// verifyKidPage checks that page is, by its own explicit marker, the page
// of kid's student -- and, when the kid has a student_number configured,
// that the number on the page is that one. It is applied to the page a
// kid's poll starts from and again to a fresh read when the poll ends.
func verifyKidPage(page *pslib.HomePage, kid Kid) error {
	selected, err := SelectedStudent(page)
	if err != nil {
		return err
	}
	if selected.ID != kid.StudentID {
		return ErrSelectionMismatch
	}
	if kid.StudentNumber != "" && selected.StudentNumber != kid.StudentNumber {
		return ErrStudentNumberMismatch
	}
	return nil
}

// classifyErr maps a library or transport error to one of the short stable
// strings used as the error_class metric label and log field.
func classifyErr(err error) string {
	switch {
	case err == nil:
		return "unknown"
	case pslib.IsSessionExpired(err), pslib.IsAuthError(err):
		return "auth"
	case errors.Is(err, ErrHostBlocked):
		return "host_blocked"
	case errors.Is(err, connector.ErrWriteBlocked):
		return "write_blocked"
	case errors.Is(err, ErrSelectionUnknown), errors.Is(err, pslib.ErrSelectionUnknown):
		return "selection_unknown"
	case errors.Is(err, ErrSelectionMismatch), errors.Is(err, pslib.ErrStudentNotSelected):
		return "selection_mismatch"
	case errors.Is(err, ErrSelectionChanged):
		return "selection_changed"
	case errors.Is(err, ErrStudentAPIIDConflict):
		return "student_api_id_conflict"
	case errors.Is(err, ErrStudentNumberMismatch):
		return "student_number_mismatch"
	case errors.Is(err, pslib.ErrSwitchNotConfirmed):
		return "switch_not_confirmed"
	case errors.Is(err, pslib.ErrRateLimited):
		return "rate_limit"
	case pslib.IsNotFound(err):
		return "not_found"
	}
	var status *pslib.HTTPStatusError
	if errors.As(err, &status) {
		return "http_status"
	}
	var libErr *pslib.Error
	if errors.As(err, &libErr) {
		switch libErr.Code {
		case pslib.CodeParseError:
			return "parse"
		case pslib.CodeNetworkError:
			return "network"
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "unknown"
}
