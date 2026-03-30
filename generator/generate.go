package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"unicode"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// TemplateData holds the variables available to every template.
type TemplateData struct {
	Name       string // connector name as given, e.g. "test-connector"
	StructName string // PascalCase, e.g. "TestConnector"
	Package    string // valid Go package name, e.g. "testconnector"
	Module     string // Go module path
}

// outputFile maps a template name to the file it should produce.
type outputFile struct {
	Template string // filename inside templates/
	Output   string // relative path inside the connector directory
}

var files = []outputFile{
	{Template: "connector.go.tmpl", Output: "connector.go"},
	{Template: "config.go.tmpl", Output: "config.go"},
	{Template: "main.go.tmpl", Output: "main.go"},
	{Template: "config.json.tmpl", Output: "config.json"},
	{Template: "Dockerfile.tmpl", Output: "Dockerfile"},
	{Template: "README.md.tmpl", Output: "README.md"},
}

// Generate creates a new connector directory at outputDir using the embedded
// templates and the given connector name.
func Generate(name, outputDir string) error {
	data := TemplateData{
		Name:       name,
		StructName: toPascalCase(name),
		Package:    toPackageName(name),
		Module:     "github.com/leftathome/glovebox",
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	for _, f := range files {
		content, err := templateFS.ReadFile(filepath.Join("templates", f.Template))
		if err != nil {
			return fmt.Errorf("read template %s: %w", f.Template, err)
		}

		tmpl, err := template.New(f.Template).Parse(string(content))
		if err != nil {
			return fmt.Errorf("parse template %s: %w", f.Template, err)
		}

		outPath := filepath.Join(outputDir, f.Output)
		out, err := os.Create(outPath)
		if err != nil {
			return fmt.Errorf("create %s: %w", f.Output, err)
		}

		if err := tmpl.Execute(out, data); err != nil {
			out.Close()
			return fmt.Errorf("execute template %s: %w", f.Template, err)
		}

		if err := out.Close(); err != nil {
			return fmt.Errorf("close %s: %w", f.Output, err)
		}
	}

	return nil
}

// toPascalCase converts a hyphen-separated name to PascalCase.
// "test-connector" -> "TestConnector"
func toPascalCase(name string) string {
	parts := strings.Split(name, "-")
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		runes := []rune(part)
		runes[0] = unicode.ToUpper(runes[0])
		b.WriteString(string(runes))
	}
	return b.String()
}

// toPackageName converts a connector name to a valid Go package name
// by removing hyphens and lowercasing.
// "test-connector" -> "testconnector"
func toPackageName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "-", ""))
}
