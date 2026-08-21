package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/leftathome/glovebox/internal/source"
	"github.com/leftathome/glovebox/internal/subject"
)

type IngestConfig struct {
	Enabled bool `json:"enabled"`
	// Port carries the plaintext connector intake, POST /v1/ingest.
	Port int `json:"port"`
	// BearerPort carries the bearer-authenticated surface --
	// /v1/archives* (spec 13) and /v1/sanitize (glovebox-t6fz).
	//
	// 0 (the default) means "share Port", which is what every install
	// before this field did: one plaintext listener carrying all three
	// route families. Setting it to a distinct port opens a second
	// listener for the bearer endpoints and leaves Port carrying only
	// /v1/ingest, which is what lets the recognizer namespace be granted
	// the archive endpoint WITHOUT also being handed unauthenticated
	// /v1/ingest (security review P0-7).
	//
	// Either way the bearer listener's lifecycle is independent of
	// TLS.Mode: those endpoints authenticate themselves and must not go
	// dark because the connector transport moved to mTLS.
	BearerPort            int   `json:"bearer_port"`
	MaxBodyBytes          int64 `json:"max_body_bytes"`
	MaxMetadataBytes      int64 `json:"max_metadata_bytes"`
	BackpressureThreshold int   `json:"backpressure_threshold"`
	RequestTimeoutSeconds int   `json:"request_timeout_seconds"`

	// Spec 10 auth + spec 13 archive-delivery sub-blocks. Both are
	// opt-in: when Auth.Enabled or Archives.Enabled is false the
	// matching boot path in main.go is skipped entirely.
	Auth     IngestAuthConfig     `json:"auth"`
	Archives IngestArchivesConfig `json:"archives"`

	// TLS configures mutual TLS on /v1/ingest. Opt-in and staged: see
	// IngestTLSConfig.
	TLS IngestTLSConfig `json:"tls"`
}

// EffectiveBearerPort resolves the port the bearer-authenticated
// endpoints (/v1/archives*, /v1/sanitize) listen on. Unset means they
// share the connector ingest port, which is the pre-existing layout.
func (c IngestConfig) EffectiveBearerPort() int {
	if c.BearerPort == 0 {
		return c.Port
	}
	return c.BearerPort
}

// BearerSplit reports whether the bearer-authenticated endpoints get a
// listener of their own rather than sharing the /v1/ingest port.
func (c IngestConfig) BearerSplit() bool {
	return c.BearerPort != 0 && c.BearerPort != c.Port
}

// BearerRoutesEnabled reports whether anything is mounted on the bearer
// surface at all. Both /v1/archives* and /v1/sanitize are gated on
// ingest.auth.enabled (archives additionally on ingest.archives.enabled,
// and Validate already refuses archives without auth), so auth is the
// single switch that decides whether the listener has any reason to
// exist.
func (c IngestConfig) BearerRoutesEnabled() bool {
	return c.Auth.Enabled
}

// IngestTLSConfig configures mutual TLS for the connector ingest path.
//
// Spec 08 section 3.10 left /v1/ingest unauthenticated, gated only by a
// NetworkPolicy podSelector. A label is not an identity -- any workload
// that can set it reaches the endpoint -- and the handler took
// metadata.source, identity.provider and destination_agent on faith, so a
// compromised connector (they all hold external credentials and parse
// hostile content) could stamp another connector's provenance onto an item
// and route it anywhere in the allowlist. Traffic was also plaintext.
//
// Mode drives a migration rather than a flag day:
//
//	disabled   -- plaintext only (the pre-existing behaviour, still default)
//	permissive -- plaintext AND mTLS listeners both serve; the
//	              transport label on glovebox_items_received_total shows
//	              how much traffic has moved
//	required   -- mTLS only; the plaintext listener is not opened
//
// Under permissive and required, a verified peer identity is bound to the
// item: metadata.source must match the connector the certificate names.
type IngestTLSConfig struct {
	// Mode is "disabled" (default), "permissive" or "required".
	Mode string `json:"mode"`
	// Port carries the mTLS listener. Defaults to Ingest.Port+1 so
	// permissive mode can serve both without a config change.
	Port int `json:"port"`
	// CertFile / KeyFile are the server certificate and key. Both are
	// re-read when they change on disk, so cert-manager rotation needs no
	// pod restart.
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// ClientCAFile is the CA bundle client certificates are verified
	// against. Use a CA dedicated to the ingest plane, not the cluster
	// edge CA: a certificate issued for any other purpose must not be
	// able to ingest.
	ClientCAFile string `json:"client_ca_file"`
	// TrustDomain is the SPIFFE trust domain expected in client
	// certificate URI SANs (spiffe://<trust-domain>/connector/<name>).
	// Defaults to "glovebox".
	TrustDomain string `json:"trust_domain"`
	// EnforceSourceMatch requires metadata.source to equal the peer
	// identity's connector name. Defaults to true whenever mTLS is on;
	// set false only while migrating a connector whose source label does
	// not yet match its certificate.
	EnforceSourceMatch *bool `json:"enforce_source_match"`
}

// TLS mode constants.
const (
	TLSModeDisabled   = "disabled"
	TLSModePermissive = "permissive"
	TLSModeRequired   = "required"
)

// Active reports whether an mTLS listener should be opened.
func (t IngestTLSConfig) Active() bool {
	return t.Mode == TLSModePermissive || t.Mode == TLSModeRequired
}

// PlaintextActive reports whether the plaintext listener should be opened.
func (t IngestTLSConfig) PlaintextActive() bool {
	return t.Mode != TLSModeRequired
}

// SourceMatchEnforced resolves the tri-state EnforceSourceMatch pointer:
// enforcement defaults ON whenever mTLS is active, because binding the
// claimed source to the verified peer is the point of the exercise --
// encryption alone would leave the spoofing gap open.
func (t IngestTLSConfig) SourceMatchEnforced() bool {
	if !t.Active() {
		return false
	}
	if t.EnforceSourceMatch == nil {
		return true
	}
	return *t.EnforceSourceMatch
}

// EffectiveTrustDomain returns the configured trust domain or the default.
func (t IngestTLSConfig) EffectiveTrustDomain() string {
	if t.TrustDomain == "" {
		return "glovebox"
	}
	return t.TrustDomain
}

// IngestAuthConfig configures the spec 10 bearer-token middleware
// applied to /v1/archives*. Mirrors values.yaml -> ingest.auth.
type IngestAuthConfig struct {
	Enabled bool `json:"enabled"`
	// Source selects where ingest bearer tokens come from:
	//   "vault" (default) -- production: Vault KV v2 via K8s auth.
	//   "env"             -- DEV ONLY: a single token from
	//                        GLOVEBOX_INGEST_SOURCE_ID/GLOVEBOX_INGEST_TOKEN.
	//   "file"            -- DEV ONLY: a JSON source-id->token map at
	//                        File.Path.
	// The non-vault sources exist so the auth + archive-delivery path is
	// testable on a single node/container without a cluster (glovebox-4ypk).
	Source                string              `json:"source"`
	Vault                 VaultClientConfig   `json:"vault"`
	File                  TokenFileConfig     `json:"file"`
	ReloadIntervalSeconds int                 `json:"reload_interval_seconds"`
	TrustedProxyCIDRs     []string            `json:"trusted_proxy_cidrs"`
	PerIPRateLimit        RateLimitWindowConf `json:"per_ip_rate_limit"`
	GlobalRateLimit       RateLimitWindowConf `json:"global_rate_limit"`
}

// TokenFileConfig configures the DEV-ONLY file-backed token source.
type TokenFileConfig struct {
	Path string `json:"path"`
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
	// RulesSHA256 optionally pins the expected digest of RulesFile. When
	// set, the daemon refuses to start if the file on disk does not match.
	//
	// The rules file arrives as a mounted ConfigMap, and every boundary in
	// the service is defined there: whoever can edit it can weaken all of
	// them at once. Pinning the digest (from Git, via GitOps) turns an
	// unreviewed edit into a failed start instead of a silently permissive
	// scanner. Empty means unpinned, which stays the default.
	RulesSHA256        string `json:"rules_sha256"`
	ScanWorkers        int    `json:"scan_workers"`
	ScanTimeoutSeconds int    `json:"scan_timeout_seconds"`
	// DeliveryTimeoutSeconds bounds a single result delivery (file move +
	// audit write). It prevents a wedged file op on a networked staging mount
	// from stalling the lone result-consumer goroutine and deadlocking the
	// whole scan pipeline (glovebox-lnzp). 0 disables the bound.
	DeliveryTimeoutSeconds int          `json:"delivery_timeout_seconds"`
	ScanChunkSizeBytes     int          `json:"scan_chunk_size_bytes"`
	SubjectsFile           string       `json:"subjects_file"`
	SourcesFile            string       `json:"sources_file"`
	Ingest                 IngestConfig `json:"ingest"`
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{
		MetricsPort:            9090,
		WatchMode:              "fsnotify",
		PollIntervalSeconds:    5,
		ScanWorkers:            4,
		ScanTimeoutSeconds:     30,
		DeliveryTimeoutSeconds: 30,
		ScanChunkSizeBytes:     262144,
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
	if v := os.Getenv("GLOVEBOX_DELIVERY_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.DeliveryTimeoutSeconds = n
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
	if v := os.Getenv("GLOVEBOX_SOURCES_FILE"); v != "" {
		cfg.SourcesFile = v
	}
}

func (c *Config) Validate() error {
	if c.SubjectsFile != "" {
		if _, err := subject.Load(c.SubjectsFile); err != nil {
			return fmt.Errorf("subjects registry: %w", err)
		}
	}
	if c.SourcesFile != "" {
		if _, err := source.Load(c.SourcesFile); err != nil {
			return fmt.Errorf("sources registry: %w", err)
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
	if err := c.Ingest.TLS.validate(c.Ingest.Port); err != nil {
		return err
	}
	if err := c.Ingest.validateBearerPort(); err != nil {
		return err
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

// validateBearerPort checks the bearer listener cannot collide with
// another listener in the same process. It runs AFTER TLS.validate, which
// is where TLS.Port picks up its Port+1 default.
func (c IngestConfig) validateBearerPort() error {
	if c.BearerPort < 0 {
		return fmt.Errorf("ingest.bearer_port must be > 0 when set, got %d", c.BearerPort)
	}
	if !c.BearerRoutesEnabled() {
		// Nothing is mounted on the bearer surface, so no listener is
		// opened and no port can collide.
		return nil
	}
	if c.TLS.Active() && c.EffectiveBearerPort() == c.TLS.Port {
		return fmt.Errorf("ingest.bearer_port %d collides with ingest.tls.port: /v1/archives and /v1/sanitize need a plaintext listener of their own in every tls mode",
			c.EffectiveBearerPort())
	}
	return nil
}

func (t *IngestTLSConfig) validate(ingestPort int) error {
	switch t.Mode {
	case "", TLSModeDisabled:
		return nil
	case TLSModePermissive, TLSModeRequired:
	default:
		return fmt.Errorf("ingest.tls.mode must be one of %q, %q, %q; got %q",
			TLSModeDisabled, TLSModePermissive, TLSModeRequired, t.Mode)
	}
	if t.CertFile == "" || t.KeyFile == "" {
		return fmt.Errorf("ingest.tls.cert_file and key_file are required when ingest.tls.mode is %q", t.Mode)
	}
	if t.ClientCAFile == "" {
		// Without a client CA there is no client verification, which
		// would leave the endpoint authenticated by nothing while looking
		// as though it were secured.
		return fmt.Errorf("ingest.tls.client_ca_file is required when ingest.tls.mode is %q", t.Mode)
	}
	if t.Port == 0 {
		t.Port = ingestPort + 1
	}
	if t.Port == ingestPort && t.Mode == TLSModePermissive {
		return fmt.Errorf("ingest.tls.port must differ from ingest.port in permissive mode (both listeners are opened)")
	}
	if t.Port <= 0 {
		return fmt.Errorf("ingest.tls.port must be > 0, got %d", t.Port)
	}
	return nil
}

func (a *IngestAuthConfig) validate() error {
	// Source selects which fields are required. Vault is the production
	// default; env/file are DEV-ONLY single-node sources (glovebox-4ypk).
	switch a.Source {
	case "", "vault":
		if a.Vault.Addr == "" {
			return fmt.Errorf("ingest.auth.vault.addr required when ingest.auth.enabled")
		}
		if a.Vault.K8sRole == "" {
			return fmt.Errorf("ingest.auth.vault.k8s_role required when ingest.auth.enabled")
		}
		if a.Vault.TokensPath == "" {
			return fmt.Errorf("ingest.auth.vault.tokens_path required when ingest.auth.enabled")
		}
	case "env":
		// No additional required fields; tokens come from
		// GLOVEBOX_INGEST_SOURCE_ID / GLOVEBOX_INGEST_TOKEN at runtime.
	case "file":
		if a.File.Path == "" {
			return fmt.Errorf("ingest.auth.file.path required when ingest.auth.source=file")
		}
	default:
		return fmt.Errorf("unknown ingest.auth.source %q (want vault|env|file)", a.Source)
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
