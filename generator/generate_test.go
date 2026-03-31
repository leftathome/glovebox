package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToPascalCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"test-connector", "TestConnector"},
		{"imap", "Imap"},
		{"my-cool-thing", "MyCoolThing"},
		{"already", "Already"},
	}
	for _, tt := range tests {
		got := toPascalCase(tt.input)
		if got != tt.want {
			t.Errorf("toPascalCase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestToPackageName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"test-connector", "testconnector"},
		{"imap", "imap"},
		{"My-Thing", "mything"},
	}
	for _, tt := range tests {
		got := toPackageName(tt.input)
		if got != tt.want {
			t.Errorf("toPackageName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGenerate(t *testing.T) {
	tmpDir := t.TempDir()
	outputDir := filepath.Join(tmpDir, "connectors", "test-connector")

	err := Generate("test-connector", outputDir)
	if err != nil {
		t.Fatalf("Generate() returned error: %v", err)
	}

	// Verify all expected files exist.
	expectedFiles := []string{
		"connector.go",
		"config.go",
		"main.go",
		"config.json",
		"Dockerfile",
		"README.md",
	}

	for _, name := range expectedFiles {
		path := filepath.Join(outputDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("expected file %s does not exist: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("expected file %s is empty", name)
		}
	}

	// Verify Go files parse correctly.
	goFiles := []string{"connector.go", "config.go", "main.go"}
	fset := token.NewFileSet()
	for _, name := range goFiles {
		path := filepath.Join(outputDir, name)
		_, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if err != nil {
			t.Errorf("generated %s does not parse as valid Go: %v", name, err)
		}
	}

	// Verify template variables were substituted correctly.
	connectorGo, err := os.ReadFile(filepath.Join(outputDir, "connector.go"))
	if err != nil {
		t.Fatalf("read connector.go: %v", err)
	}
	connectorStr := string(connectorGo)

	if !strings.Contains(connectorStr, "package main") {
		t.Error("connector.go missing expected package declaration 'package main'")
	}
	if !strings.Contains(connectorStr, "TestConnectorConnector") {
		t.Error("connector.go missing expected struct name 'TestConnectorConnector'")
	}

	// Verify main.go references.
	mainGo, err := os.ReadFile(filepath.Join(outputDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	mainStr := string(mainGo)

	if !strings.Contains(mainStr, "package main") {
		t.Error("main.go missing 'package main'")
	}
	if !strings.Contains(mainStr, `"test-connector"`) {
		t.Error("main.go missing connector name")
	}

	// Verify Dockerfile references.
	dockerfile, err := os.ReadFile(filepath.Join(outputDir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerStr := string(dockerfile)

	if !strings.Contains(dockerStr, "connectors/test-connector/") {
		t.Error("Dockerfile missing connector path")
	}

	// Verify config.json has rules.
	configJSON, err := os.ReadFile(filepath.Join(outputDir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if !strings.Contains(string(configJSON), `"rules"`) {
		t.Error("config.json missing rules key")
	}
}

func TestGenerateEmptyName(t *testing.T) {
	tmpDir := t.TempDir()
	outputDir := filepath.Join(tmpDir, "connectors", "")

	// Generate with empty string should still work (it creates files, the
	// validation is in main.go). We just verify no panic.
	err := Generate("", outputDir)
	if err != nil {
		t.Logf("Generate with empty name returned error (acceptable): %v", err)
	}
}
