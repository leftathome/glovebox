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

func TestLoadConfig_IngestAuthDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{}`), 0644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Ingest.Auth.Enabled {
		t.Error("ingest.auth.enabled default should be false")
	}
	if cfg.Ingest.Auth.Vault.Addr == "" {
		t.Error("ingest.auth.vault.addr default should be non-empty")
	}
	if cfg.Ingest.Auth.Vault.K8sRole != "glovebox-ingest" {
		t.Errorf("ingest.auth.vault.k8s_role default = %q, want glovebox-ingest", cfg.Ingest.Auth.Vault.K8sRole)
	}
	if cfg.Ingest.Auth.Vault.KVMount != "secret" {
		t.Errorf("ingest.auth.vault.kv_mount default = %q, want secret", cfg.Ingest.Auth.Vault.KVMount)
	}
	if cfg.Ingest.Auth.Vault.TokensPath != "glovebox/ingest-tokens" {
		t.Errorf("ingest.auth.vault.tokens_path default = %q, want glovebox/ingest-tokens", cfg.Ingest.Auth.Vault.TokensPath)
	}
	if cfg.Ingest.Auth.ReloadIntervalSeconds != 300 {
		t.Errorf("ingest.auth.reload_interval_seconds default = %d, want 300", cfg.Ingest.Auth.ReloadIntervalSeconds)
	}
	if cfg.Ingest.Auth.PerIPRateLimit.WindowSeconds != 60 {
		t.Errorf("ingest.auth.per_ip_rate_limit.window_seconds default = %d, want 60", cfg.Ingest.Auth.PerIPRateLimit.WindowSeconds)
	}
	if cfg.Ingest.Auth.PerIPRateLimit.MaxRejected != 10 {
		t.Errorf("ingest.auth.per_ip_rate_limit.max_rejected default = %d, want 10", cfg.Ingest.Auth.PerIPRateLimit.MaxRejected)
	}
	if cfg.Ingest.Auth.GlobalRateLimit.MaxRejected != 100 {
		t.Errorf("ingest.auth.global_rate_limit.max_rejected default = %d, want 100", cfg.Ingest.Auth.GlobalRateLimit.MaxRejected)
	}
}

func TestLoadConfig_IngestArchivesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{}`), 0644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Ingest.Archives.Enabled {
		t.Error("ingest.archives.enabled default should be false")
	}
	if cfg.Ingest.Archives.StagingRoot != "/data/archive-storage" {
		t.Errorf("ingest.archives.staging_root default = %q, want /data/archive-storage", cfg.Ingest.Archives.StagingRoot)
	}
	if cfg.Ingest.Archives.MaxUploadSize != 32212254720 {
		t.Errorf("ingest.archives.max_upload_size default = %d, want 32212254720", cfg.Ingest.Archives.MaxUploadSize)
	}
	if cfg.Ingest.Archives.GlobalHardCapPct != 95 {
		t.Errorf("ingest.archives.global_hard_cap_pct default = %d, want 95", cfg.Ingest.Archives.GlobalHardCapPct)
	}
	if cfg.Ingest.Archives.GlobalHardCapHysteresisPct != 85 {
		t.Errorf("ingest.archives.global_hard_cap_hysteresis_pct default = %d, want 85", cfg.Ingest.Archives.GlobalHardCapHysteresisPct)
	}
	if cfg.Ingest.Archives.PerSourceSoftCapPct != 40 {
		t.Errorf("ingest.archives.per_source_soft_cap_pct default = %d, want 40", cfg.Ingest.Archives.PerSourceSoftCapPct)
	}
}

func TestLoadConfig_IngestAuthArchivesJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"ingest": {
			"enabled": true,
			"port": 9091,
			"max_body_bytes": 67108864,
			"max_metadata_bytes": 262144,
			"backpressure_threshold": 100,
			"request_timeout_seconds": 60,
			"auth": {
				"enabled": true,
				"vault": {
					"addr": "http://vault.example:8200",
					"k8s_role": "test-role",
					"kv_mount": "secret",
					"tokens_path": "test/tokens"
				},
				"reload_interval_seconds": 60,
				"trusted_proxy_cidrs": ["10.0.0.0/8", "172.16.0.0/12"],
				"per_ip_rate_limit": {
					"window_seconds": 30,
					"max_rejected": 5,
					"lru_capacity": 500
				},
				"global_rate_limit": {
					"window_seconds": 30,
					"max_rejected": 50
				}
			},
			"archives": {
				"enabled": true,
				"staging_root": "/var/lib/glovebox",
				"max_upload_size": 1073741824,
				"per_source_max_concurrent": 2,
				"global_max_concurrent": 8,
				"per_source_soft_cap_pct": 25,
				"global_hard_cap_pct": 90,
				"global_hard_cap_hysteresis_pct": 80,
				"patch_idle_timeout_seconds": 120,
				"cleanup_interval_seconds": 1800,
				"cleanup_tmp_age_hours": 24,
				"cleanup_finalize_age_hours": 2,
				"done_retention_days": 14
			}
		}
	}`), 0644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Ingest.Auth.Enabled {
		t.Error("ingest.auth.enabled = false, want true")
	}
	if cfg.Ingest.Auth.Vault.Addr != "http://vault.example:8200" {
		t.Errorf("vault.addr = %q", cfg.Ingest.Auth.Vault.Addr)
	}
	if len(cfg.Ingest.Auth.TrustedProxyCIDRs) != 2 {
		t.Errorf("trusted_proxy_cidrs len = %d, want 2", len(cfg.Ingest.Auth.TrustedProxyCIDRs))
	}
	if cfg.Ingest.Auth.PerIPRateLimit.WindowSeconds != 30 {
		t.Errorf("per_ip window_seconds = %d, want 30", cfg.Ingest.Auth.PerIPRateLimit.WindowSeconds)
	}
	if !cfg.Ingest.Archives.Enabled {
		t.Error("ingest.archives.enabled = false, want true")
	}
	if cfg.Ingest.Archives.StagingRoot != "/var/lib/glovebox" {
		t.Errorf("staging_root = %q", cfg.Ingest.Archives.StagingRoot)
	}
	if cfg.Ingest.Archives.MaxUploadSize != 1073741824 {
		t.Errorf("max_upload_size = %d", cfg.Ingest.Archives.MaxUploadSize)
	}
	if cfg.Ingest.Archives.DoneRetentionDays != 14 {
		t.Errorf("done_retention_days = %d", cfg.Ingest.Archives.DoneRetentionDays)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

func TestIngestAuthValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr string
	}{
		{
			name: "auth enabled but vault addr missing",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = true
				c.Ingest.Auth.Vault.Addr = ""
			},
			wantErr: "vault.addr required",
		},
		{
			name: "auth enabled but k8s role missing",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = true
				c.Ingest.Auth.Vault.K8sRole = ""
			},
			wantErr: "vault.k8s_role required",
		},
		{
			name: "auth enabled but reload interval zero",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = true
				c.Ingest.Auth.ReloadIntervalSeconds = 0
			},
			wantErr: "reload_interval_seconds must be > 0",
		},
		{
			name: "auth enabled but per-IP window zero",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = true
				c.Ingest.Auth.PerIPRateLimit.WindowSeconds = 0
			},
			wantErr: "per_ip_rate_limit.window_seconds must be > 0",
		},
		{
			name: "auth disabled — defaults pass even if invalid",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = false
				c.Ingest.Auth.Vault.Addr = ""
			},
			wantErr: "",
		},
		{
			name: "source=env does not require vault fields",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = true
				c.Ingest.Auth.Source = "env"
				c.Ingest.Auth.Vault = VaultClientConfig{}
			},
			wantErr: "",
		},
		{
			name: "source=file requires file.path",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = true
				c.Ingest.Auth.Source = "file"
				c.Ingest.Auth.Vault = VaultClientConfig{}
			},
			wantErr: "ingest.auth.file.path required",
		},
		{
			name: "source=file with path does not require vault fields",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = true
				c.Ingest.Auth.Source = "file"
				c.Ingest.Auth.Vault = VaultClientConfig{}
				c.Ingest.Auth.File.Path = "/etc/glovebox/tokens.json"
			},
			wantErr: "",
		},
		{
			name: "unknown source rejected",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = true
				c.Ingest.Auth.Source = "bogus"
			},
			wantErr: "unknown ingest.auth.source",
		},
		{
			name: "source=vault still requires vault addr",
			modify: func(c *Config) {
				c.Ingest.Auth.Enabled = true
				c.Ingest.Auth.Source = "vault"
				c.Ingest.Auth.Vault.Addr = ""
			},
			wantErr: "vault.addr required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			os.WriteFile(path, []byte(`{}`), 0644)
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			tt.modify(&cfg)
			err = cfg.Validate()
			if tt.wantErr == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestIngestArchivesValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr string
	}{
		{
			name: "archives enabled but auth disabled",
			modify: func(c *Config) {
				c.Ingest.Archives.Enabled = true
				c.Ingest.Auth.Enabled = false
			},
			wantErr: "ingest.archives.enabled requires ingest.auth.enabled",
		},
		{
			name: "archives staging_root missing",
			modify: func(c *Config) {
				enableAuthAndArchives(c)
				c.Ingest.Archives.StagingRoot = ""
			},
			wantErr: "staging_root required",
		},
		{
			name: "hysteresis equal to hard cap",
			modify: func(c *Config) {
				enableAuthAndArchives(c)
				c.Ingest.Archives.GlobalHardCapPct = 90
				c.Ingest.Archives.GlobalHardCapHysteresisPct = 90
			},
			wantErr: "hysteresis_pct (90) must be < global_hard_cap_pct (90)",
		},
		{
			name: "hysteresis greater than hard cap",
			modify: func(c *Config) {
				enableAuthAndArchives(c)
				c.Ingest.Archives.GlobalHardCapPct = 80
				c.Ingest.Archives.GlobalHardCapHysteresisPct = 90
			},
			wantErr: "hysteresis_pct (90) must be < global_hard_cap_pct (80)",
		},
		{
			name: "hard cap over 100",
			modify: func(c *Config) {
				enableAuthAndArchives(c)
				c.Ingest.Archives.GlobalHardCapPct = 110
			},
			wantErr: "global_hard_cap_pct must be in (0,100]",
		},
		{
			name: "soft cap zero",
			modify: func(c *Config) {
				enableAuthAndArchives(c)
				c.Ingest.Archives.PerSourceSoftCapPct = 0
			},
			wantErr: "per_source_soft_cap_pct must be in (0,100]",
		},
		{
			name: "max_upload_size zero",
			modify: func(c *Config) {
				enableAuthAndArchives(c)
				c.Ingest.Archives.MaxUploadSize = 0
			},
			wantErr: "max_upload_size must be > 0",
		},
		{
			name: "valid combo",
			modify: func(c *Config) {
				enableAuthAndArchives(c)
			},
			wantErr: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			os.WriteFile(path, []byte(`{}`), 0644)
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			tt.modify(&cfg)
			err = cfg.Validate()
			if tt.wantErr == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func enableAuthAndArchives(c *Config) {
	c.Ingest.Auth.Enabled = true
	c.Ingest.Archives.Enabled = true
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 ||
		(len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestLoadConfig_HelmRenderedFixture pins the Go config schema against
// a real chart-rendered config.json (auth + archives both enabled).
// Regenerate with:
//
//	helm template charts/glovebox/ \
//	  --set ingest.auth.enabled=true \
//	  --set ingest.archives.enabled=true \
//	  | awk '/config.json: \|/,/rules.json: \|/' | sed -n '2,/^  rules\\.json/{ /^  rules\\.json/d; s/^    //; p; }' \
//	  > internal/config/testdata/helm-rendered-ingest-enabled.json
//
// A failure here means the chart's configmap.yaml emits keys the Go
// IngestAuth/Archives structs don't recognize -- or vice versa. Fix
// one side before re-running.
func TestLoadConfig_HelmRenderedFixture(t *testing.T) {
	cfg, err := LoadConfig("testdata/helm-rendered-ingest-enabled.json")
	if err != nil {
		t.Fatalf("LoadConfig on helm-rendered fixture: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on helm-rendered fixture: %v", err)
	}
	if !cfg.Ingest.Auth.Enabled {
		t.Error("ingest.auth.enabled = false; chart should emit true when --set ingest.auth.enabled=true")
	}
	if cfg.Ingest.Auth.Vault.Addr == "" {
		t.Error("ingest.auth.vault.addr is empty; chart -> Go schema mismatch on vault.addr key")
	}
	if cfg.Ingest.Auth.Vault.K8sRole == "" {
		t.Error("ingest.auth.vault.k8s_role is empty; chart -> Go schema mismatch on vault.k8s_role key")
	}
	if cfg.Ingest.Auth.ReloadIntervalSeconds == 0 {
		t.Error("ingest.auth.reload_interval_seconds is 0; chart -> Go schema mismatch")
	}
	if cfg.Ingest.Auth.PerIPRateLimit.WindowSeconds == 0 {
		t.Error("per_ip_rate_limit.window_seconds is 0; chart -> Go schema mismatch (chart uses '60s' string; configmap.yaml must strip to int)")
	}
	if !cfg.Ingest.Archives.Enabled {
		t.Error("ingest.archives.enabled = false; chart should emit true when --set ingest.archives.enabled=true")
	}
	if cfg.Ingest.Archives.StagingRoot == "" {
		t.Error("ingest.archives.staging_root is empty; chart -> Go schema mismatch")
	}
	if cfg.Ingest.Archives.MaxUploadSize == 0 {
		t.Error("ingest.archives.max_upload_size is 0; chart -> Go schema mismatch")
	}
	if cfg.Ingest.Archives.GlobalHardCapPct == 0 {
		t.Error("ingest.archives.global_hard_cap_pct is 0; chart -> Go schema mismatch")
	}
	if cfg.Ingest.Archives.DoneRetentionDays == 0 {
		t.Error("ingest.archives.done_retention_days is 0; chart -> Go schema mismatch (chart projects from retention.doneArchiveDays)")
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

// writeSubjectsFile writes raw JSON to a temp file and returns its path.
func writeSubjectsFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "subjects.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeSubjectsFile: %v", err)
	}
	return path
}

// validSubjectsJSON is a minimal well-formed registry.
const validSubjectsJSON = `{
	"enforce": true,
	"subjects": [
		{
			"entity_id": "alice",
			"display": "Alice",
			"principals": ["alice@example.com"],
			"default_audience": ["subject"]
		}
	]
}`

// TestSubjectsFile_ValidRegistry: subjects_file pointing at a valid registry
// -> LoadConfig + Validate succeed.
func TestSubjectsFile_ValidRegistry(t *testing.T) {
	subjPath := writeSubjectsFile(t, validSubjectsJSON)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := `{"subjects_file":"` + subjPath + `"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SubjectsFile != subjPath {
		t.Errorf("SubjectsFile = %q, want %q", cfg.SubjectsFile, subjPath)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: unexpected error: %v", err)
	}
}

// TestSubjectsFile_MalformedDuplicatePrincipal: a registry where two entries
// share the same principal must make Validate return an error.
func TestSubjectsFile_MalformedDuplicatePrincipal(t *testing.T) {
	badJSON := `{
		"enforce": true,
		"subjects": [
			{
				"entity_id": "alice",
				"display": "Alice",
				"principals": ["shared@example.com"],
				"default_audience": ["subject"]
			},
			{
				"entity_id": "bob",
				"display": "Bob",
				"principals": ["shared@example.com"],
				"default_audience": ["subject"]
			}
		]
	}`
	subjPath := writeSubjectsFile(t, badJSON)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := `{"subjects_file":"` + subjPath + `"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate: expected error for duplicate principal, got nil")
	}
	if !contains(err.Error(), "subjects registry:") {
		t.Errorf("Validate error %q does not contain expected prefix", err.Error())
	}
}

// TestSubjectsFile_MalformedBadAudience: a registry with an illegal
// default_audience combo must make Validate return an error.
func TestSubjectsFile_MalformedBadAudience(t *testing.T) {
	badJSON := `{
		"enforce": true,
		"subjects": [
			{
				"entity_id": "alice",
				"display": "Alice",
				"principals": ["alice@example.com"],
				"default_audience": ["public", "subject"]
			}
		]
	}`
	subjPath := writeSubjectsFile(t, badJSON)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := `{"subjects_file":"` + subjPath + `"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate: expected error for bad audience combo, got nil")
	}
	if !contains(err.Error(), "subjects registry:") {
		t.Errorf("Validate error %q does not contain expected prefix", err.Error())
	}
}

// TestSubjectsFile_Absent: no subjects_file -> Validate succeeds.
func TestSubjectsFile_Absent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SubjectsFile != "" {
		t.Errorf("SubjectsFile = %q, want empty", cfg.SubjectsFile)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: unexpected error: %v", err)
	}
}

// TestSubjectsEnvOverrides verifies the GLOVEBOX_SUBJECTS_FILE env var is
// applied.
func TestSubjectsEnvOverrides(t *testing.T) {
	subjPath := writeSubjectsFile(t, validSubjectsJSON)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("GLOVEBOX_SUBJECTS_FILE", subjPath)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SubjectsFile != subjPath {
		t.Errorf("SubjectsFile = %q, want %q", cfg.SubjectsFile, subjPath)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: unexpected error: %v", err)
	}
}
