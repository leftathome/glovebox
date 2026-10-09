package powerschool

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"strings"

	pslib "github.com/leftathome/powerschool-go"
)

// Errors that come back from the portal side are never logged verbatim.
//
// A library or transport error's text is built upstream of this package:
// *url.Error carries the whole request URL, whose query names a student's
// enrolment and school (frn, schoolid); a status or parse error can quote
// the response, which is a child's school record. The pod log is shipped to
// the cluster's log store and read by people and agents who are not
// entitled to that record.
//
// So the log line carries two things only, both assembled here from values
// with a closed vocabulary: error_class (classifyErr) and error_detail
// (describeErr). Neither ever contains err.Error().

// errAttrs returns the slog attributes for err: its class and a sanitised
// description. Use it in place of "error", err.
func errAttrs(err error) []any {
	return []any{
		slog.String("error_class", classifyErr(err)),
		slog.String("error_detail", describeErr(err)),
	}
}

// describeErr renders err's chain as a list of kinds, outermost first, for
// example "powerschool-go:NETWORK_ERROR > url:Post > http:500(1832b)". Each
// element is a library error code, an HTTP status, a known sentinel's name
// or a Go type name -- never message text. The chain is cut at 8 links.
func describeErr(err error) string {
	if err == nil {
		return "none"
	}
	var parts []string
	for e := err; e != nil && len(parts) < 8; e = errors.Unwrap(e) {
		parts = append(parts, describeOne(e))
	}
	return strings.Join(parts, " > ")
}

// sentinels are the comparable errors whose identity is worth naming.
var sentinels = []struct {
	err  error
	name string
}{
	{pslib.ErrSessionExpired, "session_expired"},
	{pslib.ErrAuthFailed, "auth_failed"},
	{pslib.ErrSelectionUnknown, "lib_selection_unknown"},
	{pslib.ErrStudentNotSelected, "lib_student_not_selected"},
	{pslib.ErrSwitchNotConfirmed, "switch_not_confirmed"},
	{pslib.ErrRateLimited, "rate_limited"},
	{pslib.ErrNotFound, "not_found"},
	{ErrHostBlocked, "host_blocked"},
	{ErrSelectionUnknown, "selection_unknown"},
	{ErrSelectionMismatch, "selection_mismatch"},
	{ErrStudentNumberMismatch, "student_number_mismatch"},
}

// describeOne names a single link of an error chain.
func describeOne(e error) string {
	// Comparing interface values panics when the dynamic type is not
	// comparable (an error struct holding a slice, say).
	if reflect.TypeOf(e).Comparable() {
		for _, s := range sentinels {
			if e == s.err {
				return s.name
			}
		}
	}
	switch v := e.(type) {
	case *pslib.Error:
		return "powerschool-go:" + safeToken(string(v.Code))
	case *pslib.HTTPStatusError:
		return fmt.Sprintf("http:%d(%db)", v.StatusCode, v.BodyLen)
	case *url.Error:
		// Op is the HTTP method ("Get", "Post"). The URL is dropped.
		return "url:" + safeToken(v.Op)
	}
	// A Go type name, e.g. *errors.errorString. Fixed at compile time.
	return safeToken(fmt.Sprintf("%T", e))
}

// safeToken keeps letters, digits and a little punctuation, and caps the
// length, so a value that was expected to be an identifier cannot smuggle
// free text (a query string, a name) into the log.
func safeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '*', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	return b.String()
}
