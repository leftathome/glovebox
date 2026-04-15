// Package main implements the mbox importer.
//
// This file provides a streaming mbox parser that yields parsed messages
// with extracted headers. See docs/specs/09-mbox-importer-design.md §3.2
// for the design.
//
// Split behavior: the scanner treats any line starting with "From " (capital
// F, "From", space) as a message separator, per RFC 4155. In strict mbox
// format, such lines inside a body are escaped as ">From "; however,
// real-world mboxes do not always honor this. This parser matches the
// behavior of standard bufio.Scanner-based mbox readers and does not attempt
// to distinguish "From " lines that appear to be quoted email bodies from
// true separators. If a user's archive produces mis-splits in practice, a
// heuristic (e.g. requiring a header-like second line) can be added later.
package main

import (
	"bufio"
	"bytes"
	"io"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// DefaultBufferSize is the default maximum token size for the mbox scanner.
// Large messages (notably those with inline attachments) can exceed the
// stdlib bufio.Scanner default of 64 KiB, so we default to 64 MiB.
const DefaultBufferSize = 64 * 1024 * 1024

// initialBufferSize is the starting capacity of the bufio.Scanner buffer.
// The scanner will grow this up to DefaultBufferSize on demand.
const initialBufferSize = 64 * 1024

// Message represents a single parsed message from an mbox stream.
type Message struct {
	// Raw is the complete RFC 5322 message bytes (excluding the From_
	// separator line). Consumers receive a freshly-allocated slice they
	// may retain.
	Raw []byte

	// ByteOffset is the position in the mbox where this message starts
	// (the byte offset of the From_ separator line, or 0 for a message
	// at the start of a stream that has no leading separator).
	ByteOffset int64

	// MessageID from the Message-ID header; empty if missing.
	MessageID string

	// Date parsed from the Date header; zero value if missing or
	// unparseable.
	Date time.Time

	// From is the addr-spec only (display name stripped); empty if the
	// From header is missing or unparseable.
	From string

	// Subject from the Subject header.
	Subject string

	// ListID from List-Id or List-Post (List-Id preferred); empty if
	// neither present. Angle brackets are stripped.
	ListID string

	// GmailLabels parsed from X-Gmail-Labels (comma-separated).
	GmailLabels []string

	// Size is len(Raw).
	Size int

	// HeaderParseError is non-nil if the RFC 5322 header block could not be
	// fully parsed by net/mail. Informational only; the message has
	// best-effort extracted headers (via a forgiving manual pass) and
	// should be processed normally. This is not a signal that the message
	// is broken.
	HeaderParseError error
}

// Scanner streams messages from an mbox input.
type Scanner struct {
	r          io.Reader
	bufSize    int
	scanner    *bufio.Scanner
	cur        *Message
	err        error
	nextOffset int64 // running total of bytes consumed so far
	started    bool
}

// NewScanner returns a Scanner that reads from r. Buffer size defaults to
// DefaultBufferSize (64 MiB); use SetBufferSize before the first Scan call
// to change it.
func NewScanner(r io.Reader) *Scanner {
	return &Scanner{
		r:       r,
		bufSize: DefaultBufferSize,
	}
}

// SetBufferSize sets the maximum token size in bytes. Must be called before
// the first call to Scan.
func (s *Scanner) SetBufferSize(bytes int) {
	s.bufSize = bytes
}

// Scan advances to the next message. It returns true if a message is
// available via Message(), and false at EOF or on a fatal error. Check
// Err() for a non-nil fatal error.
func (s *Scanner) Scan() bool {
	if !s.started {
		s.start()
	}
	if s.err != nil {
		return false
	}
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil {
			s.err = err
		}
		return false
	}
	tok := s.scanner.Bytes()

	// The split function returns the entire region including the leading
	// From_ line (if any); we need to record the offset of this region,
	// then strip the From_ line before parsing. The offset tracking is
	// maintained by the split function via a closure (see start()).
	offset := s.nextOffset
	s.nextOffset += int64(len(tok))

	// Split off the From_ separator line if present.
	body := tok
	if bytes.HasPrefix(body, []byte("From ")) {
		if nl := bytes.IndexByte(body, '\n'); nl >= 0 {
			body = body[nl+1:]
		} else {
			// A "From " line with no newline; nothing else in the
			// stream. Treat body as empty.
			body = nil
		}
	}
	// Trim a single trailing blank line that the split function leaves
	// attached (the blank line separates messages and is not part of the
	// message body).
	body = trimTrailingMboxBlank(body)

	// Copy so the caller may retain the slice even after the scanner
	// reuses its internal buffer.
	raw := make([]byte, len(body))
	copy(raw, body)

	msg := &Message{
		Raw:        raw,
		ByteOffset: offset,
		Size:       len(raw),
	}
	parseHeaders(msg)
	s.cur = msg
	return true
}

// Message returns the current message. Valid only after Scan() returns true.
func (s *Scanner) Message() *Message {
	return s.cur
}

// Err returns the first non-EOF fatal error encountered by the Scanner.
func (s *Scanner) Err() error {
	return s.err
}

func (s *Scanner) start() {
	s.started = true
	sc := bufio.NewScanner(s.r)
	// Allocate the buffer at the configured max size. We pass the same
	// value as both initial buffer capacity and max to avoid surprising
	// growth behavior.
	buf := make([]byte, 0, initialBufferSize)
	sc.Buffer(buf, s.bufSize)
	sc.Split(splitMbox)
	s.scanner = sc
}

// splitMbox is a bufio.SplitFunc that splits on lines beginning with
// "From " (at the very start of the stream or after a newline). The token
// returned includes the leading From_ line.
//
// Split behavior for content beginning with "From " inside a message body:
// any such line is treated as a new message separator. This matches the
// behavior of common mbox readers. Bodies that happen to contain "From "
// at a line start will be mis-split; strict mbox format escapes these as
// ">From ", which this scanner then does not mis-split.
func splitMbox(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if len(data) == 0 {
		if atEOF {
			return 0, nil, nil
		}
		return 0, nil, nil
	}

	// Determine the start of the current message. If data begins with
	// "From " we include it; otherwise the data up to the next "From "
	// at a line start is a message that had no leading separator (a
	// malformed or truncated mbox start).
	start := 0
	if bytes.HasPrefix(data, []byte("From ")) {
		start = 0
	}

	// Search for the next "\nFrom " after start+1 (so we don't match
	// the current message's own separator). If data does not begin with
	// "From ", we still look for the next "\nFrom " starting from offset
	// 0.
	searchFrom := start + 1
	if searchFrom > len(data) {
		searchFrom = len(data)
	}
	idx := indexLineFrom(data, searchFrom)
	if idx >= 0 {
		// Token is data[start:idx+1] -- includes the trailing newline
		// before the next From_ line. Advance past that newline so the
		// next call sees data starting with "From ".
		return idx + 1, data[start : idx+1], nil
	}
	if atEOF {
		if len(data) == start {
			return 0, nil, nil
		}
		return len(data), data[start:], nil
	}
	// Need more data.
	return 0, nil, nil
}

// indexLineFrom returns the index within data of a newline byte whose
// following bytes are "From " (i.e., the start of a new mbox separator
// line). Searches starting at offset from. Returns -1 if no such position
// exists in data.
func indexLineFrom(data []byte, from int) int {
	i := from
	for i < len(data) {
		nl := bytes.IndexByte(data[i:], '\n')
		if nl < 0 {
			return -1
		}
		pos := i + nl
		// Check if the bytes after the newline begin with "From ".
		if pos+1+5 <= len(data) {
			if bytes.Equal(data[pos+1:pos+1+5], []byte("From ")) {
				return pos
			}
			i = pos + 1
			continue
		}
		// Not enough data after the newline to decide; return -1 so
		// bufio reads more.
		return -1
	}
	return -1
}

// trimTrailingMboxBlank removes a single trailing "\n" or "\r\n" that the
// splitter leaves on the message (the blank line that separates mbox
// messages). It does not strip meaningful blank lines that are part of the
// message itself.
func trimTrailingMboxBlank(b []byte) []byte {
	// mbox convention is that a blank line precedes each From_ line; the
	// splitter includes that blank line in the prior token. Remove it
	// (single trailing newline).
	if len(b) >= 2 && b[len(b)-2] == '\r' && b[len(b)-1] == '\n' {
		return b[:len(b)-2]
	}
	if len(b) >= 1 && b[len(b)-1] == '\n' {
		return b[:len(b)-1]
	}
	return b
}

// parseHeaders populates m's header-derived fields from m.Raw. If
// net/mail.ReadMessage fails, m.HeaderParseError is set and we fall back
// to a best-effort line scan to extract as many headers as possible.
func parseHeaders(m *Message) {
	// Try net/mail first.
	msg, err := mail.ReadMessage(bytes.NewReader(m.Raw))
	if err == nil {
		applyHeaders(m, msg.Header)
		return
	}
	m.HeaderParseError = err

	// Fallback: manually extract the header block (up to the first blank
	// line) and populate a mail.Header ourselves, skipping any line that
	// does not look like a header.
	hdr := manualParseHeaders(m.Raw)
	applyHeaders(m, hdr)
}

// manualParseHeaders does a forgiving parse of the header block. Lines that
// cannot be split on ":" are silently dropped, but well-formed neighbors
// are preserved. Continuation lines (starting with space or tab) are folded
// into the preceding header value.
func manualParseHeaders(raw []byte) mail.Header {
	hdr := make(mail.Header)
	// Find the end of the header block: the first "\n\n" or "\n\r\n".
	end := len(raw)
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		end = i
	}
	if i := bytes.Index(raw, []byte("\n\r\n")); i >= 0 && i < end {
		end = i
	}
	block := raw[:end]

	var curKey string
	var curVal strings.Builder
	flush := func() {
		if curKey != "" {
			hdr[curKey] = append(hdr[curKey], curVal.String())
		}
		curKey = ""
		curVal.Reset()
	}

	lines := bytes.Split(block, []byte("\n"))
	for _, ln := range lines {
		// Strip a trailing \r.
		if len(ln) > 0 && ln[len(ln)-1] == '\r' {
			ln = ln[:len(ln)-1]
		}
		if len(ln) == 0 {
			continue
		}
		if ln[0] == ' ' || ln[0] == '\t' {
			// Continuation of previous header.
			if curKey != "" {
				curVal.WriteByte(' ')
				curVal.Write(bytes.TrimLeft(ln, " \t"))
			}
			continue
		}
		colon := bytes.IndexByte(ln, ':')
		if colon <= 0 {
			// Malformed line; skip.
			continue
		}
		flush()
		curKey = textproto.CanonicalMIMEHeaderKey(string(ln[:colon]))
		v := ln[colon+1:]
		v = bytes.TrimLeft(v, " \t")
		curVal.Write(v)
	}
	flush()
	return hdr
}

// applyHeaders fills m's fields from a mail.Header.
func applyHeaders(m *Message, hdr mail.Header) {
	m.MessageID = strings.TrimSpace(hdr.Get("Message-Id"))
	m.Subject = hdr.Get("Subject")

	if f := hdr.Get("From"); f != "" {
		if addr, err := mail.ParseAddress(f); err == nil {
			m.From = addr.Address
		} else {
			// Best-effort: strip display name manually by taking
			// whatever is between angle brackets, else the raw
			// value trimmed.
			m.From = bestEffortAddress(f)
		}
	}

	if d := hdr.Get("Date"); d != "" {
		if t, err := mail.ParseDate(d); err == nil {
			m.Date = t
		}
	}

	// List-Id preferred over List-Post.
	if v := hdr.Get("List-Id"); v != "" {
		m.ListID = stripAngleBrackets(v)
	} else if v := hdr.Get("List-Post"); v != "" {
		m.ListID = stripAngleBrackets(v)
	}

	if v := hdr.Get("X-Gmail-Labels"); v != "" {
		parts := strings.Split(v, ",")
		labels := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				labels = append(labels, p)
			}
		}
		if len(labels) > 0 {
			m.GmailLabels = labels
		}
	}
}

// bestEffortAddress extracts an addr-spec from a From header value when
// mail.ParseAddress fails. It prefers text between angle brackets; falls
// back to the trimmed raw value.
func bestEffortAddress(s string) string {
	if i := strings.IndexByte(s, '<'); i >= 0 {
		if j := strings.IndexByte(s[i+1:], '>'); j >= 0 {
			return strings.TrimSpace(s[i+1 : i+1+j])
		}
	}
	return strings.TrimSpace(s)
}

// stripAngleBrackets returns the content of the first <...> pair, or the
// trimmed input if no brackets are present. For List-Post values of the
// form "<mailto:list@example.com>", we strip the brackets; the "mailto:"
// prefix is retained verbatim (callers that care about addr-spec can
// further process).
func stripAngleBrackets(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '<'); i >= 0 {
		if j := strings.IndexByte(s[i+1:], '>'); j >= 0 {
			return strings.TrimSpace(s[i+1 : i+1+j])
		}
	}
	return s
}
