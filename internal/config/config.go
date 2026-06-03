package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/leftathome/glovebox/internal/subject"
)

type IngestConfig struct {
	Enabled               bool  `json:"enabled"`
	Port                  int   `json:"port"`
	MaxBodyBytes          int64 `json:"max_body_bytes"`
	MaxMetadataBytes      int64 `json:"max_metadata_bytes"`
	BackpressureThreshold int   `json:"backpressure_threshold"`
	RequestTimeoutSeconds int   `json:"request_timeout_seconds"`

	// Spec 10 auth + spec 13 archive-delivery sub-blocks. Both are
	// opt-in: when Auth.Enabled or Archives.Enabled is false the
	// matching boot path in main.go is skipped entirely.
	Auth     IngestAuthConfig     `json:"auth"`
	Archives IngestArchivesConfig `json:"archives"`
}

// IngestAuthConfig configures the spec 10 bearer-token middleware
// applied to /v1/archives*. Mirrors values.yaml -> ingest.auth.
type IngestAuthConfig struct {
	Enabled               bool                `json:"enabled"`
	Vault                 VaultClientConfig   `json:"vault"`
	ReloadIntervalSeconds int                 `json:"reload_interval_seconds"`
	TrustedProxyCIDRs     []string            `json:"trusted_proxy_cidrs"`
	PerIPRateLimit        RateLimitWindowConf `json:"per_ip_rate_limit"`
	GlobalRateLimit       RateLimitWindowConf `json:"global_rate_limit"`
}

// VaultClientConfig is the in-process Vault client wiring for the
// TokenStore reload path. AuthMethod is always Kubernetes auth for now;
// the field is reserved for future expansion.
type VaultClientConfig struct {
	Addr       string `json:"addr"`
	K8sRole    string `json:"k8s_role"`
	KVMount    string `json:"kv_mount"`
	TokensPath string `json:"tokens_path"`
}

// RateLimitWindowConf parameterizes either of the two rate-limit
// buckets (per-IP / global). WindowSeconds + MaxRejected map directly
// to auth.RateLimitConfig.Window / Max*Rejected.
type RateLimitWindowConf struct {
	WindowSeconds int `json:"window_seconds"`
	MaxRejected   int `json:"max_rejected"`
	LRUCapacity   int `json:"lru_capacity"`
}

// IngestArchivesConfig configures the spec 13 archive-delivery
// endpoint (/v1/archives*). StagingRoot anchors both .tmp-archives/
// and archives/ on the same filesystem (st_dev identity is enforced at
// boot).
type IngestArchivesConfig struct {
	Enabled                    bool   `json:"enabled"`
	StagingRoot                string `json:"staging_root"`
	MaxUploadSize              int64  `json:"max_upload_size"`
	PerSourceMaxConcurrent     int    `json:"per_source_max_concurrent"`
	GlobalMaxConcurrent        int    `json:"global_max_concurrent"`
	PerSourceSoftCapPct        int    `json:"per_source_soft_cap_pct"`
	GlobalHardCapPct           int    `json:"global_hard_cap_pct"`
	GlobalHardCapHysteresisPct int    `json:"global_hard_cap_hysteresis_pct"`
	PatchIdleTimeoutSeconds    int    `json:"patch_idle_timeout_seconds"`
	CleanupIntervalSeconds     int    `json:"cleanup_interval_seconds"`
	CleanupTmpAgeHours         int    `json:"cleanup_tmp_age_hours"`
	CleanupFinalizeAgeHours    int    `json:"cleanup_finalize_age_hours"`
	DoneRetentionDays          int    `json:"done_retention_days"`
}

type Config struct {
	StagingDir          string   `json:"staging_dir"`
	QuarantineDir       string   `json:"quarantine_dir"`
	AuditDir            string   `json:"audit_dir"`
	FailedDir           string   `json:"failed_dir"`
	AgentsDir           string   `json:"agents_dir"`
	SharedDir           string   `json:"shared_dir"`
	AgentAllowlist      []string `json:"agent_allowlist"`
	MetricsPort         int      `json:"metrics_port"`
	WatchMode           string   `json:"watch_mode"`
	PollIntervalSeconds int      `json:"poll_interval_seconds"`
	RulesFile           string   `json:"rules_file"`
	ScanWorkers         int      `json:"scan_workers"`
	ScanTimeoutSeconds  int      `json:"scan_timeout_seconds"`
	ScanChunkSizeBytes  int          `json:"scan_chunk_size_bytes"`
	SubjectsFile        string        `json:"subjects_file"`
	SubjectsEnforce     bool          `json:"subjects_enforce"`
	Ingest              IngestConfig  `json:"ingest"`
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{
		MetricsPort:         9090,
		WatchMode:           "fsnotify",
		PollIntervalSeconds: 5,
		ScanWorkers:         4,
		ScanTimeoutSeconds:  30,
		ScanChunkSizeBytes:  262144,
		Ingest: IngestConfig{
			Enabled:               true,
			Port:                  9091,
			MaxBodyBytes:          67108864,
			MaxMetadataBytes:      262144,
			BackpressureThreshold: 100,
			RequestTimeoutSeconds: 60,
			Auth: IngestAuthConfig{
				Enabled: false,
				Vault: VaultClientConfig{
					Addr:       "http://vault.vault.svc.cluster.local:8200",
					K8sRole:    "glovebox-ingest",
					KVMount:    "secret",
					TokensPath: "glovebox/ingest-tokens",
				},
				ReloadIntervalSeconds: 300,
				PerIPRateLimit: RateLimitWindowConf{
					WindowSeconds: 60,
					MaxRejected:   10,
					LRUCapacity:   1000,
				},
				GlobalRateLimit: RateLimitWindowConf{
					WindowSeconds: 60,
					MaxRejected:   100,
				},
			},
			Archives: IngestArchivesConfig{
				Enabled:                    false,
				StagingRoot:                "/data/archive-storage",
				MaxUploadSize:              32212254720, // 30 GiB
				PerSourceMaxConcurrent:     4,
				GlobalMaxConcurrent:        32,
				PerSourceSoftCapPct:        40,
				GlobalHardCapPct:           95,
				GlobalHardCapHysteresisPct: 85,
				PatchIdleTimeoutSeconds:    300,
				CleanupIntervalSeconds:     3600,
				CleanupTmpAgeHours:         72,
				CleanupFinalizeAgeHours:    1,
				DoneRetentionDays:          7,
			},
		},
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, err
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, err
		}
	}

	applyEnvOverrides(&cfg)
	return cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("GLOVEBOX_STAGING_DIR"); v != "" {
		cfg.StagingDir = v
	}
	if v := os.Getenv("GLOVEBOX_QUARANTINE_DIR"); v != "" {
		cfg.QuarantineDir = v
	}
	if v := os.Getenv("GLOVEBOX_AUDIT_DIR"); v != "" {
		cfg.AuditDir = v
	}
	if v := os.Getenv("GLOVEBOX_FAILED_DIR"); v != "" {
		cfg.FailedDir = v
	}
	if v := os.Getenv("GLOVEBOX_AGENTS_DIR"); v != "" {
		cfg.AgentsDir = v
	}
	if v := os.Getenv("GLOVEBOX_SHARED_DIR"); v != "" {
		cfg.SharedDir = v
	}
	if v := os.Getenv("GLOVEBOX_METRICS_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MetricsPort = n
		}
	}
	if v := os.Getenv("GLOVEBOX_WATCH_MODE"); v != "" {
		cfg.WatchMode = v
	}
	if v := os.Getenv("GLOVEBOX_RULES_FILE"); v != "" {
		cfg.RulesFile = v
	}
	if v := os.Getenv("GLOVEBOX_SCAN_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ScanWorkers = n
		}
	}
	if v := os.Getenv("GLOVEBOX_SCAN_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ScanTimeoutSeconds = n
		}
	}
	if v := os.Getenv("GLOVEBOX_INGEST_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Ingest.Enabled = b
		}
	}
	if v := os.Getenv("GLOVEBOX_INGEST_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Ingest.Port = n
		}
	}
	if v := os.Getenv("GLOVEBOX_INGEST_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Ingest.MaxBodyBytes = n
		}
	}
	if v := os.Getenv("GLOVEBOX_INGEST_BACKPRESSURE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Ingest.BackpressureThreshold = n
		}
	}
	if v := os.Getenv("GLOVEBOX_INGEST_MAX_METADATA_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Ingest.MaxMetadataBytes = n
		}
	}
	if v := os.Getenv("GLOVEBOX_INGEST_REQUEST_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Ingest.RequestTimeoutSeconds = n
		}
	}
	if v := os.Getenv("GLOVEBOX_SUBJECTS_FILE"); v != "" {
		cfg.SubjectsFile = v
	}
	if v := os.Getenv("GLOVEBOX_SUBJECTS_ENFORCE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.SubjectsEnforce = b
		}
	}
}

func (c *Config) Validate() error {
	if c.SubjectsFile != "" {
		if _, err := subject.Load(c.SubjectsFile); err != nil {
			return fmt.Errorf("subjects registry: %w", err)
		}
	}
	if !c.Ingest.Enabled {
		return nil
	}
	if c.Ingest.Port <= 0 {
		return fmt.Errorf("ingest.port must be > 0 when ingest is enabled, got %d", c.Ingest.Port)
	}
	if c.Ingest.MaxBodyBytes <= 0 {
		return fmt.Errorf("ingest.max_body_bytes must be > 0 when ingest is enabled, got %d", c.Ingest.MaxBodyBytes)
	}
	if c.Ingest.BackpressureThreshold <= 0 {
		return fmt.Errorf("ingest.backpressure_threshold must be > 0 when ingest is enabled, got %d", c.Ingest.BackpressureThreshold)
	}
	if c.Ingest.Auth.Enabled {
		if err := c.Ingest.Auth.validate(); err != nil {
			return err
		}
	}
	if c.Ingest.Archives.Enabled {
		// Archive listener depends on bearer-token auth; refusing here is
		// friendlier than the listener mounting the 503 fallback at boot.
		if !c.Ingest.Auth.Enabled {
			return fmt.Errorf("ingest.archives.enabled requires ingest.auth.enabled (spec 13 §5.2 / spec 10)")
		}
		if err := c.Ingest.Archives.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (a *IngestAuthConfig) validate() error {
	if a.Vault.Addr == "" {
		return fmt.Errorf("ingest.auth.vault.addr required when ingest.auth.enabled")
	}
	if a.Vault.K8sRole == "" {
		return fmt.Errorf("ingest.auth.vault.k8s_role required when ingest.auth.enabled")
	}
	if a.Vault.TokensPath == "" {
		return fmt.Errorf("ingest.auth.vault.tokens_path required when ingest.auth.enabled")
	}
	if a.ReloadIntervalSeconds <= 0 {
		return fmt.Errorf("ingest.auth.reload_interval_seconds must be > 0, got %d", a.ReloadIntervalSeconds)
	}
	if a.PerIPRateLimit.WindowSeconds <= 0 {
		return fmt.Errorf("ingest.auth.per_ip_rate_limit.window_seconds must be > 0, got %d", a.PerIPRateLimit.WindowSeconds)
	}
	if a.PerIPRateLimit.MaxRejected <= 0 {
		return fmt.Errorf("ingest.auth.per_ip_rate_limit.max_rejected must be > 0, got %d", a.PerIPRateLimit.MaxRejected)
	}
	if a.GlobalRateLimit.WindowSeconds <= 0 {
		return fmt.Errorf("ingest.auth.global_rate_limit.window_seconds must be > 0, got %d", a.GlobalRateLimit.WindowSeconds)
	}
	if a.GlobalRateLimit.MaxRejected <= 0 {
		return fmt.Errorf("ingest.auth.global_rate_limit.max_rejected must be > 0, got %d", a.GlobalRateLimit.MaxRejected)
	}
	return nil
}

func (ar *IngestArchivesConfig) validate() error {
	if ar.StagingRoot == "" {
		return fmt.Errorf("ingest.archives.staging_root required when ingest.archives.enabled")
	}
	if ar.MaxUploadSize <= 0 {
		return fmt.Errorf("ingest.archives.max_upload_size must be > 0, got %d", ar.MaxUploadSize)
	}
	if ar.PerSourceMaxConcurrent <= 0 {
		return fmt.Errorf("ingest.archives.per_source_max_concurrent must be > 0, got %d", ar.PerSourceMaxConcurrent)
	}
	if ar.GlobalMaxConcurrent <= 0 {
		return fmt.Errorf("ingest.archives.global_max_concurrent must be > 0, got %d", ar.GlobalMaxConcurrent)
	}
	if ar.GlobalHardCapPct <= 0 || ar.GlobalHardCapPct > 100 {
		return fmt.Errorf("ingest.archives.global_hard_cap_pct must be in (0,100], got %d", ar.GlobalHardCapPct)
	}
	if ar.GlobalHardCapHysteresisPct <= 0 || ar.GlobalHardCapHysteresisPct > 100 {
		return fmt.Errorf("ingest.archives.global_hard_cap_hysteresis_pct must be in (0,100], got %d", ar.GlobalHardCapHysteresisPct)
	}
	if ar.GlobalHardCapHysteresisPct >= ar.GlobalHardCapPct {
		return fmt.Errorf("ingest.archives.global_hard_cap_hysteresis_pct (%d) must be < global_hard_cap_pct (%d)",
			ar.GlobalHardCapHysteresisPct, ar.GlobalHardCapPct)
	}
	if ar.PerSourceSoftCapPct <= 0 || ar.PerSourceSoftCapPct > 100 {
		return fmt.Errorf("ingest.archives.per_source_soft_cap_pct must be in (0,100], got %d", ar.PerSourceSoftCapPct)
	}
	if ar.PatchIdleTimeoutSeconds <= 0 {
		return fmt.Errorf("ingest.archives.patch_idle_timeout_seconds must be > 0, got %d", ar.PatchIdleTimeoutSeconds)
	}
	if ar.CleanupIntervalSeconds <= 0 {
		return fmt.Errorf("ingest.archives.cleanup_interval_seconds must be > 0, got %d", ar.CleanupIntervalSeconds)
	}
	return nil
}

