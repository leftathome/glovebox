package engine

import (
	"io"
)

const (
	defaultSampleSize = 64 * 1024 // 64KB for prefix+suffix sampling
)

type ScanFunc func(content []byte) ([]Signal, error)

// ScanContent reads all content from the reader, runs matchers against
// the full content, and runs detectors against a sampled prefix+suffix
// for large content. Phase 1 reads all content into memory; the per-item
// scan timeout in the worker pool bounds memory exposure.
//
// TODO: implement true chunked streaming for Phase 2 to bound memory
// independent of scan timeout.
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

	sample := sampleContent(content, defaultSampleSize)
	for _, scan := range detectors {
		signals, err := scan(sample)
		if err != nil {
			return nil, err
		}
		allSignals = append(allSignals, signals...)
	}

	return allSignals, nil
}

func sampleContent(content []byte, sampleSize int) []byte {
	if len(content) <= sampleSize*2 {
		return content
	}
	sample := make([]byte, 0, sampleSize*2)
	sample = append(sample, content[:sampleSize]...)
	sample = append(sample, content[len(content)-sampleSize:]...)
	return sample
}
