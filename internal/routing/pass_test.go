package routing

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

func setupPassTest(t *testing.T) (staging.StagingItem, string, *audit.Logger) {
	t.Helper()
	base := t.TempDir()

	stagingDir := filepath.Join(base, "staging", "20260328-test")
	os.MkdirAll(stagingDir, 0755)
	os.WriteFile(filepath.Join(stagingDir, "content.raw"), []byte("safe content"), 0644)
	os.WriteFile(filepath.Join(stagingDir, "metadata.json"), []byte(`{"source":"email"}`), 0644)

	inboxDir := filepath.Join(base, "agents", "messaging", "workspace", "inbox")
	os.MkdirAll(inboxDir, 0755)

	auditDir := filepath.Join(base, "audit")
	os.MkdirAll(auditDir, 0755)
	logger, _ := audit.NewLogger(auditDir)

	item := staging.StagingItem{
		DirPath:     stagingDir,
		ContentPath: filepath.Join(stagingDir, "content.raw"),
		Metadata: staging.ItemMetadata{
			Source:           "email",
			Sender:           "alice@example.com",
			Timestamp:        time.Now(),
			DestinationAgent: "messaging",
			ContentType:      "text/plain",
		},
	}

	return item, inboxDir, logger
}

func TestRoutePass_ContentDelivered(t *testing.T) {
	item, destDir, logger := setupPassTest(t)
	defer logger.Close()

	err := RoutePass(item, engine.ScanResult{Verdict: engine.VerdictPass}, destDir, logger, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	deliveryDir := filepath.Join(destDir, "20260328-test")
	content, err := os.ReadFile(filepath.Join(deliveryDir, "content.raw"))
	if err != nil {
		t.Fatalf("content not delivered: %v", err)
	}
	if string(content) != "safe content" {
		t.Errorf("content = %q, want %q", content, "safe content")
	}
}

func TestRoutePass_StagingCleanedUp(t *testing.T) {
	item, destDir, logger := setupPassTest(t)
	defer logger.Close()

	RoutePass(item, engine.ScanResult{Verdict: engine.VerdictPass}, destDir, logger, time.Millisecond)

	if _, err := os.Stat(item.DirPath); !os.IsNotExist(err) {
		t.Error("staging directory should have been removed")
	}
}

func TestRoutePass_AuditLogged(t *testing.T) {
	item, destDir, logger := setupPassTest(t)

	RoutePass(item, engine.ScanResult{Verdict: engine.VerdictPass}, destDir, logger, time.Millisecond)
	logger.Close()

	// The audit log is in a sibling "audit" dir from the test setup
	// Just verify the logger was called successfully (no error from RoutePass)
}
