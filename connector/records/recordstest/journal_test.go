package recordstest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"


	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/connector/records/recordstest"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

var now = time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)

func registry(t *testing.T) *record.Registry {
	t.Helper()
	reg, err := record.NewRegistry([]record.Type{
		{Name: "t1", SchemaVersion: 1, Owner: "src", DefaultAudience: []string{"guardians"},
			Fields: map[string]record.FieldDef{"a": {Type: record.String}, "d": {Type: record.Date}}},
		{Name: "t2", SchemaVersion: 1, Owner: "src", DefaultAudience: []string{"guardians"},
			Fields: map[string]record.FieldDef{"a": {Type: record.String}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func snap(subject string, observed time.Time) journalapi.Snapshot {
	f := records.NewFields(nil)
	f.String("a", "x")
	f.Date("d", observed)
	return records.NewSnapshot("t1", 1, records.Scope("src", "s", subject, records.Part("course", "c1")), true, nil, observed,
		[]journalapi.Record{{Key: "k1", DataSubject: subject, Fields: f.Map()}})
}

func body(t *testing.T, s journalapi.Snapshot) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type rig struct {
	j *recordstest.Journal
	c *records.Client
}

func newRig(t *testing.T) rig {
	t.Helper()
	j := recordstest.New(t, "tok-1")
	j.Registry = registry(t)
	j.Now = func() time.Time { return now }
	tok := filepath.Join(t.TempDir(), "token")
	if err := writeFile(tok, "tok-1"); err != nil {
		t.Fatal(err)
	}
	c, err := records.NewClient(j.URL, records.TokenFile(tok), 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rig{j: j, c: c}
}

func (r rig) send(t *testing.T, b []byte) records.Answer {
	t.Helper()
	a, err := r.c.Send(context.Background(), b)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return a
}

func expect(t *testing.T, a records.Answer, code int, status, reason string, act records.Action) {
	t.Helper()
	if a.Code != code || a.Status != status || a.Reason != reason {
		t.Fatalf("answer %+v, want %d %s/%s", a, code, status, reason)
	}
	if got := records.Classify(a.Code, a.Status, a.Reason); got != act {
		t.Fatalf("classified %v, want %v", got, act)
	}
}

// One test per row of docs/journal-api.md "Answers, and what the
// connector's spool does", through the real client and classifier.
func TestTableRowAccepted(t *testing.T) {
	r := newRig(t)
	s := snap("e_child1", now)
	expect(t, r.send(t, body(t, s)), 200, "applied", "", records.ActDelete)
	expect(t, r.send(t, body(t, s)), 200, "duplicate", "", records.ActDelete)
	older := snap("e_child1", now.Add(-time.Hour))
	expect(t, r.send(t, body(t, older)), 200, "late", "", records.ActDelete)
	r.j.HoldNext()
	held := snap("e_child1", now.Add(time.Minute))
	expect(t, r.send(t, body(t, held)), 200, "held", "", records.ActDelete)
	expect(t, r.send(t, body(t, held)), 200, "held", "", records.ActDelete) // a resend of a held snapshot
	if got := len(r.j.Received()); got != 3 {
		t.Fatalf("received %d, want 3 (the duplicate is not stored twice)", got)
	}
}

func TestTableRowErasedSubject(t *testing.T) {
	r := newRig(t)
	r.j.Erase("e_child1")
	expect(t, r.send(t, body(t, snap("e_child1", now))), 410, "rejected", journalapi.ReasonErasedSubject, records.ActErased)
	expect(t, r.send(t, body(t, snap("e_child2", now))), 200, "applied", "", records.ActDelete)
}

func TestTableRowPolicy(t *testing.T) {
	r := newRig(t)
	r.j.Deny("t1")
	expect(t, r.send(t, body(t, snap("e_child1", now))), 403, "rejected", journalapi.ReasonPolicy, records.ActReject)
	r2 := newRig(t)
	r2.j.NoWrites(true)
	expect(t, r2.send(t, []byte(`not even json`)), 403, "rejected", journalapi.ReasonPolicy, records.ActReject)
}

func TestTableRowTooLarge(t *testing.T) {
	r := newRig(t)
	big := snap("e_child1", now)
	big.Records[0].Fields["a"] = json.RawMessage(`"` + strings.Repeat("a", 2000) + `"`)
	expect(t, r.send(t, body(t, big)), 413, "rejected", journalapi.ReasonTooLarge, records.ActReject)
	huge := append([]byte(`{"x":"`), bytes.Repeat([]byte("a"), 9<<20)...)
	huge = append(huge, `"}`...)
	expect(t, r.send(t, huge), 413, "rejected", journalapi.ReasonTooLarge, records.ActReject)
}

func TestTableRowSchema(t *testing.T) {
	r := newRig(t)
	expect(t, r.send(t, []byte(`{"snapshot_id":1}`)), 422, "rejected", journalapi.ReasonSchema, records.ActReject)
	unknownField := bytes.Replace(body(t, snap("e_child1", now)), []byte(`"complete":`), []byte(`"extra":1,"complete":`), 1)
	expect(t, r.send(t, unknownField), 422, "rejected", journalapi.ReasonSchema, records.ActReject)
	wrongOwner := snap("e_child1", now)
	wrongOwner.Scope[0][1] = "other" // A2: owner != source is schema, not policy
	expect(t, r.send(t, body(t, wrongOwner)), 422, "rejected", journalapi.ReasonSchema, records.ActReject)
}

func TestTableRowFutureObservedAt(t *testing.T) {
	r := newRig(t)
	expect(t, r.send(t, body(t, snap("e_child1", now.Add(11*time.Minute)))), 422, "rejected", journalapi.ReasonFutureObservedAt, records.ActReject)
}

func TestTableRowDuplicateKey(t *testing.T) {
	r := newRig(t)
	s := snap("e_child1", now)
	s.Records = append(s.Records, s.Records[0])
	expect(t, r.send(t, body(t, s)), 422, "rejected", journalapi.ReasonDuplicateKey, records.ActReject)
}

func TestTableRowSnapshotIDReused(t *testing.T) {
	r := newRig(t)
	s := snap("e_child1", now)
	expect(t, r.send(t, body(t, s)), 200, "applied", "", records.ActDelete)
	other := snap("e_child2", now)
	other.SnapshotID = s.SnapshotID
	expect(t, r.send(t, body(t, other)), 422, "rejected", journalapi.ReasonSnapshotIDReused, records.ActReject)
	otherType := s
	otherType.RecordType = "t2"
	expect(t, r.send(t, body(t, otherType)), 422, "rejected", journalapi.ReasonSnapshotIDReused, records.ActReject)
}

func TestTableRowUnknownSubject(t *testing.T) {
	r := newRig(t)
	r.j.Unknown("e_child2", true)
	s := snap("e_child2", now)
	expect(t, r.send(t, body(t, s)), 409, "rejected", journalapi.ReasonUnknownSubject, records.ActRetryScope)
	r.j.Unknown("e_child2", false)
	expect(t, r.send(t, body(t, s)), 200, "applied", "", records.ActDelete)
}

func TestTableRowBadRequest(t *testing.T) {
	r := newRig(t)
	b := body(t, snap("e_child1", now))
	for _, req := range []*http.Request{
		mustReq(t, r.j.URL+journalapi.PathRecords+"?on_behalf_of=e_parent1", "application/json", b),
		mustReq(t, r.j.URL+journalapi.PathRecords, "text/plain", b),
	} {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		a := decode(t, resp)
		expect(t, a, 400, "rejected", journalapi.ReasonBadRequest, records.ActRetryAlert)
	}
}

func TestTableRowUnauthenticated(t *testing.T) {
	r := newRig(t)
	b := body(t, snap("e_child1", now))
	for _, auth := range []string{"", "Bearer wrong", "Basic tok-1", "Bearer  tok-1"} {
		req := mustReq(t, r.j.URL+journalapi.PathRecords, "application/json", b)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		} else {
			req.Header.Del("Authorization")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%q: no WWW-Authenticate", auth)
		}
		expect(t, decode(t, resp), 401, "rejected", journalapi.ReasonUnauthenticated, records.ActRetryAlert)
	}
}

func TestTableRowForbidden(t *testing.T) {
	r := newRig(t)
	r.j.Forbid(true)
	expect(t, r.send(t, body(t, snap("e_child1", now))), 403, "rejected", journalapi.ReasonForbidden, records.ActRetryAlert)
}

func TestTableRowNotFound(t *testing.T) {
	r := newRig(t)
	resp, err := http.Get(r.j.URL + journalapi.PathRecords) // wrong method, no token
	if err != nil {
		t.Fatal(err)
	}
	expect(t, decode(t, resp), 404, "rejected", journalapi.ReasonNotFound, records.ActRetryAlert)
	resp, err = http.Post(r.j.URL+"/v1/nope", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, decode(t, resp), 404, "rejected", journalapi.ReasonNotFound, records.ActRetryAlert)
}

func TestTableRowUnavailable(t *testing.T) {
	r := newRig(t)
	r.j.SetReady(false)
	expect(t, r.send(t, body(t, snap("e_child1", now))), 503, "rejected", journalapi.ReasonUnavailable, records.ActRetry)
	r.j.SetReady(true)
	expect(t, r.send(t, body(t, snap("e_child1", now))), 200, "applied", "", records.ActDelete)
}

// At the in-flight limit the journal answers 503 unavailable with
// Retry-After: 1.
func TestTableRowInFlightLimit(t *testing.T) {
	r := newRig(t)
	release := r.j.Block()
	var wg sync.WaitGroup
	for i := 0; i < recordstest.MaxInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := records.NewClient(r.j.URL, func() (string, error) { return "tok-1", nil }, 5*time.Second, nil)
			_, _ = c.Send(context.Background(), body(t, snap("e_child1", now)))
		}()
	}
	// Wait until every slot is held (the blocked requests hold them).
	deadline := time.Now().Add(10 * time.Second)
	for r.j.InFlight() < recordstest.MaxInFlight {
		if time.Now().After(deadline) {
			release()
			t.Fatalf("only %d slots held", r.j.InFlight())
		}
		time.Sleep(5 * time.Millisecond)
	}
	a := r.send(t, body(t, snap("e_child1", now)))
	release()
	wg.Wait()
	expect(t, a, 503, "rejected", journalapi.ReasonUnavailable, records.ActRetry)
	if a.RetryAfter != time.Second {
		t.Fatalf("Retry-After %v", a.RetryAfter)
	}
}

func TestTableRowAuthUnavailable(t *testing.T) {
	r := newRig(t)
	r.j.SetAuthUnavailable(true)
	expect(t, r.send(t, body(t, snap("e_child1", now))), 503, "rejected", journalapi.ReasonAuthUnavailable, records.ActRetry)
}

func TestTableRowInternalAndScript(t *testing.T) {
	r := newRig(t)
	r.j.Next(recordstest.Answer{Code: 500, Reason: journalapi.ReasonInternal},
		recordstest.Answer{Code: 503, Reason: journalapi.ReasonUnavailable, RetryAfter: 120})
	expect(t, r.send(t, body(t, snap("e_child1", now))), 500, "rejected", journalapi.ReasonInternal, records.ActRetry)
	a := r.send(t, body(t, snap("e_child1", now)))
	if a.RetryAfter != 2*time.Minute {
		t.Fatalf("%+v", a)
	}
	if len(r.j.Received()) != 0 {
		t.Fatal("a scripted answer stored a snapshot")
	}
}

func TestLoseNextAnswerAppliesThenDrops(t *testing.T) {
	r := newRig(t)
	r.j.LoseNextAnswer()
	s := snap("e_child1", now)
	if _, err := r.c.Send(context.Background(), body(t, s)); err == nil {
		t.Fatal("a dropped answer is not a transport error")
	}
	expect(t, r.send(t, body(t, s)), 200, "duplicate", "", records.ActDelete)
	if len(r.j.Received()) != 1 {
		t.Fatal("not applied once")
	}
	answers := r.j.Answers()
	if len(answers) != 2 || !answers[0].Drop || answers[1].Status != "duplicate" {
		t.Fatalf("answers %+v", answers)
	}
}

func TestWithoutRegistryOnlyDecodingAndScope(t *testing.T) {
	j := recordstest.New(t, "tok-1")
	c, _ := records.NewClient(j.URL, func() (string, error) { return "tok-1", nil }, 5*time.Second, nil)
	s := snap("e_child1", now)
	s.RecordType = "anything"
	a, _ := c.Send(context.Background(), body(t, s))
	if a.Code != 200 || a.Status != "applied" {
		t.Fatalf("%+v", a)
	}
	s.SnapshotID = "not-a-uuid"
	a, _ = c.Send(context.Background(), body(t, s))
	if a.Code != 422 {
		t.Fatalf("%+v", a)
	}
}

func mustReq(t *testing.T, url, ctype string, b []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer tok-1")
	return req
}

func decode(t *testing.T, resp *http.Response) records.Answer {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r journalapi.Response
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("answer is not JSON: %q", raw)
	}
	return records.Answer{Code: resp.StatusCode, Status: r.Status, Reason: r.Reason}
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }
