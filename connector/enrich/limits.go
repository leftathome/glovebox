package enrich

import (
	"errors"
	"fmt"
)

// Bounds on a single enricher run.
//
// The enrichers shell out to pandoc, tesseract and pdftotext -- mature C and
// Haskell parsers, but parsers, running on files an attacker chose. Argument
// injection is already impossible (fixed argv, content on stdin), so what is
// left is what those processes do with a hostile file: run forever, or emit
// output until memory runs out. Neither was bounded.
const (
	// DefaultTimeout bounds one enricher invocation. Generous enough for
	// OCR of a large scan, short enough that a wedged process cannot hold
	// a connector's Commit open indefinitely.
	DefaultTimeout = 120 // seconds

	// DefaultMaxOutputBytes caps the text an enricher may produce. Output
	// is extracted text, so this is far above any legitimate document and
	// still bounds a decompression-bomb style expansion. It matches the
	// ingest body limit, since a larger artifact could never be ingested
	// anyway.
	DefaultMaxOutputBytes = 64 << 20

	// MaxStderrBytes caps a child's stderr transcript. The transcript is
	// only used to build a diagnostic message, so a chatty failure must
	// not become its own memory problem.
	MaxStderrBytes = 64 << 10
)

// ErrOutputTooLarge is returned when an enricher's output exceeds its cap.
var ErrOutputTooLarge = errors.New("enricher output exceeded the configured limit")

// LimitedWriter accumulates output up to a byte cap and then fails.
//
// It fails rather than truncating on purpose: enricher output is scanned and
// then handed to an agent, and a silently truncated document is worse than
// no document -- it looks complete while missing whatever came after the cut,
// which on a hostile file is exactly where the interesting part would be.
type LimitedWriter struct {
	buf   []byte
	limit int
	over  bool
}

// NewLimitedWriter returns a writer bounded to limit bytes. A limit <= 0
// uses DefaultMaxOutputBytes.
func NewLimitedWriter(limit int) *LimitedWriter {
	if limit <= 0 {
		limit = DefaultMaxOutputBytes
	}
	return &LimitedWriter{limit: limit}
}

// Write implements io.Writer. Once the cap is exceeded the writer records
// the fact and discards further data; it keeps reporting success so the
// child process is not killed by a broken pipe before the caller can read
// its stderr for a better diagnostic.
func (w *LimitedWriter) Write(p []byte) (int, error) {
	if w.over {
		return len(p), nil
	}
	if len(w.buf)+len(p) > w.limit {
		w.over = true
		// Keep what fits, so a caller that wants a diagnostic prefix has
		// something to look at.
		room := w.limit - len(w.buf)
		if room > 0 {
			w.buf = append(w.buf, p[:room]...)
		}
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// Bytes returns the accumulated output.
func (w *LimitedWriter) Bytes() []byte { return w.buf }

// String returns the accumulated output as a string, so a LimitedWriter
// can stand in for a bytes.Buffer in diagnostic paths.
func (w *LimitedWriter) String() string { return string(w.buf) }

// Len returns the number of bytes retained.
func (w *LimitedWriter) Len() int { return len(w.buf) }

// Exceeded reports whether the cap was hit.
func (w *LimitedWriter) Exceeded() bool { return w.over }

// Err returns ErrOutputTooLarge (wrapped with context) when the cap was
// exceeded, nil otherwise.
func (w *LimitedWriter) Err(producer string) error {
	if !w.over {
		return nil
	}
	return fmt.Errorf("enrich/%s: %w (limit %d bytes).\n"+
		"  CHECK: the source file may be crafted to expand without bound.\n"+
		"  FIX:   inspect the quarantined item; raise the limit only if the document is known-good.",
		producer, ErrOutputTooLarge, w.limit)
}
