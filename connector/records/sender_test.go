package records

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector/records/recordstest"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

type senderRig struct {
	dir    string
	clock  *fakeClock
	j      *recordstest.Journal
	spool  *Spool
	sender *Sender
	m      *testMetrics
	logs   *logBuffer
}

func newSenderRig(t *testing.T) *senderRig {
	t.Helper()
	r := &senderRig{dir: t.TempDir(), clock: newClock(at(15, 0)), m: newTestMetrics(t)}
	r.j = recordstest.New(t, "tok-1")
	r.j.Registry = testRegistry(t)
	r.j.Now = r.clock.Now
	logs, log := newLogBuffer()
	r.logs = logs
	sp, err := OpenSpool(r.dir, testSpoolConfig(), r.m.Metrics, log, r.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	r.spool = sp
	tok := filepath.Join(t.TempDir(), "token")
	mustWrite(t, tok, "tok-1")
	c, err := NewClient(r.j.URL, TokenFile(tok), 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.sender = NewSender(sp, c, r.m.Metrics, testSpoolConfig(), log, r.clock.Now)
	r.sender.jitter = func() float64 { return 0 }
	return r
}

func (r *senderRig) put(t *testing.T, subject, course string, observed time.Time) journalapi.Snapshot {
	t.Helper()
	s := testSnapshot(t, subject, course, observed)
	if err := r.spool.Put(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (r *senderRig) pending(t *testing.T) []string {
	t.Helper()
	es, err := r.spool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	return ids(es)
}

func receivedIDs(j *recordstest.Journal) []string {
	var out []string
	for _, s := range j.Received() {
		out = append(out, s.SnapshotID)
	}
	return out
}

func TestDrainSendsOldestFirstPerScope(t *testing.T) {
	r := newSenderRig(t)
	a1 := r.put(t, "e_child1", "c1", at(13, 0))
	b1 := r.put(t, "e_child1", "c2", at(13, 1))
	a2 := r.put(t, "e_child1", "c1", at(13, 2))
	res := r.sender.Drain(context.Background())
	if res.Sent != 3 || res.Remaining != 0 || !res.NextDue.IsZero() {
		t.Fatalf("result %+v", res)
	}
	if got, want := strings.Join(receivedIDs(r.j), ","), strings.Join([]string{a1.SnapshotID, a2.SnapshotID, b1.SnapshotID}, ","); got != want {
		t.Fatalf("arrival order %s, want %s", got, want)
	}
	for _, a := range r.j.Answers() {
		if a.Status != journalapi.StatusApplied {
			t.Fatalf("answer %+v (late means the order was wrong)", a)
		}
	}
}

func TestDrainAcceptedFilesAreDeleted(t *testing.T) {
	r := newSenderRig(t)
	for i := 0; i < 5; i++ {
		r.put(t, "e_child1", "c1", at(13, i))
	}
	r.j.Next(recordstest.Answer{Code: 200, Status: "applied"}, recordstest.Answer{Code: 200, Status: "duplicate"},
		recordstest.Answer{Code: 200, Status: "late"}, recordstest.Answer{Code: 200, Status: "held"},
		recordstest.Answer{Code: 200, Status: "discarded"})
	if res := r.sender.Drain(context.Background()); res.Sent != 5 {
		t.Fatalf("%+v", res)
	}
	if p := r.pending(t); len(p) != 0 {
		t.Fatalf("pending %v", p)
	}
	if r.m.counter(t, metricAnswers, "action", "delete") != 5 {
		t.Fatal("answers not counted")
	}
}

func TestDrainRejectsOnlyPermanentReasons(t *testing.T) {
	r := newSenderRig(t)
	reasons := []recordstest.Answer{
		{Code: 403, Reason: journalapi.ReasonPolicy}, {Code: 413, Reason: journalapi.ReasonTooLarge},
		{Code: 422, Reason: journalapi.ReasonSchema}, {Code: 422, Reason: journalapi.ReasonDuplicateKey},
		{Code: 422, Reason: journalapi.ReasonSnapshotIDReused},
	}
	for i := range reasons {
		r.put(t, "e_child1", "c1", at(13, i))
	}
	// One more, refused by the fake's own validation: observed_at ahead.
	r.put(t, "e_child1", "c1", r.clock.Now().Add(11*time.Minute))
	keep := r.put(t, "e_child1", "c2", at(14, 0))
	r.j.Next(reasons...)
	res := r.sender.Drain(context.Background())
	if res.Sent != 1 || strings.Join(r.pending(t), ",") != "" {
		t.Fatalf("%+v pending %v", res, r.pending(t))
	}
	if got := receivedIDs(r.j); len(got) != 1 || got[0] != keep.SnapshotID {
		t.Fatalf("received %v", got)
	}
	if n := listNames(t, rejectedDir(r.dir)); len(n) != 6 {
		t.Fatalf("rejected %v", n)
	}
	for _, reason := range []string{"policy", "too_large", "schema", "duplicate_key", "snapshot_id_reused", "future_observed_at"} {
		if r.m.counter(t, metricRejected, "reason", reason) != 1 {
			t.Errorf("reason %s not counted", reason)
		}
	}
}

func TestDrainUnknownSubjectBlocksOnlyThatScope(t *testing.T) {
	r := newSenderRig(t)
	r.j.Unknown("e_child2", true)
	b1 := r.put(t, "e_child2", "c1", at(13, 0))
	b2 := r.put(t, "e_child2", "c1", at(13, 1))
	a1 := r.put(t, "e_child1", "c1", at(13, 2))
	res := r.sender.Drain(context.Background())
	if res.Sent != 1 || res.Remaining != 2 || !res.NextDue.Equal(r.clock.Now().Add(30*time.Second)) {
		t.Fatalf("%+v", res)
	}
	if got := receivedIDs(r.j); len(got) != 1 || got[0] != a1.SnapshotID {
		t.Fatalf("received %v", got)
	}
	// The blocked scope's second file was not sent.
	if n := len(r.j.Answers()); n != 2 {
		t.Fatalf("%d answers, want 2", n)
	}
	// Still blocked before its backoff ends.
	r.clock.Advance(29 * time.Second)
	r.sender.Drain(context.Background())
	if n := len(r.j.Answers()); n != 2 {
		t.Fatalf("blocked scope retried early: %d answers", n)
	}
	// Second 409: the scope's backoff doubles.
	r.clock.Advance(2 * time.Second)
	res = r.sender.Drain(context.Background())
	if !res.NextDue.Equal(r.clock.Now().Add(60 * time.Second)) {
		t.Fatalf("second backoff %+v", res)
	}
	r.j.Unknown("e_child2", false)
	r.clock.Advance(61 * time.Second)
	r.sender.Drain(context.Background())
	got := receivedIDs(r.j)
	if strings.Join(got, ",") != strings.Join([]string{a1.SnapshotID, b1.SnapshotID, b2.SnapshotID}, ",") {
		t.Fatalf("received %v", got)
	}
	if len(r.spool.unknown) != 0 {
		t.Fatal("unknown mark kept after delivery")
	}
}

func TestDrainUnavailableStopsThePass(t *testing.T) {
	r := newSenderRig(t)
	r.put(t, "e_child1", "c1", at(13, 0))
	r.put(t, "e_child2", "c1", at(13, 1))
	r.j.SetReady(false)
	res := r.sender.Drain(context.Background())
	if n := len(r.j.Answers()); n != 1 {
		t.Fatalf("%d sends after a 503, want 1", n)
	}
	if res.NextDue.Before(r.clock.Now().Add(30*time.Second)) || res.Remaining != 2 {
		t.Fatalf("%+v", res)
	}
	r.j.SetReady(true)
	r.sender.Drain(context.Background())
	if n := len(r.j.Answers()); n != 1 {
		t.Fatal("sent during the global backoff")
	}
	r.clock.Advance(30 * time.Second)
	if res := r.sender.Drain(context.Background()); res.Sent != 2 {
		t.Fatalf("%+v", res)
	}
}

func TestDrainHonoursRetryAfter(t *testing.T) {
	r := newSenderRig(t)
	r.put(t, "e_child1", "c1", at(13, 0))
	r.j.Next(recordstest.Answer{Code: 503, Reason: journalapi.ReasonUnavailable, RetryAfter: 120})
	res := r.sender.Drain(context.Background())
	if !res.NextDue.Equal(r.clock.Now().Add(120 * time.Second)) {
		t.Fatalf("%+v", res)
	}
	// A Retry-After shorter than the backoff does not shorten it.
	r.clock.Advance(120 * time.Second)
	r.j.Next(recordstest.Answer{Code: 503, Reason: journalapi.ReasonUnavailable, RetryAfter: 1})
	res = r.sender.Drain(context.Background())
	if !res.NextDue.Equal(r.clock.Now().Add(60 * time.Second)) {
		t.Fatalf("%+v", res)
	}
}

func TestDrainDeploymentFaultsAreKeptAndCounted(t *testing.T) {
	cases := map[string]func(r *senderRig){
		"401":           func(r *senderRig) { r.j.Token = "other" },
		"403 forbidden": func(r *senderRig) { r.j.Forbid(true) },
		"400": func(r *senderRig) {
			r.j.Next(recordstest.Answer{Code: 400, Reason: journalapi.ReasonBadRequest})
		},
		"404": func(r *senderRig) {
			r.j.Next(recordstest.Answer{Code: 404, Reason: journalapi.ReasonNotFound})
		},
		"unknown reason": func(r *senderRig) {
			r.j.Next(recordstest.Answer{Code: 422, Reason: "brand_new_reason"})
		},
		"200 rejected": func(r *senderRig) {
			r.j.Next(recordstest.Answer{Code: 200, Status: "rejected"})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newSenderRig(t)
			s := r.put(t, "e_child1", "c1", at(13, 0))
			r.put(t, "e_child2", "c1", at(13, 1))
			setup(r)
			res := r.sender.Drain(context.Background())
			if len(r.pending(t)) != 2 || res.NextDue.IsZero() || len(r.j.Answers()) != 1 {
				t.Fatalf("%+v pending %v", res, r.pending(t))
			}
			if n := listNames(t, rejectedDir(r.dir)); len(n) != 0 {
				t.Fatalf("rejected %v", n)
			}
			if r.m.counter(t, metricAnswers, "action", "retry_alert") != 1 {
				t.Fatal("not counted as retry_alert")
			}
			if !strings.Contains(r.logs.String(), `"level":"ERROR"`) {
				t.Fatal("not logged as an error")
			}
			if strings.Contains(r.logs.String(), s.SnapshotID) {
				t.Fatal("a log line names the file")
			}
		})
	}
}

func TestDrainBackoffGrowsAndResets(t *testing.T) {
	r := newSenderRig(t)
	r.sender.cfg.MaxBackoffSeconds = 100
	r.put(t, "e_child1", "c1", at(13, 0))
	r.j.SetReady(false)
	for _, want := range []time.Duration{30 * time.Second, 60 * time.Second, 100 * time.Second, 100 * time.Second} {
		res := r.sender.Drain(context.Background())
		if got := res.NextDue.Sub(r.clock.Now()); got != want {
			t.Fatalf("backoff %v, want %v", got, want)
		}
		r.clock.Advance(want)
	}
	r.j.SetReady(true)
	r.sender.Drain(context.Background())
	r.put(t, "e_child1", "c1", at(13, 1))
	r.j.SetReady(false)
	if res := r.sender.Drain(context.Background()); res.NextDue.Sub(r.clock.Now()) != 30*time.Second {
		t.Fatalf("not reset: %+v", res)
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	r := newSenderRig(t)
	for _, j := range []float64{-0.2, 0.2} {
		r.sender.jitter = func() float64 { return j }
		if got, want := r.sender.backoff(1, 0), time.Duration(float64(30*time.Second)*(1+j)); got != want {
			t.Errorf("jitter %v: %v, want %v", j, got, want)
		}
	}
	for i := 0; i < 1000; i++ {
		if v := NewSender(nil, nil, nil, testSpoolConfig(), nil, nil).jitter(); v < -0.2 || v > 0.2 {
			t.Fatalf("jitter %v", v)
		}
	}
	r.sender.jitter = func() float64 { return 0 }
	if r.sender.backoff(500, 0) != time.Hour {
		t.Fatal("a large step overflows the cap")
	}
}

func TestDrainTransportErrorIsRetried(t *testing.T) {
	r := newSenderRig(t)
	r.put(t, "e_child1", "c1", at(13, 0))
	r.j.Close()
	res := r.sender.Drain(context.Background())
	if len(r.pending(t)) != 1 || res.NextDue.IsZero() {
		t.Fatalf("%+v", res)
	}
	if r.m.counter(t, metricAnswers, "code", "0", "action", "retry") != 1 {
		t.Fatal("transport error not counted")
	}
}

func TestDrainResendAfterLostAnswerIsDuplicate(t *testing.T) {
	r := newSenderRig(t)
	s := r.put(t, "e_child1", "c1", at(13, 0))
	r.j.LoseNextAnswer()
	r.sender.Drain(context.Background())
	if len(r.pending(t)) != 1 {
		t.Fatal("file deleted without an answer")
	}
	r.clock.Advance(time.Minute)
	r.sender.Drain(context.Background())
	if len(r.pending(t)) != 0 {
		t.Fatal("not deleted after the duplicate")
	}
	answers := r.j.Answers()
	if len(answers) != 2 || answers[1].Status != journalapi.StatusDuplicate {
		t.Fatalf("answers %+v", answers)
	}
	if got := receivedIDs(r.j); len(got) != 1 || got[0] != s.SnapshotID {
		t.Fatalf("applied %v", got)
	}
}

func TestDrainErasedSubjectPurgesItsFiles(t *testing.T) {
	r := newSenderRig(t)
	r.j.Erase("e_child1")
	rej := r.put(t, "e_child1", "c9", at(12, 0))
	es, _ := r.spool.Pending()
	_ = r.spool.Reject(es[0], "schema")
	r.put(t, "e_child1", "c1", at(13, 0))
	r.put(t, "e_child1", "c1", at(13, 1))
	r.put(t, "e_child1", "c2", at(13, 2))
	keep := r.put(t, "e_child2", "c1", at(13, 3))
	res := r.sender.Drain(context.Background())
	if res.Sent != 1 || len(r.pending(t)) != 0 {
		t.Fatalf("%+v pending %v", res, r.pending(t))
	}
	if n := len(r.j.Answers()); n != 2 {
		t.Fatalf("%d answers: the subject's other files were sent", n)
	}
	if got := receivedIDs(r.j); len(got) != 1 || got[0] != keep.SnapshotID {
		t.Fatalf("received %v", got)
	}
	if n := listNames(t, rejectedDir(r.dir)); len(n) != 0 {
		t.Fatalf("rejected file of the erased subject kept: %v (%s)", n, rej.SnapshotID)
	}
	if r.m.counter(t, metricDropped, "reason", DropErasedSubject) != 4 {
		t.Fatal("not counted")
	}
}

func TestDrainStopsWithItsContext(t *testing.T) {
	r := newSenderRig(t)
	r.put(t, "e_child1", "c1", at(13, 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := r.sender.Drain(ctx)
	if len(r.j.Answers()) != 0 || len(r.pending(t)) != 1 || !r.sender.globalUntil.IsZero() {
		t.Fatalf("%+v", res)
	}
}

// max_age is applied by the pass itself, with no Put to trigger it: a
// scope the journal keeps answering 409 expires instead of being resent.
func TestDrainAppliesMaxAgeWithoutPuts(t *testing.T) {
	r := newSenderRig(t)
	r.j.Unknown("e_child1", true)
	r.put(t, "e_child1", "c1", at(13, 0))
	r.sender.Drain(context.Background())
	r.clock.Advance(8 * 24 * time.Hour)
	r.sender.Drain(context.Background())
	if n := len(r.j.Answers()); n != 1 {
		t.Fatalf("%d answers: an expired file was sent", n)
	}
	if p := r.pending(t); len(p) != 0 {
		t.Fatalf("pending %v", p)
	}
	if r.m.counter(t, metricDropped, "reason", DropUnknownSubjectExpired) != 1 {
		t.Fatal("not counted")
	}
}

// ... and during a global backoff, which can outlast max_age.
func TestDrainAppliesMaxAgeDuringBackoff(t *testing.T) {
	r := newSenderRig(t)
	r.spool.cfg.MaxAgeHours = 1
	r.put(t, "e_child1", "c1", at(14, 30))
	r.j.Next(recordstest.Answer{Code: 503, Reason: journalapi.ReasonUnavailable, RetryAfter: 7200})
	r.sender.Drain(context.Background())
	r.clock.Advance(time.Hour)
	res := r.sender.Drain(context.Background())
	if res.Sent != 0 || res.Remaining != 0 || len(r.pending(t)) != 0 {
		t.Fatalf("%+v pending %v", res, r.pending(t))
	}
	if r.m.counter(t, metricDropped, "reason", DropMaxAge) != 1 {
		t.Fatal("not counted")
	}
}

// An erase purges its subject only: e_child10 is not e_child1.
func TestDrainErasedSubjectPurgeIsExact(t *testing.T) {
	r := newSenderRig(t)
	r.j.Erase("e_child1")
	r.put(t, "e_child1", "c1", at(13, 0))
	keep := r.put(t, "e_child10", "c1", at(13, 1))
	keepRej := r.put(t, "e_child10", "c9", at(12, 0))
	es, _ := r.spool.Pending()
	if es[0].ID != keepRej.SnapshotID {
		t.Fatalf("order %v", ids(es))
	}
	_ = r.spool.Reject(es[0], "schema")
	r.sender.Drain(context.Background())
	if got := receivedIDs(r.j); len(got) != 1 || got[0] != keep.SnapshotID {
		t.Fatalf("received %v", got)
	}
	if n := listNames(t, rejectedDir(r.dir)); len(n) != 1 {
		t.Fatalf("rejected file of another subject purged: %v", n)
	}
	if r.m.counter(t, metricDropped, "reason", DropErasedSubject) != 1 {
		t.Fatal("dropped count")
	}
	if err := r.spool.Put(testSnapshot(t, "e_child10", "c1", at(13, 2))); err != nil || len(r.pending(t)) != 1 {
		t.Fatal("a later snapshot of another subject was refused")
	}
}

// A 410 for a scope without a data subject deletes that file, once.
func TestDrainErasedWithoutASubjectDeletesTheFile(t *testing.T) {
	r := newSenderRig(t)
	s := NewSnapshot("t1", 1, Scope("src", "s", ""), true, nil, at(13, 0), nil)
	if err := r.spool.Put(s); err != nil {
		t.Fatal(err)
	}
	r.j.Next(recordstest.Answer{Code: 410, Reason: journalapi.ReasonErasedSubject})
	r.sender.Drain(context.Background())
	r.clock.Advance(time.Hour)
	r.sender.Drain(context.Background())
	if n := len(r.j.Answers()); n != 1 {
		t.Fatalf("%d answers: the file was resent", n)
	}
	if p := r.pending(t); len(p) != 0 {
		t.Fatalf("pending %v", p)
	}
	if r.m.counter(t, metricDropped, "reason", DropErasedSubject) != 1 {
		t.Fatal("not counted")
	}
}

// A 409's Retry-After lengthens that scope's backoff.
func TestDrainUnknownSubjectHonoursRetryAfter(t *testing.T) {
	r := newSenderRig(t)
	r.put(t, "e_child1", "c1", at(13, 0))
	r.j.Next(recordstest.Answer{Code: 409, Reason: journalapi.ReasonUnknownSubject, RetryAfter: 300})
	res := r.sender.Drain(context.Background())
	if !res.NextDue.Equal(r.clock.Now().Add(300 * time.Second)) {
		t.Fatalf("%+v", res)
	}
	if b := r.sender.scopes; len(b) != 1 {
		t.Fatalf("scope backoffs %v", b)
	}
}

// slowTransport advances the clock while a request is in flight.
type slowTransport struct {
	clock *fakeClock
	d     time.Duration
}

func (s slowTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.clock.Advance(s.d)
	return http.DefaultTransport.RoundTrip(req)
}

// A backoff runs from the answer, not from the start of the pass.
func TestDrainBackoffStartsAtTheAnswer(t *testing.T) {
	r := newSenderRig(t)
	c, err := NewClient(r.j.URL, TokenFile(newTokenFile(t, "tok-1")), 5*time.Second, slowTransport{r.clock, 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.sender.client = c
	r.put(t, "e_child1", "c1", at(13, 0))
	r.put(t, "e_child2", "c1", at(13, 1))
	r.j.SetAuthUnavailable(true)
	res := r.sender.Drain(context.Background())
	if !res.NextDue.Equal(r.clock.Now().Add(30 * time.Second)) {
		t.Fatalf("global backoff %+v, now %v", res, r.clock.Now())
	}
	r.j.SetAuthUnavailable(false)
	r.sender.globalUntil = time.Time{}
	r.j.Next(recordstest.Answer{Code: 409, Reason: journalapi.ReasonUnknownSubject})
	r.sender.scopes = map[string]scopeBackoff{}
	r.sender.Drain(context.Background())
	if len(r.sender.scopes) != 1 {
		t.Fatalf("scope backoffs %v", r.sender.scopes)
	}
	// The 409 came 10 s before the end of the pass (the next send took 10 s).
	for _, b := range r.sender.scopes {
		if !b.until.Equal(r.clock.Now().Add(20 * time.Second)) {
			t.Fatalf("scope backoff until %v, now %v", b.until, r.clock.Now())
		}
	}
}

// A token file that cannot be read sends nothing and is alerted on as a
// deployment fault, under its own code.
func TestDrainWithoutATokenIsAlerted(t *testing.T) {
	r := newSenderRig(t)
	c, err := NewClient(r.j.URL, TokenFile(newTokenFile(t, " \n")), 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.sender.client = c
	r.put(t, "e_child1", "c1", at(13, 0))
	res := r.sender.Drain(context.Background())
	if len(r.j.Answers()) != 0 || len(r.pending(t)) != 1 || res.NextDue.IsZero() {
		t.Fatalf("%+v", res)
	}
	if r.m.counter(t, metricAnswers, "code", codeToken, "action", "retry_alert") != 1 {
		t.Fatal("not counted as code=token retry_alert")
	}
	if logs := r.logs.String(); !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, `"code":"token"`) {
		t.Fatalf("not logged as an error:\n%s", logs)
	}
}
