package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// TestHTTPBackendRunsEnrichmentAndSendsSidecars verifies the HTTP backend
// reaches enrichment parity with the filesystem backend (glovebox-afq4.12):
// commitHTTP runs the enrichment pipeline connector-side, so the POSTed
// metadata carries Enrichments[] and the produced sidecar artifacts are
// sent as multipart "sidecar" parts. Before this change, HTTP-backend items
// were committed completely unenriched.
func TestHTTPBackendRunsEnrichmentAndSendsSidecars(t *testing.T) {
	var got parsedIngest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = parseIngestRequest(t, r)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	reg := enrich.NewRegistry()
	reg.Register(&fakeEnricher{
		name:      "fake",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
			if err := os.WriteFile(filepath.Join(outputDir, "content.extracted.md"), []byte("EXTRACTED"), 0644); err != nil {
				return nil, err
			}
			return []enrich.Artifact{{Filename: "content.extracted.md", Kind: "extracted-text", Producer: "fake"}}, nil
		},
	})

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	backend.SetEnrichRegistry(reg)
	item, err := backend.NewItem(validHTTPOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("hello body")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// content.raw still arrives as the "content" part, unchanged.
	if string(got.content) != "hello body" {
		t.Errorf("content = %q, want %q", got.content, "hello body")
	}

	// metadata.enrichments[] populated (parity with filesystem backend).
	enrRaw, ok := got.metadata["enrichments"].([]any)
	if !ok || len(enrRaw) != 1 {
		t.Fatalf("expected 1 enrichment record in metadata, got %v", got.metadata["enrichments"])
	}
	rec, _ := enrRaw[0].(map[string]any)
	if rec["producer"] != "fake" {
		t.Errorf("enrichment producer = %v, want fake", rec["producer"])
	}
	if rec["status"] != "ok" {
		t.Errorf("enrichment status = %v, want ok", rec["status"])
	}
	if rec["source"] != "content.raw" {
		t.Errorf("enrichment source = %v, want content.raw", rec["source"])
	}

	// The produced sidecar is delivered as a "sidecar" part.
	if got.sidecars == nil {
		t.Fatal("no sidecar parts received")
	}
	if string(got.sidecars["content.extracted.md"]) != "EXTRACTED" {
		t.Errorf("sidecar content.extracted.md = %q, want EXTRACTED", got.sidecars["content.extracted.md"])
	}
	// content.raw must NOT be duplicated as a sidecar part.
	if _, dup := got.sidecars["content.raw"]; dup {
		t.Error("content.raw must not be sent as a sidecar part")
	}
}
