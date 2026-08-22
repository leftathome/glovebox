package engine

import (
	"regexp"
)

// Structured (non-prose) regions of a content item: ASCII armour, data:
// URIs and the long unbroken token runs that carry encoded payloads.
//
// These are recognised here so that analysis which is only meaningful over
// natural language -- language identification, today -- can be asked about
// the prose in an item rather than about its attachments. A base64 PNG is
// not badly-written English; it is not writing at all, and a model that is
// forced to name a language for it will name one confidently and wrongly.
//
// Nothing here decides anything on its own, and nothing here is a
// suppression: the matchers, the encoding-anomaly detector and the
// decode-then-scan views all continue to see the full content. Only the
// question "what language is this written in?" is asked of the prose
// alone.
var (
	// RFC 7468 / OpenPGP ASCII armour: PEM certificates and keys, PGP
	// messages and signatures, OpenSSH keys. The unterminated alternative
	// matters because a detector may be handed a sample rather than the
	// whole document, which can cut a block in half.
	armourBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?(?:-----END [A-Z0-9 ]+-----|\z)`)

	// data: URIs, the inline-attachment form. The payload is matched to
	// the first whitespace or markup delimiter, which is where an inline
	// image in an HTML mail ends.
	dataURI = regexp.MustCompile(`(?i)\bdata:[a-z0-9.+-]*/?[a-z0-9.+-]*[;,][^\s"'<>)\]]{16,}`)

	// Unbroken runs in the alphabets encoded payloads are written in.
	// Length alone is not enough to call one non-prose -- German builds
	// legitimate 49-character compounds -- so a candidate must also look
	// encoded; see looksEncoded.
	tokenRun    = regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`)
	tokenRunURL = regexp.MustCompile(`[A-Za-z0-9_-]{40,}={0,2}`)
	hexRunLong  = regexp.MustCompile(`(?:[0-9a-fA-F]{2}){16,}`)
)

// looksEncoded reports whether a long unbroken run (the regexes above
// have already applied the length bar) is an encoded payload rather than a
// word.
//
// The discriminator is case and digits, not length. Base64, base64url and
// hex of any real payload carry digits, and base64 carries capitals in the
// middle of the run; a word -- "Donaudampfschifffahrtsgesellschaftskapitaenswitwe",
// 49 characters of ordinary German -- carries neither. Requiring the shape
// as well as the length is what lets the claim to eat no prose be true
// rather than merely usually true.
func looksEncoded(run []byte) bool {
	digits, upper := 0, 0
	for i, b := range run {
		switch {
		case b >= '0' && b <= '9':
			digits++
		case b >= 'A' && b <= 'Z':
			// A leading capital is how a sentence and a proper noun
			// start; capitals further in are how base64 looks.
			if i > 0 {
				upper++
			}
		}
	}
	return digits > 0 || upper > 0
}

// stripRuns replaces every run matching re that looksEncoded with a single
// space, leaving runs that are merely long alone.
func stripRuns(re *regexp.Regexp, content []byte) []byte {
	return re.ReplaceAllFunc(content, func(run []byte) []byte {
		if !looksEncoded(run) {
			return run
		}
		return []byte(" ")
	})
}

// StripStructured returns content with armoured and structured regions
// replaced by a single space each, leaving the prose around them.
//
// The replacement is a space rather than nothing so that removing a blob
// from the middle of a sentence cannot fuse the words on either side of it
// into a token that was never written.
//
// This builds a scan-only view. The item delivered to the agent is
// untouched (spec 04 section 1.1), as it is for every other derived view
// in this package.
func StripStructured(content []byte) []byte {
	out := armourBlock.ReplaceAll(content, []byte(" "))
	out = dataURI.ReplaceAll(out, []byte(" "))
	for _, re := range []*regexp.Regexp{tokenRun, tokenRunURL, hexRunLong} {
		out = stripRuns(re, out)
	}
	return out
}

// NonSpaceLen counts the bytes in content that are not ASCII whitespace.
// Paired with StripStructured it answers "how much prose is actually
// here", which a raw length cannot: a document that is 2 KiB of base64 and
// one short caption is long and almost wordless.
func NonSpaceLen(content []byte) int {
	n := 0
	for _, b := range content {
		switch b {
		case ' ', '\t', '\n', '\r', '\v', '\f':
		default:
			n++
		}
	}
	return n
}
