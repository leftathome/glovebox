package journal

import (
	"time"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/scan"
)

// TextScanner gives one stored text value its verdict (4.11).
type TextScanner interface {
	Verdict(text string) string
	// Version names the scanner and ruleset; a rescan re-scans values
	// whose verdict came from another version.
	Version() string
}

type engineScanner struct {
	run     func(text string) (string, error)
	version string
	timeout time.Duration
}

// NewTextScanner runs the core's own scanner (the one items go through)
// over a text value. A scan error, a panic or a timeout is VerdictError,
// which withholds the value: scanning never blocks a write, and never
// releases a value it could not judge.
func NewTextScanner(sc *scan.Scanner, version string, timeout time.Duration) TextScanner {
	run := func(text string) (string, error) {
		res, err := sc.Scan([]byte(text), "text/plain")
		if err != nil {
			return "", err
		}
		if res.Verdict == engine.VerdictPass {
			return record.VerdictPass, nil
		}
		return string(res.Verdict), nil
	}
	return &engineScanner{run: run, version: version, timeout: timeout}
}

func (e *engineScanner) Version() string { return e.version }

func (e *engineScanner) Verdict(text string) string {
	out := make(chan string, 1)
	go func() {
		defer func() {
			if recover() != nil {
				out <- record.VerdictError
			}
		}()
		v, err := e.run(text)
		if err != nil || v == "" {
			v = record.VerdictError
		}
		out <- v
	}()
	timer := time.NewTimer(e.timeout)
	defer timer.Stop()
	select {
	case v := <-out:
		return v
	case <-timer.C:
		return record.VerdictError
	}
}
