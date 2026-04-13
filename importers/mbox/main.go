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

	fs.String("source", "", "path to the mbox file")
	fs.String("filter", "", "path to filter JSON (optional)")
	fs.String("ingest-url", "", "glovebox ingest URL")
	fs.String("source-name", "", "value for ingest metadata's source field")
	fs.Int("concurrency", 8, "parallel ingest workers")
	fs.Bool("survey-only", false, "generate/update survey only; skip ingest")
	fs.Bool("regenerate-survey", false, "force survey regeneration")
	fs.String("resume", "", "override automatic resume decision (true|false)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "mbox-importer: not implemented yet (skeleton)")
	fmt.Fprintln(os.Stderr, "see docs/specs/09-mbox-importer-design.md")
	os.Exit(1)
}
