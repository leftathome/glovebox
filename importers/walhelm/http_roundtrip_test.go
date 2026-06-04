// http_roundtrip_test.go -- regression test locking in the HTTP staging
// backend -> /v1/ingest -> on-disk metadata.json provenance round-trip.
//
// The spec-15 resolver reads data_subject (and relies on audience + the
// acquisition identity) from the staged metadata.json. When a connector is
// configured with the HTTP backend (--ingest-url) instead of a filesystem
// StagingWriter, those provenance fields travel a different path:
//
//	connector.HTTPStagingBackend.Commit()
//	  -> marshal full ItemMetadata into a multipart "metadata" part
//	  -> POST /v1/ingest
//	  -> ingest.Handler persists the metadata bytes verbatim to metadata.json
//
// This test stands up the REAL ingest handler on an httptest.Server and the
// REAL connector.HTTPStagingBackend pointed at it (no mocks on the core path),
// then asserts data_subject / audience / identity survive the full
// backend -> POST -> handler -> disk round-trip. It complements
// TestE2E_ProvenanceSurvivesStaging in e2e_test.go, which covers the
// filesystem backend.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/ingest"
	"github.com/leftathome/glovebox/internal/staging"
)

// httpRoundtripIngestConfig returns an IngestConfig with generous limits so the
// round-trip is exercised end-to-end without tripping size/backpressure gates.
func httpRoundtripIngestConfig() config.IngestConfig {
	return config.IngestConfig{
		Enabled:               true,
		Port:                  0,
		MaxBodyBytes:          1 << 20, // 1 MiB
		MaxMetadataBytes:      1 << 16, // 64 KiB
		BackpressureThreshold: 100,
		RequestTimeoutSeconds: 60,
	}
}

// TestHTTPBackendProvenanceRoundTrip drives a single item through the REAL
// HTTP backend and the REAL /v1/ingest handler, then reads the staged
// metadata.json off disk and asserts the producer-asserted provenance
// (data_subject, audience, identity) survived unchanged.
func TestHTTPBackendProvenanceRoundTrip(t *testing.T) {
	const destAgent = "health-agent"

	// REAL ingest handler on a temp staging dir, with an allowlist that
	// permits the destination agent. Mirrors internal/ingest/handler_test.go's
	// newTestHandler wiring (NewHandler + SetReady + a /v1/ingest mux).
	stagingDir := t.TempDir()
	handler := ingest.NewHandler(stagingDir, httpRoundtripIngestConfig(), []string{destAgent})
	handler.SetReady()

	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", handler)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// REAL HTTP staging backend pointed at the test server's /v1/ingest.
	// WithRetry keeps the backoff tiny so a (not expected) transient retry
	// does not stall the test.
	backend := connector.NewHTTPStagingBackend(ts.URL+"/v1/ingest", "walhelm", ts.Client()).
		WithRetry(2, time.Millisecond)

	item, err := backend.NewItem(connector.ItemOptions{
		Source:           "walhelm",
		Sender:           "care-team",
		Subject:          "weekly summary",
		Timestamp:        time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
		DestinationAgent: destAgent,
		ContentType:      "message/rfc822",
		DataSubject:      "walhelm:9f2a",
		Audience:         []string{"subject", "guardians"},
		Identity: &connector.Identity{
			Provider:   "kp-wa",
			AuthMethod: "browser_session",
			AccountID:  "leftathome",
		},
	})
	if err != nil {
		t.Fatalf("backend.NewItem: %v", err)
	}

	if err := item.WriteContent([]byte("From: care@example.com\r\nSubject: weekly summary\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("WriteContent: %v", err)
	}

	if err := item.Commit(); err != nil {
		t.Fatalf("Commit (POST to /v1/ingest): %v", err)
	}

	// Locate the single staged item the handler persisted. Item names are
	// generated server-side, so scan the staging dir for the non-hidden
	// directory the handler renamed into place.
	meta := readOnlyStagedMetadata(t, stagingDir)

	// Provenance must have survived backend -> POST -> handler -> disk.
	if meta.DataSubject != "walhelm:9f2a" {
		t.Errorf("data_subject = %q, want walhelm:9f2a", meta.DataSubject)
	}
	wantAud := []string{"subject", "guardians"}
	if !slices.Equal(meta.Audience, wantAud) {
		t.Errorf("audience = %v, want %v", meta.Audience, wantAud)
	}
	if meta.Identity == nil {
		t.Fatalf("identity is nil; want acquisition identity")
	}
	if meta.Identity.Provider != "kp-wa" {
		t.Errorf("identity.provider = %q, want kp-wa", meta.Identity.Provider)
	}
	if meta.Identity.AuthMethod != "browser_session" {
		t.Errorf("identity.auth_method = %q, want browser_session", meta.Identity.AuthMethod)
	}
	if meta.Identity.AccountID != "leftathome" {
		t.Errorf("identity.account_id = %q, want leftathome", meta.Identity.AccountID)
	}
}

// readOnlyStagedMetadata finds the single non-hidden item directory the ingest
// handler staged under stagingDir and returns its parsed metadata.json. It
// fails the test if there is not exactly one staged item.
func readOnlyStagedMetadata(t *testing.T, stagingDir string) staging.ItemMetadata {
	t.Helper()

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}

	var itemDirs []string
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] != '.' {
			itemDirs = append(itemDirs, e.Name())
		}
	}
	if len(itemDirs) != 1 {
		t.Fatalf("staged %d item dirs (%v), want exactly 1", len(itemDirs), itemDirs)
	}

	metaBytes, err := os.ReadFile(filepath.Join(stagingDir, itemDirs[0], "metadata.json"))
	if err != nil {
		t.Fatalf("read staged metadata.json: %v", err)
	}
	var meta staging.ItemMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("unmarshal staged metadata.json: %v", err)
	}
	return meta
}
