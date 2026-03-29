package engine

import (
	"math"
	"testing"
)

func TestScoreSignals_NoSignals(t *testing.T) {
	result := ScoreSignals(nil, nil, 0.8)
	if result.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want pass", result.Verdict)
	}
	if result.TotalScore != 0.0 {
		t.Errorf("total = %f, want 0.0", result.TotalScore)
	}
}

func TestScoreSignals_BelowThreshold(t *testing.T) {
	signals := []Signal{{Name: "test", Weight: 0.5, Matched: "x"}}
	result := ScoreSignals(signals, nil, 0.8)
	if result.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want pass", result.Verdict)
	}
}

func TestScoreSignals_AtThreshold(t *testing.T) {
	signals := []Signal{{Name: "test", Weight: 0.8, Matched: "x"}}
	result := ScoreSignals(signals, nil, 0.8)
	if result.Verdict != VerdictQuarantine {
		t.Errorf("verdict = %q, want quarantine", result.Verdict)
	}
}

func TestScoreSignals_AboveThreshold(t *testing.T) {
	signals := []Signal{
		{Name: "a", Weight: 0.5, Matched: "x"},
		{Name: "b", Weight: 0.5, Matched: "y"},
	}
	result := ScoreSignals(signals, nil, 0.8)
	if result.Verdict != VerdictQuarantine {
		t.Errorf("verdict = %q, want quarantine", result.Verdict)
	}
	if result.TotalScore != 1.0 {
		t.Errorf("total = %f, want 1.0", result.TotalScore)
	}
}

func TestScoreSignals_BoostMultiplier(t *testing.T) {
	signals := []Signal{{Name: "test", Weight: 0.5, Matched: "x"}}
	boosts := []BoostRule{{Name: "lang", BoostFactor: 1.5}}
	result := ScoreSignals(signals, boosts, 0.8)
	expected := 0.75
	if math.Abs(result.TotalScore-expected) > 0.001 {
		t.Errorf("total = %f, want %f", result.TotalScore, expected)
	}
	if result.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want pass (0.75 < 0.8)", result.Verdict)
	}
}

func TestScoreSignals_BoostPushesOverThreshold(t *testing.T) {
	signals := []Signal{{Name: "test", Weight: 0.6, Matched: "x"}}
	boosts := []BoostRule{{Name: "lang", BoostFactor: 1.5}}
	result := ScoreSignals(signals, boosts, 0.8)
	if result.Verdict != VerdictQuarantine {
		t.Errorf("verdict = %q, want quarantine (0.6*1.5=0.9 >= 0.8)", result.Verdict)
	}
}

func TestScoreSignals_BoostOnlyNoSignals(t *testing.T) {
	boosts := []BoostRule{{Name: "lang", BoostFactor: 1.5}}
	result := ScoreSignals(nil, boosts, 0.8)
	if result.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want pass (boost with no signals = 0)", result.Verdict)
	}
	if result.TotalScore != 0.0 {
		t.Errorf("total = %f, want 0.0", result.TotalScore)
	}
}

func TestScoreSignals_ThresholdBoundary(t *testing.T) {
	signals := []Signal{{Name: "test", Weight: 0.79, Matched: "x"}}
	result := ScoreSignals(signals, nil, 0.8)
	if result.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want pass (0.79 < 0.8)", result.Verdict)
	}
}

func TestScoreSignals_SignalsPreserved(t *testing.T) {
	signals := []Signal{
		{Name: "a", Weight: 0.3, Matched: "x"},
		{Name: "b", Weight: 0.4, Matched: "y"},
	}
	result := ScoreSignals(signals, nil, 0.8)
	if len(result.Signals) != 2 {
		t.Errorf("signals len = %d, want 2", len(result.Signals))
	}
}
