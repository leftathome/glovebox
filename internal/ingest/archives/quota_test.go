// Tests for the storage quota measurement (spec 13 §5.4).
//
// Coverage:
//
//   TestDirBytes_SumsRegularFiles            -- recursive byte total
//   TestDirBytes_MissingRootReturnsZero      -- absent dir is 0, no error
//   TestPerSourceBytes_AttributesByMetadata  -- delivered_by counts
//   TestPerSourceBytes_SkipsDotDirs          -- archives/.done/ excluded
//   TestPerSourceBytes_MissingMetadataSkipped -- in-progress publish window
//   TestMeasureOnce_PublishesGauges          -- atomics updated
//   TestQuotaHysteresis_TripAt95             -- false -> true at 95%
//   TestQuotaHysteresis_StaysAt90            -- true stays true at 90%
//   TestQuotaHysteresis_LiftAt84             -- true -> false at 84%
//   TestQuotaHysteresis_NoLowerCross         -- 86% does not lift
//   TestNewQuotaState_DefaultsAreSafe        -- ShouldBlock false, Pct 0
//   TestPerSourceBytes_DefaultsUnknown       -- empty delivered_by

package archives

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFinalizedArchive(t *testing.T, root, archiveID, deliveredBy string, body []byte) {
	t.Helper()
	dir := filepath.Join(root, "archives", archiveID)
	if err := os.MkdirAll(filepath.Join(dir, "raw"), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	receipt := FinalizeReceipt{
		ArchiveID:   archiveID,
		DeliveredBy: deliveredBy,
		MediaType:   "archive/mbox",
		SizeBytes:   int64(len(body)),
	}
	mb, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), mb, 0o600); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "raw", "content"), body, 0o600); err != nil {
		t.Fatalf("write raw content: %v", err)
	}
}

func TestDirBytes_SumsRegularFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "f1"), make([]byte, 100), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "f2"), make([]byte, 250), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "f3"), make([]byte, 50), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := dirBytes(root); got != 400 {
		t.Errorf("dirBytes = %d, want 400", got)
	}
}

func TestDirBytes_MissingRootReturnsZero(t *testing.T) {
	if got := dirBytes("/nonexistent-path-for-quota-test"); got != 0 {
		t.Errorf("dirBytes(missing) = %d, want 0", got)
	}
}

func TestPerSourceBytes_AttributesByMetadata(t *testing.T) {
	root := t.TempDir()
	writeFinalizedArchive(t, root, "a-1", "alpha", make([]byte, 1024))
	writeFinalizedArchive(t, root, "a-2", "alpha", make([]byte, 512))
	writeFinalizedArchive(t, root, "b-1", "beta", make([]byte, 2048))

	got := perSourceBytes(filepath.Join(root, "archives"))
	// dirBytes sums metadata.json + content; assert relative magnitudes
	// since exact bytes include JSON overhead.
	if got["alpha"] <= 1024+512 {
		t.Errorf("alpha bytes = %d, expected > %d", got["alpha"], 1024+512)
	}
	if got["beta"] <= 2048 {
		t.Errorf("beta bytes = %d, expected > %d", got["beta"], 2048)
	}
	if got["alpha"] >= got["beta"] {
		t.Errorf("alpha %d should be smaller than beta %d", got["alpha"], got["beta"])
	}
}

func TestPerSourceBytes_SkipsDotDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "archives", ".done", "old-id"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "archives", ".done", "old-id", "f"), make([]byte, 999), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeFinalizedArchive(t, root, "a-1", "alpha", make([]byte, 100))

	got := perSourceBytes(filepath.Join(root, "archives"))
	if _, ok := got[".done"]; ok {
		t.Errorf("perSourceBytes leaked .done entry: %v", got)
	}
	if got["alpha"] == 0 {
		t.Errorf("alpha entry missing: %v", got)
	}
}

func TestPerSourceBytes_MissingMetadataSkipped(t *testing.T) {
	root := t.TempDir()
	// Subdirectory exists but no metadata.json yet (mid-publish window
	// or a manual operator placement). Must be silently skipped.
	if err := os.MkdirAll(filepath.Join(root, "archives", "no-meta"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "archives", "no-meta", "stray"), make([]byte, 50), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeFinalizedArchive(t, root, "good", "alpha", make([]byte, 100))

	got := perSourceBytes(filepath.Join(root, "archives"))
	if _, ok := got["unknown"]; ok {
		t.Errorf("missing metadata.json should NOT produce unknown: %v", got)
	}
	if got["alpha"] == 0 {
		t.Errorf("good entry missing: %v", got)
	}
}

func TestPerSourceBytes_DefaultsUnknown(t *testing.T) {
	root := t.TempDir()
	// A metadata.json with an empty delivered_by lands under "unknown".
	dir := filepath.Join(root, "archives", "blank-deliverer")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mb, _ := json.Marshal(map[string]string{"delivered_by": ""})
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), mb, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := perSourceBytes(filepath.Join(root, "archives"))
	if got["unknown"] == 0 {
		t.Errorf("expected unknown bucket, got %v", got)
	}
}

func TestNewQuotaState_DefaultsAreSafe(t *testing.T) {
	q := NewQuotaState()
	if q.ShouldBlock() {
		t.Error("new QuotaState should not block")
	}
	if got := q.Pct(); got != 0 {
		t.Errorf("Pct = %v, want 0", got)
	}
	if got := q.PerSourceBytes(); len(got) != 0 {
		t.Errorf("PerSourceBytes = %v, want empty", got)
	}
}

func TestMeasureOnce_PublishesGauges(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".tmp-archives"), 0o700); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir archives: %v", err)
	}
	// 5000 bytes split across tmp + archives.
	writeFinalizedArchive(t, root, "a-1", "alpha", make([]byte, 1000))
	if err := os.WriteFile(filepath.Join(root, ".tmp-archives", "deadbeef"), make([]byte, 2000), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	q := NewQuotaState()
	cfg := QuotaConfig{
		StagingRoot:   root,
		CapacityBytes: 10000,
		HardTripPct:   95.0,
		HardLiftPct:   85.0,
	}
	measureOnce(context.Background(), cfg, q)

	// Combined bytes should exceed the raw 3000 by metadata.json size,
	// but still be well under 95% of 10000.
	if q.ShouldBlock() {
		t.Error("ShouldBlock = true at ~30% pct")
	}
	if got := q.Pct(); got < 20 || got > 50 {
		t.Errorf("Pct = %v, want range ~30%%", got)
	}
	if got := q.PerSourceBytes(); got["alpha"] == 0 {
		t.Errorf("PerSourceBytes missing alpha: %v", got)
	}
}

// hysteresisHelper drives the trip/lift decision without performing a
// filesystem walk: we directly seed q.pct and call the transition logic
// inline by constructing measureOnce with capacity=100 and pre-staged
// archives whose total bytes hit the desired percentage. To make this
// deterministic without writing megabytes, we use capacity=100 and
// scaling.
func hysteresisCapacity(t *testing.T, root string, wantPct int) QuotaConfig {
	t.Helper()
	return QuotaConfig{
		StagingRoot:   root,
		CapacityBytes: 100, // bytes; tests stage exactly wantPct bytes
		HardTripPct:   95.0,
		HardLiftPct:   85.0,
	}
}

func stageBytes(t *testing.T, root string, n int) {
	t.Helper()
	tmp := filepath.Join(root, ".tmp-archives")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir archives: %v", err)
	}
	// Replace the file each time so the total matches n exactly. Use
	// the tmp file path (no per-source metadata.json overhead).
	if err := os.WriteFile(filepath.Join(tmp, "size-driver"), make([]byte, n), 0o600); err != nil {
		t.Fatalf("write driver: %v", err)
	}
}

func TestQuotaHysteresis_TripAt95(t *testing.T) {
	root := t.TempDir()
	q := NewQuotaState()
	cfg := hysteresisCapacity(t, root, 95)

	stageBytes(t, root, 95)
	measureOnce(context.Background(), cfg, q)
	if !q.ShouldBlock() {
		t.Errorf("ShouldBlock=false at pct=%v, expected trip", q.Pct())
	}
}

func TestQuotaHysteresis_StaysAt90(t *testing.T) {
	root := t.TempDir()
	q := NewQuotaState()
	cfg := hysteresisCapacity(t, root, 95)

	// First trip at 95.
	stageBytes(t, root, 95)
	measureOnce(context.Background(), cfg, q)
	if !q.ShouldBlock() {
		t.Fatalf("did not trip at 95")
	}
	// Drop to 90, should stay tripped (above lift threshold of 85).
	stageBytes(t, root, 90)
	measureOnce(context.Background(), cfg, q)
	if !q.ShouldBlock() {
		t.Errorf("lifted at pct=%v, expected to stay tripped", q.Pct())
	}
}

func TestQuotaHysteresis_LiftAt84(t *testing.T) {
	root := t.TempDir()
	q := NewQuotaState()
	cfg := hysteresisCapacity(t, root, 95)

	stageBytes(t, root, 95)
	measureOnce(context.Background(), cfg, q)
	if !q.ShouldBlock() {
		t.Fatalf("did not trip at 95")
	}
	stageBytes(t, root, 84)
	measureOnce(context.Background(), cfg, q)
	if q.ShouldBlock() {
		t.Errorf("did not lift at pct=%v, expected lift", q.Pct())
	}
}

func TestQuotaHysteresis_NoLowerCross(t *testing.T) {
	// Starting at false, crossing only the lift threshold (e.g. 86%
	// from 50%) should NOT trip the bool.
	root := t.TempDir()
	q := NewQuotaState()
	cfg := hysteresisCapacity(t, root, 95)

	stageBytes(t, root, 86)
	measureOnce(context.Background(), cfg, q)
	if q.ShouldBlock() {
		t.Errorf("ShouldBlock=true at pct=%v, expected no trip below 95", q.Pct())
	}
}

func TestStartQuotaGoroutine_ExitsOnCtxCancel(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir archives: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".tmp-archives"), 0o700); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	q := NewQuotaState()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		StartQuotaGoroutine(ctx, QuotaConfig{
			StagingRoot:   root,
			CapacityBytes: 1000,
			Interval:      10 * time.Millisecond,
		}, q)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine did not exit within 1s after ctx cancel")
	}
}
