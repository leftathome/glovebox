package corpus_test

import (
	"bytes"
	"sync"
	"testing"

	"github.com/leftathome/glovebox/internal/corpus"
)

const (
	corpusDir = "../../testdata/adversarial-corpus"
	rulesFile = "../../configs/default-rules.json"
)

// The corpus carries several ~140 KiB mid-document items, and scanning
// them under -race is not cheap. Every test below asks the same question
// of the same immutable run, so scan once and share it.
var (
	once             sync.Once
	sharedReport     corpus.Report
	sharedThresholds corpus.Thresholds
	sharedErr        error
)

func runCorpus(t *testing.T) (corpus.Report, corpus.Thresholds) {
	t.Helper()
	once.Do(func() {
		manifest, err := corpus.Load(corpusDir)
		if err != nil {
			sharedErr = err
			return
		}
		if sharedThresholds, sharedErr = corpus.LoadThresholds(corpusDir); sharedErr != nil {
			return
		}
		sc, err := corpus.NewShippedScanner(rulesFile)
		if err != nil {
			sharedErr = err
			return
		}
		sharedReport, sharedErr = corpus.Run(sc, corpusDir, manifest)
	})
	if sharedErr != nil {
		t.Fatalf("run corpus: %v", sharedErr)
	}
	return sharedReport, sharedThresholds
}

// The headline numbers. Both are gated: detection alone would be satisfied
// by a scanner that quarantines every item, which is why the benign set is
// not optional.
func TestCorpus_MeetsCommittedThresholds(t *testing.T) {
	report, thresholds := runCorpus(t)

	var buf bytes.Buffer
	report.WriteTo(&buf, corpusDir, thresholds)
	t.Log("\n" + buf.String())

	for _, failure := range report.CheckThresholds(thresholds) {
		t.Error(failure)
	}
}

// Per-case expectations, so a rate that happens to hold while two different
// cases swap places (one newly missed, one newly caught) is still a failure.
// A case flagged known_gap inverts: it is expected to be wrong, and getting
// it right fails until the manifest is updated. That is the only way a gap
// can close without anyone noticing it closed.
func TestCorpus_EveryCaseBehavesAsRecorded(t *testing.T) {
	report, _ := runCorpus(t)

	for _, o := range report.Outcomes {
		o := o
		t.Run(o.Case.ID, func(t *testing.T) {
			switch {
			case o.Case.KnownGap && o.Correct:
				t.Errorf("case is marked known_gap but the engine now gets it right: %s\n"+
					"  the gap has closed -- clear known_gap/gap_note in the manifest, re-measure, "+
					"and tighten thresholds.json\n  recorded gap: %s",
					o.Describe(), o.Case.GapNote)
			case !o.Case.KnownGap && !o.Correct:
				t.Errorf("%s\n  %s\n  If this is a real gap rather than a regression, record it as "+
					"known_gap with a gap_note -- do not delete or weaken the fixture.",
					o.Describe(), o.Case.Note)
			}
		})
	}
}

// A corpus that quietly lost its fixtures, or a manifest that grew a class
// nobody scans, would still satisfy every rate above. Pin the shape.
func TestCorpus_CoversEveryEvasionClass(t *testing.T) {
	report, _ := runCorpus(t)

	required := []string{
		"homoglyph", "invisible", "encoded", "mid-document", "metadata", "plain",
	}
	present := map[string]int{}
	for _, s := range report.ByClass() {
		present[s.Class] = s.Total
	}
	for _, class := range required {
		if present[class] == 0 {
			t.Errorf("no cases in class %q -- the corpus no longer covers it", class)
		}
	}
	if report.Malicious < 30 {
		t.Errorf("malicious set is down to %d cases; shrinking it inflates the detection rate", report.Malicious)
	}
	if report.Benign < 15 {
		t.Errorf("benign set is down to %d cases; without it detection rate means nothing", report.Benign)
	}
}
