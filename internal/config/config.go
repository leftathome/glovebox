package config

import (
	"encoding/json"
	"os"
	"strconv"
)

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
	ScanChunkSizeBytes  int      `json:"scan_chunk_size_bytes"`
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{
		MetricsPort:         9090,
		WatchMode:           "fsnotify",
		PollIntervalSeconds: 5,
		ScanWorkers:         4,
		ScanTimeoutSeconds:  30,
		ScanChunkSizeBytes:  262144,
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
}

