package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type SignalEntry struct {
	Name    string  `json:"name"`
	Weight  float64 `json:"weight"`
	Matched string  `json:"matched"`
}

type AuditEntry struct {
	Timestamp      string        `json:"timestamp"`
	Source         string        `json:"source"`
	Sender         string        `json:"sender"`
	ContentHash    string        `json:"content_hash"`
	ContentLength  int64         `json:"content_length"`
	Signals        []SignalEntry `json:"signals"`
	TotalScore     float64       `json:"total_score"`
	Verdict        string        `json:"verdict"`
	Destination    string        `json:"destination"`
	ScanDurationMs int64         `json:"scan_duration_ms"`
}

type PassEntry struct {
	AuditEntry
}

type RejectEntry struct {
	AuditEntry
	Reason string `json:"reason"`
}

type Logger struct {
	mu       sync.Mutex
	passFile *os.File
	rejectFile *os.File
}

func NewLogger(dir string) (*Logger, error) {
	passPath := filepath.Join(dir, "pass.jsonl")
	pf, err := os.OpenFile(passPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", passPath, err)
	}

	rejectPath := filepath.Join(dir, "rejected.jsonl")
	rf, err := os.OpenFile(rejectPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		pf.Close()
		return nil, fmt.Errorf("open audit log %s: %w", rejectPath, err)
	}

	return &Logger{passFile: pf, rejectFile: rf}, nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var firstErr error
	if l.passFile != nil {
		if err := l.passFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if l.rejectFile != nil {
		if err := l.rejectFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (l *Logger) LogPass(entry PassEntry) error {
	return l.appendJSONL(l.passFile, entry)
}

func (l *Logger) LogReject(entry RejectEntry) error {
	return l.appendJSONL(l.rejectFile, entry)
}

func (l *Logger) appendJSONL(f *os.File, v any) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal audit entry: %w", err)
	}

	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}

	return nil
}
