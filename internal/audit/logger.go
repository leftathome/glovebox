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

type PassEntry struct {
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

type RejectEntry struct {
	Timestamp      string        `json:"timestamp"`
	Source         string        `json:"source"`
	Sender         string        `json:"sender"`
	ContentHash    string        `json:"content_hash"`
	ContentLength  int64         `json:"content_length"`
	Signals        []SignalEntry `json:"signals"`
	TotalScore     float64       `json:"total_score"`
	Verdict        string        `json:"verdict"`
	Reason         string        `json:"reason"`
	Destination    string        `json:"destination"`
	ScanDurationMs int64         `json:"scan_duration_ms"`
}

type Logger struct {
	dir string
	mu  sync.Mutex
}

func NewLogger(dir string) (*Logger, error) {
	return &Logger{dir: dir}, nil
}

func (l *Logger) LogPass(entry PassEntry) error {
	return l.appendJSONL("pass.jsonl", entry)
}

func (l *Logger) LogReject(entry RejectEntry) error {
	return l.appendJSONL("rejected.jsonl", entry)
}

func (l *Logger) appendJSONL(filename string, v any) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal audit entry: %w", err)
	}

	path := filepath.Join(l.dir, filename)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open audit log %s: %w", path, err)
	}
	defer f.Close()

	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write audit log %s: %w", path, err)
	}

	return nil
}
