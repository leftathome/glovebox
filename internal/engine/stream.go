package engine

import (
	"io"
)

const (
	defaultChunkSize  = 256 * 1024 // 256KB
	defaultSampleSize = 64 * 1024  // 64KB for prefix+suffix sampling
)

type ScanFunc func(content []byte) ([]Signal, error)

// StreamingScan reads content in chunks and runs matchers against each chunk
// with overlap to catch cross-boundary matches. Custom detectors that need
// a global view receive a sampled prefix+suffix.
func StreamingScan(reader io.Reader, chunkSize int, matchers []ScanFunc, detectors []ScanFunc) ([]Signal, error) {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}

	// Read all content -- for Phase 1, content is bounded by scan timeout.
	// Streaming chunked matching will be refined when the pipeline is assembled.
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	var allSignals []Signal

	// Run matchers against full content
	for _, scan := range matchers {
		signals, err := scan(content)
		if err != nil {
			return nil, err
		}
		allSignals = append(allSignals, signals...)
	}

	// Run detectors against sampled prefix+suffix for large content
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
