// e2e_test.go -- synthetic end-to-end test of the walhelm provenance +
// resolution flow (spec 15 sec 8). It wires the REAL SP1 components with no
// mocks on the core path:
//
//  1. archives.Finalize: an archive/walhelm-export tar upload is finalized into
//     a staged archive dir (archives/<id>/) carrying a FinalizeReceipt
//     (metadata.json) with Acquisition + DataSubject + Audience, plus tree/.
//  2. walhelmImporter.Import: runs against the staged dir with a filesystem
//     StagingWriter (the real connector backend), staging one item per tree
//     file. Each item carries the receipt's provenance.
//  3. routing.SubjectGate: applied to each staged item against a
//     subject.SubjectRegistry. Resolved subjects pass (metadata rewritten to the
//     entity_id, then RoutePass to a destination); unresolved subjects with
//     enforce=on quarantine via RouteQuarantine("subject_unresolved").
//
// The afq4.12 round-trip assertion proves provenance survives the
// importer->staging path the resolver depends on: after Import, each on-disk
// metadata.json carries data_subject (the pre-resolution principal), audience,
// and the acquisition identity (provider/auth_method/account_id from acq_*).
//
// Archive-setup approach: REAL archives.Finalize. The finalize_test.go helpers
// (setupFixture/buildFinalizeTar) are test-only and live in the archives
// package, so they are not importable here; instead we construct the
// UploadState + Metadata directly (all exported fields) and build the tar with
// archive/tar. This exercises the genuine sha256-verify + untar + receipt-write
// publish path rather than a hand-crafted staged dir.
//
// Backend coverage: filesystem StagingWriter only. The round-trip assertion
// reads the committed metadata.json off disk.
// TODO: HTTP-backend round-trip (POST to a test ingest server) is not covered
// here; it would require standing up the ingest server in-test. The filesystem
// path is the one the resolver consumes today.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/ingest"
	"github.com/leftathome/glovebox/internal/ingest/archives"
	"github.com/leftathome/glovebox/internal/routing"
	"github.com/leftathome/glovebox/internal/staging"
	"github.com/leftathome/glovebox/internal/subject"
)

// e2ePrincipal is the producer-asserted data_subject principal carried through
// the whole flow. The registry maps it to entity_id e_1 in the resolved
// scenario and omits it in the unresolved scenario.
const e2ePrincipal = "walhelm:9f2a"

// e2eTreeFiles is the synthetic archive tree: two files in distinct
// data-category folders so the wildcard rule routes both.
func e2eTreeFiles() map[string][]byte {
	return map[string][]byte{
		"message/a.eml": []byte("From: a@example.com\r\nSubject: hi\r\n\r\nbody\r\n"),
		"lab/b.json":    []byte(`{"result":"ok"}`),
	}
}

// buildWalhelmTar builds an uncompressed tarball from name->bytes, mirroring
// the shape archives.Finalize's MediaTar branch expects. Kept local so this
// test does not depend on the archives package's test-only buildFinalizeTar.
func buildWalhelmTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	// Deterministic entry order for a stable sha256.
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		body := files[name]
		hdr := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o600,
			Size:     int64(len(body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader %q: %v", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("tar Write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

// finalizeWalhelmArchive runs the REAL archives.Finalize over a synthetic
// archive/walhelm-export upload and returns the published staged archive dir
// (<stagingRoot>/archives/<archiveID>) plus the receipt. The UploadState +
// Metadata are constructed directly (all exported) to match what the PATCH
// handler + ParseUploadMetadata would have produced for a walhelm upload.
func finalizeWalhelmArchive(t *testing.T, files map[string][]byte) (string, *archives.FinalizeReceipt) {
	t.Helper()

	tarball := buildWalhelmTar(t, files)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".tmp-archives"), 0o700); err != nil {
		t.Fatalf("mkdir .tmp-archives: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir archives: %v", err)
	}

	uploadID := "e2edeadbeefe2edeadbeefe2edeadbeef"
	archiveID := "e2e-walhelm-archive-001"

	// Write the tmp file as the PATCH path would have done.
	tmpPath := filepath.Join(root, ".tmp-archives", uploadID)
	if err := os.WriteFile(tmpPath, tarball, 0o600); err != nil {
		t.Fatalf("write tmp upload: %v", err)
	}

	// Rolling sha256 as the TeeReader-fed PATCH handler would have left it.
	h := sha256.New()
	h.Write(tarball)
	sum := hex.EncodeToString(h.Sum(nil))

	meta := &archives.Metadata{
		ArchiveID:           archiveID,
		ArchiveFilename:     "walhelm.tar",
		SubtreeRelativePath: ".",
		MediaType:           "archive/walhelm-export",
		MatcherID:           "test/matcher",
		Provider:            "test",
		SHA256:              sum,
		SizeBytes:           int64(len(tarball)),
		// Producer-asserted provenance (spec 15 sec 4.2), as
		// ParseUploadMetadata would set for a walhelm-export upload.
		AcqProvider:   "kp-wa",
		AcqAccountID:  "leftathome",
		AcqAuthMethod: "browser_session",
		DataSubject:   e2ePrincipal,
		Audience:      []string{"subject"},
	}

	st := &archives.UploadState{
		ID:           uploadID,
		SourceID:     "recognizer",
		ArchiveID:    archiveID,
		UploadLength: int64(len(tarball)),
		Offset:       int64(len(tarball)),
		Hasher:       h,
		Meta:         meta,
	}

	// delivered_by drives receipt.DeliveredBy (the staging Sender) + the
	// receipt.Identity block; the acquisition identity is separate.
	ctx := ingest.WithDeliveredBy(context.Background(), "recognizer")
	receipt, err := archives.Finalize(ctx, st, archives.FinalizeConfig{StagingRoot: root})
	if err != nil {
		t.Fatalf("archives.Finalize: %v", err)
	}

	stagedDir := filepath.Join(root, "archives", archiveID)
	return stagedDir, receipt
}

// newE2EImporter builds a walhelmImporter wired to a real filesystem
// StagingWriter committing into a fresh temp staging dir, plus a wildcard
// matcher routing every entry to destAgent. Returns the importer and the
// staging dir so committed items can be read back.
func newE2EImporter(t *testing.T, destAgent string) (*walhelmImporter, string) {
	t.Helper()

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "walhelm")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	writer.SetEnrichRegistry(enrich.NewRegistry())

	fw := &connector.Framework{
		Name:    "walhelm",
		Logger:  testLogger(),
		Matcher: connector.NewRuleMatcher([]connector.Rule{{Match: "*", Destination: destAgent}}),
		Backend: writer,
	}
	return &walhelmImporter{fw: fw, sourceName: "walhelm-src", concurrency: 2}, stagingDir
}

// readStagedItems reads every committed staging item directory under stagingDir
// into a staging.StagingItem (DirPath/ContentPath/Metadata) so the routing
// layer can operate on it exactly as the real scanner would. The .tmp subtree
// is skipped. Results are sorted by content_type for deterministic assertions.
func readStagedItems(t *testing.T, stagingDir string) []staging.StagingItem {
	t.Helper()

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}

	var items []staging.StagingItem
	for _, e := range entries {
		if !e.IsDir() || e.Name() == ".tmp" {
			continue
		}
		dir := filepath.Join(stagingDir, e.Name())
		metaBytes, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
		if err != nil {
			t.Fatalf("read metadata.json in %s: %v", dir, err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			t.Fatalf("unmarshal metadata.json in %s: %v", dir, err)
		}
		items = append(items, staging.StagingItem{
			DirPath:     dir,
			ContentPath: filepath.Join(dir, "content.raw"),
			Metadata:    meta,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].Metadata.ContentType < items[j].Metadata.ContentType
	})
	return items
}

// writeE2ERegistry writes a subjects.json mapping e2ePrincipal -> e_1 (with a
// default audience) when registerPrincipal is true, and an empty subjects list
// otherwise. enforce is templated. Returns the loaded registry.
func writeE2ERegistry(t *testing.T, registerPrincipal, enforce bool) *subject.SubjectRegistry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "subjects.json")

	enforceStr := "false"
	if enforce {
		enforceStr = "true"
	}

	var subjectsJSON string
	if registerPrincipal {
		subjectsJSON = `[
			{
				"entity_id": "e_1",
				"display": "Walhelm Subject",
				"principals": ["` + e2ePrincipal + `"],
				"default_audience": ["subject", "guardians"]
			}
		]`
	} else {
		subjectsJSON = `[]`
	}

	content := `{"enforce": ` + enforceStr + `, "subjects": ` + subjectsJSON + `}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	reg, err := subject.Load(path)
	if err != nil {
		t.Fatalf("subject.Load: %v", err)
	}
	return reg
}

// newE2ELogger returns an audit.Logger writing into a fresh temp dir, plus that
// dir so the test can read the resulting jsonl streams.
func newE2ELogger(t *testing.T) (*audit.Logger, string) {
	t.Helper()
	auditDir := t.TempDir()
	logger, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}
	return logger, auditDir
}

// assertProvenanceSurvived checks the afq4.12 round-trip contract on a staged
// item's on-disk metadata: data_subject (the principal, pre-resolution),
// audience, and the acquisition identity (from acq_*) must all be present.
func assertProvenanceSurvived(t *testing.T, item staging.StagingItem) {
	t.Helper()

	// Read the on-disk metadata.json (not the in-memory copy) to prove the
	// importer->staging persistence carried provenance.
	metaBytes, err := os.ReadFile(filepath.Join(item.DirPath, "metadata.json"))
	if err != nil {
		t.Fatalf("read on-disk metadata.json: %v", err)
	}
	var meta staging.ItemMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("unmarshal on-disk metadata.json: %v", err)
	}

	if meta.DataSubject != e2ePrincipal {
		t.Errorf("on-disk data_subject = %q, want %q (principal, pre-resolution)", meta.DataSubject, e2ePrincipal)
	}
	if len(meta.Audience) != 1 || meta.Audience[0] != "subject" {
		t.Errorf("on-disk audience = %v, want [subject]", meta.Audience)
	}
	if meta.Identity == nil {
		t.Fatalf("on-disk identity is nil; want acquisition identity")
	}
	if meta.Identity.Provider != "kp-wa" {
		t.Errorf("on-disk identity.provider = %q, want kp-wa", meta.Identity.Provider)
	}
	if meta.Identity.AuthMethod != "browser_session" {
		t.Errorf("on-disk identity.auth_method = %q, want browser_session", meta.Identity.AuthMethod)
	}
	if meta.Identity.AccountID != "leftathome" {
		t.Errorf("on-disk identity.account_id = %q, want leftathome", meta.Identity.AccountID)
	}
}

// TestE2E_ProvenanceSurvivesStaging drives Finalize -> Import and asserts the
// afq4.12 round-trip: every staged item's on-disk metadata.json carries the
// producer-asserted data_subject, audience, and acquisition identity. This is
// the provenance contract the resolver depends on.
func TestE2E_ProvenanceSurvivesStaging(t *testing.T) {
	stagedDir, receipt := finalizeWalhelmArchive(t, e2eTreeFiles())

	// Sanity: the receipt that Finalize produced must itself carry provenance.
	if receipt.MediaType != "archive/walhelm-export" {
		t.Fatalf("receipt media_type = %q, want archive/walhelm-export", receipt.MediaType)
	}
	if receipt.DataSubject != e2ePrincipal {
		t.Fatalf("receipt data_subject = %q, want %q", receipt.DataSubject, e2ePrincipal)
	}
	if receipt.Acquisition == nil || receipt.Acquisition.Provider != "kp-wa" {
		t.Fatalf("receipt acquisition = %+v, want provider kp-wa", receipt.Acquisition)
	}

	imp, stagingDir := newE2EImporter(t, "health-agent")
	if err := imp.Import(context.Background(), stagedDir, nil, nil, zeroDecision()); err != nil {
		t.Fatalf("Import: %v", err)
	}

	items := readStagedItems(t, stagingDir)
	if len(items) != 2 {
		t.Fatalf("staged %d items, want 2", len(items))
	}
	for _, it := range items {
		assertProvenanceSurvived(t, it)
	}
}

// TestE2E_WalhelmExport_ResolvedRoutesToDestination drives the full happy path:
// Finalize -> Import -> SubjectGate (registered principal, enforce on) ->
// RoutePass. Asserts each item resolves (GatePass), the on-disk metadata.json
// is rewritten to the entity_id, and the destination copy carries entity_id +
// the registry default audience.
func TestE2E_WalhelmExport_ResolvedRoutesToDestination(t *testing.T) {
	stagedDir, _ := finalizeWalhelmArchive(t, e2eTreeFiles())

	imp, stagingDir := newE2EImporter(t, "health-agent")
	if err := imp.Import(context.Background(), stagedDir, nil, nil, zeroDecision()); err != nil {
		t.Fatalf("Import: %v", err)
	}

	items := readStagedItems(t, stagingDir)
	if len(items) != 2 {
		t.Fatalf("staged %d items, want 2", len(items))
	}

	reg := writeE2ERegistry(t, true /*register*/, true /*enforce*/)
	logger, _ := newE2ELogger(t)
	defer logger.Close()
	destDir := t.TempDir()

	for _, item := range items {
		// Pre-gate: the staged metadata carries the raw principal.
		if item.Metadata.DataSubject != e2ePrincipal {
			t.Fatalf("pre-gate data_subject = %q, want %q", item.Metadata.DataSubject, e2ePrincipal)
		}

		action, err := routing.SubjectGate(&item, reg)
		if err != nil {
			t.Fatalf("SubjectGate: %v", err)
		}
		if action != routing.GatePass {
			t.Fatalf("action = %v, want GatePass", action)
		}
		// In-memory metadata rewritten to entity_id.
		if item.Metadata.DataSubject != "e_1" {
			t.Errorf("in-memory data_subject = %q, want e_1", item.Metadata.DataSubject)
		}

		// Persisted (on-disk) metadata.json rewritten to entity_id.
		onDisk := readSingleMetadata(t, item.DirPath)
		if onDisk.DataSubject != "e_1" {
			t.Errorf("persisted data_subject = %q, want e_1", onDisk.DataSubject)
		}

		// Route the resolved item to the destination.
		result := engine.ScanResult{Verdict: engine.VerdictPass}
		if err := routing.RoutePass(item, result, destDir, logger, time.Millisecond); err != nil {
			t.Fatalf("RoutePass: %v", err)
		}

		// Destination copy must carry entity_id and the item's own audience.
		// ResolveItem fills the registry default_audience ONLY when the item
		// declared none; the walhelm receipt declared audience=[subject], so
		// that producer-asserted value is preserved through resolution (the
		// registry default [subject, guardians] is NOT substituted).
		destMeta := readSingleMetadata(t, filepath.Join(destDir, filepath.Base(item.DirPath)))
		if destMeta.DataSubject != "e_1" {
			t.Errorf("dest data_subject = %q, want e_1", destMeta.DataSubject)
		}
		wantAud := []string{"subject"}
		if len(destMeta.Audience) != len(wantAud) {
			t.Fatalf("dest audience = %v, want %v", destMeta.Audience, wantAud)
		}
		for i, a := range wantAud {
			if destMeta.Audience[i] != a {
				t.Errorf("dest audience[%d] = %q, want %q", i, destMeta.Audience[i], a)
			}
		}
	}
}

// TestE2E_WalhelmExport_UnresolvedQuarantines drives Finalize -> Import ->
// SubjectGate with a registry that does NOT contain the principal and
// enforce=on. Each item must GateQuarantine and route via
// RouteQuarantine("subject_unresolved"); the rejected.jsonl must carry the
// reason and no destination copy may exist.
func TestE2E_WalhelmExport_UnresolvedQuarantines(t *testing.T) {
	stagedDir, _ := finalizeWalhelmArchive(t, e2eTreeFiles())

	imp, stagingDir := newE2EImporter(t, "health-agent")
	if err := imp.Import(context.Background(), stagedDir, nil, nil, zeroDecision()); err != nil {
		t.Fatalf("Import: %v", err)
	}

	items := readStagedItems(t, stagingDir)
	if len(items) != 2 {
		t.Fatalf("staged %d items, want 2", len(items))
	}

	reg := writeE2ERegistry(t, false /*register*/, true /*enforce*/)
	logger, auditDir := newE2ELogger(t)
	defer logger.Close()
	destDir := t.TempDir()
	quarantineDir := t.TempDir()
	notifyDir := filepath.Join(t.TempDir(), "notifications")

	for _, item := range items {
		baseID := filepath.Base(item.DirPath)

		action, err := routing.SubjectGate(&item, reg)
		if err != nil {
			t.Fatalf("SubjectGate: %v", err)
		}
		if action != routing.GateQuarantine {
			t.Fatalf("action = %v, want GateQuarantine", action)
		}

		result := engine.ScanResult{Verdict: engine.VerdictQuarantine}
		if err := routing.RouteQuarantine(item, result, quarantineDir, notifyDir, logger, 0.8, time.Millisecond, routing.ReasonSubjectUnresolved); err != nil {
			t.Fatalf("RouteQuarantine: %v", err)
		}

		// No destination copy may exist for a quarantined item.
		if _, err := os.Stat(filepath.Join(destDir, baseID)); !os.IsNotExist(err) {
			t.Errorf("destination copy exists for quarantined item, want none (err=%v)", err)
		}
	}

	rejected, err := os.ReadFile(filepath.Join(auditDir, "rejected.jsonl"))
	if err != nil {
		t.Fatalf("read rejected.jsonl: %v", err)
	}
	if !bytes.Contains(rejected, []byte(`"reason":"subject_unresolved"`)) {
		t.Errorf("rejected.jsonl missing subject_unresolved reason:\n%s", rejected)
	}
}

// readSingleMetadata reads + unmarshals metadata.json from dir.
func readSingleMetadata(t *testing.T, dir string) staging.ItemMetadata {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json in %s: %v", dir, err)
	}
	var meta staging.ItemMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal metadata.json in %s: %v", dir, err)
	}
	return meta
}
