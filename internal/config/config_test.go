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

func TestLoadConfigDefaults_Ingest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{}`), 0644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Ingest.Enabled {
		t.Error("ingest.enabled default should be true")
	}
	if cfg.Ingest.Port != 9091 {
		t.Errorf("ingest.port default = %d, want 9091", cfg.Ingest.Port)
	}
	if cfg.Ingest.MaxBodyBytes != 67108864 {
		t.Errorf("ingest.max_body_bytes default = %d, want 67108864", cfg.Ingest.MaxBodyBytes)
	}
	if cfg.Ingest.MaxMetadataBytes != 262144 {
		t.Errorf("ingest.max_metadata_bytes default = %d, want 262144", cfg.Ingest.MaxMetadataBytes)
	}
	if cfg.Ingest.BackpressureThreshold != 100 {
		t.Errorf("ingest.backpressure_threshold default = %d, want 100", cfg.Ingest.BackpressureThreshold)
	}
	if cfg.Ingest.RequestTimeoutSeconds != 60 {
		t.Errorf("ingest.request_timeout_seconds default = %d, want 60", cfg.Ingest.RequestTimeoutSeconds)
	}
}

func TestLoadConfigWithIngestBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"ingest": {
			"enabled": false,
			"port": 8080,
			"max_body_bytes": 1048576,
			"max_metadata_bytes": 4096,
			"backpressure_threshold": 50,
			"request_timeout_seconds": 30
		}
	}`), 0644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Ingest.Enabled {
		t.Error("ingest.enabled should be false")
	}
	if cfg.Ingest.Port != 8080 {
		t.Errorf("ingest.port = %d, want 8080", cfg.Ingest.Port)
	}
	if cfg.Ingest.MaxBodyBytes != 1048576 {
		t.Errorf("ingest.max_body_bytes = %d, want 1048576", cfg.Ingest.MaxBodyBytes)
	}
	if cfg.Ingest.MaxMetadataBytes != 4096 {
		t.Errorf("ingest.max_metadata_bytes = %d, want 4096", cfg.Ingest.MaxMetadataBytes)
	}
	if cfg.Ingest.BackpressureThreshold != 50 {
		t.Errorf("ingest.backpressure_threshold = %d, want 50", cfg.Ingest.BackpressureThreshold)
	}
	if cfg.Ingest.RequestTimeoutSeconds != 30 {
		t.Errorf("ingest.request_timeout_seconds = %d, want 30", cfg.Ingest.RequestTimeoutSeconds)
	}
}

func TestIngestEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{}`), 0644)

	t.Setenv("GLOVEBOX_INGEST_ENABLED", "false")
	t.Setenv("GLOVEBOX_INGEST_PORT", "7070")
	t.Setenv("GLOVEBOX_INGEST_MAX_BODY_BYTES", "999999")
	t.Setenv("GLOVEBOX_INGEST_BACKPRESSURE_THRESHOLD", "200")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Ingest.Enabled {
		t.Error("ingest.enabled should be false after env override")
	}
	if cfg.Ingest.Port != 7070 {
		t.Errorf("ingest.port = %d, want 7070", cfg.Ingest.Port)
	}
	if cfg.Ingest.MaxBodyBytes != 999999 {
		t.Errorf("ingest.max_body_bytes = %d, want 999999", cfg.Ingest.MaxBodyBytes)
	}
	if cfg.Ingest.BackpressureThreshold != 200 {
		t.Errorf("ingest.backpressure_threshold = %d, want 200", cfg.Ingest.BackpressureThreshold)
	}
}

func TestIngestValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid defaults",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name: "invalid port",
			modify: func(c *Config) {
				c.Ingest.Port = 0
			},
			wantErr: true,
		},
		{
			name: "negative port",
			modify: func(c *Config) {
				c.Ingest.Port = -1
			},
			wantErr: true,
		},
		{
			name: "invalid max_body_bytes",
			modify: func(c *Config) {
				c.Ingest.MaxBodyBytes = 0
			},
			wantErr: true,
		},
		{
			name: "invalid backpressure_threshold",
			modify: func(c *Config) {
				c.Ingest.BackpressureThreshold = -5
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			os.WriteFile(path, []byte(`{}`), 0644)
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("unexpected error loading config: %v", err)
			}
			tt.modify(&cfg)
			err = cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestIngestDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{}`), 0644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Disable ingest and set invalid values
	cfg.Ingest.Enabled = false
	cfg.Ingest.Port = -1
	cfg.Ingest.MaxBodyBytes = 0
	cfg.Ingest.BackpressureThreshold = 0

	if err := cfg.Validate(); err != nil {
		t.Errorf("validation should pass when ingest is disabled, got: %v", err)
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
