package main

// End-to-end integration test for the mbox importer (bead glovebox-544,
// spec 09 §10 success criteria).
//
// The test drives the full importer via runCtx() against a programmatically
// generated fixture archive and an httptest.Server standing in for the
// glovebox ingest endpoint. It exercises:
//
//   - filtering (include/exclude by Gmail label, default-exclude),
//   - destination routing via the config rule matcher,
//   - in-archive dedup by Message-ID,
//   - manifest counters and filter_hit_counts,
//   - second-run no-op (already-complete archive),
//   - interruption + resume across a mid-flight context cancel.
//
// Nothing here uses time.Sleep for synchronization: the ingest mock signals
// receipt over channels and the test cancels/releases deterministically.
// Timeouts are safety nets only.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/staging"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// fixtureSpec describes one message to render into the fixture mbox. The
// fields cover everything the importer's parser, filter, and router consult.
type fixtureSpec struct {
	id        string // Message-ID value (with angle brackets)
	from      string // addr-spec
	subject   string
	label     string // X-Gmail-Labels value; "" means no label header
	listID    string // List-Id value; "" means none
	date      time.Time
	bodyLines int  // number of filler body lines (size variation)
	malformed bool // inject a colon-less header line so net/mail.ReadMessage fails
}

// fixtureSpecs is the ordered set of messages written into the fixture.
//
// Routing/filter intent (see filterJSON + configJSON below):
//   - INBOX, Work  -> included and routed to inbox-agent / work-agent
//   - Spam, Category/Promotions -> excluded by an explicit rule
//   - Personal     -> excluded by the implicit default (no matching rule)
//
// All Message-IDs here are unique. One INBOX message is malformed (net/mail
// fails) but its headers are recoverable by the parser's forgiving fallback,
// so it still routes and ingests normally. Cross-run dedup is covered by its
// own fixture in TestE2E_DedupAcrossResume.
//
// Includable messages are front-loaded so the interruption sub-test reliably
// cancels while includable work is still in flight.
var fixtureSpecs = []fixtureSpec{
	{id: "<m0@example.com>", from: "alice@example.com", subject: "msg-00 inbox", label: "INBOX", date: mkDate("2024-01-02T08:00:00Z"), bodyLines: 3},
	{id: "<m1@example.com>", from: "bob@work.example.com", subject: "msg-01 inbox", label: "INBOX", date: mkDate("2024-01-03T08:00:00Z"), bodyLines: 12},
	{id: "<m2@example.com>", from: "carol@team.example.com", subject: "msg-02 work", label: "Work", listID: "team.example.com", date: mkDate("2024-01-04T08:00:00Z"), bodyLines: 40},
	{id: "<m3@example.com>", from: "dave@example.com", subject: "msg-03 inbox", label: "INBOX", date: mkDate("2024-01-05T08:00:00Z"), bodyLines: 5},
	{id: "<m4@example.com>", from: "erin@team.example.com", subject: "msg-04 work", label: "Work", date: mkDate("2024-02-01T08:00:00Z"), bodyLines: 8},
	{id: "<m5@example.com>", from: "frank@example.com", subject: "msg-05 inbox", label: "INBOX", date: mkDate("2024-02-02T08:00:00Z"), bodyLines: 2},
	{id: "<s6@example.com>", from: "spammer@spam.example.com", subject: "msg-06 spam", label: "Spam", date: mkDate("2024-02-03T08:00:00Z"), bodyLines: 6},
	{id: "<p7@example.com>", from: "promo@shop.example.com", subject: "msg-07 promo", label: "Category/Promotions", date: mkDate("2024-02-04T08:00:00Z"), bodyLines: 10},
	{id: "<m8@example.com>", from: "grace@example.com", subject: "msg-08 inbox", label: "INBOX", date: mkDate("2024-03-01T08:00:00Z"), bodyLines: 20},
	{id: "<pe9@example.com>", from: "pal@friends.example.com", subject: "msg-09 personal", label: "Personal", date: mkDate("2024-03-02T08:00:00Z"), bodyLines: 4},
	{id: "<m10@example.com>", from: "heidi@team.example.com", subject: "msg-10 work", label: "Work", date: mkDate("2024-03-03T08:00:00Z"), bodyLines: 7},
	{id: "<s11@example.com>", from: "spammer2@spam.example.com", subject: "msg-11 spam", label: "Spam", date: mkDate("2024-03-04T08:00:00Z"), bodyLines: 3},
	{id: "<m12@example.com>", from: "ivan@example.com", subject: "msg-12 inbox", label: "INBOX", date: mkDate("2024-03-05T08:00:00Z"), bodyLines: 5},
	{id: "<m13@example.com>", from: "judy2@example.com", subject: "msg-13 inbox", label: "INBOX", date: mkDate("2024-03-06T08:00:00Z"), bodyLines: 5},
	{id: "<p14@example.com>", from: "promo2@shop.example.com", subject: "msg-14 promo", label: "Category/Promotions", date: mkDate("2024-03-07T08:00:00Z"), bodyLines: 9},
	{id: "<s15@example.com>", from: "spammer3@spam.example.com", subject: "msg-15 spam", label: "Spam", date: mkDate("2024-04-01T08:00:00Z"), bodyLines: 2},
	{id: "<m16@example.com>", from: "judy@example.com", subject: "msg-16 inbox malformed", label: "INBOX", date: mkDate("2024-04-02T08:00:00Z"), bodyLines: 6, malformed: true},
	{id: "<m17@example.com>", from: "ken@team.example.com", subject: "msg-17 work", label: "Work", date: mkDate("2024-04-03T08:00:00Z"), bodyLines: 30},
	{id: "<pe18@example.com>", from: "mate@friends.example.com", subject: "msg-18 personal", label: "Personal", date: mkDate("2024-04-04T08:00:00Z"), bodyLines: 4},
	{id: "<p19@example.com>", from: "promo3@shop.example.com", subject: "msg-19 promo", label: "Category/Promotions", date: mkDate("2024-05-01T08:00:00Z"), bodyLines: 11},
	{id: "<s20@example.com>", from: "spammer4@spam.example.com", subject: "msg-20 spam", label: "Spam", date: mkDate("2024-05-02T08:00:00Z"), bodyLines: 3},
	{id: "<m21@example.com>", from: "laura@example.com", subject: "msg-21 inbox", label: "INBOX", date: mkDate("2024-05-03T08:00:00Z"), bodyLines: 15},
	{id: "<p22@example.com>", from: "promo4@shop.example.com", subject: "msg-22 promo", label: "Category/Promotions", date: mkDate("2024-05-04T08:00:00Z"), bodyLines: 5},
	{id: "<s23@example.com>", from: "spammer5@spam.example.com", subject: "msg-23 spam", label: "Spam", date: mkDate("2024-05-05T08:00:00Z"), bodyLines: 2},
}

// Derived expectations for the clean full run.
const (
	wantSeen         = 24 // len(fixtureSpecs)
	wantIngested     = 13 // 9 INBOX (incl. malformed) + 4 Work
	wantFiltered     = 11 // 5 Spam + 4 Category/Promotions + 2 Personal
	wantErrored      = 0
	wantDedupSkipped = 0 // all Message-IDs are unique in this fixture
)

func mkDate(rfc3339 string) time.Time {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		panic(err)
	}
	return t
}

// writeE2EFixtureMbox renders fixtureSpecs into a self-contained mbox file and
// returns its path.
func writeE2EFixtureMbox(t *testing.T, dir string) string {
	return writeMbox(t, dir, fixtureSpecs)
}

// writeMbox renders an arbitrary set of specs into a self-contained mbox file
// and returns its path.
func writeMbox(t *testing.T, dir string, specs []fixtureSpec) string {
	t.Helper()
	var b strings.Builder
	for _, s := range specs {
		// From_ separator line (RFC 4155). The parser strips it; the asctime
		// is cosmetic.
		fmt.Fprintf(&b, "From %s %s\n", s.from, s.date.Format(time.ANSIC))
		fmt.Fprintf(&b, "Message-ID: %s\n", s.id)
		fmt.Fprintf(&b, "Date: %s\n", s.date.Format(time.RFC1123Z))
		fmt.Fprintf(&b, "From: Display Name <%s>\n", s.from)
		fmt.Fprintf(&b, "To: recipient@example.com\n")
		fmt.Fprintf(&b, "Subject: %s\n", s.subject)
		if s.label != "" {
			fmt.Fprintf(&b, "X-Gmail-Labels: %s\n", s.label)
		}
		if s.listID != "" {
			fmt.Fprintf(&b, "List-Id: <%s>\n", s.listID)
		}
		if s.malformed {
			// A header line with no colon makes net/mail.ReadMessage fail;
			// the parser's forgiving fallback still recovers the headers
			// above, so the message routes and ingests normally.
			fmt.Fprintf(&b, "This line has no colon and breaks strict header parsing\n")
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "Body of %s.\n", s.subject)
		for i := 0; i < s.bodyLines; i++ {
			fmt.Fprintf(&b, "Filler line %d for size variation in %s.\n", i, s.id)
		}
		// Trailing blank line separating this message from the next From_.
		b.WriteString("\n")
	}
	path := filepath.Join(dir, "fixture.mbox")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write fixture mbox: %v", err)
	}
	return path
}

const filterJSON = `{
  "schema_version": "1.0",
  "filter_rules": [
    {"match": {"label": "INBOX"}, "action": "include"},
    {"match": {"label": "Work"}, "action": "include"},
    {"match": {"label": "Spam"}, "action": "exclude"},
    {"match": {"label": "Category/Promotions"}, "action": "exclude"}
  ]
}`

// configJSON routes the two includable labels to distinct destination agents.
// There is no wildcard rule: a message that survives the filter but matches no
// rule (e.g. a header-less fragment) fails routing and is counted as errored
// rather than silently delivered.
const configJSON = `{
  "rules": [
    {"match": "label:INBOX", "destination": "inbox-agent", "tags": {"queue": "inbox"}},
    {"match": "label:Work", "destination": "work-agent"}
  ],
  "fetch_limits": {}
}`

const sourceName = "mbox-e2e"

// writeSupportFiles writes the filter + config next to a fresh state dir and
// returns (filterPath, configPath, stateDir).
func writeSupportFiles(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	filterPath := filepath.Join(dir, "filter.json")
	configPath := filepath.Join(dir, "config.json")
	stateDir := filepath.Join(dir, "state")
	if err := os.WriteFile(filterPath, []byte(filterJSON), 0o644); err != nil {
		t.Fatalf("write filter: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(configJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	return filterPath, configPath, stateDir
}

// ---------------------------------------------------------------------------
// Ingest mock
// ---------------------------------------------------------------------------

// receivedItem is the subset of ingest metadata the test asserts against.
type receivedItem struct {
	source        string
	sender        string
	subject       string
	destination   string
	contentType   string
	originArchive string // tags["origin_archive"], unique per source position
	contentLen    int
}

// ingestMock is a thread-safe httptest handler that records every multipart
// POST and returns 202 Accepted. An optional gate blocks the first POST until
// the test releases it, enabling a deterministic mid-flight cancel.
type ingestMock struct {
	mu    sync.Mutex
	items []receivedItem

	gate        bool
	gateArmed   bool
	firstPostCh chan struct{}
	releaseCh   chan struct{}
}

func newIngestMock() *ingestMock {
	return &ingestMock{
		firstPostCh: make(chan struct{}, 1),
		releaseCh:   make(chan struct{}),
	}
}

// enableGate arms the gate so the first POST blocks until release() is called.
func (m *ingestMock) enableGate() {
	m.gate = true
	m.gateArmed = true
}

func (m *ingestMock) release() { close(m.releaseCh) }

func (m *ingestMock) handler(w http.ResponseWriter, r *http.Request) {
	meta, content, err := parseIngestPOST(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	first := false
	if m.gate && m.gateArmed {
		m.gateArmed = false
		first = true
	}
	m.items = append(m.items, receivedItem{
		source:        meta.Source,
		sender:        meta.Sender,
		subject:       meta.Subject,
		destination:   meta.DestinationAgent,
		contentType:   meta.ContentType,
		originArchive: meta.Tags["origin_archive"],
		contentLen:    len(content),
	})
	m.mu.Unlock()

	if first {
		// Signal receipt, then block until the test releases us. The test
		// cancels the run between these two events.
		m.firstPostCh <- struct{}{}
		<-m.releaseCh
	}

	w.WriteHeader(http.StatusAccepted)
}

func (m *ingestMock) snapshot() []receivedItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]receivedItem, len(m.items))
	copy(out, m.items)
	return out
}

func (m *ingestMock) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// parseIngestPOST mirrors what the real ingest endpoint reads: a
// multipart/form-data body with a JSON "metadata" part and a "content" part.
func parseIngestPOST(r *http.Request) (staging.ItemMetadata, []byte, error) {
	var meta staging.ItemMetadata
	var content []byte

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return meta, nil, fmt.Errorf("unexpected content type %q: %v", r.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	gotMeta := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return meta, nil, fmt.Errorf("read part: %w", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return meta, nil, fmt.Errorf("read part body: %w", err)
		}
		switch part.FormName() {
		case "metadata":
			if err := json.Unmarshal(data, &meta); err != nil {
				return meta, nil, fmt.Errorf("decode metadata: %w", err)
			}
			gotMeta = true
		case "content":
			content = data
		}
	}
	if !gotMeta {
		return meta, nil, fmt.Errorf("missing metadata part")
	}
	return meta, content, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// freePort returns a currently-free TCP port. Each run() invocation binds its
// own health server, so a fresh port per call avoids collisions across the
// sequential runs in this test (HealthPort==0 would mean 8080, not "random").
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type runArgs struct {
	source      string
	filter      string
	config      string
	stateDir    string
	ingestURL   string
	concurrency int
	healthPort  int
}

func (a runArgs) slice() []string {
	return []string{
		"--source", a.source,
		"--filter", a.filter,
		"--config", a.config,
		"--state-dir", a.stateDir,
		"--ingest-url", a.ingestURL,
		"--source-name", sourceName,
		"--concurrency", fmt.Sprintf("%d", a.concurrency),
		"--health-port", fmt.Sprintf("%d", a.healthPort),
	}
}

func loadManifestForSource(t *testing.T, source string) *ImportManifestV1 {
	t.Helper()
	mf, err := LoadManifest(ManifestPath(source))
	if err != nil {
		t.Fatalf("load manifest %s: %v", ManifestPath(source), err)
	}
	return mf
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestE2E_FullRun_FilterRouteDedupManifest exercises a single clean import and
// asserts the ingest count, manifest counters, and filter_hit_counts.
func TestE2E_FullRun_FilterRouteDedupManifest(t *testing.T) {
	dir := t.TempDir()
	source := writeE2EFixtureMbox(t, dir)
	filterPath, configPath, stateDir := writeSupportFiles(t, dir)

	mock := newIngestMock()
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	code := runCtx(context.Background(), runArgs{
		source:      source,
		filter:      filterPath,
		config:      configPath,
		stateDir:    stateDir,
		ingestURL:   srv.URL,
		concurrency: 4,
		healthPort:  freePort(t),
	}.slice())
	if code != 0 {
		t.Fatalf("runCtx exit code = %d, want 0", code)
	}

	// 1) Ingest count observed by the mock.
	if got := mock.count(); got != wantIngested {
		t.Errorf("mock received %d POSTs, want %d", got, wantIngested)
	}

	// 2) Manifest counters.
	mf := loadManifestForSource(t, source)
	if string(mf.StatusValue) != "complete" {
		t.Errorf("manifest status = %q, want complete", mf.StatusValue)
	}
	if mf.Counts.MessagesSeen != wantSeen {
		t.Errorf("MessagesSeen = %d, want %d", mf.Counts.MessagesSeen, wantSeen)
	}
	if mf.Counts.MessagesIngested != wantIngested {
		t.Errorf("MessagesIngested = %d, want %d", mf.Counts.MessagesIngested, wantIngested)
	}
	if mf.Counts.MessagesFiltered != wantFiltered {
		t.Errorf("MessagesFiltered = %d, want %d", mf.Counts.MessagesFiltered, wantFiltered)
	}
	if mf.Counts.MessagesErrored != wantErrored {
		t.Errorf("MessagesErrored = %d, want %d", mf.Counts.MessagesErrored, wantErrored)
	}
	if mf.Counts.MessagesDedupSkipped != wantDedupSkipped {
		t.Errorf("MessagesDedupSkipped = %d, want %d", mf.Counts.MessagesDedupSkipped, wantDedupSkipped)
	}

	// 3) filter_hit_counts keyed per spec ruleKeyFor().
	wantHits := map[string]int{
		"rule_2_label_Spam":                5,
		"rule_3_label_Category/Promotions": 4,
		"default_excluded":                 2,
	}
	for k, want := range wantHits {
		if got := mf.FilterHitCounts[k]; got != want {
			t.Errorf("FilterHitCounts[%q] = %d, want %d", k, got, want)
		}
	}
	for k := range mf.FilterHitCounts {
		if _, ok := wantHits[k]; !ok {
			t.Errorf("unexpected filter_hit_counts key %q = %d", k, mf.FilterHitCounts[k])
		}
	}

	// 4) MessageIDsIngested holds exactly the unique ingested ids.
	if got := len(mf.MessageIDsIngested); got != wantIngested {
		t.Errorf("len(MessageIDsIngested) = %d, want %d", got, wantIngested)
	}

	// 5) Per-item metadata sanity: every POST routed to a known destination,
	//    carried the right source + content type, and a non-empty body.
	for _, it := range mock.snapshot() {
		if it.source != sourceName {
			t.Errorf("item %q source = %q, want %q", it.subject, it.source, sourceName)
		}
		if it.contentType != "message/rfc822" {
			t.Errorf("item %q content_type = %q, want message/rfc822", it.subject, it.contentType)
		}
		if it.destination != "inbox-agent" && it.destination != "work-agent" {
			t.Errorf("item %q destination = %q, want inbox-agent or work-agent", it.subject, it.destination)
		}
		if it.sender == "" {
			t.Errorf("item %q has empty sender", it.subject)
		}
		if it.originArchive == "" || !strings.HasPrefix(it.originArchive, sourceName+":") {
			t.Errorf("item %q origin_archive = %q, want %s:<offset>", it.subject, it.originArchive, sourceName)
		}
		if it.contentLen == 0 {
			t.Errorf("item %q delivered empty content", it.subject)
		}
	}

	// 6) The malformed message still made it through (best-effort parse).
	if !hasSubject(mock.snapshot(), "msg-16 inbox malformed") {
		t.Errorf("malformed message msg-16 was not ingested; parser should recover it best-effort")
	}
}

// TestE2E_SecondRun_NoAdditionalIngest verifies that re-running the importer
// against an already-completed archive (same source + state dir) delivers zero
// additional items. The mechanism is the resume table's ExitComplete branch:
// a manifest with status=complete short-circuits before any work.
func TestE2E_SecondRun_NoAdditionalIngest(t *testing.T) {
	dir := t.TempDir()
	source := writeE2EFixtureMbox(t, dir)
	filterPath, configPath, stateDir := writeSupportFiles(t, dir)

	mock := newIngestMock()
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	args := func() []string {
		return runArgs{
			source:      source,
			filter:      filterPath,
			config:      configPath,
			stateDir:    stateDir,
			ingestURL:   srv.URL,
			concurrency: 4,
			healthPort:  freePort(t),
		}.slice()
	}

	if code := runCtx(context.Background(), args()); code != 0 {
		t.Fatalf("first run exit code = %d, want 0", code)
	}
	afterFirst := mock.count()
	if afterFirst != wantIngested {
		t.Fatalf("after first run mock count = %d, want %d", afterFirst, wantIngested)
	}

	if code := runCtx(context.Background(), args()); code != 0 {
		t.Fatalf("second run exit code = %d, want 0", code)
	}
	if got := mock.count(); got != afterFirst {
		t.Errorf("second run added %d POSTs, want 0 (total %d)", got-afterFirst, got)
	}
}

// TestE2E_InterruptResume cancels a run mid-flight, then re-runs to completion
// and asserts delivery consistency.
//
// Determinism: the ingest mock gates the first POST. With --concurrency=1 only
// one POST is ever in flight; the test cancels the context while that first
// POST is blocked, then releases it. So run 1 delivers exactly one item, then
// the producer/worker drain and the manifest is flushed at status=interrupted.
//
// AT-LEAST-ONCE across a hard mid-flight cancel (glovebox-92qr). The resume
// point is a LOW-WATER mark -- the earliest byte offset still in flight (scanned
// + dispatched to the pool but not yet confirmed ingested) -- so messages that
// were queued/in-flight when the cancel landed are re-read on resume and
// re-attempted; cross-run dedup (seeded from manifest.message_ids_ingested)
// skips the ones already confirmed. This test asserts:
//   - the interrupted run leaves a resumable manifest (status=interrupted,
//     non-zero byte offset, preserved Message-IDs),
//   - the resumed run completes (status=complete),
//   - EVERY includable message is delivered across the two runs (at-least-once:
//     no message dropped by the cancel), and
//   - no source position is delivered more than once and every delivered item
//     is a real includable message (exactly-once for this all-unique-ID
//     fixture; no spurious deliveries).
func TestE2E_InterruptResume(t *testing.T) {
	dir := t.TempDir()
	source := writeE2EFixtureMbox(t, dir)
	filterPath, configPath, stateDir := writeSupportFiles(t, dir)

	mock := newIngestMock()
	mock.enableGate()
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	// --- Run 1: cancel mid-flight -----------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	args1 := runArgs{
		source:      source,
		filter:      filterPath,
		config:      configPath,
		stateDir:    stateDir,
		ingestURL:   srv.URL,
		concurrency: 1,
		healthPort:  freePort(t),
	}.slice()
	go func() { done <- runCtx(ctx, args1) }()

	// Wait for the first POST to arrive (and block in the gate), then cancel
	// the run and release the gate so the blocked POST returns 202.
	select {
	case <-mock.firstPostCh:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("timed out waiting for first ingest POST")
	}
	cancel()
	mock.release()

	select {
	case code := <-done:
		// A cancelled run is a clean exit per main.go (so the next run resumes).
		if code != 0 {
			t.Fatalf("interrupted run exit code = %d, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for interrupted run to return")
	}

	run1Items := mock.snapshot()
	if len(run1Items) != 1 {
		t.Fatalf("run 1 delivered %d items, want exactly 1 (gated first POST)", len(run1Items))
	}

	mf1 := loadManifestForSource(t, source)
	if string(mf1.StatusValue) != "interrupted" {
		t.Errorf("after interrupt, manifest status = %q, want interrupted", mf1.StatusValue)
	}
	if mf1.ResumeState.ByteOffset <= 0 {
		t.Errorf("after interrupt, resume byte offset = %d, want > 0", mf1.ResumeState.ByteOffset)
	}
	if mf1.Counts.MessagesIngested != 1 {
		t.Errorf("after interrupt, MessagesIngested = %d, want 1", mf1.Counts.MessagesIngested)
	}
	if len(mf1.MessageIDsIngested) != 1 {
		t.Errorf("after interrupt, len(MessageIDsIngested) = %d, want 1", len(mf1.MessageIDsIngested))
	}

	// --- Run 2: resume to completion --------------------------------------
	args2 := runArgs{
		source:      source,
		filter:      filterPath,
		config:      configPath,
		stateDir:    stateDir,
		ingestURL:   srv.URL,
		concurrency: 4,
		healthPort:  freePort(t),
	}.slice()
	if code := runCtx(context.Background(), args2); code != 0 {
		t.Fatalf("resume run exit code = %d, want 0", code)
	}

	mf2 := loadManifestForSource(t, source)
	if string(mf2.StatusValue) != "complete" {
		t.Errorf("after resume, manifest status = %q, want complete", mf2.StatusValue)
	}

	// Build the set of includable source subjects for spurious-delivery check.
	includable := map[string]bool{}
	for _, s := range fixtureSpecs {
		if s.label == "INBOX" || s.label == "Work" {
			includable[s.subject] = true
		}
	}

	all := mock.snapshot()
	// Uniqueness is keyed on subject (a stable per-message identity). The
	// origin_archive byte-offset tag is also asserted unique below: the scanner
	// reports absolute archive offsets even after the resume seek (glovebox-gtxt),
	// so a message ingested in run 2 carries its true archive position, distinct
	// from every run-1 tag.
	seenSubject := map[string]int{}
	seenOrigin := map[string]int{}
	for _, it := range all {
		seenSubject[it.subject]++
		seenOrigin[it.originArchive]++
		if !includable[it.subject] {
			t.Errorf("spurious delivery: subject %q (origin %q) is not an includable message",
				it.subject, it.originArchive)
		}
	}

	// origin_archive provenance must be absolute and unique per delivered
	// message across the interrupt + resume. A relative (restart-at-0) offset
	// after the resume would collide with a run-1 tag and surface here.
	for _, it := range all {
		if n := seenOrigin[it.originArchive]; n != 1 {
			t.Errorf("origin_archive %q delivered %d times, want unique per message "+
				"(relative offset after resume?)", it.originArchive, n)
		}
	}

	// At-least-once (glovebox-92qr): every includable message must be delivered
	// across the interrupt + resume -- none dropped by the mid-flight cancel.
	for subj := range includable {
		if seenSubject[subj] == 0 {
			t.Errorf("at-least-once violated: includable message %q was never delivered "+
				"(dropped on resume -- producer-ahead resume offset?)", subj)
		}
	}

	// At-most-once: dedup (Message-ID, seeded from the manifest) + low-water
	// resume means no message is delivered twice across the two runs.
	for subj, n := range seenSubject {
		if n != 1 {
			t.Errorf("message %q delivered %d times, want exactly 1", subj, n)
		}
	}

	delivered := len(seenSubject)
	t.Logf("interrupt/resume: delivered %d/%d includable messages (run1=%d, run2=%d)",
		delivered, wantIngested, len(run1Items), len(all)-len(run1Items))

	if delivered != wantIngested {
		t.Errorf("delivered %d distinct includable messages, want %d (exactly-once across interrupt+resume)",
			delivered, wantIngested)
	}
}

// dedupSpecs is a fixture for cross-run dedup: idx0 and idx6 share Message-ID
// <dupkey@example.com>. The interrupt run ingests idx0 (recording the id in
// the manifest); the resumed run re-reads forward, reaches idx6, and skips it
// because the dedup set was seeded from the persisted manifest. idx6 sits well
// past the worst-case resume offset (end of idx3) so it is always re-read.
var dedupSpecs = []fixtureSpec{
	{id: "<dupkey@example.com>", from: "a@example.com", subject: "dd-A first", label: "INBOX", date: mkDate("2024-01-01T08:00:00Z"), bodyLines: 3},
	{id: "<dd1@example.com>", from: "b@example.com", subject: "dd-1", label: "INBOX", date: mkDate("2024-01-02T08:00:00Z"), bodyLines: 3},
	{id: "<dd2@example.com>", from: "c@example.com", subject: "dd-2", label: "INBOX", date: mkDate("2024-01-03T08:00:00Z"), bodyLines: 3},
	{id: "<dd3@example.com>", from: "d@example.com", subject: "dd-3", label: "INBOX", date: mkDate("2024-01-04T08:00:00Z"), bodyLines: 3},
	{id: "<dd4@example.com>", from: "e@example.com", subject: "dd-4", label: "INBOX", date: mkDate("2024-01-05T08:00:00Z"), bodyLines: 3},
	{id: "<dd5@example.com>", from: "f@example.com", subject: "dd-5", label: "INBOX", date: mkDate("2024-01-06T08:00:00Z"), bodyLines: 3},
	{id: "<dupkey@example.com>", from: "a@example.com", subject: "dd-Z duplicate", label: "INBOX", date: mkDate("2024-01-07T08:00:00Z"), bodyLines: 3},
	{id: "<dd7@example.com>", from: "g@example.com", subject: "dd-7", label: "INBOX", date: mkDate("2024-01-08T08:00:00Z"), bodyLines: 3},
}

// TestE2E_DedupAcrossResume verifies dedup-by-Message-ID persisted across runs
// (spec 09 §3.7): a message whose Message-ID was ingested by a prior run is
// skipped when re-read by a resumed run, because RunOneShot seeds the dedup set
// from manifest.message_ids_ingested before scanning resumes.
//
// (Note: in-archive dedup of two near-adjacent duplicates within a SINGLE run
// is best-effort, not guaranteed -- the producer can scan the second copy
// before the collector has recorded the first as ingested. The reliable,
// deterministic dedup guarantee is the cross-run one exercised here.)
func TestE2E_DedupAcrossResume(t *testing.T) {
	dir := t.TempDir()
	source := writeMbox(t, dir, dedupSpecs)
	filterPath, configPath, stateDir := writeSupportFiles(t, dir)

	mock := newIngestMock()
	mock.enableGate()
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	// Run 1: cancel right after the first POST (id <dupkey@example.com>).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	args1 := runArgs{
		source: source, filter: filterPath, config: configPath, stateDir: stateDir,
		ingestURL: srv.URL, concurrency: 1, healthPort: freePort(t),
	}.slice()
	go func() { done <- runCtx(ctx, args1) }()

	select {
	case <-mock.firstPostCh:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("timed out waiting for first ingest POST")
	}
	cancel()
	mock.release()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("interrupted run exit code = %d, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for interrupted run to return")
	}

	mf1 := loadManifestForSource(t, source)
	if len(mf1.MessageIDsIngested) != 1 || mf1.MessageIDsIngested[0] != "<dupkey@example.com>" {
		t.Fatalf("run 1 MessageIDsIngested = %v, want [<dupkey@example.com>]", mf1.MessageIDsIngested)
	}

	// Run 2: resume to completion. The second copy of <dupkey@example.com>
	// (subject "dd-Z duplicate") must be skipped by cross-run dedup.
	args2 := runArgs{
		source: source, filter: filterPath, config: configPath, stateDir: stateDir,
		ingestURL: srv.URL, concurrency: 4, healthPort: freePort(t),
	}.slice()
	if code := runCtx(context.Background(), args2); code != 0 {
		t.Fatalf("resume run exit code = %d, want 0", code)
	}

	mf2 := loadManifestForSource(t, source)
	if string(mf2.StatusValue) != "complete" {
		t.Errorf("after resume, manifest status = %q, want complete", mf2.StatusValue)
	}
	if mf2.Counts.MessagesDedupSkipped < 1 {
		t.Errorf("MessagesDedupSkipped = %d, want >= 1 (the re-read <dupkey@example.com>)", mf2.Counts.MessagesDedupSkipped)
	}
	if hasSubject(mock.snapshot(), "dd-Z duplicate") {
		t.Errorf("duplicate-Message-ID message dd-Z was delivered; cross-run dedup failed")
	}
}

func hasSubject(items []receivedItem, subj string) bool {
	for _, it := range items {
		if it.subject == subj {
			return true
		}
	}
	return false
}
