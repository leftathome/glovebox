package connector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/staging"
)

// A connector that has not declared a tier must fail at bootstrap. This is the
// safety property the whole change exists for: the alternative failure is
// silent misrouting into per-agent recall, discovered only by measuring the
// index months later (openclaw-iw1s).
func TestNewFrameworkRejectsUndeclaredTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier Tier
	}{
		{"unset", Tier("")},
		{"wrong case", Tier("Feed")},
		{"plural", Tier("feeds")},
		{"unknown", Tier("durable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := frameworkTestOpts(t, "fw-tier", pickPort(t), &mockPollConnector{})
			opts.Tier = tc.tier

			fw, err := NewFramework(opts)
			if err == nil {
				fw.Shutdown()
				t.Fatalf("NewFramework accepted tier %q; a connector must not start without a valid declaration", string(tc.tier))
			}
			// Permanent: retrying will not help, the code is wrong.
			if !IsPermanent(err) {
				t.Errorf("tier error should be permanent, got %v", err)
			}
			if !strings.Contains(err.Error(), "Tier") {
				t.Errorf("error should name the missing field, got %v", err)
			}
		})
	}
}

func TestNewFrameworkAcceptsDeclaredTier(t *testing.T) {
	for _, tier := range []Tier{TierFeed, TierPersonal} {
		t.Run(string(tier), func(t *testing.T) {
			opts := frameworkTestOpts(t, "fw-tier-ok", pickPort(t), &mockPollConnector{})
			opts.Tier = tier

			fw, err := NewFramework(opts)
			if err != nil {
				t.Fatalf("NewFramework(tier=%q): %v", string(tier), err)
			}
			fw.Shutdown()
		})
	}
}

// The declaration must actually reach metadata.json, since that file is the
// entire contract with openclaw's triage.
func TestCommittedItemCarriesDeclaredTier(t *testing.T) {
	for _, tier := range []Tier{TierFeed, TierPersonal} {
		t.Run(string(tier), func(t *testing.T) {
			stagingDir := filepath.Join(t.TempDir(), "staging")
			if err := os.MkdirAll(stagingDir, 0755); err != nil {
				t.Fatal(err)
			}
			w, err := NewStagingWriter(stagingDir, "test-connector")
			if err != nil {
				t.Fatal(err)
			}
			w.SetTier(tier)

			item, err := w.NewItem(validItemOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := item.WriteContent([]byte("body")); err != nil {
				t.Fatal(err)
			}
			if err := item.Commit(); err != nil {
				t.Fatal(err)
			}

			meta := readOnlyStagedMetadata(t, stagingDir)
			if meta.Tier != string(tier) {
				t.Fatalf("metadata.json tier = %q, want %q", meta.Tier, string(tier))
			}
		})
	}
}

// An undeclared writer must omit `tier` entirely rather than emit "". triage
// distinguishes absent (fall back to the legacy source allowlist) from set, so
// an empty string on the wire would be a third state neither side handles.
func TestUndeclaredWriterOmitsTierKey(t *testing.T) {
	stagingDir := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		t.Fatal(err)
	}
	w, err := NewStagingWriter(stagingDir, "test-connector")
	if err != nil {
		t.Fatal(err)
	}
	// deliberately no SetTier

	item, err := w.NewItem(validItemOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	raw := readOnlyStagedMetadataRaw(t, stagingDir)
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if _, present := generic["tier"]; present {
		t.Fatalf("metadata.json contains a `tier` key for an undeclared writer; "+
			"triage must be able to tell absent from set. Got: %s", string(raw))
	}
}

// Every connector and importer binary must declare a tier in its
// connector.Options literal. The runtime check in NewFramework already
// guarantees this, but only once the binary is actually run -- which for a
// connector nobody has deployed yet could be months later. This catches it in
// CI at the moment the connector is written, which is the point.
func TestEveryConnectorDeclaresATier(t *testing.T) {
	roots := []string{filepath.Join("..", "connectors"), filepath.Join("..", "importers")}
	optsLiteral := regexp.MustCompile(`connector\.Options\{`)
	tierField := regexp.MustCompile(`Tier:\s*connector\.Tier(Feed|Personal)\b`)

	checked := 0
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || filepath.Base(path) != "main.go" {
				return err
			}
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			// Only binaries that actually bootstrap the framework are in scope;
			// e.g. schoology-auth-refresher is a cronjob that stages nothing.
			if !optsLiteral.Match(src) {
				return nil
			}
			checked++
			if !tierField.Match(src) {
				t.Errorf("%s builds a connector.Options without a Tier declaration; "+
					"every connector must declare TierFeed or TierPersonal (see connector/tier.go)", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	// Guard the guard: if the walk silently matched nothing, the test would
	// pass while checking nothing at all.
	if checked < 20 {
		t.Fatalf("only found %d framework binaries to check; the walk is probably broken", checked)
	}
}

// validItemOptions is the minimum ItemOptions that passes metadata validation;
// none of these fields are what these tests are about.
func validItemOptions() ItemOptions {
	return ItemOptions{
		Source:           "test",
		Sender:           "tester@example.com",
		Subject:          "subject",
		Timestamp:        time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "main",
		ContentType:      "text/plain",
	}
}

func readOnlyStagedMetadataRaw(t *testing.T, stagingDir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(stagingDir, e.Name(), "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	t.Fatalf("no committed item found under %s", stagingDir)
	return nil
}

func readOnlyStagedMetadata(t *testing.T, stagingDir string) staging.ItemMetadata {
	t.Helper()
	var meta staging.ItemMetadata
	if err := json.Unmarshal(readOnlyStagedMetadataRaw(t, stagingDir), &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}
