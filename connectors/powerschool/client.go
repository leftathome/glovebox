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
// Notes on the real library signatures (v0.1.0):
//   - GetGrades and GetAssignments accept a studentID but do not act on it:
//     both return data for whichever student the portal session currently
//     has selected. The connector therefore never trusts the argument and
//     checks the selection itself (see SelectedStudentID).
//   - GetAssignments mutates the *Grade it is given (it caches the section
//     and student ids it scraped from the scores page).
type PowerSchoolClient interface {
	GetStudents(ctx context.Context) ([]*pslib.Student, error)
	GetGrades(ctx context.Context, studentID string) ([]*pslib.Grade, error)
	GetAssignments(ctx context.Context, studentID string, grade *pslib.Grade) ([]*pslib.Assignment, error)
}

// ErrBrowserReads is returned by NewProductionClient when the linked
// powerschool-go cannot route its page reads through the injected
// http.Client.
var ErrBrowserReads = errors.New("powerschool: linked powerschool-go reads pages through a headless browser, " +
	"outside the read-only transport; a release with WithHTTPPageFetch is required (spec 16 section 5.3)")

// httpPageFetcher is implemented by *powerschool.Client from the release
// that adds WithHTTPPageFetch. v0.1.0 does not implement it: there,
// GetStudents and GetGrades each start a local headless Chrome, and nothing
// Chrome sends passes through Transport.
//
// The capability is probed through an interface rather than called
// directly so this package builds against v0.1.0 and fails closed at
// startup instead of failing to compile. Once go.mod pins a release that
// has the option, replace the probe with pslib.WithHTTPPageFetch() at
// construction.
type httpPageFetcher interface {
	SetHTTPPageFetch(on bool)
	UsesHTTPPageFetch() bool
}

// requireHTTPPageFetch turns HTTP page fetching on and confirms it took.
// lib is `any` so the two outcomes can be tested without a library release.
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

func (p *productionClient) GetStudents(ctx context.Context) ([]*pslib.Student, error) {
	return p.lib.GetStudents(ctx)
}

func (p *productionClient) GetGrades(ctx context.Context, studentID string) ([]*pslib.Grade, error) {
	return p.lib.GetGrades(ctx, studentID)
}

func (p *productionClient) GetAssignments(ctx context.Context, studentID string, grade *pslib.Grade) ([]*pslib.Assignment, error) {
	return p.lib.GetAssignments(ctx, studentID, grade)
}

// ErrSelectionUnknown is returned by SelectedStudentID when the portal page
// does not reveal which student it is showing.
var ErrSelectionUnknown = errors.New("powerschool: cannot determine which student the session has selected")

// SelectedStudentID returns the id of the student the portal session is
// currently showing.
//
// The library returns grades and assignments for that student whatever id
// the caller asks for, so this is the only thing standing between a second
// child's label and the first child's records. The library enriches exactly
// one student -- the selected one -- with the details from the page's
// student-demographics table; that student's StudentNumber is the signal.
// Zero or several enriched students is treated as unknown, and the caller
// stages nothing.
func SelectedStudentID(students []*pslib.Student) (string, error) {
	selected := ""
	for _, s := range students {
		if s == nil || s.StudentNumber == "" {
			continue
		}
		if selected != "" {
			return "", ErrSelectionUnknown
		}
		selected = s.ID
	}
	if selected == "" {
		return "", ErrSelectionUnknown
	}
	return selected, nil
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
	case errors.Is(err, ErrSelectionUnknown):
		return "selection_unknown"
	case errors.Is(err, pslib.ErrRateLimited):
		return "rate_limit"
	case pslib.IsNotFound(err):
		return "not_found"
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
