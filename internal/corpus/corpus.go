// Package corpus runs the checked-in adversarial corpus
// (testdata/adversarial-corpus) through the real scan path and reports
// detection and false-positive rates.
//
// The efficacy fixes each landed with a targeted regression test, which
// proves one bypass is closed but says nothing about the scanner as a
// whole: nothing measured how much of a red-team set is caught, and
// nothing bounded what legitimate mail it destroys on the way. A scanner
// that quarantines everything scores 100% detection and is useless. Both
// numbers therefore come from the same run, over the same fixtures, and
// both are gated in CI (scripts/corpus-gate.sh).
//
// The scanner is built exactly the way production builds it -- rules
// loaded from configs/default-rules.json, the default detector registry,
// scan.New -- so a regression in the shipped rules file shows up here and
// not only in a hand-built ruleset that no deployment ever uses.
package corpus

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/leftathome/glovebox/internal/detector"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/scan"
)

// Expected verdicts, matching engine.Verdict values.
const (
	ExpectQuarantine = "quarantine"
	ExpectPass       = "pass"
)

// Case is one corpus entry: a fixture file plus the channel metadata it
// arrives with and the verdict the scanner is expected to reach.
type Case struct {
	ID          string `json:"id"`
	File        string `json:"file"`
	ContentType string `json:"content_type"`
	// Subject, Sender and Source are the metadata fields the pipeline
	// hands to scan.ScanWithMetadata (internal/pipeline/worker.go). They
	// are part of the attack surface: metadata is delivered verbatim into
	// the agent inbox on PASS.
	Subject string `json:"subject"`
	Sender  string `json:"sender"`
	Source  string `json:"source"`
	Expect  string `json:"expect"`
	Class   string `json:"class"`
	Note    string `json:"note"`
	// KnownGap marks a case the engine does not get right today. The case
	// stays in the corpus and stays counted in the rates -- hiding a miss
	// would make the numbers a lie -- but the per-case assertion inverts,
	// so closing the gap fails loudly and prompts an update here.
	KnownGap bool   `json:"known_gap"`
	GapNote  string `json:"gap_note"`
}

// Manifest is the corpus index.
type Manifest struct {
	Cases []Case `json:"cases"`
}

// Thresholds are the committed floors/ceilings the CI gate enforces. They
// record what the engine actually achieves, not what anyone wishes it
// achieved: raising them is a deliberate act after measuring an
// improvement.
type Thresholds struct {
	MinDetectionRate     float64 `json:"min_detection_rate"`
	MaxFalsePositiveRate float64 `json:"max_false_positive_rate"`
	Note                 string  `json:"note"`
}

// Outcome is one scanned case.
type Outcome struct {
	Case    Case
	Verdict string
	Score   float64
	Signals []string
	// Correct reports whether the verdict matched Case.Expect.
	Correct bool
}

// Report aggregates a whole corpus run.
type Report struct {
	Outcomes []Outcome

	Malicious int
	Detected  int
	Benign    int
	FalsePos  int
}

// Load reads the corpus manifest from dir.
func Load(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if len(m.Cases) == 0 {
		// An empty corpus would sail through every threshold while
		// measuring nothing -- the exact silent-success failure the
		// build-target check guards against elsewhere.
		return nil, fmt.Errorf("manifest lists no cases")
	}
	seen := make(map[string]bool, len(m.Cases))
	for _, c := range m.Cases {
		switch c.Expect {
		case ExpectQuarantine, ExpectPass:
		default:
			return nil, fmt.Errorf("case %q: unknown expect %q", c.ID, c.Expect)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
	}
	return &m, nil
}

// LoadThresholds reads the committed gate thresholds from dir.
func LoadThresholds(dir string) (Thresholds, error) {
	var t Thresholds
	data, err := os.ReadFile(filepath.Join(dir, "thresholds.json"))
	if err != nil {
		return t, fmt.Errorf("read thresholds: %w", err)
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return t, fmt.Errorf("parse thresholds: %w", err)
	}
	return t, nil
}

// NewShippedScanner builds the scanner the same way main.go does: the
// shipped rules file plus the default detector registry.
func NewShippedScanner(rulesPath string) (*scan.Scanner, error) {
	f, err := os.Open(rulesPath)
	if err != nil {
		return nil, fmt.Errorf("open rules: %w", err)
	}
	defer f.Close()
	rules, err := engine.LoadRules(f)
	if err != nil {
		return nil, fmt.Errorf("load rules: %w", err)
	}
	sc, err := scan.New(rules, detector.NewDefaultRegistry())
	if err != nil {
		return nil, fmt.Errorf("build scanner: %w", err)
	}
	return sc, nil
}

// Run scans every case in the manifest and aggregates the results.
func Run(sc *scan.Scanner, dir string, m *Manifest) (Report, error) {
	var rep Report
	for _, c := range m.Cases {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(c.File)))
		if err != nil {
			return rep, fmt.Errorf("case %q: %w", c.ID, err)
		}
		// Exactly the call the worker pool makes, with the same metadata
		// fields in the same order.
		res, err := sc.ScanWithMetadata(content, c.ContentType, []string{c.Subject, c.Sender, c.Source})
		if err != nil {
			return rep, fmt.Errorf("case %q: scan: %w", c.ID, err)
		}
		out := Outcome{
			Case:    c,
			Verdict: string(res.Verdict),
			Score:   res.TotalScore,
			Signals: signalNames(res.Signals),
		}
		out.Correct = out.Verdict == c.Expect
		if c.Expect == ExpectQuarantine {
			rep.Malicious++
			if out.Correct {
				rep.Detected++
			}
		} else {
			rep.Benign++
			if !out.Correct {
				rep.FalsePos++
			}
		}
		rep.Outcomes = append(rep.Outcomes, out)
	}
	return rep, nil
}

// DetectionRate is the fraction of malicious cases quarantined.
func (r Report) DetectionRate() float64 {
	if r.Malicious == 0 {
		return 0
	}
	return float64(r.Detected) / float64(r.Malicious)
}

// FalsePositiveRate is the fraction of benign cases quarantined.
func (r Report) FalsePositiveRate() float64 {
	if r.Benign == 0 {
		return 0
	}
	return float64(r.FalsePos) / float64(r.Benign)
}

// Misses returns the malicious cases that were not quarantined.
func (r Report) Misses() []Outcome { return r.filter(ExpectQuarantine) }

// FalsePositives returns the benign cases that were quarantined.
func (r Report) FalsePositives() []Outcome { return r.filter(ExpectPass) }

func (r Report) filter(expect string) []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Case.Expect == expect && !o.Correct {
			out = append(out, o)
		}
	}
	return out
}

// Unexpected returns cases marked as known gaps that the engine now gets
// right. They are not failures of the engine -- they are failures of this
// file to keep up, and they must be surfaced or the corpus quietly stops
// describing reality.
func (r Report) Unexpected() []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Case.KnownGap && o.Correct {
			out = append(out, o)
		}
	}
	return out
}

// ByClass summarises detection per corpus class, so a rate that holds up
// overall while one whole evasion family regresses is still visible.
func (r Report) ByClass() []ClassStat {
	idx := map[string]*ClassStat{}
	for _, o := range r.Outcomes {
		s, ok := idx[o.Case.Class]
		if !ok {
			s = &ClassStat{Class: o.Case.Class}
			idx[o.Case.Class] = s
		}
		s.Total++
		if o.Correct {
			s.Correct++
		}
	}
	stats := make([]ClassStat, 0, len(idx))
	for _, s := range idx {
		stats = append(stats, *s)
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Class < stats[j].Class })
	return stats
}

// ClassStat is a per-class tally.
type ClassStat struct {
	Class   string
	Total   int
	Correct int
}

// WriteTo prints the human-readable report CI logs carry.
func (r Report) WriteTo(w io.Writer, dir string, t Thresholds) {
	fmt.Fprintf(w, "adversarial corpus: %s\n", dir)
	fmt.Fprintf(w, "cases: %d (%d malicious, %d benign)\n\n", len(r.Outcomes), r.Malicious, r.Benign)

	fmt.Fprintf(w, "%-28s %5s %5s\n", "class", "ok", "total")
	for _, s := range r.ByClass() {
		fmt.Fprintf(w, "%-28s %5d %5d\n", s.Class, s.Correct, s.Total)
	}
	fmt.Fprintln(w)

	if misses := r.Misses(); len(misses) > 0 {
		fmt.Fprintf(w, "MISSED (%d malicious case(s) not quarantined):\n", len(misses))
		for _, o := range misses {
			fmt.Fprintf(w, "  %-40s score=%.2f signals=%v%s\n", o.Case.ID, o.Score, o.Signals, gapSuffix(o))
		}
		fmt.Fprintln(w)
	}
	if fps := r.FalsePositives(); len(fps) > 0 {
		fmt.Fprintf(w, "FALSE POSITIVES (%d benign case(s) quarantined):\n", len(fps))
		for _, o := range fps {
			fmt.Fprintf(w, "  %-40s score=%.2f signals=%v%s\n", o.Case.ID, o.Score, o.Signals, gapSuffix(o))
		}
		fmt.Fprintln(w)
	}
	if fixed := r.Unexpected(); len(fixed) > 0 {
		fmt.Fprintf(w, "NO LONGER FAILING (%d case(s) marked known_gap now correct -- update the manifest):\n", len(fixed))
		for _, o := range fixed {
			fmt.Fprintf(w, "  %-40s verdict=%s score=%.2f\n", o.Case.ID, o.Verdict, o.Score)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "detection rate:       %2d/%2d = %6.2f%%   (floor %6.2f%%)\n",
		r.Detected, r.Malicious, 100*r.DetectionRate(), 100*t.MinDetectionRate)
	fmt.Fprintf(w, "false-positive rate:  %2d/%2d = %6.2f%%   (ceiling %6.2f%%)\n",
		r.FalsePos, r.Benign, 100*r.FalsePositiveRate(), 100*t.MaxFalsePositiveRate)
}

func gapSuffix(o Outcome) string {
	if !o.Case.KnownGap {
		return ""
	}
	return "  [known gap: " + o.Case.GapNote + "]"
}

// CheckThresholds reports every way the run falls short of the committed
// gate. Float comparison carries a small epsilon so the recorded rate
// (rounded when it was committed) cannot fail against itself.
func (r Report) CheckThresholds(t Thresholds) []string {
	const eps = 1e-9
	var failures []string
	if r.DetectionRate() < t.MinDetectionRate-eps {
		failures = append(failures, fmt.Sprintf(
			"detection rate %.2f%% is below the committed floor %.2f%% (%d/%d malicious cases quarantined)",
			100*r.DetectionRate(), 100*t.MinDetectionRate, r.Detected, r.Malicious))
	}
	if r.FalsePositiveRate() > t.MaxFalsePositiveRate+eps {
		failures = append(failures, fmt.Sprintf(
			"false-positive rate %.2f%% is above the committed ceiling %.2f%% (%d/%d benign cases quarantined)",
			100*r.FalsePositiveRate(), 100*t.MaxFalsePositiveRate, r.FalsePos, r.Benign))
	}
	return failures
}

func signalNames(signals []engine.Signal) []string {
	names := make([]string, 0, len(signals))
	for _, s := range signals {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names
}

// Describe renders a one-line summary, used in test failure output.
func (o Outcome) Describe() string {
	return fmt.Sprintf("%s (%s): verdict=%s want=%s score=%.2f signals=[%s]",
		o.Case.ID, o.Case.Class, o.Verdict, o.Case.Expect, o.Score, strings.Join(o.Signals, " "))
}
