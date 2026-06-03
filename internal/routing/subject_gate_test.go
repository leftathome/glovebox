package routing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
	"github.com/leftathome/glovebox/internal/subject"
)

// subjectsRegistryJSON maps the principal walhelm:9f2a to entity_id e_1 with a
// default audience of [subject, guardians]. enforce is templated by the caller.
func writeGateRegistry(t *testing.T, enforce bool) *subject.SubjectRegistry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "subjects.json")
	content := `{
		"enforce": ` + boolStr(enforce) + `,
		"subjects": [
			{
				"entity_id": "e_1",
				"display": "Walhelm Subject",
				"principals": ["walhelm:9f2a"],
				"default_audience": ["subject", "guardians"]
			}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	reg, err := subject.Load(path)
	if err != nil {
		t.Fatalf("subject.Load: %v", err)
	}
	return reg
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// setupGateItem stages an item on disk with the given data_subject and returns
// the StagingItem plus the temp dirs needed to route it.
func setupGateItem(t *testing.T, dataSubject string) (staging.StagingItem, string, string, string, *audit.Logger) {
	t.Helper()
	base := t.TempDir()

	stagingDir := filepath.Join(base, "staging", "20260603-gateitem")
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "content.raw"), []byte("benign health record"), 0644); err != nil {
		t.Fatalf("write content: %v", err)
	}

	meta := staging.ItemMetadata{
		Source:           "walhelm",
		Sender:           "recognizer",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "health",
		ContentType:      "text/plain",
		DataSubject:      dataSubject,
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "metadata.json"), metaBytes, 0644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	destDir := filepath.Join(base, "agents", "health")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}

	quarantineDir := filepath.Join(base, "quarantine")
	if err := os.MkdirAll(quarantineDir, 0755); err != nil {
		t.Fatalf("mkdir quarantine: %v", err)
	}

	auditDir := filepath.Join(base, "audit")
	if err := os.MkdirAll(auditDir, 0755); err != nil {
		t.Fatalf("mkdir audit: %v", err)
	}
	logger, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	item := staging.StagingItem{
		DirPath:     stagingDir,
		ContentPath: filepath.Join(stagingDir, "content.raw"),
		Metadata:    meta,
	}
	return item, destDir, quarantineDir, auditDir, logger
}

// TestSubjectGate_ResolvedRoutesWithEntityID: a registered principal, clean
// scan, enforce=true -> GatePass with the on-disk + in-memory metadata rewritten
// to the entity_id and the registry default audience, so a subsequent RoutePass
// delivers metadata.json carrying data_subject==e_1 and audience==[subject,guardians].
func TestSubjectGate_ResolvedRoutesWithEntityID(t *testing.T) {
	reg := writeGateRegistry(t, true)
	item, destDir, _, _, logger := setupGateItem(t, "walhelm:9f2a")
	defer logger.Close()

	action, err := SubjectGate(&item, reg)
	if err != nil {
		t.Fatalf("SubjectGate: %v", err)
	}
	if action != GatePass {
		t.Fatalf("action = %v, want GatePass", action)
	}
	if item.Metadata.DataSubject != "e_1" {
		t.Errorf("in-memory DataSubject = %q, want e_1", item.Metadata.DataSubject)
	}

	// Route to destination using the resolved metadata.
	result := engine.ScanResult{Verdict: engine.VerdictPass}
	if err := RoutePass(item, result, destDir, logger, time.Millisecond); err != nil {
		t.Fatalf("RoutePass: %v", err)
	}

	// The destination copy's metadata.json must carry the resolved entity_id
	// and the registry default audience.
	destMeta := filepath.Join(destDir, filepath.Base(item.DirPath), "metadata.json")
	data, err := os.ReadFile(destMeta)
	if err != nil {
		t.Fatalf("read dest metadata: %v", err)
	}
	var got staging.ItemMetadata
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal dest metadata: %v", err)
	}
	if got.DataSubject != "e_1" {
		t.Errorf("dest data_subject = %q, want e_1", got.DataSubject)
	}
	wantAud := []string{"subject", "guardians"}
	if len(got.Audience) != len(wantAud) {
		t.Fatalf("dest audience = %v, want %v", got.Audience, wantAud)
	}
	for i, a := range wantAud {
		if got.Audience[i] != a {
			t.Errorf("dest audience[%d] = %q, want %q", i, got.Audience[i], a)
		}
	}
}

// TestSubjectGate_UnresolvedEnforceQuarantines: an unknown subject, enforce=true
// -> GateQuarantine; routing it produces a rejected.jsonl entry with
// reason==subject_unresolved and no destination copy.
func TestSubjectGate_UnresolvedEnforceQuarantines(t *testing.T) {
	reg := writeGateRegistry(t, true)
	item, destDir, quarantineDir, auditDir, logger := setupGateItem(t, "walhelm:unknown")
	defer logger.Close()
	notifyDir := filepath.Join(t.TempDir(), "notifications")

	action, err := SubjectGate(&item, reg)
	if err != nil {
		t.Fatalf("SubjectGate: %v", err)
	}
	if action != GateQuarantine {
		t.Fatalf("action = %v, want GateQuarantine", action)
	}

	result := engine.ScanResult{Verdict: engine.VerdictQuarantine}
	if err := RouteQuarantine(item, result, quarantineDir, notifyDir, logger, 0.8, time.Millisecond, ReasonSubjectUnresolved); err != nil {
		t.Fatalf("RouteQuarantine: %v", err)
	}

	rejected, err := os.ReadFile(filepath.Join(auditDir, "rejected.jsonl"))
	if err != nil {
		t.Fatalf("read rejected.jsonl: %v", err)
	}
	if !strings.Contains(string(rejected), `"reason":"subject_unresolved"`) {
		t.Errorf("rejected.jsonl does not contain subject_unresolved reason: %s", rejected)
	}

	// No destination copy must exist.
	if _, err := os.Stat(filepath.Join(destDir, filepath.Base(item.DirPath))); !os.IsNotExist(err) {
		t.Errorf("destination copy exists, want none (err=%v)", err)
	}
}

// TestSubjectGate_SubjectlessBypass: an item with no data_subject, enforce=true
// -> GatePass (bypass), metadata untouched.
func TestSubjectGate_SubjectlessBypass(t *testing.T) {
	reg := writeGateRegistry(t, true)
	item, _, _, _, logger := setupGateItem(t, "")
	defer logger.Close()

	action, err := SubjectGate(&item, reg)
	if err != nil {
		t.Fatalf("SubjectGate: %v", err)
	}
	if action != GatePass {
		t.Fatalf("action = %v, want GatePass", action)
	}
	if item.Metadata.DataSubject != "" {
		t.Errorf("DataSubject = %q, want empty (untouched)", item.Metadata.DataSubject)
	}
	if item.Metadata.Audience != nil {
		t.Errorf("Audience = %v, want nil (untouched)", item.Metadata.Audience)
	}
}

// TestSubjectGate_UnresolvedEnforceOffPasses: an unknown subject with enforce=false
// -> GatePass (gate disabled); metadata is left as the original principal.
func TestSubjectGate_UnresolvedEnforceOffPasses(t *testing.T) {
	reg := writeGateRegistry(t, false)
	item, _, _, _, logger := setupGateItem(t, "walhelm:unknown")
	defer logger.Close()

	action, err := SubjectGate(&item, reg)
	if err != nil {
		t.Fatalf("SubjectGate: %v", err)
	}
	if action != GatePassUnresolved {
		t.Fatalf("action = %v, want GatePassUnresolved (enforce off)", action)
	}
	if item.Metadata.DataSubject != "walhelm:unknown" {
		t.Errorf("DataSubject = %q, want walhelm:unknown (unresolved, untouched)", item.Metadata.DataSubject)
	}
}

// TestSubjectGate_PersistFailureQuarantines verifies the fail-closed contract:
// when persistMetadata fails (DirPath points to a non-existent directory so
// os.OpenFile for the .tmp cannot succeed), SubjectGate must return a non-nil
// error AND an action other than GatePass (fail closed -- callers that check
// only the action must not route the item as if it succeeded).
func TestSubjectGate_PersistFailureQuarantines(t *testing.T) {
	reg := writeGateRegistry(t, true)
	item, _, _, _, logger := setupGateItem(t, "walhelm:9f2a")
	defer logger.Close()

	// Point DirPath at a non-existent directory so the .tmp file cannot be
	// created, forcing persistMetadata to fail.
	item.DirPath = filepath.Join(t.TempDir(), "does-not-exist", "subdir")

	action, err := SubjectGate(&item, reg)
	if err == nil {
		t.Fatalf("SubjectGate returned nil error on persist failure, want non-nil")
	}
	if action == GatePass {
		t.Errorf("action = GatePass on persist failure, want GateQuarantine (fail-closed)")
	}
}
