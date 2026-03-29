package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"staging_dir": "/data/staging",
		"quarantine_dir": "/data/quarantine",
		"audit_dir": "/data/audit",
		"failed_dir": "/data/failed",
		"agents_dir": "/data/agents",
		"shared_dir": "/data/shared",
		"agent_allowlist": ["messaging", "media"],
		"metrics_port": 9090,
		"watch_mode": "fsnotify",
		"poll_interval_seconds": 10,
		"rules_file": "/etc/rules.json",
		"scan_workers": 8,
		"scan_timeout_seconds": 60,
		"scan_chunk_size_bytes": 524288
	}`), 0644)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StagingDir != "/data/staging" {
		t.Errorf("staging_dir = %q, want /data/staging", cfg.StagingDir)
	}
	if len(cfg.AgentAllowlist) != 2 {
		t.Errorf("agent_allowlist len = %d, want 2", len(cfg.AgentAllowlist))
	}
	if cfg.ScanWorkers != 8 {
		t.Errorf("scan_workers = %d, want 8", cfg.ScanWorkers)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadConfig_EnvVarOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"staging_dir": "/original"}`), 0644)
	t.Setenv("GLOVEBOX_STAGING_DIR", "/overridden")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StagingDir != "/overridden" {
		t.Errorf("staging_dir = %q, want /overridden", cfg.StagingDir)
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{}`), 0644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ScanWorkers != 4 {
		t.Errorf("scan_workers default = %d, want 4", cfg.ScanWorkers)
	}
	if cfg.ScanTimeoutSeconds != 30 {
		t.Errorf("scan_timeout default = %d, want 30", cfg.ScanTimeoutSeconds)
	}
	if cfg.ScanChunkSizeBytes != 262144 {
		t.Errorf("scan_chunk_size default = %d, want 262144", cfg.ScanChunkSizeBytes)
	}
	if cfg.WatchMode != "fsnotify" {
		t.Errorf("watch_mode default = %q, want fsnotify", cfg.WatchMode)
	}
	if cfg.MetricsPort != 9090 {
		t.Errorf("metrics_port default = %d, want 9090", cfg.MetricsPort)
	}
}

func TestLoadConfig_AllEnvVars(t *testing.T) {
	t.Setenv("GLOVEBOX_STAGING_DIR", "/env/staging")
	t.Setenv("GLOVEBOX_QUARANTINE_DIR", "/env/quarantine")
	t.Setenv("GLOVEBOX_AUDIT_DIR", "/env/audit")
	t.Setenv("GLOVEBOX_AGENTS_DIR", "/env/agents")
	t.Setenv("GLOVEBOX_SCAN_WORKERS", "16")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StagingDir != "/env/staging" {
		t.Errorf("staging_dir = %q, want /env/staging", cfg.StagingDir)
	}
	if cfg.ScanWorkers != 16 {
		t.Errorf("scan_workers = %d, want 16", cfg.ScanWorkers)
	}
}
