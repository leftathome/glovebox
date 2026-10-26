package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
)

func post(r *rig, token string, body any) (int, journalapi.Response) {
	r.t.Helper()
	var resp journalapi.Response
	code := r.do("POST", journalapi.PathRecords, token, body, &resp)
	return code, resp
}

// The 4.3 response table, row by row.
func TestRecordsResponseTable(t *testing.T) {
	r := newRig(t)
	ok := assignments(1, "e_child1", "powerschool", T0.Add(-time.Hour), one("k", "e_child1", `{"title": "A"}`))
	if code, resp := post(r, "ps", ok); code != 200 || resp.Status != journalapi.StatusApplied || resp.Stats == nil || resp.Stats.Created != 1 {
		t.Fatalf("applied: %d %+v", code, resp)
	}
	if code, resp := post(r, "ps", ok); code != 200 || resp.Status != journalapi.StatusDuplicate || resp.Stats == nil || resp.Stats.Created != 1 {
		t.Errorf("duplicate: %d %+v", code, resp)
	}
	// The writer stored is the authenticated caller, namespace/name.
	var writer string
	if err := r.db.QueryRow(`SELECT writer FROM snapshot WHERE id = $1::uuid`, uid(1)).Scan(&writer); err != nil || writer != journaltest.SAPowerSchool {
		t.Errorf("stored writer %q, %v", writer, err)
	}
	for _, reader := range []string{"gw", "dash", "admin"} {
		if code, resp := post(r, reader, ok); code != 403 || resp.Reason != journalapi.ReasonPolicy || resp.Stats != nil {
			t.Errorf("%s probed a snapshot id: %d %+v", reader, code, resp)
		}
	}
	late := assignments(2, "e_child1", "powerschool", T0.Add(-2*time.Hour))
	if code, resp := post(r, "ps", late); code != 200 || resp.Status != journalapi.StatusLate {
		t.Errorf("late: %d %+v", code, resp)
	}
	lms := func(n int, source string) journalapi.Snapshot {
		s := assignments(n, "e_child1", source, T0)
		s.RecordType = "lms_assignment"
		return s
	}
	for name, c := range map[string]struct {
		token  string
		body   any
		status int
		reason string
	}{
		// Validation ties the scope's source to the type's owner (Task 7),
		// before policy: both rows are 422 schema, not the draft's 403.
		"other source":    {"ps", assignments(3, "e_child1", "schoology", T0), 422, journalapi.ReasonSchema},
		"owner != source": {"shared", assignments(4, "e_child1", "schoology", T0), 422, journalapi.ReasonSchema},
		"policy: type":    {"ps", lms(10, "schoology"), 403, journalapi.ReasonPolicy},
		"policy: write_scopes": {"scoped", func() journalapi.Snapshot {
			s := assignments(11, "e_child1", "powerschool", T0)
			s.Scope[3][1] = "c2"
			return s
		}(), 403, journalapi.ReasonPolicy},
		"policy: write_scopes part missing": {"scoped", func() journalapi.Snapshot {
			s := assignments(13, "e_child1", "powerschool", T0)
			s.Scope = s.Scope[:3] // no course part
			return s
		}(), 403, journalapi.ReasonPolicy},
		"unknown subject":       {"ps", assignments(5, "e_stranger", "powerschool", T0, one("k", "e_stranger", `{"title": "A"}`)), 409, journalapi.ReasonUnknownSubject},
		"future observed_at":    {"ps", assignments(6, "e_child1", "powerschool", T0.Add(time.Hour)), 422, journalapi.ReasonFutureObservedAt},
		"duplicate key":         {"ps", assignments(7, "e_child1", "powerschool", T0, one("k", "e_child1", `{}`), one("k", "e_child1", `{}`)), 422, journalapi.ReasonDuplicateKey},
		"schema: unknown field": {"ps", assignments(8, "e_child1", "powerschool", T0, one("k", "e_child1", `{"grade": "A"}`)), 422, journalapi.ReasonSchema},
		"schema: unknown key":   {"ps", `{"snapshot_id": "` + uid(9) + `", "grants": []}`, 422, journalapi.ReasonSchema},
		"too large: body":       {"ps", `{"snapshot_id": "` + strings.Repeat("x", 9<<20) + `"}`, 413, journalapi.ReasonTooLarge},
		"too large: records": {"ps", func() journalapi.Snapshot {
			s := assignments(12, "e_child1", "powerschool", T0)
			s.Records = make([]journalapi.Record, 5001)
			return s
		}(), 413, journalapi.ReasonTooLarge},
	} {
		code, resp := post(r, c.token, c.body)
		if code != c.status || resp.Reason != c.reason || resp.Status != journalapi.StatusRejected || resp.Stats != nil {
			t.Errorf("%s: %d %+v", name, code, resp)
		}
	}
	// The other side of the policy rows: what the policy allows is applied.
	for name, c := range map[string]struct {
		token string
		body  journalapi.Snapshot
	}{
		"shared writes lms":    {"shared", lms(20, "schoology")},
		"scoped writes c1":     {"scoped", assignments(21, "e_child2", "powerschool", T0)},
		"schoology writes lms": {"sg", lms(22, "schoology")},
	} {
		if code, resp := post(r, c.token, c.body); code != 200 || resp.Status != journalapi.StatusApplied {
			t.Errorf("%s: %d %+v", name, code, resp)
		}
	}
}

func TestHeldAnswerAndResend(t *testing.T) {
	r := newRig(t)
	var recs []journalapi.Record
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		recs = append(recs, one(k, "e_child1", `{"title": "A"}`))
	}
	r.do("POST", journalapi.PathRecords, "ps", assignments(1, "e_child1", "powerschool", T0.Add(-2*time.Hour), recs...), nil)
	held := assignments(2, "e_child1", "powerschool", T0.Add(-time.Hour), recs[:2]...)
	for i := 0; i < 3; i++ {
		var resp journalapi.Response
		if code := r.do("POST", journalapi.PathRecords, "ps", held, &resp); code != 200 || resp.Status != journalapi.StatusHeld || resp.Stats == nil || resp.Stats.Removed != 3 {
			t.Errorf("attempt %d: %d %+v", i, code, resp)
		}
	}
	// "held" is counted once; the resends are duplicates.
	m := r.scrape()
	for _, want := range []string{`record_type="assignment",status="held"} 1`, `record_type="assignment",status="duplicate"} 2`} {
		if !metricLine(m, want) {
			t.Errorf("metrics lack %s:\n%s", want, m)
		}
	}
}

// metricLine reports whether a snapshots_total series ends with want,
// ignoring the OpenTelemetry scope labels between.
func metricLine(page, want string) bool {
	labels, value, _ := strings.Cut(want, "} ")
	re := regexp.MustCompile(`(?m)^glovebox_journal_snapshots_total\{[^}]*` + regexp.QuoteMeta(labels) + `[^}]*\} ` + regexp.QuoteMeta(value) + `$`)
	return re.MatchString(page)
}

// Step 1 runs before validation: a resend is answered as it was even when
// it would not validate now (here: the journal clock stepped back, so the
// snapshot is "in the future").
func TestResendIsAnsweredBeforeValidation(t *testing.T) {
	r := newRig(t)
	s := assignments(1, "e_child1", "powerschool", T0, one("k", "e_child1", `{"title": "A"}`))
	if code, resp := post(r, "ps", s); code != 200 || resp.Status != journalapi.StatusApplied {
		t.Fatalf("first: %d %+v", code, resp)
	}
	r.setNow(T0.Add(-3 * time.Hour))
	if code, resp := post(r, "ps", s); code != 200 || resp.Status != journalapi.StatusDuplicate || resp.Stats == nil || resp.Stats.Created != 1 {
		t.Errorf("resend after clock step: %d %+v", code, resp)
	}
	fresh := assignments(2, "e_child1", "powerschool", T0, one("k", "e_child1", `{"title": "A"}`))
	if code, resp := post(r, "ps", fresh); code != 422 || resp.Reason != journalapi.ReasonFutureObservedAt {
		t.Errorf("a new snapshot under the stepped clock: %d %+v", code, resp)
	}
}

// An id is a duplicate only for the same writer, record type and scope;
// any other use is 422 snapshot_id_reused and reveals nothing.
func TestSnapshotIDReuse(t *testing.T) {
	r := newRig(t)
	s := assignments(1, "e_child1", "powerschool", T0, one("k", "e_child1", `{"title": "A"}`))
	if code, resp := post(r, "ps", s); code != 200 || resp.Status != journalapi.StatusApplied {
		t.Fatalf("first: %d %+v", code, resp)
	}
	otherScope := s
	otherScope.Scope = append([]journalapi.ScopePart(nil), s.Scope...)
	otherScope.Scope[3] = journalapi.ScopePart{"course", "c9"}
	otherType := assignments(1, "e_child1", "powerschool", T0)
	otherType.RecordType = "attendance_day" // the same canonical scope, another type
	for name, c := range map[string]struct {
		token string
		body  journalapi.Snapshot
	}{
		"same writer, other scope": {"ps", otherScope},
		"same writer, other type":  {"ps", otherType},
		"other writer, same body":  {"shared", s},
	} {
		code, resp := post(r, c.token, c.body)
		if code != 422 || resp.Reason != journalapi.ReasonSnapshotIDReused || resp.Status != journalapi.StatusRejected || resp.Stats != nil {
			t.Errorf("%s: %d %+v", name, code, resp)
		}
	}
	// Without a parseable scope there is nothing to compare: the lookup is
	// skipped and validation refuses the snapshot.
	bad := s
	bad.Scope = []journalapi.ScopePart{{"surface", "assignments"}}
	if code, resp := post(r, "ps", bad); code != 422 || resp.Reason != journalapi.ReasonSchema {
		t.Errorf("unparseable scope: %d %+v", code, resp)
	}
	m := r.scrape()
	if metricLine(m, `status="duplicate"} 1`) {
		t.Errorf("a refused reuse was counted as a duplicate")
	}
}

// An erased subject is banned: 410, from Apply.
func TestErasedSubjectIs410(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.Erase(context.Background(), journalapiAdmin, "e_child2"); err != nil {
		t.Fatal(err)
	}
	code, resp := post(r, "ps", assignments(1, "e_child2", "powerschool", T0, one("k", "e_child2", `{"title": "A"}`)))
	if code != 410 || resp.Reason != journalapi.ReasonErasedSubject || resp.Stats != nil {
		t.Errorf("erased: %d %+v", code, resp)
	}
}

const journalapiAdmin = "openclaw/glovebox-journal-admin"

// Decoding is strict: duplicate, case-variant and unknown keys and trailing
// data are refused; the same snapshot written plainly is applied.
func TestStrictDecoding(t *testing.T) {
	r := newRig(t)
	raw, _ := json.Marshal(assignments(1, "e_child1", "powerschool", T0, one("k", "e_child1", `{"title": "A"}`)))
	body := string(raw)
	for name, b := range map[string]string{
		"duplicate key":       `{"snapshot_id": "` + uid(2) + `", ` + body[1:],
		"case-variant key":    `{"Snapshot_ID": "` + uid(2) + `", ` + strings.Replace(body[1:], `"snapshot_id":"`+uid(1)+`",`, "", 1),
		"case-variant record": strings.Replace(body, `"key":"k"`, `"KEY":"k"`, 1),
		"duplicate field":     strings.Replace(body, `"title":"A"`, `"title":"A","title":"B"`, 1),
		"trailing document":   body + `{}`,
		"not an object":       `[` + body + `]`,
		"empty":               ``,
	} {
		if code, resp := post(r, "ps", b); code != 422 || resp.Reason != journalapi.ReasonSchema || resp.Detail != "the body is not a snapshot" {
			t.Errorf("%s: %d %+v", name, code, resp)
		}
	}
	if code, resp := post(r, "ps", body+"\n"); code != 200 || resp.Status != journalapi.StatusApplied {
		t.Errorf("plain: %d %+v", code, resp)
	}
}

func TestContentType(t *testing.T) {
	r := newRig(t)
	s := assignments(1, "e_child1", "powerschool", T0, one("k", "e_child1", `{"title": "A"}`))
	for _, ct := range []string{"", "text/plain", "application/jsonx", "application/x-www-form-urlencoded"} {
		code, raw := r.send("POST", journalapi.PathRecords, "ps", ct, s)
		var resp journalapi.Response
		_ = json.Unmarshal(raw, &resp)
		if code != 400 || resp.Reason != journalapi.ReasonBadRequest {
			t.Errorf("%q: %d %s", ct, code, raw)
		}
	}
	code, raw := r.send("POST", journalapi.PathRecords, "ps", "application/json; charset=utf-8", s)
	if code != 200 {
		t.Errorf("json with charset: %d %s", code, raw)
	}
}

// The body limit is 8 MiB exactly: a valid snapshot padded to the limit is
// applied, one byte more is 413.
func TestBodyLimitBoundary(t *testing.T) {
	r := newRig(t)
	pad := func(n int) []byte {
		raw, _ := json.Marshal(assignments(n, "e_child1", "powerschool", T0, one("k", "e_child1", `{"title": "A"}`)))
		return append(raw, bytes.Repeat([]byte(" "), maxSnapshotBytes-len(raw))...)
	}
	if code, resp := post(r, "ps", pad(1)); code != 200 || resp.Status != journalapi.StatusApplied {
		t.Errorf("exactly 8 MiB: %d %+v", code, resp)
	}
	if code, resp := post(r, "ps", append(pad(2), ' ')); code != 413 || resp.Reason != journalapi.ReasonTooLarge {
		t.Errorf("8 MiB + 1: %d %+v", code, resp)
	}
	if maxSnapshotBytes != 8<<20 {
		t.Errorf("maxSnapshotBytes = %d", maxSnapshotBytes)
	}
}

// A refusal names fields and positions, never a value the sender sent.
func TestRefusalsDoNotEchoValues(t *testing.T) {
	r := newRig(t)
	for name, b := range map[string]any{
		"bad value":     assignments(1, "e_child1", "powerschool", T0, one("k", "e_child1", `{"score": "SECRETVALUE"}`)),
		"unknown field": `{"snapshot_id": "` + uid(2) + `", "SECRETVALUE": 1}`,
		"bad scope":     `{"snapshot_id": "` + uid(3) + `", "scope": "SECRETVALUE"}`,
		"scope source":  assignments(4, "e_child1", "SECRETVALUE", T0),
	} {
		code, raw := r.send("POST", journalapi.PathRecords, "ps", "application/json", b)
		if code/100 != 4 || strings.Contains(string(raw), "SECRETVALUE") {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
}

// Without a roster nobody is a known subject: 409, retried.
func TestNoRosterIs409(t *testing.T) {
	r := newRig(t)
	r.ros.Store(nil)
	code, resp := post(r, "ps", assignments(1, "e_child1", "powerschool", T0, one("k", "e_child1", `{"title": "A"}`)))
	if code != 409 || resp.Reason != journalapi.ReasonUnknownSubject {
		t.Errorf("no roster: %d %+v", code, resp)
	}
}

// A database failure is a 500 that says nothing about it.
func TestDatabaseFailureIs500WithoutInternals(t *testing.T) {
	// A failure inside Apply, after the step-1 lookup succeeded.
	r := newRig(t)
	if _, err := r.db.Exec(`DROP TABLE erasure CASCADE`); err != nil {
		t.Fatal(err)
	}
	s := assignments(1, "e_child1", "powerschool", T0, one("k", "e_child1", `{"title": "A"}`))
	code, raw := r.send("POST", journalapi.PathRecords, "ps", "application/json", s)
	if code != 500 || !bytes.Equal(raw, []byte(`{"status":"rejected","reason":"internal"}`+"\n")) {
		t.Errorf("apply failed: %d %s", code, raw)
	}
	if !strings.Contains(r.logs.String(), "what=apply") {
		t.Errorf("the failure was not logged for the operator: %s", r.logs.String())
	}

	// A failure of the step-1 lookup.
	r = newRig(t)
	if err := r.j.Close(); err != nil {
		t.Fatal(err)
	}
	code, raw = r.send("POST", journalapi.PathRecords, "ps", "application/json", s)
	var resp journalapi.Response
	_ = json.Unmarshal(raw, &resp)
	if code != 500 || resp.Reason != journalapi.ReasonInternal || resp.Detail != "" || strings.Contains(string(raw), "sql") {
		t.Errorf("closed database: %d %s", code, raw)
	}
	if !strings.Contains(r.logs.String(), "what=lookup") {
		t.Errorf("the lookup failure was not reported as such: %s", r.logs.String())
	}
	// Without a parseable id the lookup is skipped and Apply is never reached:
	// validation answers first.
	bad := s
	bad.SnapshotID = "not-a-uuid"
	if code, resp := post(r, "ps", bad); code != 422 || resp.Reason != journalapi.ReasonSchema {
		t.Errorf("bad id: %d %+v", code, resp)
	}
}
