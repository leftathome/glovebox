package html

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// stageSource copies a fixture file into a fresh outputDir under a given
// destination basename and returns (sourcePath, outputDir). It places the
// source inside outputDir so the enricher's writes don't collide with
// the test fixture under testdata/.
func stageSource(t *testing.T, fixture, destName string) (sourcePath, outputDir string) {
	t.Helper()
	outputDir = t.TempDir()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	sourcePath = filepath.Join(outputDir, destName)
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return sourcePath, outputDir
}

func TestApplies_HTMLContentType(t *testing.T) {
	e := &Enricher{}
	cases := []struct {
		name string
		ct   string
		want bool
	}{
		{"plain html", "text/html", true},
		{"html with charset", "text/html; charset=utf-8", true},
		{"uppercase", "TEXT/HTML", true},
		{"mixed case", "Text/Html", true},
		{"pdf", "application/pdf", false},
		{"plain text", "text/plain", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := staging.ItemMetadata{ContentType: tc.ct}
			got := e.Applies(meta, "/tmp/whatever")
			if got != tc.want {
				t.Errorf("Applies(ContentType=%q) = %v, want %v", tc.ct, got, tc.want)
			}
		})
	}
}

func TestEnrich_SimpleParagraph(t *testing.T) {
	src, dir := stageSource(t, "simple-paragraph.html", "content.raw")
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/html"}

	arts, err := e.Enrich(context.Background(), src, meta, dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}
	if arts[0].Filename != "content.extracted.md" {
		t.Errorf("Filename = %q, want content.extracted.md", arts[0].Filename)
	}
	if arts[0].Kind != "extracted-text" {
		t.Errorf("Kind = %q, want extracted-text", arts[0].Kind)
	}
	if arts[0].Producer != "html" {
		t.Errorf("Producer = %q, want html", arts[0].Producer)
	}

	body := readArtifact(t, dir, arts[0].Filename)
	if !strings.Contains(body, "Hello") {
		t.Errorf("output does not contain 'Hello': %q", body)
	}
}

func TestEnrich_NestedLists(t *testing.T) {
	src, dir := stageSource(t, "nested-lists.html", "content.raw")
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/html"}

	arts, err := e.Enrich(context.Background(), src, meta, dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	body := readArtifact(t, dir, arts[0].Filename)
	// Verify all leaf text items survive extraction; we depend on
	// whatever the underlying helper produces for formatting.
	for _, want := range []string{"Alpha", "Alpha-One", "Alpha-One-First", "Alpha-One-Second", "Alpha-Two", "Beta"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q: %q", want, body)
		}
	}
}

func TestEnrich_TrackingPixelsStripped(t *testing.T) {
	src, dir := stageSource(t, "tracking-pixels.html", "content.raw")
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/html"}

	arts, err := e.Enrich(context.Background(), src, meta, dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	body := readArtifact(t, dir, arts[0].Filename)
	if strings.Contains(body, "track.gif") {
		t.Errorf("output contains tracking-pixel URL 'track.gif': %q", body)
	}
	if !strings.Contains(body, "visible content") {
		t.Errorf("output missing visible text 'visible content': %q", body)
	}
}

func TestEnrich_ScriptsDropped(t *testing.T) {
	src, dir := stageSource(t, "scripts.html", "content.raw")
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/html"}

	arts, err := e.Enrich(context.Background(), src, meta, dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	body := readArtifact(t, dir, arts[0].Filename)
	if !strings.Contains(body, "visible") {
		t.Errorf("output missing 'visible': %q", body)
	}
	if strings.Contains(body, "alert") {
		t.Errorf("output contains script body 'alert': %q", body)
	}
}

func TestEnrich_StyleBlocksDropped(t *testing.T) {
	src, dir := stageSource(t, "style.html", "content.raw")
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/html"}

	arts, err := e.Enrich(context.Background(), src, meta, dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	body := readArtifact(t, dir, arts[0].Filename)
	if !strings.Contains(body, "visible") {
		t.Errorf("output missing 'visible': %q", body)
	}
	if strings.Contains(body, "color:red") {
		t.Errorf("output contains style body 'color:red': %q", body)
	}
}

func TestEnrich_BrokenHTMLDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Enrich panicked on broken HTML: %v", r)
		}
	}()
	src, dir := stageSource(t, "broken.html", "content.raw")
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/html"}

	arts, err := e.Enrich(context.Background(), src, meta, dir)
	if err != nil {
		t.Fatalf("Enrich on tag soup: %v", err)
	}
	body := readArtifact(t, dir, arts[0].Filename)
	// Any reasonable text extraction surfaces the inline text; exact
	// formatting depends on whatever the underlying helper does.
	if !strings.Contains(body, "tag soup") {
		t.Errorf("output missing 'tag soup': %q", body)
	}
}

func TestEnrich_AttachmentSourceUsesIndexedFilename(t *testing.T) {
	// Attachment-source convention (spec §4.3 / §5):
	// attachment-<n>-<name>.<ext>  ->  content.attachment-<n>.extracted.md
	src, dir := stageSource(t, "simple-paragraph.html", "attachment-3-newsletter.html")
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/html"}

	arts, err := e.Enrich(context.Background(), src, meta, dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}
	want := "content.attachment-3.extracted.md"
	if arts[0].Filename != want {
		t.Errorf("Filename = %q, want %q", arts[0].Filename, want)
	}
	body := readArtifact(t, dir, arts[0].Filename)
	if !strings.Contains(body, "Hello") {
		t.Errorf("output missing 'Hello': %q", body)
	}
}

func TestEnrich_RegistersInDefault(t *testing.T) {
	// init() must register an "html" enricher in enrich.Default. We
	// can't introspect the Default registry directly, but we can run
	// it against a known HTML source and verify an html-produced
	// artifact comes back.
	dir := t.TempDir()
	src := filepath.Join(dir, "content.raw")
	if err := os.WriteFile(src, []byte("<p>Registered</p>"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	meta := staging.ItemMetadata{ContentType: "text/html"}
	arts, errs := enrich.Default.ApplyAll(context.Background(), src, meta, dir)
	if len(errs) > 0 {
		t.Fatalf("ApplyAll returned errors: %+v", errs)
	}
	found := false
	for _, a := range arts {
		if a.Producer == "html" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no html-produced artifact in Default registry output: %+v", arts)
	}
}

func readArtifact(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}
