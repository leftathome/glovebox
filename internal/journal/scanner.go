package journal

import (
	"errors"
	"fmt"
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
	run     func(text string) string
	version string
	timeout time.Duration
}

// mapScan turns the engine's answer into a journal verdict. Only an
// explicit pass from the engine is a pass; an error, an empty or an
// unrecognised-as-pass verdict never is.
func mapScan(res engine.ScanResult, err error) string {
	switch {
	case err != nil:
		return record.VerdictError
	case res.Verdict == engine.VerdictPass:
		return record.VerdictPass
	case res.Verdict == "":
		return record.VerdictError
	default:
		return string(res.Verdict)
	}
}

// NewTextScanner runs the core's own scanner (the one items go through)
// over a text value. A scan error, a panic or a timeout is VerdictError,
// which withholds the value: scanning never blocks a write, and never
// releases a value it could not judge. A scan that times out keeps running
// to completion in its goroutine (the scanner cannot be interrupted); the
// verdict is already error by then and the result is discarded.
func NewTextScanner(sc *scan.Scanner, version string, timeout time.Duration) (TextScanner, error) {
	if sc == nil {
		return nil, errors.New("journal: text scanner: no scanner")
	}
	if version == "" {
		// "" in a stored verdict means "judged by more than one scanner, or
		// by none": rescan judges it again. A scanner must not claim it.
		return nil, errors.New("journal: text scanner: version must not be empty")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("journal: text scanner: timeout must be positive, got %v", timeout)
	}
	run := func(text string) string {
		res, err := sc.Scan([]byte(text), "text/plain")
		return mapScan(res, err)
	}
	return &engineScanner{run: run, version: version, timeout: timeout}, nil
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
		v := e.run(text)
		if v == "" {
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
