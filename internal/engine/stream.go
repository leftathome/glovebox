package engine

import (
	"io"
)

const (
	// DefaultSampleSize bounds the prefix+suffix window handed to
	// detectors that opt into sampling. Only language detection does:
	// see SampleContent.
	DefaultSampleSize = 64 * 1024
)

type ScanFunc func(content []byte) ([]Signal, error)

// ScanContent reads the content and runs every matcher and detector
// against all of it.
//
// Detectors used to receive only a 64 KB prefix plus a 64 KB suffix. For
// anything larger than 128 KB that meant an injection placed in the middle
// of a document was invisible to encoding_anomaly and template_structure --
// a payload could simply be padded past the sampling window. Sampling is
// now a per-detector opt-in applied by the caller (see internal/scan), so
// the security-relevant detectors see the whole document and only language
// detection, where a sample genuinely represents the whole, still uses one.
//
// Memory note: this reads the item fully into memory. Spec 04 section 6.6
// described chunked streaming with pattern-length overlap; that is not what
// is implemented, and the spec has been corrected rather than left
// describing a guarantee the code does not provide. Item size is bounded
// upstream (ingest max_body_bytes) and by the per-item scan timeout.
func ScanContent(reader io.Reader, matchers []ScanFunc, detectors []ScanFunc) ([]Signal, error) {
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	var allSignals []Signal

	for _, scan := range matchers {
		signals, err := scan(content)
		if err != nil {
			return nil, err
		}
		allSignals = append(allSignals, signals...)
	}

	for _, scan := range detectors {
		signals, err := scan(content)
		if err != nil {
			return nil, err
		}
		allSignals = append(allSignals, signals...)
	}

	return allSignals, nil
}

// SampleContent returns the first and last sampleSize bytes of content,
// or content itself when it is small enough.
//
// This is only appropriate for a detector whose answer is a property of
// the document as a whole -- language being the example -- where a sample
// is representative and an attacker gains nothing but the loss of a
// booster by hiding text outside it. It must not be used for detectors
// that look for a payload, because a payload can be positioned.
func SampleContent(content []byte, sampleSize int) []byte {
	if len(content) <= sampleSize*2 {
		return content
	}
	sample := make([]byte, 0, sampleSize*2)
	sample = append(sample, content[:sampleSize]...)
	sample = append(sample, content[len(content)-sampleSize:]...)
	return sample
}
