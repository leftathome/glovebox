package scan_test

import (
	"os"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/detector"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/scan"
)

func middocScanner(t *testing.T) *scan.Scanner {
	t.Helper()
	f, err := os.Open("../../configs/default-rules.json")
	if err != nil {
		t.Fatalf("open default-rules.json: %v", err)
	}
	defer f.Close()
	rules, err := engine.LoadRules(f)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	sc, err := scan.New(rules, detector.NewDefaultRegistry())
	if err != nil {
		t.Fatalf("scan.New: %v", err)
	}
	return sc
}

// padded buries payload in the middle of a document large enough that the
// old prefix+suffix window (64 KiB each end) could not see it.
func padded(payload string) []byte {
	filler := strings.Repeat("The quarterly report continues on the next page. ", 2000)
	var b strings.Builder
	b.WriteString("Quarterly report\n\n")
	b.WriteString(filler)
	b.WriteString("\n")
	b.WriteString(payload)
	b.WriteString("\n")
	b.WriteString(filler)
	b.WriteString("\nEnd of report.\n")
	return []byte(b.String())
}

// tagged renders s in the Unicode Tags block: invisible, and detected by
// invisible_smuggling rather than by any matcher.
func tagged(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r <= 0x7E {
			b.WriteRune(0xE0000 + r)
		}
	}
	return b.String()
}

// This is the case that isolates the sampling bug.
//
// The hidden text is deliberately innocuous ("note to self"), so no matcher
// fires on it even after the Tags block is decoded into the scan buffer.
// The only rule that can catch it is invisible_smuggling, a detector -- and
// detectors used to receive just a 64 KiB prefix plus a 64 KiB suffix, so a
// payload padded into the middle of a large document was invisible to them.
//
// A base64 or homoglyph payload would not isolate anything here: those are
// caught by the full-content matcher and decode passes regardless of
// detector sampling, so such a test passes with or without this fix.
func TestMidDocument_DetectorOnlyPayloadIsFound(t *testing.T) {
	sc := middocScanner(t)

	doc := padded("Attachment: " + tagged("note to self"))
	if len(doc) < 128*1024 {
		t.Fatalf("test document is %d bytes; it must exceed the old 128 KiB sampling window", len(doc))
	}

	res, err := sc.Scan(doc, "text/plain")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Verdict != engine.VerdictQuarantine {
		t.Errorf("verdict = %q (score %.2f, signals %v), want quarantine: a detector-only payload padded into the middle of a large document must not evade the detectors",
			res.Verdict, res.TotalScore, names(res.Signals))
	}
	if !fired(res.Signals, "invisible_smuggling") {
		t.Errorf("invisible_smuggling did not fire; signals: %v", names(res.Signals))
	}
}

// The same document without the payload must still pass, so the fix is not
// simply "quarantine anything large".
func TestMidDocument_LargeBenignDocumentStillPasses(t *testing.T) {
	sc := middocScanner(t)

	res, err := sc.Scan(padded("Revenue was up four percent on the prior quarter."), "text/plain")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Verdict != engine.VerdictPass {
		t.Errorf("verdict = %q (score %.2f, signals %v), want pass for a large benign document",
			res.Verdict, res.TotalScore, names(res.Signals))
	}
}

// Language detection keeps its sample: it is a whole-document property and
// the expensive part of a scan. Hiding non-English text outside the window
// forfeits a booster rather than evading a detection, so sampling is safe
// there -- this pins that it still works on a large document.
func TestMidDocument_LanguageDetectionStillSamples(t *testing.T) {
	sc := middocScanner(t)

	doc := strings.Repeat("Le rapport trimestriel se poursuit a la page suivante. ", 3000)
	res, err := sc.Scan([]byte(doc), "text/plain")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !fired(res.Signals, "non_english_content") {
		t.Errorf("language detection did not fire on a large French document; signals: %v", names(res.Signals))
	}
}

func fired(signals []engine.Signal, name string) bool {
	for _, s := range signals {
		if s.Name == name {
			return true
		}
	}
	return false
}

func names(signals []engine.Signal) []string {
	out := make([]string, 0, len(signals))
	for _, s := range signals {
		out = append(out, s.Name)
	}
	return out
}
