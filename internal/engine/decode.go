package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// Bounds on decode work. maxDecodedBytes caps the synthetic buffer so a
// content item full of base64 cannot multiply memory use; maxDecodeDepth
// catches double-encoding ("base64 of base64") without unbounded recursion.
const (
	maxDecodedBytes = 1 << 20 // 1 MiB
	maxDecodeDepth  = 2
	minDecodedLen   = 8
	printableRatio  = 0.80
)

var (
	// Deliberately shorter than the encoding-anomaly detector's {50,}
	// threshold. That detector answers "does this look suspicious"; this
	// one answers "what does it say", and the answer is only kept when the
	// decoded text itself trips a rule -- so a lower bound costs nothing
	// but catches payloads split into short runs to dodge the detector.
	base64Run  = regexp.MustCompile(`[A-Za-z0-9+/]{16,}={0,2}`)
	base64URL  = regexp.MustCompile(`[A-Za-z0-9_-]{16,}={0,2}`)
	hexRun     = regexp.MustCompile(`(?:[0-9a-fA-F]{2}){16,}`)
	percentRun = regexp.MustCompile(`(?:%[0-9a-fA-F]{2})+`)
)

// ExtractDecoded finds encoded payloads embedded in content and returns
// their decoded text, joined by newlines, for scanning by the same rules
// that scan the plain content.
//
// This closes the gap where an injection is base64-encoded inside
// otherwise-benign text: the encoding-anomaly detector flags the *presence*
// of a blob (weight 0.7, below the 0.8 quarantine threshold) but nothing
// ever reads it, so "ignore all previous instructions" travelled through
// the scanner as an unread string and was decoded downstream -- by the
// agent, or by a tool the agent called.
//
// Decoding here does not violate the "content is never modified" invariant
// (spec 04 section 1.1): the decoded text is a scan-only side buffer, and
// the item delivered to the agent remains byte-identical. This is the same
// pattern the HTML strip already uses.
//
// Returns nil when nothing decodable is found, so callers can skip a
// redundant scan pass.
func ExtractDecoded(content []byte) []byte {
	var out bytes.Buffer
	extractInto(&out, content, 0)
	if out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}

func extractInto(out *bytes.Buffer, content []byte, depth int) {
	if depth >= maxDecodeDepth || out.Len() >= maxDecodedBytes {
		return
	}

	for _, decoded := range decodeCandidates(content) {
		// Truncate against the remaining budget rather than merely
		// checking it: a single blob can decode to far more than the cap,
		// and this runs on attacker-supplied input.
		remaining := maxDecodedBytes - out.Len()
		if remaining <= 0 {
			return
		}
		if len(decoded) > remaining {
			decoded = decoded[:remaining]
		}
		out.Write(decoded)
		out.WriteByte('\n')
		// Recurse: catches payloads encoded more than once.
		extractInto(out, decoded, depth+1)
	}
}

func decodeCandidates(content []byte) [][]byte {
	var found [][]byte

	for _, m := range base64Run.FindAll(content, -1) {
		if d, ok := tryBase64(m, base64.StdEncoding, base64.RawStdEncoding); ok {
			found = append(found, d)
		}
	}
	for _, m := range base64URL.FindAll(content, -1) {
		// Skip runs the standard alphabet already covered: only attempt
		// URL-alphabet decoding when the run actually uses - or _.
		if !bytes.ContainsAny(m, "-_") {
			continue
		}
		if d, ok := tryBase64(m, base64.URLEncoding, base64.RawURLEncoding); ok {
			found = append(found, d)
		}
	}
	for _, m := range hexRun.FindAll(content, -1) {
		d := make([]byte, hex.DecodedLen(len(m)))
		n, err := hex.Decode(d, m)
		if err != nil {
			continue
		}
		if isLikelyText(d[:n]) {
			found = append(found, d[:n])
		}
	}
	for _, m := range percentRun.FindAll(content, -1) {
		if d, ok := tryPercent(m); ok {
			found = append(found, d)
		}
	}

	return found
}

func tryBase64(run []byte, encs ...*base64.Encoding) ([]byte, bool) {
	// A base64 body must be a multiple of 4 for the padded encodings; try
	// the raw (unpadded) variant as a fallback rather than trimming, so a
	// legitimately padded blob is not silently mangled.
	for _, enc := range encs {
		d := make([]byte, enc.DecodedLen(len(run)))
		n, err := enc.Decode(d, run)
		if err != nil {
			continue
		}
		if isLikelyText(d[:n]) {
			return d[:n], true
		}
	}
	return nil, false
}

func tryPercent(run []byte) ([]byte, bool) {
	d := make([]byte, 0, len(run)/3)
	for i := 0; i+2 < len(run); i += 3 {
		var b [1]byte
		if _, err := hex.Decode(b[:], run[i+1:i+3]); err != nil {
			return nil, false
		}
		d = append(d, b[0])
	}
	if !isLikelyText(d) {
		return nil, false
	}
	return d, true
}

// isLikelyText reports whether decoded bytes look like human-readable text
// rather than binary. Compressed data, hashes and random tokens decode to
// high-entropy bytes and are rejected here, which keeps the decoded buffer
// (and therefore the false-positive surface) small.
func isLikelyText(data []byte) bool {
	if len(data) < minDecodedLen {
		return false
	}
	if !utf8.Valid(data) {
		return false
	}
	printable, total := 0, 0
	for _, r := range string(data) {
		total++
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsPrint(r) {
			printable++
		}
	}
	if total == 0 {
		return false
	}
	return float64(printable)/float64(total) >= printableRatio
}
