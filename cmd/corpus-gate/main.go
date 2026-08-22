// Command corpus-gate scans the checked-in adversarial corpus through the
// shipped scan path, prints the detection and false-positive rates, and
// exits non-zero if either has crossed the committed threshold.
//
// It is the CI gate (scripts/corpus-gate.sh) and the local one-liner for
// "did my rule change help or hurt". It is not a shipped component: it
// reads fixtures out of the repository, so it has no meaning outside it,
// and it is deliberately absent from scripts/build-targets.sh.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/leftathome/glovebox/internal/corpus"
)

func main() {
	dir := flag.String("corpus", "testdata/adversarial-corpus", "corpus directory")
	rules := flag.String("rules", "configs/default-rules.json", "rules file to scan with")
	verbose := flag.Bool("v", false, "print every case, not only failures")
	flag.Parse()

	if err := run(*dir, *rules, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "corpus-gate: %v\n", err)
		os.Exit(1)
	}
}

func run(dir, rules string, verbose bool) error {
	manifest, err := corpus.Load(dir)
	if err != nil {
		return err
	}
	thresholds, err := corpus.LoadThresholds(dir)
	if err != nil {
		return err
	}
	sc, err := corpus.NewShippedScanner(rules)
	if err != nil {
		return err
	}
	report, err := corpus.Run(sc, dir, manifest)
	if err != nil {
		return err
	}

	if verbose {
		for _, o := range report.Outcomes {
			fmt.Println(o.Describe())
		}
		fmt.Println()
	}
	report.WriteTo(os.Stdout, dir, thresholds)

	failures := report.CheckThresholds(thresholds)
	if len(failures) == 0 {
		return nil
	}
	fmt.Println()
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "FAIL: %s\n", f)
	}
	return fmt.Errorf("%d threshold(s) breached", len(failures))
}
