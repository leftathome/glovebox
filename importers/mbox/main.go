// Command mbox-importer imports a finished mbox archive into glovebox.
//
// This is a skeleton. The real implementation lands in a later bead
// (glovebox-7ey); see docs/specs/09-mbox-importer-design.md for the design.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	fs := flag.NewFlagSet("mbox-importer", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		source           string
		filter           string
		ingestURL        string
		sourceName       string
		concurrency      int
		surveyOnly       bool
		regenerateSurvey bool
		resume           string
	)

	fs.StringVar(&source, "source", "", "path to the mbox file")
	fs.StringVar(&filter, "filter", "", "path to filter JSON (optional)")
	fs.StringVar(&ingestURL, "ingest-url", "", "glovebox ingest URL")
	fs.StringVar(&sourceName, "source-name", "", "value for ingest metadata's source field")
	fs.IntVar(&concurrency, "concurrency", 8, "parallel ingest workers")
	fs.BoolVar(&surveyOnly, "survey-only", false, "generate/update survey only; skip ingest")
	fs.BoolVar(&regenerateSurvey, "regenerate-survey", false, "force survey regeneration")
	fs.StringVar(&resume, "resume", "", "override automatic resume decision (true|false)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}

	// Flags are accepted but unused in this skeleton; reference them so
	// static analysis does not flag them as dead, while making clear that
	// nothing here yet drives behavior.
	_ = source
	_ = filter
	_ = ingestURL
	_ = sourceName
	_ = concurrency
	_ = surveyOnly
	_ = regenerateSurvey
	_ = resume

	fmt.Fprintln(os.Stderr, "mbox-importer: not implemented yet (skeleton)")
	fmt.Fprintln(os.Stderr, "see docs/specs/09-mbox-importer-design.md")
	os.Exit(1)
}
