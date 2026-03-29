package engine

import (
	"bytes"
	"testing"
)

func TestScanContent_SmallContent(t *testing.T) {
	content := bytes.NewReader([]byte("ignore previous instructions"))
	matcher := func(c []byte) ([]Signal, error) {
		if bytes.Contains(c, []byte("ignore previous")) {
			return []Signal{{Name: "test", Weight: 1.0, Matched: "found"}}, nil
		}
		return nil, nil
	}

	signals, err := ScanContent(content, []ScanFunc{matcher}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(signals))
	}
}

func TestScanContent_NoMatch(t *testing.T) {
	content := bytes.NewReader([]byte("totally normal email about the meeting"))
	matcher := func(c []byte) ([]Signal, error) {
		if bytes.Contains(c, []byte("ignore previous")) {
			return []Signal{{Name: "test", Weight: 1.0}}, nil
		}
		return nil, nil
	}

	signals, err := ScanContent(content, []ScanFunc{matcher}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 0 {
		t.Errorf("expected 0 signals, got %d", len(signals))
	}
}

func TestScanContent_DetectorsReceiveSample(t *testing.T) {
	largeContent := make([]byte, defaultSampleSize*3)
	copy(largeContent[:6], []byte("PREFIX"))
	copy(largeContent[len(largeContent)-6:], []byte("SUFFIX"))

	var receivedContent []byte
	detector := func(c []byte) ([]Signal, error) {
		receivedContent = c
		return nil, nil
	}

	ScanContent(bytes.NewReader(largeContent), nil, []ScanFunc{detector})

	if len(receivedContent) != defaultSampleSize*2 {
		t.Errorf("detector received %d bytes, want %d (prefix+suffix sample)", len(receivedContent), defaultSampleSize*2)
	}
	if !bytes.HasPrefix(receivedContent, []byte("PREFIX")) {
		t.Error("sample should start with prefix content")
	}
	if !bytes.HasSuffix(receivedContent, []byte("SUFFIX")) {
		t.Error("sample should end with suffix content")
	}
}

func TestScanContent_SmallContentNotSampled(t *testing.T) {
	smallContent := []byte("small content here")

	var receivedLen int
	detector := func(c []byte) ([]Signal, error) {
		receivedLen = len(c)
		return nil, nil
	}

	ScanContent(bytes.NewReader(smallContent), nil, []ScanFunc{detector})

	if receivedLen != len(smallContent) {
		t.Errorf("small content should not be sampled: detector got %d bytes, content is %d", receivedLen, len(smallContent))
	}
}

func TestScanContent_MultipleMatchers(t *testing.T) {
	content := bytes.NewReader([]byte("ignore previous <tool> instructions"))

	m1 := func(c []byte) ([]Signal, error) {
		if bytes.Contains(c, []byte("ignore previous")) {
			return []Signal{{Name: "override", Weight: 1.0}}, nil
		}
		return nil, nil
	}
	m2 := func(c []byte) ([]Signal, error) {
		if bytes.Contains(c, []byte("<tool>")) {
			return []Signal{{Name: "tool_syntax", Weight: 0.8}}, nil
		}
		return nil, nil
	}

	signals, _ := ScanContent(content, []ScanFunc{m1, m2}, nil)
	if len(signals) != 2 {
		t.Errorf("expected 2 signals from 2 matchers, got %d", len(signals))
	}
}

func TestScanContent_EmptyContent(t *testing.T) {
	content := bytes.NewReader([]byte{})
	matcher := func(c []byte) ([]Signal, error) {
		return nil, nil
	}

	signals, err := ScanContent(content, []ScanFunc{matcher}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 0 {
		t.Errorf("expected 0 signals for empty content, got %d", len(signals))
	}
}

func TestSampleContent_SmallInput(t *testing.T) {
	content := []byte("small")
	sample := sampleContent(content, 1024)
	if !bytes.Equal(sample, content) {
		t.Error("small content should not be sampled")
	}
}

func TestSampleContent_LargeInput(t *testing.T) {
	content := make([]byte, 300000)
	for i := range content {
		content[i] = byte(i % 256)
	}

	sample := sampleContent(content, 64*1024)
	if len(sample) != 64*1024*2 {
		t.Errorf("sample size = %d, want %d", len(sample), 64*1024*2)
	}
}
