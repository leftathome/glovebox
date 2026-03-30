package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 3 || os.Args[1] != "new-connector" {
		fmt.Fprintf(os.Stderr, "Usage: %s new-connector <name>\n", filepath.Base(os.Args[0]))
		os.Exit(1)
	}

	name := os.Args[2]
	if name == "" {
		fmt.Fprintln(os.Stderr, "error: connector name must not be empty")
		os.Exit(1)
	}

	// Default output base is connectors/ relative to current working directory.
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	outputDir := filepath.Join(cwd, "connectors", name)

	if err := Generate(name, outputDir); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("connector scaffolded at %s\n", outputDir)
}
