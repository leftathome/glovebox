package routing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/staging"
)

func TestRouteReject_StagingRemoved(t *testing.T) {
	base := t.TempDir()
	itemDir := filepath.Join(base, "staging", "bad-item")
	os.MkdirAll(itemDir, 0755)
	os.WriteFile(filepath.Join(itemDir, "content.raw"), []byte("junk"), 0644)

	auditDir := filepath.Join(base, "audit")
	os.MkdirAll(auditDir, 0755)
	logger, _ := audit.NewLogger(auditDir)
	defer logger.Close()

	err := RouteReject(itemDir, "malformed_metadata", nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(itemDir); !os.IsNotExist(err) {
		t.Error("staging directory should have been removed")
	}
}

func TestRouteReject_AuditLogged(t *testing.T) {
	base := t.TempDir()
	itemDir := filepath.Join(base, "staging", "bad-item")
	os.MkdirAll(itemDir, 0755)

	auditDir := filepath.Join(base, "audit")
	os.MkdirAll(auditDir, 0755)
	logger, _ := audit.NewLogger(auditDir)

	RouteReject(itemDir, "content_unreadable", nil, logger)
	logger.Close()

	data, err := os.ReadFile(filepath.Join(auditDir, "rejected.jsonl"))
	if err != nil {
		t.Fatalf("audit log not written: %v", err)
	}
	if len(data) == 0 {
		t.Error("audit log is empty")
	}
}

func TestRouteReject_NilMetadata(t *testing.T) {
	base := t.TempDir()
	itemDir := filepath.Join(base, "staging", "bad-item")
	os.MkdirAll(itemDir, 0755)

	auditDir := filepath.Join(base, "audit")
	os.MkdirAll(auditDir, 0755)
	logger, _ := audit.NewLogger(auditDir)
	defer logger.Close()

	err := RouteReject(itemDir, "malformed_metadata", nil, logger)
	if err != nil {
		t.Fatalf("should work with nil metadata: %v", err)
	}
}

func TestRouteReject_WithMetadata(t *testing.T) {
	base := t.TempDir()
	itemDir := filepath.Join(base, "staging", "bad-item")
	os.MkdirAll(itemDir, 0755)

	auditDir := filepath.Join(base, "audit")
	os.MkdirAll(auditDir, 0755)
	logger, _ := audit.NewLogger(auditDir)
	defer logger.Close()

	meta := &staging.ItemMetadata{
		Source:           "email",
		Sender:           "bad@actor.com",
		DestinationAgent: "messaging",
	}

	err := RouteReject(itemDir, "source_auth_failure", meta, logger)
	if err != nil {
		t.Fatal(err)
	}
}
