package engine

type Signal struct {
	Name    string
	Weight  float64
	Matched string
}

type BoostRule struct {
	Name        string
	BoostFactor float64
}

type Verdict string

const (
	VerdictPass       Verdict = "pass"
	VerdictQuarantine Verdict = "quarantine"
)

type ScanResult struct {
	Signals    []Signal
	TotalScore float64
	Verdict    Verdict
}

func ScoreSignals(signals []Signal, boostRules []BoostRule, threshold float64) ScanResult {
	boostMultiplier := 1.0
	for _, br := range boostRules {
		boostMultiplier *= br.BoostFactor
	}

	total := 0.0
	for _, sig := range signals {
		total += sig.Weight * boostMultiplier
	}

	verdict := VerdictPass
	if total >= threshold {
		verdict = VerdictQuarantine
	}

	return ScanResult{
		Signals:    signals,
		TotalScore: total,
		Verdict:    verdict,
	}
}
