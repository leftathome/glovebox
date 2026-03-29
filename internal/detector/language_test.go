package detector

import (
	"strings"
	"testing"
)

func newLangDetector(t *testing.T) *LanguageDetectionDetector {
	t.Helper()
	return NewLanguageDetectionDetector()
}

func TestLanguageDetector_English(t *testing.T) {
	d := newLangDetector(t)
	signals, err := d.Detect([]byte("Hello, this is a completely normal English email about our upcoming meeting next week."))
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 0 {
		t.Errorf("expected no signal for English, got %d: %v", len(signals), signals)
	}
}

func TestLanguageDetector_French(t *testing.T) {
	d := newLangDetector(t)
	signals, _ := d.Detect([]byte("Bonjour, je voudrais vous informer que la reunion de demain est annulee. Merci de votre comprehension."))
	if len(signals) != 1 {
		t.Fatalf("expected 1 signal for French, got %d", len(signals))
	}
	if !strings.Contains(signals[0].Matched, "French") {
		t.Errorf("expected French detection, got %q", signals[0].Matched)
	}
}

func TestLanguageDetector_ShortText(t *testing.T) {
	d := newLangDetector(t)
	signals, _ := d.Detect([]byte("Bonjour"))
	if len(signals) != 0 {
		t.Errorf("short text should not trigger, got %d signals", len(signals))
	}
}

func TestLanguageDetector_EmptyContent(t *testing.T) {
	d := newLangDetector(t)
	signals, _ := d.Detect([]byte{})
	if len(signals) != 0 {
		t.Errorf("empty content should not trigger, got %d signals", len(signals))
	}
}

func TestLanguageDetector_WeightIsZero(t *testing.T) {
	d := newLangDetector(t)
	signals, _ := d.Detect([]byte("Bonjour, je voudrais vous informer que la reunion de demain est annulee. Merci de votre comprehension."))
	if len(signals) == 1 && signals[0].Weight != 0.0 {
		t.Errorf("weight should be 0.0 (booster), got %f", signals[0].Weight)
	}
}
