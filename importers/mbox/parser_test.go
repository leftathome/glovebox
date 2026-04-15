package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scanAll reads every message from the scanner and returns them. It fails
// the test immediately on a scanner error.
func scanAll(t *testing.T, path string) []*Message {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })

	s := NewScanner(f)
	var out []*Message
	for s.Scan() {
		out = append(out, s.Message())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scanner error on %s: %v", path, err)
	}
	return out
}

func TestScanner_SmallMbox_Count(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	if got, want := len(msgs), 7; got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
}

func TestScanner_SmallMbox_FirstMessage_PlainHeaders(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	m := msgs[0]
	if m.MessageID != "<plain-001@example.com>" {
		t.Errorf("MessageID = %q", m.MessageID)
	}
	if m.From != "alice@example.com" {
		t.Errorf("From = %q, want addr-spec only", m.From)
	}
	if m.Subject != "Plain message one" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.Date.IsZero() {
		t.Errorf("Date is zero, want parsed")
	}
	if m.HeaderParseError != nil {
		t.Errorf("unexpected HeaderParseError: %v", m.HeaderParseError)
	}
	if m.Size != len(m.Raw) {
		t.Errorf("Size %d != len(Raw) %d", m.Size, len(m.Raw))
	}
}

func TestScanner_SmallMbox_MultipartMessage(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	m := msgs[1]
	if m.MessageID != "<multipart-002@example.com>" {
		t.Fatalf("MessageID = %q", m.MessageID)
	}
	// Body must still contain both parts of the multipart message.
	if !bytes.Contains(m.Raw, []byte("Plain text version.")) {
		t.Errorf("multipart body missing plain part")
	}
	if !bytes.Contains(m.Raw, []byte("<p>HTML version.</p>")) {
		t.Errorf("multipart body missing html part")
	}
	if !bytes.Contains(m.Raw, []byte("--BOUNDARY--")) {
		t.Errorf("multipart body missing closing boundary")
	}
	if m.From != "carol@example.com" {
		t.Errorf("From = %q, want carol@example.com", m.From)
	}
}

func TestScanner_SmallMbox_GmailLabels(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	m := msgs[2]
	if m.MessageID != "<gmail-003@example.com>" {
		t.Fatalf("MessageID = %q", m.MessageID)
	}
	want := []string{"INBOX", "IMPORTANT", "Category/Updates"}
	if len(m.GmailLabels) != len(want) {
		t.Fatalf("GmailLabels = %v, want %v", m.GmailLabels, want)
	}
	for i, w := range want {
		if m.GmailLabels[i] != w {
			t.Errorf("GmailLabels[%d] = %q, want %q", i, m.GmailLabels[i], w)
		}
	}
}

func TestScanner_SmallMbox_ListID(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	m := msgs[3]
	if m.MessageID != "<list-004@example.com>" {
		t.Fatalf("MessageID = %q", m.MessageID)
	}
	if m.ListID != "dev.example.com" {
		t.Errorf("ListID = %q, want dev.example.com (angle brackets stripped)", m.ListID)
	}
}

func TestScanner_SmallMbox_ListPostFallback(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	m := msgs[4]
	if m.MessageID != "<listpost-005@example.com>" {
		t.Fatalf("MessageID = %q", m.MessageID)
	}
	// When only List-Post is present we fall back to it; "mailto:" prefix
	// is retained.
	if m.ListID != "mailto:list@example.com" {
		t.Errorf("ListID = %q, want mailto:list@example.com", m.ListID)
	}
}

func TestScanner_SmallMbox_EscapedFromInBody_NotMisSplit(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	m := msgs[5]
	if m.MessageID != "<quotedfrom-006@example.com>" {
		t.Fatalf("MessageID = %q", m.MessageID)
	}
	// The escaped ">From " body line must remain inside this message and
	// must NOT cause a mis-split.
	if !bytes.Contains(m.Raw, []byte(">From somebody@example.com")) {
		t.Errorf("escaped From_ line missing from body; likely mis-split")
	}
	if !bytes.Contains(m.Raw, []byte("Best,")) {
		t.Errorf("message body truncated; likely mis-split")
	}
}

func TestScanner_SmallMbox_FinalMessage(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	m := msgs[6]
	if m.MessageID != "<final-007@example.com>" {
		t.Fatalf("MessageID = %q", m.MessageID)
	}
	if m.Subject != "Final message" {
		t.Errorf("Subject = %q", m.Subject)
	}
}

func TestScanner_SmallMbox_ByteOffsetsMonotonic(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	var prev int64 = -1
	for i, m := range msgs {
		if m.ByteOffset <= prev && i > 0 {
			t.Errorf("ByteOffset not strictly increasing: msg[%d].ByteOffset=%d, prev=%d",
				i, m.ByteOffset, prev)
		}
		prev = m.ByteOffset
	}
	// And: first offset is 0.
	if msgs[0].ByteOffset != 0 {
		t.Errorf("first ByteOffset = %d, want 0", msgs[0].ByteOffset)
	}
}

func TestScanner_SmallMbox_ErrNilAfterSuccess(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "small.mbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	s := NewScanner(f)
	for s.Scan() {
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

func TestScanner_MalformedMbox_HeaderParseErrorAndContinues(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "malformed.mbox"))
	if got, want := len(msgs), 3; got != want {
		t.Fatalf("message count = %d, want %d (malformed should still be emitted)", got, want)
	}

	// First message parses cleanly.
	if msgs[0].HeaderParseError != nil {
		t.Errorf("msgs[0].HeaderParseError = %v, want nil", msgs[0].HeaderParseError)
	}
	if msgs[0].MessageID != "<good-001@example.com>" {
		t.Errorf("msgs[0].MessageID = %q", msgs[0].MessageID)
	}

	// Second message has malformed headers (no colons).
	if msgs[1].HeaderParseError == nil {
		t.Errorf("msgs[1].HeaderParseError = nil, want non-nil for malformed headers")
	}

	// Third message parses cleanly after the malformed one.
	if msgs[2].HeaderParseError != nil {
		t.Errorf("msgs[2].HeaderParseError = %v, want nil", msgs[2].HeaderParseError)
	}
	if msgs[2].MessageID != "<good-003@example.com>" {
		t.Errorf("msgs[2].MessageID = %q", msgs[2].MessageID)
	}
}

func TestScanner_EmptyMbox(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "empty.mbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	s := NewScanner(f)
	if s.Scan() {
		t.Errorf("Scan() = true on empty mbox, want false")
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
	if s.Message() != nil {
		t.Errorf("Message() = %+v, want nil", s.Message())
	}
}

func TestScanner_TrailingData(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "trailing-data.mbox"))
	// Both messages should be emitted; the second one ends mid-body but
	// we still return what we have.
	if got, want := len(msgs), 2; got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
	if msgs[0].MessageID != "<truncated-001@example.com>" {
		t.Errorf("msgs[0].MessageID = %q", msgs[0].MessageID)
	}
	if msgs[1].MessageID != "<truncated-002@example.com>" {
		t.Errorf("msgs[1].MessageID = %q", msgs[1].MessageID)
	}
}

// TestScanner_FromHeaderAddrSpec ensures display names are stripped even
// when the From header contains quoted display names.
func TestScanner_FromHeaderAddrSpec(t *testing.T) {
	msgs := scanAll(t, filepath.Join("testdata", "small.mbox"))
	// msgs[1] has From: "Carol D." <carol@example.com>
	if msgs[1].From != "carol@example.com" {
		t.Errorf("msgs[1].From = %q, want carol@example.com", msgs[1].From)
	}
}

// TestScanner_SetBufferSize exercises the SetBufferSize option.
func TestScanner_SetBufferSize(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "small.mbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	s := NewScanner(f)
	s.SetBufferSize(1024 * 1024) // 1 MiB -- still ample for fixtures
	var count int
	for s.Scan() {
		count++
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v", err)
	}
	if count != 7 {
		t.Errorf("count = %d, want 7", count)
	}
}

// TestScanner_InMemoryReader uses a bytes.Reader to confirm the parser
// works on any io.Reader, not only *os.File.
func TestScanner_InMemoryReader(t *testing.T) {
	src := strings.Join([]string{
		"From MAILER Mon Apr 11 00:00:00 2026",
		"Message-ID: <mem-001@example.com>",
		"From: mem@example.com",
		"Subject: In memory",
		"",
		"Body.",
		"",
		"From MAILER Mon Apr 11 00:01:00 2026",
		"Message-ID: <mem-002@example.com>",
		"From: mem2@example.com",
		"Subject: In memory two",
		"",
		"Body two.",
		"",
	}, "\n")

	s := NewScanner(strings.NewReader(src))
	var ids []string
	for s.Scan() {
		ids = append(ids, s.Message().MessageID)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d messages, want 2: %v", len(ids), ids)
	}
	if ids[0] != "<mem-001@example.com>" || ids[1] != "<mem-002@example.com>" {
		t.Errorf("ids = %v", ids)
	}
}
