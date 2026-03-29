# Glovebox Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the glovebox content scanning service -- a deterministic, parallel content scanner that filters prompt injections before content reaches OpenClaw agent workspaces.

**Architecture:** Filesystem-watcher-driven pipeline with parallel scan workers. Connectors write items to a staging directory; the glovebox validates, pre-processes, scans (heuristic pattern matching + custom detectors), and routes to PASS/QUARANTINE/REJECT. Streaming scan for bounded memory. Fail-closed on audit failure. OCI image deployed via Helm to Kubernetes.

**Tech Stack:** Go 1.24+, fsnotify, lingua-go, golang.org/x/text (NFKC normalization + confusables), golang.org/x/net/html, OpenTelemetry Go SDK + Prometheus exporter, Docker (distroless), Helm 3

**Review fixes applied:** StagingItem stores ContentPath not []byte (streaming-compatible). Confusables table added to pre-processing (NFKC alone doesn't handle cross-script homoglyphs). Sanitizer handles runes above U+FFFF. Task 25 split into worker pool and router. Failed-item rescan task added. Regex validated at rule load time. HTML dual-scan noted in streaming task.

**Spec:** `docs/specs/04-glovebox-design.md` (v1.1)

**Beads tracker:** Use `bd show <id>` for full context on any task. Use `bd update <id> --claim` before starting and `bd close <id>` when done.

---

## File Structure

```
glovebox/
  go.mod
  go.sum
  main.go                              # CLI entry point, --config flag, starts glovebox
  cmd/
    glovebox/
      main.go                          # Wires everything together, starts watcher + workers
  internal/
    config/
      config.go                        # Config struct, LoadConfig, env var overrides
      config_test.go
    staging/
      types.go                         # StagingItem, ItemMetadata, ValidationError types
      types_test.go
      metadata.go                      # ParseMetadata, Validate
      metadata_test.go
      scanner.go                       # ReadStagingItem from directory
      scanner_test.go
    engine/
      rules.go                         # Rule, RuleConfig, LoadRules, validation
      rules_test.go
      matcher.go                       # Matcher interface, Substring/CaseInsensitive/Regex
      matcher_test.go
      scoring.go                       # ScoreSignals, ScanResult, Verdict
      scoring_test.go
      preprocess.go                    # Preprocess (NFKC, zero-width strip, HTML strip)
      preprocess_test.go
      stream.go                        # StreamingScan (chunked reader with overlap)
      stream_test.go
    detector/
      registry.go                      # DetectorRegistry, custom detector interface
      encoding.go                      # EncodingAnomalyDetector
      encoding_test.go
      template.go                      # TemplateStructureDetector
      template_test.go
      language.go                      # LanguageDetectionDetector (lingua-go)
      language_test.go
    routing/
      safety.go                        # ValidateDestination (allowlist + path canonicalization)
      safety_test.go
      pass.go                          # RoutePass
      pass_test.go
      quarantine.go                    # RouteQuarantine + sanitization + notification
      quarantine_test.go
      reject.go                        # RouteReject
      reject_test.go
      sanitize.go                      # SanitizeContent
      sanitize_test.go
      notify.go                        # WriteQuarantineNotification (Maildir-style)
      notify_test.go
      pending.go                       # WritePending, RemovePending, CleanStalePending
      pending_test.go
    audit/
      logger.go                        # AuditLogger, LogPass, LogReject, degraded mode
      logger_test.go
    pipeline/
      worker.go                        # WorkerPool, scan workers, item channel
      worker_test.go
      router.go                        # Router (ordered delivery, FIFO)
      router_test.go
    watcher/
      watcher.go                       # DirectoryWatcher (fsnotify + polling fallback)
      watcher_test.go
    metrics/
      metrics.go                       # OTel meter provider, all metric definitions
      metrics_test.go
  configs/
    default-config.json                # Default configuration
    default-rules.json                 # Default Phase 1 heuristic rules
  integration_test.go                  # End-to-end pipeline tests (build tag: integration)
  container_test.sh                    # Container-level test script
  Dockerfile
  charts/
    glovebox/
      Chart.yaml
      values.yaml
      templates/
        deployment.yaml
        configmap.yaml
        service.yaml
        networkpolicy.yaml
        pvc.yaml
```

---

## Layer 0: Foundation (no dependencies -- can run in parallel)

These tasks have zero dependencies and can all be implemented concurrently.

---

### Task 1: Go Module Init and Project Scaffold
**Bead:** n/a (prerequisite)

**Files:**
- Create: `go.mod`
- Create: `main.go`
- Create: `.golangci.yml`

- [ ] **Step 1: Initialize Go module**

```bash
cd /mnt/c/Users/steve/Code/glovebox
go mod init github.com/leftathome/glovebox
```

- [ ] **Step 2: Create minimal main.go**

```go
// main.go
package main

import "fmt"

func main() {
    fmt.Println("glovebox v0.1.0")
}
```

- [ ] **Step 3: Create golangci-lint config**

```yaml
# .golangci.yml
linters:
  enable:
    - govet
    - staticcheck
    - errcheck
    - ineffassign
    - unused
run:
  timeout: 5m
```

- [ ] **Step 4: Verify build**

Run: `go build -o /tmp/glovebox . && /tmp/glovebox`
Expected: prints "glovebox v0.1.0"

- [ ] **Step 5: Commit**

```bash
git add go.mod main.go .golangci.yml
git commit -m "feat: init Go module and project scaffold"
```

---

### Task 2: Staging Item Types and Metadata Parsing
**Bead:** `glovebox-oo8`

**Files:**
- Create: `internal/staging/types.go`
- Create: `internal/staging/types_test.go`

- [ ] **Step 1: Write failing tests for metadata parsing**

```go
// internal/staging/types_test.go
package staging

import (
    "strings"
    "testing"
    "time"
)

func TestParseMetadata_ValidJSON(t *testing.T) {
    input := `{
        "source": "email",
        "sender": "alice@example.com",
        "subject": "Hello",
        "timestamp": "2026-03-28T12:00:00Z",
        "destination_agent": "messaging",
        "content_type": "text/plain",
        "ordered": true
    }`
    meta, err := ParseMetadata(strings.NewReader(input))
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if meta.Source != "email" {
        t.Errorf("source = %q, want %q", meta.Source, "email")
    }
    if meta.Sender != "alice@example.com" {
        t.Errorf("sender = %q, want %q", meta.Sender, "alice@example.com")
    }
    if meta.Subject != "Hello" {
        t.Errorf("subject = %q, want %q", meta.Subject, "Hello")
    }
    if meta.DestinationAgent != "messaging" {
        t.Errorf("destination_agent = %q, want %q", meta.DestinationAgent, "messaging")
    }
    if meta.ContentType != "text/plain" {
        t.Errorf("content_type = %q, want %q", meta.ContentType, "text/plain")
    }
    if !meta.Ordered {
        t.Error("ordered = false, want true")
    }
    expectedTime, _ := time.Parse(time.RFC3339, "2026-03-28T12:00:00Z")
    if !meta.Timestamp.Equal(expectedTime) {
        t.Errorf("timestamp = %v, want %v", meta.Timestamp, expectedTime)
    }
}

func TestParseMetadata_InvalidJSON(t *testing.T) {
    _, err := ParseMetadata(strings.NewReader("{not json"))
    if err == nil {
        t.Fatal("expected error for invalid JSON")
    }
}

func TestParseMetadata_UnknownFieldsIgnored(t *testing.T) {
    input := `{"source":"email","sender":"a@b.com","timestamp":"2026-03-28T12:00:00Z","destination_agent":"messaging","content_type":"text/plain","extra_field":"ignored"}`
    _, err := ParseMetadata(strings.NewReader(input))
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
}

func TestParseMetadata_OrderedDefaultsFalse(t *testing.T) {
    input := `{"source":"email","sender":"a@b.com","timestamp":"2026-03-28T12:00:00Z","destination_agent":"messaging","content_type":"text/plain"}`
    meta, err := ParseMetadata(strings.NewReader(input))
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if meta.Ordered {
        t.Error("ordered should default to false")
    }
}

func TestParseMetadata_AuthFailureField(t *testing.T) {
    input := `{"source":"email","sender":"a@b.com","timestamp":"2026-03-28T12:00:00Z","destination_agent":"messaging","content_type":"text/plain","auth_failure":true}`
    meta, err := ParseMetadata(strings.NewReader(input))
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if !meta.AuthFailure {
        t.Error("auth_failure = false, want true")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /mnt/c/Users/steve/Code/glovebox && go test ./internal/staging/ -v -run TestParseMetadata`
Expected: FAIL -- package/types don't exist yet

- [ ] **Step 3: Implement types and parser**

```go
// internal/staging/types.go
package staging

import (
    "encoding/json"
    "io"
    "time"
)

// ItemMetadata represents the parsed metadata.json from a staging item.
type ItemMetadata struct {
    Source           string    `json:"source"`
    Sender           string    `json:"sender"`
    Subject          string    `json:"subject"`
    Timestamp        time.Time `json:"timestamp"`
    DestinationAgent string    `json:"destination_agent"`
    ContentType      string    `json:"content_type"`
    Ordered          bool      `json:"ordered"`
    AuthFailure      bool      `json:"auth_failure"`
}

// StagingItem represents a validated item read from the staging directory.
// Content is accessed via ContentPath (not loaded into memory) to support
// streaming scan with bounded memory.
type StagingItem struct {
    DirPath     string
    ContentPath string       // Path to content.raw -- read lazily via io.Reader during scan
    Metadata    ItemMetadata
}

// ParseMetadata reads and parses a metadata.json from the given reader.
func ParseMetadata(r io.Reader) (ItemMetadata, error) {
    var meta ItemMetadata
    if err := json.NewDecoder(r).Decode(&meta); err != nil {
        return ItemMetadata{}, err
    }
    return meta, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/staging/ -v -run TestParseMetadata`
Expected: all PASS

- [ ] **Step 5: Run go vet**

Run: `go vet ./internal/staging/`
Expected: clean

- [ ] **Step 6: Commit**

```bash
git add internal/staging/types.go internal/staging/types_test.go
git commit -m "feat(staging): add item types and metadata parsing"
```

---

### Task 3: Configuration and CLI
**Bead:** `glovebox-5mq`

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `configs/default-config.json`

- [ ] **Step 1: Write failing tests**

```go
// internal/config/config_test.go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -v`
Expected: FAIL

- [ ] **Step 3: Implement config loading**

```go
// internal/config/config.go
package config

import (
    "encoding/json"
    "os"
    "strconv"
)

type Config struct {
    StagingDir         string   `json:"staging_dir"`
    QuarantineDir      string   `json:"quarantine_dir"`
    AuditDir           string   `json:"audit_dir"`
    FailedDir          string   `json:"failed_dir"`
    AgentsDir          string   `json:"agents_dir"`
    SharedDir          string   `json:"shared_dir"`
    AgentAllowlist     []string `json:"agent_allowlist"`
    MetricsPort        int      `json:"metrics_port"`
    WatchMode          string   `json:"watch_mode"`
    PollIntervalSeconds int     `json:"poll_interval_seconds"`
    RulesFile          string   `json:"rules_file"`
    ScanWorkers        int      `json:"scan_workers"`
    ScanTimeoutSeconds int      `json:"scan_timeout_seconds"`
    ScanChunkSizeBytes int      `json:"scan_chunk_size_bytes"`
}

func LoadConfig(path string) (Config, error) {
    cfg := Config{
        MetricsPort:        9090,
        WatchMode:          "fsnotify",
        PollIntervalSeconds: 5,
        ScanWorkers:        4,
        ScanTimeoutSeconds: 30,
        ScanChunkSizeBytes: 262144,
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
    applyDefaults(&cfg)

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

func applyDefaults(cfg *Config) {
    if cfg.ScanWorkers == 0 {
        cfg.ScanWorkers = 4
    }
    if cfg.ScanTimeoutSeconds == 0 {
        cfg.ScanTimeoutSeconds = 30
    }
    if cfg.ScanChunkSizeBytes == 0 {
        cfg.ScanChunkSizeBytes = 262144
    }
    if cfg.MetricsPort == 0 {
        cfg.MetricsPort = 9090
    }
    if cfg.WatchMode == "" {
        cfg.WatchMode = "fsnotify"
    }
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/config/ -v`
Expected: all PASS

- [ ] **Step 5: Create default config file**

```json
// configs/default-config.json
{
  "staging_dir": "/data/glovebox/staging",
  "quarantine_dir": "/data/glovebox/quarantine",
  "audit_dir": "/data/glovebox/audit",
  "failed_dir": "/data/glovebox/failed",
  "agents_dir": "/data/agents",
  "shared_dir": "/data/shared",
  "agent_allowlist": ["messaging", "media", "calendar", "itinerary"],
  "metrics_port": 9090,
  "watch_mode": "fsnotify",
  "poll_interval_seconds": 5,
  "rules_file": "/etc/glovebox/rules.json",
  "scan_workers": 4,
  "scan_timeout_seconds": 30,
  "scan_chunk_size_bytes": 262144
}
```

- [ ] **Step 6: Commit**

```bash
git add internal/config/ configs/default-config.json
git commit -m "feat(config): config loading with env var overrides and defaults"
```

---

### Task 4: Rule Configuration Loading
**Bead:** `glovebox-cji`

**Files:**
- Create: `internal/engine/rules.go`
- Create: `internal/engine/rules_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/engine/rules_test.go
package engine

import (
    "strings"
    "testing"
)

func TestLoadRules_Valid(t *testing.T) {
    input := `{
        "rules": [
            {"name": "test_rule", "patterns": ["bad"], "weight": 0.5, "match_type": "substring"}
        ],
        "quarantine_threshold": 0.8
    }`
    rc, err := LoadRules(strings.NewReader(input))
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(rc.Rules) != 1 {
        t.Fatalf("rules len = %d, want 1", len(rc.Rules))
    }
    if rc.Rules[0].Name != "test_rule" {
        t.Errorf("name = %q, want test_rule", rc.Rules[0].Name)
    }
    if rc.QuarantineThreshold != 0.8 {
        t.Errorf("threshold = %f, want 0.8", rc.QuarantineThreshold)
    }
}

func TestLoadRules_InvalidWeight(t *testing.T) {
    tests := []struct {
        name  string
        weight string
    }{
        {"negative", "-0.1"},
        {"over_one", "1.1"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            input := `{"rules":[{"name":"r","patterns":["x"],"weight":` + tt.weight + `,"match_type":"substring"}],"quarantine_threshold":0.8}`
            _, err := LoadRules(strings.NewReader(input))
            if err == nil {
                t.Fatal("expected error for invalid weight")
            }
        })
    }
}

func TestLoadRules_UnknownMatchType(t *testing.T) {
    input := `{"rules":[{"name":"r","patterns":["x"],"weight":0.5,"match_type":"magic"}],"quarantine_threshold":0.8}`
    _, err := LoadRules(strings.NewReader(input))
    if err == nil {
        t.Fatal("expected error for unknown match_type")
    }
}

func TestLoadRules_CustomDetectorMissingDetectorField(t *testing.T) {
    input := `{"rules":[{"name":"r","patterns":[],"weight":0.5,"match_type":"custom_detector"}],"quarantine_threshold":0.8}`
    _, err := LoadRules(strings.NewReader(input))
    if err == nil {
        t.Fatal("expected error for custom_detector without detector field")
    }
}

func TestLoadRules_InvalidThreshold(t *testing.T) {
    tests := []struct {
        name      string
        threshold string
    }{
        {"zero", "0.0"},
        {"negative", "-1.0"},
        {"too_high", "3.0"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            input := `{"rules":[{"name":"r","patterns":["x"],"weight":0.5,"match_type":"substring"}],"quarantine_threshold":` + tt.threshold + `}`
            _, err := LoadRules(strings.NewReader(input))
            if err == nil {
                t.Fatalf("expected error for threshold %s", tt.threshold)
            }
        })
    }
}

func TestLoadRules_EmptyRulesRejected(t *testing.T) {
    input := `{"rules":[],"quarantine_threshold":0.8}`
    _, err := LoadRules(strings.NewReader(input))
    if err == nil {
        t.Fatal("expected error for empty rules")
    }
}

func TestLoadRules_InvalidBoostFactor(t *testing.T) {
    input := `{"rules":[{"name":"r","patterns":[],"weight":0.0,"match_type":"custom_detector","detector":"test","behavior":"weight_booster","boost_factor":5.0}],"quarantine_threshold":0.8}`
    _, err := LoadRules(strings.NewReader(input))
    if err == nil {
        t.Fatal("expected error for boost_factor > 3.0")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -v -run TestLoadRules`
Expected: FAIL

- [ ] **Step 3: Implement rule types and loading**

```go
// internal/engine/rules.go
package engine

import (
    "encoding/json"
    "fmt"
    "io"
    "regexp"
)

type MatchType string

const (
    MatchSubstring              MatchType = "substring"
    MatchSubstringCaseInsensitive MatchType = "substring_case_insensitive"
    MatchRegex                  MatchType = "regex"
    MatchCustomDetector         MatchType = "custom_detector"
)

type Rule struct {
    Name        string    `json:"name"`
    Patterns    []string  `json:"patterns"`
    Weight      float64   `json:"weight"`
    MatchType   MatchType `json:"match_type"`
    Detector    string    `json:"detector"`
    Behavior    string    `json:"behavior"`
    BoostFactor float64   `json:"boost_factor"`
}

type RuleConfig struct {
    Rules               []Rule  `json:"rules"`
    QuarantineThreshold float64 `json:"quarantine_threshold"`
}

func LoadRules(r io.Reader) (RuleConfig, error) {
    var rc RuleConfig
    if err := json.NewDecoder(r).Decode(&rc); err != nil {
        return RuleConfig{}, fmt.Errorf("parsing rules JSON: %w", err)
    }

    if err := validateRuleConfig(rc); err != nil {
        return RuleConfig{}, err
    }

    return rc, nil
}

func validateRuleConfig(rc RuleConfig) error {
    if len(rc.Rules) == 0 {
        return fmt.Errorf("rules must not be empty")
    }
    if rc.QuarantineThreshold <= 0.0 || rc.QuarantineThreshold > 2.0 {
        return fmt.Errorf("quarantine_threshold must be in (0.0, 2.0], got %f", rc.QuarantineThreshold)
    }

    validMatchTypes := map[MatchType]bool{
        MatchSubstring:              true,
        MatchSubstringCaseInsensitive: true,
        MatchRegex:                  true,
        MatchCustomDetector:         true,
    }

    for i, rule := range rc.Rules {
        if rule.Weight < 0.0 || rule.Weight > 1.0 {
            return fmt.Errorf("rule[%d] %q: weight must be in [0.0, 1.0], got %f", i, rule.Name, rule.Weight)
        }
        if !validMatchTypes[rule.MatchType] {
            return fmt.Errorf("rule[%d] %q: unknown match_type %q", i, rule.Name, rule.MatchType)
        }
        if rule.MatchType == MatchCustomDetector && rule.Detector == "" {
            return fmt.Errorf("rule[%d] %q: custom_detector requires non-empty detector field", i, rule.Name)
        }
        if rule.MatchType == MatchRegex {
            for _, p := range rule.Patterns {
                if _, err := regexp.Compile(p); err != nil {
                    return fmt.Errorf("rule[%d] %q: invalid regex pattern %q: %w", i, rule.Name, p, err)
                }
            }
        }
        if rule.Behavior == "weight_booster" {
            if rule.BoostFactor < 1.0 || rule.BoostFactor > 3.0 {
                return fmt.Errorf("rule[%d] %q: boost_factor must be in [1.0, 3.0], got %f", i, rule.Name, rule.BoostFactor)
            }
        }
    }

    return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine/ -v -run TestLoadRules`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/engine/rules.go internal/engine/rules_test.go
git commit -m "feat(engine): rule configuration loading with validation"
```

---

### Task 5: Audit Logging
**Bead:** `glovebox-yg2`

**Files:**
- Create: `internal/audit/logger.go`
- Create: `internal/audit/logger_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/audit/logger_test.go
package audit

import (
    "bufio"
    "encoding/json"
    "os"
    "path/filepath"
    "testing"
)

func TestLogPass_AppendsValidJSONL(t *testing.T) {
    dir := t.TempDir()
    logger, err := NewLogger(dir)
    if err != nil {
        t.Fatalf("NewLogger: %v", err)
    }

    entry := PassEntry{
        Timestamp:     "2026-03-28T12:00:00Z",
        Source:        "email",
        Sender:        "alice@example.com",
        ContentHash:   "abc123",
        ContentLength: 100,
        Signals:       []SignalEntry{},
        TotalScore:    0.0,
        Verdict:       "pass",
        Destination:   "messaging",
        ScanDurationMs: 42,
    }

    if err := logger.LogPass(entry); err != nil {
        t.Fatalf("LogPass: %v", err)
    }

    data, _ := os.ReadFile(filepath.Join(dir, "pass.jsonl"))
    var decoded PassEntry
    if err := json.Unmarshal(data, &decoded); err != nil {
        t.Fatalf("invalid JSONL: %v", err)
    }
    if decoded.Source != "email" {
        t.Errorf("source = %q, want email", decoded.Source)
    }
}

func TestLogReject_AppendsValidJSONL(t *testing.T) {
    dir := t.TempDir()
    logger, _ := NewLogger(dir)

    entry := RejectEntry{
        Timestamp:     "2026-03-28T12:00:00Z",
        Source:        "email",
        Sender:        "attacker@evil.com",
        ContentHash:   "def456",
        ContentLength: 500,
        Signals: []SignalEntry{
            {Name: "instruction_override", Weight: 1.0, Matched: "ignore previous"},
        },
        TotalScore:    1.0,
        Verdict:       "quarantine",
        Reason:        "threshold_exceeded",
        Destination:   "messaging",
        ScanDurationMs: 15,
    }

    if err := logger.LogReject(entry); err != nil {
        t.Fatalf("LogReject: %v", err)
    }

    data, _ := os.ReadFile(filepath.Join(dir, "rejected.jsonl"))
    var decoded RejectEntry
    if err := json.Unmarshal(data, &decoded); err != nil {
        t.Fatalf("invalid JSONL: %v", err)
    }
    if decoded.Verdict != "quarantine" {
        t.Errorf("verdict = %q, want quarantine", decoded.Verdict)
    }
    if len(decoded.Signals) != 1 {
        t.Errorf("signals len = %d, want 1", len(decoded.Signals))
    }
}

func TestLogPass_MultipleWrites(t *testing.T) {
    dir := t.TempDir()
    logger, _ := NewLogger(dir)

    for i := 0; i < 3; i++ {
        logger.LogPass(PassEntry{
            Timestamp: "2026-03-28T12:00:00Z",
            Source:    "email",
            Sender:    "a@b.com",
            Verdict:   "pass",
        })
    }

    f, _ := os.Open(filepath.Join(dir, "pass.jsonl"))
    defer f.Close()
    scanner := bufio.NewScanner(f)
    count := 0
    for scanner.Scan() {
        count++
        var entry PassEntry
        if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
            t.Errorf("line %d: invalid JSON: %v", count, err)
        }
    }
    if count != 3 {
        t.Errorf("line count = %d, want 3", count)
    }
}

func TestLogPass_SingleLinePerEntry(t *testing.T) {
    dir := t.TempDir()
    logger, _ := NewLogger(dir)

    logger.LogPass(PassEntry{
        Timestamp: "2026-03-28T12:00:00Z",
        Source:    "email",
        Sender:    "has\nnewline@test.com",
        Verdict:   "pass",
    })

    f, _ := os.Open(filepath.Join(dir, "pass.jsonl"))
    defer f.Close()
    scanner := bufio.NewScanner(f)
    lineCount := 0
    for scanner.Scan() {
        lineCount++
    }
    if lineCount != 1 {
        t.Errorf("expected 1 line, got %d (newline in field leaked)", lineCount)
    }
}

func TestLogPass_WriteFailureReturnsError(t *testing.T) {
    logger, _ := NewLogger("/nonexistent/path/that/does/not/exist")
    err := logger.LogPass(PassEntry{Verdict: "pass"})
    if err == nil {
        t.Fatal("expected error for write to nonexistent path")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/audit/ -v`
Expected: FAIL

- [ ] **Step 3: Implement audit logger**

```go
// internal/audit/logger.go
package audit

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "sync"
)

type SignalEntry struct {
    Name    string  `json:"name"`
    Weight  float64 `json:"weight"`
    Matched string  `json:"matched"`
}

type PassEntry struct {
    Timestamp      string        `json:"timestamp"`
    Source         string        `json:"source"`
    Sender         string        `json:"sender"`
    ContentHash    string        `json:"content_hash"`
    ContentLength  int64         `json:"content_length"`
    Signals        []SignalEntry `json:"signals"`
    TotalScore     float64       `json:"total_score"`
    Verdict        string        `json:"verdict"`
    Destination    string        `json:"destination"`
    ScanDurationMs int64         `json:"scan_duration_ms"`
}

type RejectEntry struct {
    Timestamp      string        `json:"timestamp"`
    Source         string        `json:"source"`
    Sender         string        `json:"sender"`
    ContentHash    string        `json:"content_hash"`
    ContentLength  int64         `json:"content_length"`
    Signals        []SignalEntry `json:"signals"`
    TotalScore     float64       `json:"total_score"`
    Verdict        string        `json:"verdict"`
    Reason         string        `json:"reason"`
    Destination    string        `json:"destination"`
    ScanDurationMs int64         `json:"scan_duration_ms"`
}

type Logger struct {
    dir string
    mu  sync.Mutex
}

func NewLogger(dir string) (*Logger, error) {
    return &Logger{dir: dir}, nil
}

func (l *Logger) LogPass(entry PassEntry) error {
    return l.appendJSONL("pass.jsonl", entry)
}

func (l *Logger) LogReject(entry RejectEntry) error {
    return l.appendJSONL("rejected.jsonl", entry)
}

func (l *Logger) appendJSONL(filename string, v any) error {
    l.mu.Lock()
    defer l.mu.Unlock()

    data, err := json.Marshal(v)
    if err != nil {
        return fmt.Errorf("marshal audit entry: %w", err)
    }

    path := filepath.Join(l.dir, filename)
    f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        return fmt.Errorf("open audit log %s: %w", path, err)
    }
    defer f.Close()

    data = append(data, '\n')
    if _, err := f.Write(data); err != nil {
        return fmt.Errorf("write audit log %s: %w", path, err)
    }

    return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/audit/ -v`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/audit/
git commit -m "feat(audit): append-only JSONL audit logger"
```

---

### Task 6: Content Pre-processing Pipeline
**Bead:** `glovebox-9sq`

**Files:**
- Create: `internal/engine/preprocess.go`
- Create: `internal/engine/preprocess_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/engine/preprocess_test.go
package engine

import (
    "bytes"
    "testing"
)

func TestPreprocess_LatinTextUnchanged(t *testing.T) {
    input := []byte("Hello, this is a normal email.")
    result := Preprocess(input, "text/plain")
    if !bytes.Equal(result.Normalized, input) {
        t.Errorf("normalized = %q, want %q", result.Normalized, input)
    }
    if !bytes.Equal(result.Original, input) {
        t.Error("original should be preserved")
    }
}

func TestPreprocess_CyrillicHomoglyphNormalized(t *testing.T) {
    // Cyrillic 'o' U+043E should normalize to Latin 'o'
    input := []byte("ign\xd0\xbere previous")  // "ignоre previous" with Cyrillic o
    result := Preprocess(input, "text/plain")
    if !bytes.Contains(result.Normalized, []byte("ignore previous")) {
        t.Errorf("NFKC normalization did not convert Cyrillic homoglyph: %q", result.Normalized)
    }
}

func TestPreprocess_ZeroWidthCharsStripped(t *testing.T) {
    // U+200B zero-width space between 'ig' and 'nore'
    input := []byte("ig\xe2\x80\x8bnore previous")
    result := Preprocess(input, "text/plain")
    if !bytes.Contains(result.Normalized, []byte("ignore previous")) {
        t.Errorf("zero-width chars not stripped: %q", result.Normalized)
    }
}

func TestPreprocess_HTMLStripped(t *testing.T) {
    input := []byte("<p>Hello <b>world</b></p>")
    result := Preprocess(input, "text/html")
    if !bytes.Contains(result.Normalized, []byte("Hello world")) {
        t.Errorf("HTML not stripped: %q", result.Normalized)
    }
    if bytes.Contains(result.Normalized, []byte("<p>")) {
        t.Error("HTML tags still present in normalized")
    }
}

func TestPreprocess_HTMLEntitiesDecoded(t *testing.T) {
    input := []byte("ignore &amp; previous &lt;instructions&gt;")
    result := Preprocess(input, "text/html")
    if !bytes.Contains(result.Normalized, []byte("ignore & previous <instructions>")) {
        t.Errorf("HTML entities not decoded: %q", result.Normalized)
    }
}

func TestPreprocess_PlainTextSkipsHTMLStrip(t *testing.T) {
    input := []byte("<not html> just text with angle brackets")
    result := Preprocess(input, "text/plain")
    // For text/plain, HTML stripping should NOT be applied
    if !bytes.Contains(result.Normalized, []byte("<not html>")) {
        t.Errorf("text/plain should not strip HTML: %q", result.Normalized)
    }
}

func TestPreprocess_OriginalPreserved(t *testing.T) {
    input := []byte("ig\xe2\x80\x8bnore previous")
    result := Preprocess(input, "text/plain")
    if !bytes.Equal(result.Original, input) {
        t.Error("original content must be preserved byte-identical")
    }
}

func TestPreprocess_FullwidthNormalized(t *testing.T) {
    // Fullwidth Latin 'A' U+FF21 should normalize to 'A'
    input := []byte("\xef\xbc\xa1BC")
    result := Preprocess(input, "text/plain")
    if !bytes.Contains(result.Normalized, []byte("ABC")) {
        t.Errorf("fullwidth not normalized: %q", result.Normalized)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -v -run TestPreprocess`
Expected: FAIL

- [ ] **Step 3: Add dependencies**

Run: `go get golang.org/x/text/unicode/norm golang.org/x/text/secure/precis golang.org/x/net/html`

Note: NFKC normalization alone does NOT handle cross-script confusables (e.g., Cyrillic 'o' U+043E vs Latin 'o'). The pre-processor must include a confusables mapping table from Unicode TR39 or use `golang.org/x/text/secure/precis` profiles. If the Cyrillic homoglyph test fails, implement a confusables lookup table for the most common Latin-lookalike characters (Cyrillic, Greek, etc.).

- [ ] **Step 4: Implement pre-processing**

```go
// internal/engine/preprocess.go
package engine

import (
    "bytes"
    "io"
    "strings"

    "golang.org/x/net/html"
    "golang.org/x/text/unicode/norm"
)

type PreprocessedContent struct {
    Original   []byte   // Preserved byte-identical
    Normalized []byte   // After NFKC + zero-width strip + HTML strip
    RawHTML    []byte   // For text/html: original HTML (rules run against both)
}

// zeroWidthChars are Unicode code points to strip before matching.
var zeroWidthRunes = []rune{
    0x200B, // zero-width space
    0x200C, // zero-width non-joiner
    0x200D, // zero-width joiner
    0xFEFF, // byte order mark / zero-width no-break space
    0x2060, // word joiner
    0x200E, // left-to-right mark
    0x200F, // right-to-left mark
}

func Preprocess(content []byte, contentType string) PreprocessedContent {
    original := make([]byte, len(content))
    copy(original, content)

    // Step 1: NFKC normalization (maps homoglyphs and compatibility chars)
    normalized := norm.NFKC.Bytes(content)

    // Step 2: Strip zero-width characters
    normalized = stripZeroWidth(normalized)

    result := PreprocessedContent{
        Original:   original,
        Normalized: normalized,
    }

    // Step 3: HTML stripping for text/html
    if strings.HasPrefix(contentType, "text/html") {
        result.RawHTML = normalized
        result.Normalized = stripHTML(normalized)
    }

    return result
}

func stripZeroWidth(data []byte) []byte {
    s := string(data)
    for _, r := range zeroWidthRunes {
        s = strings.ReplaceAll(s, string(r), "")
    }
    return []byte(s)
}

func stripHTML(data []byte) []byte {
    tokenizer := html.NewTokenizer(bytes.NewReader(data))
    var buf bytes.Buffer

    for {
        tt := tokenizer.Next()
        switch tt {
        case html.ErrorToken:
            if tokenizer.Err() == io.EOF {
                return buf.Bytes()
            }
            return buf.Bytes()
        case html.TextToken:
            buf.Write(tokenizer.Text())
        }
    }
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/engine/ -v -run TestPreprocess`
Expected: all PASS (note: Cyrillic homoglyph test may need refinement -- NFKC does not map all Cyrillic to Latin. If it fails, adjust test to use a character that NFKC does normalize, or add a separate confusables mapping step.)

- [ ] **Step 6: Commit**

```bash
git add internal/engine/preprocess.go internal/engine/preprocess_test.go go.mod go.sum
git commit -m "feat(engine): content pre-processing with NFKC, zero-width strip, HTML strip"
```

---

### Task 7: Content Sanitization for Quarantine
**Bead:** `glovebox-jx4`

**Files:**
- Create: `internal/routing/sanitize.go`
- Create: `internal/routing/sanitize_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/routing/sanitize_test.go
package routing

import (
    "bytes"
    "strings"
    "testing"
)

func TestSanitizeContent_ShortContent(t *testing.T) {
    input := []byte("Hello, this is safe content.")
    result := SanitizeContent(input)
    if !bytes.Contains(result, []byte("Hello, this is safe content.")) {
        t.Errorf("short content should be fully included: %q", result)
    }
    if !bytes.Contains(result, []byte("UNTRUSTED QUARANTINED CONTENT")) {
        t.Error("missing UNTRUSTED header")
    }
}

func TestSanitizeContent_LongContentTruncated(t *testing.T) {
    input := bytes.Repeat([]byte("A"), 10000)
    result := SanitizeContent(input)
    // Count 'A's in output (should be at most 4096)
    aCount := bytes.Count(result, []byte("A"))
    if aCount > 4096 {
        t.Errorf("content not truncated: %d chars of A found", aCount)
    }
}

func TestSanitizeContent_NonASCIIEscaped(t *testing.T) {
    input := []byte("hello \xc3\xa9 world") // e-acute
    result := SanitizeContent(input)
    if bytes.Contains(result, []byte("\xc3\xa9")) {
        t.Error("non-ASCII byte should be escaped")
    }
    if !bytes.Contains(result, []byte(`\u00e9`)) {
        t.Errorf("expected unicode escape for e-acute: %q", result)
    }
}

func TestSanitizeContent_ControlCharsEscaped(t *testing.T) {
    input := []byte("line1\x00line2\x01line3")
    result := SanitizeContent(input)
    if bytes.Contains(result, []byte{0x00}) {
        t.Error("null byte should be escaped")
    }
}

func TestSanitizeContent_WrappedInMarkers(t *testing.T) {
    result := SanitizeContent([]byte("test"))
    s := string(result)
    if !strings.HasPrefix(s, "--- UNTRUSTED QUARANTINED CONTENT") {
        t.Error("missing header marker")
    }
    if !strings.HasSuffix(strings.TrimSpace(s), "--- END UNTRUSTED CONTENT ---") {
        t.Error("missing footer marker")
    }
}

func TestSanitizeContent_Empty(t *testing.T) {
    result := SanitizeContent([]byte{})
    if !bytes.Contains(result, []byte("UNTRUSTED")) {
        t.Error("empty content should still produce valid wrapper")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/routing/ -v -run TestSanitizeContent`
Expected: FAIL

- [ ] **Step 3: Implement sanitization**

```go
// internal/routing/sanitize.go
package routing

import (
    "fmt"
    "strings"
    "unicode/utf8"
)

const maxSanitizedChars = 4096

func SanitizeContent(content []byte) []byte {
    var b strings.Builder

    b.WriteString("--- UNTRUSTED QUARANTINED CONTENT (first 4096 chars) ---\n")

    charCount := 0
    for i := 0; i < len(content) && charCount < maxSanitizedChars; {
        r, size := utf8.DecodeRune(content[i:])
        if r == utf8.RuneError && size <= 1 {
            // Invalid UTF-8 byte -- escape as hex
            b.WriteString(fmt.Sprintf("\\x%02x", content[i]))
            i++
            charCount++
            continue
        }

        if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
            // Control character -- escape
            b.WriteString(fmt.Sprintf("\\u%04x", r))
        } else if r > 0x7E {
            // Non-ASCII -- escape (8-digit form for runes above BMP)
            if r > 0xFFFF {
                b.WriteString(fmt.Sprintf("\\U%08x", r))
            } else {
                b.WriteString(fmt.Sprintf("\\u%04x", r))
            }
        } else {
            b.WriteRune(r)
        }

        i += size
        charCount++
    }

    b.WriteString("\n--- END UNTRUSTED CONTENT ---\n")

    return []byte(b.String())
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/routing/ -v -run TestSanitizeContent`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/routing/sanitize.go internal/routing/sanitize_test.go
git commit -m "feat(routing): content sanitization for quarantine"
```

---

## Layer 1: Depends on Layer 0

These tasks depend on Layer 0 completions but can run in parallel with each other.

---

### Task 8: Metadata Validation
**Bead:** `glovebox-3tr` (depends on: `glovebox-oo8`)

**Files:**
- Create: `internal/staging/metadata.go`
- Modify: `internal/staging/metadata_test.go` (new file alongside types_test.go)

Full TDD cycle: write tests for each validation rule (required fields, allowlist, length limits, control chars, auth_failure), then implement `Validate(meta ItemMetadata, allowlist []string) []ValidationError`. See bead description for complete exit criteria.

- [ ] **Step 1: Write failing tests** covering: all-valid passes, each missing required field, invalid destination_agent, multiple errors reported, field length limits, control chars rejected, auth_failure detected
- [ ] **Step 2: Run tests -- verify FAIL**
- [ ] **Step 3: Implement Validate function**
- [ ] **Step 4: Run tests -- verify PASS**
- [ ] **Step 5: go vet clean**
- [ ] **Step 6: Commit**: `feat(staging): metadata validation with allowlist and field constraints`

---

### Task 9: Pattern Matchers
**Bead:** `glovebox-g40` (depends on: `glovebox-cji`)

**Files:**
- Create: `internal/engine/matcher.go`
- Create: `internal/engine/matcher_test.go`

Full TDD cycle: define `Matcher` interface with `Match(content []byte, patterns []string) ([]MatchResult, error)`. Implement `SubstringMatcher`, `CaseInsensitiveMatcher`, `RegexMatcher`. Tests per matcher: single match, multiple matches, no match, empty content, empty patterns. Regex: invalid regex returns error at construction time.

- [ ] **Step 1: Write failing tests** for all three matchers
- [ ] **Step 2: Run tests -- verify FAIL**
- [ ] **Step 3: Implement matchers**
- [ ] **Step 4: Run tests -- verify PASS**
- [ ] **Step 5: go vet clean**
- [ ] **Step 6: Commit**: `feat(engine): pattern matchers (substring, case-insensitive, regex)`

---

### Task 10: Signal Scoring and Verdict Logic
**Bead:** `glovebox-md9` (depends on: `glovebox-cji`)

**Files:**
- Create: `internal/engine/scoring.go`
- Create: `internal/engine/scoring_test.go`

Full TDD cycle: implement `ScoreSignals(signals []Signal, boostRules []BoostRule, threshold float64) ScanResult`. Tests: no signals -> pass, below threshold -> pass, at threshold -> quarantine, multiple signals summing above -> quarantine, boost multiplier math, boost-only -> pass, boundary cases.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(engine): signal scoring with boost multiplier and verdict logic`

---

### Task 11: Audit Failure Degraded Mode
**Bead:** `glovebox-2v0` (depends on: `glovebox-yg2`)

**Files:**
- Modify: `internal/audit/logger.go` (add degraded mode tracking)
- Modify: `internal/audit/logger_test.go` (add degraded mode tests)

Full TDD cycle: add `InDegradedMode() bool` to Logger. On write failure, set degraded. On successful write, clear degraded. Tests: normal writes not degraded, failed write -> degraded, successful write after failure -> clears degraded.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(audit): fail-closed degraded mode on write failure`

---

### Task 12: Path Safety Validation
**Bead:** `glovebox-fkt` (depends on: `glovebox-5mq`)

**Files:**
- Create: `internal/routing/safety.go`
- Create: `internal/routing/safety_test.go`

Full TDD cycle. See bead exit criteria.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(routing): path safety validation with allowlist and traversal prevention`

---

### Task 13: Pending Item Placeholders
**Bead:** `glovebox-8hm` (depends on: `glovebox-oo8`)

**Files:**
- Create: `internal/routing/pending.go`
- Create: `internal/routing/pending_test.go`

Full TDD cycle. See bead exit criteria.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(routing): pending item placeholder lifecycle`

---

### Task 14: Quarantine Notification Files
**Bead:** `glovebox-8kh` (depends on: `glovebox-oo8`)

**Files:**
- Create: `internal/routing/notify.go`
- Create: `internal/routing/notify_test.go`

Full TDD cycle. See bead exit criteria.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(routing): Maildir-style quarantine notifications`

---

## Layer 2: Depends on Layer 1

---

### Task 15: Staging Directory Scanner
**Bead:** `glovebox-c0o` (depends on: `glovebox-oo8`, `glovebox-3tr`)

**Files:**
- Create: `internal/staging/scanner.go`
- Create: `internal/staging/scanner_test.go`

Full TDD cycle: `ReadStagingItem(dirPath string, allowlist []string) (StagingItem, error)`. Tests: valid dir, missing content.raw, missing metadata.json, empty dir, validation failure.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(staging): directory scanner reads and validates staging items`

---

### Task 16: Encoding Anomaly Detector
**Bead:** `glovebox-09m` (depends on: `glovebox-g40`)

**Files:**
- Create: `internal/detector/registry.go` (detector interface + registry)
- Create: `internal/detector/encoding.go`
- Create: `internal/detector/encoding_test.go`

- [ ] **Step 1: Define detector interface and registry in registry.go**
- [ ] **Step 2: Write failing tests** for encoding detector
- [ ] **Step 3: Implement encoding anomaly detector**
- [ ] **Step 4: Run tests -- verify PASS**
- [ ] **Step 5: Commit**: `feat(detector): encoding anomaly detector (base64, zero-width, unicode escapes)`

---

### Task 17: Prompt Template Structure Detector
**Bead:** `glovebox-1qk` (depends on: `glovebox-g40`)

**Files:**
- Create: `internal/detector/template.go`
- Create: `internal/detector/template_test.go`

Full TDD cycle. Key test: "you are invited" does NOT trigger, "You are a helpful assistant" DOES.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(detector): prompt template structure detector`

---

### Task 18: Language Detection Detector
**Bead:** `glovebox-2g3` (depends on: `glovebox-g40`)

**Files:**
- Create: `internal/detector/language.go`
- Create: `internal/detector/language_test.go`

Run: `go get github.com/pemistahl/lingua-go` first.

Full TDD cycle. Key tests: English no-signal, French fires, short text no-signal.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(detector): language detection using lingua-go`

---

### Task 19: Streaming Content Scanner
**Bead:** `glovebox-3q3` (depends on: `glovebox-g40`, `glovebox-9sq`)

**Files:**
- Create: `internal/engine/stream.go`
- Create: `internal/engine/stream_test.go`

Full TDD cycle: `StreamingScan(reader io.Reader, contentType string, matchers, detectors, chunkSize)`. Tests: single chunk, cross-boundary match, prefix+suffix sampling.

**Important:** For `text/html` content, matchers MUST run against BOTH the raw HTML and the HTML-stripped plain text (spec Section 6.2). The pre-processor produces both in `PreprocessedContent`. Test that a payload hidden in HTML tags (e.g., `ign<!-- -->ore previous`) is caught by the stripped-text scan even when the raw scan misses it.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(engine): streaming content scanner with bounded memory`

---

### Task 20: PASS Verdict Routing
**Bead:** `glovebox-97f` (depends on: `glovebox-oo8`, `glovebox-yg2`, `glovebox-fkt`, `glovebox-8hm`)

**Files:**
- Create: `internal/routing/pass.go`
- Create: `internal/routing/pass_test.go`

Full TDD cycle. See bead exit criteria.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(routing): PASS verdict routing to agent workspace`

---

### Task 21: QUARANTINE Verdict Routing
**Bead:** `glovebox-1vk` (depends on: `glovebox-oo8`, `glovebox-yg2`, `glovebox-jx4`, `glovebox-8kh`, `glovebox-fkt`, `glovebox-8hm`)

**Files:**
- Create: `internal/routing/quarantine.go`
- Create: `internal/routing/quarantine_test.go`

Full TDD cycle. See bead exit criteria.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(routing): QUARANTINE verdict routing with sanitization and notification`

---

### Task 22: REJECT Verdict Routing
**Bead:** `glovebox-ou1` (depends on: `glovebox-yg2`, `glovebox-fkt`)

**Files:**
- Create: `internal/routing/reject.go`
- Create: `internal/routing/reject_test.go`

Full TDD cycle. See bead exit criteria.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(routing): REJECT verdict routing with cleanup`

---

## Layer 3: Depends on Layer 2

---

### Task 23: Default Rules Configuration
**Bead:** `glovebox-l3x` (depends on: `glovebox-cji`, `glovebox-09m`, `glovebox-1qk`, `glovebox-2g3`)

**Files:**
- Create: `configs/default-rules.json`
- Modify: `internal/detector/registry.go` (register all detectors)
- Create: `internal/detector/registry_test.go`

- [ ] **Step 1: Create default-rules.json** with all 6 Phase 1 rules per spec section 6.3
- [ ] **Step 2: Write test** that loads default-rules.json, verifies 6 rules, verifies each detector resolves
- [ ] **Step 3: Implement detector registration wiring**
- [ ] **Step 4: Run tests -- verify PASS**
- [ ] **Step 5: Commit**: `feat(detector): default rules config and detector registry`

---

### Task 24: Staging Watcher
**Bead:** `glovebox-lg3` (depends on: `glovebox-c0o`)

**Files:**
- Create: `internal/watcher/watcher.go`
- Create: `internal/watcher/watcher_test.go`

Full TDD cycle: fsnotify primary, polling fallback, FIFO by directory name, context cancellation, stale .pending.json cleanup.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(watcher): filesystem watcher with fsnotify and polling fallback`

---

### Task 25a: Scan Worker Pool
**Bead:** `glovebox-6ic` (depends on: `glovebox-3q3`, `glovebox-md9`)

**Files:**
- Create: `internal/pipeline/worker.go`
- Create: `internal/pipeline/worker_test.go`

Implement the worker pool: configurable N goroutines pulling StagingItems from a channel, running pre-process + streaming scan + scoring, producing ScanResults on an output channel. Per-item context deadline for scan timeout. Workers do NOT call routing -- they produce results only.

- [ ] **Step 1: Write failing tests**: N workers process N items concurrently (timing test), scan timeout produces quarantine result, pool drains on context cancellation
- [ ] **Step 2: Run tests -- verify FAIL**
- [ ] **Step 3: Implement worker pool**
- [ ] **Step 4: Run tests -- verify PASS**
- [ ] **Step 5: go vet clean**
- [ ] **Step 6: Commit**: `feat(pipeline): parallel scan worker pool with timeout`

---

### Task 25b: Ordered Delivery Router
**Bead:** `glovebox-6ic` (continued)

**Files:**
- Create: `internal/pipeline/router.go`
- Create: `internal/pipeline/router_test.go`

Implement the router: receives ScanResults from worker pool, calls verdict routing (PASS/QUARANTINE/REJECT) and audit logging, delivers ordered items in FIFO order (holds completed results until all prior items for same destination are delivered). Unordered items delivered immediately.

Dependencies: Tasks 20-22 (verdict routing), Task 5/11 (audit logger).

- [ ] **Step 1: Write failing tests**: ordered items delivered in FIFO despite out-of-order completion, unordered items delivered immediately, routing errors handled gracefully
- [ ] **Step 2: Run tests -- verify FAIL**
- [ ] **Step 3: Implement router**
- [ ] **Step 4: Run tests -- verify PASS**
- [ ] **Step 5: go vet clean**
- [ ] **Step 6: Commit**: `feat(pipeline): ordered delivery router`

---

### Task 25c: Failed Item Rescan
**Bead:** n/a (gap from review -- covers spec Section 7.5 and Section 13)

**Files:**
- Create: `internal/pipeline/failed.go`
- Create: `internal/pipeline/failed_test.go`

On each processing tick, check the `failed/` directory for items. Items found there are fed back into the scan pipeline (full rescan, not stale verdict). This prevents TOCTOU -- if rules were updated between the original scan and the retry, the new rules apply.

- [ ] **Step 1: Write failing tests**: item in failed/ dir is re-read and fed to scan channel, item is rescanned (not routed with old verdict), after successful routing item is removed from failed/
- [ ] **Step 2: Run tests -- verify FAIL**
- [ ] **Step 3: Implement failed item recovery**
- [ ] **Step 4: Run tests -- verify PASS**
- [ ] **Step 5: go vet clean**
- [ ] **Step 6: Commit**: `feat(pipeline): failed item rescan with fresh verdict`

---

### Task 26: Dockerfile
**Bead:** `glovebox-ol3` (depends on: `glovebox-5mq`)

**Files:**
- Create: `Dockerfile`

- [ ] **Step 1: Write Dockerfile**

```dockerfile
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /glovebox .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /glovebox /glovebox
COPY configs/default-config.json /etc/glovebox/config.json
COPY configs/default-rules.json /etc/glovebox/rules.json
USER nonroot:nonroot
ENTRYPOINT ["/glovebox"]
CMD ["--config", "/etc/glovebox/config.json"]
```

- [ ] **Step 2: Build and verify**

Run: `docker build -t glovebox:dev .`
Expected: builds successfully

- [ ] **Step 3: Verify image size**

Run: `docker images glovebox:dev --format '{{.Size}}'`
Expected: under 50MB

- [ ] **Step 4: Verify it runs**

Run: `docker run --rm glovebox:dev --help`
Expected: prints help/version

- [ ] **Step 5: Commit**

```bash
git add Dockerfile
git commit -m "feat: multi-stage Dockerfile with distroless runtime"
```

---

### Task 27: OpenTelemetry Metrics
**Bead:** `glovebox-rob`

**Files:**
- Create: `internal/metrics/metrics.go`
- Create: `internal/metrics/metrics_test.go`

Run: `go get go.opentelemetry.io/otel go.opentelemetry.io/otel/exporters/prometheus go.opentelemetry.io/otel/sdk/metric` first.

Full TDD cycle: register all 10 metrics, expose /metrics endpoint, verify Prometheus text format.

- [ ] **Step 1-6:** Standard TDD cycle
- [ ] **Commit**: `feat(metrics): OpenTelemetry instrumentation with Prometheus exporter`

---

## Layer 4: Assembly

---

### Task 28: Main Entry Point (Wire Everything Together)
**Bead:** n/a (assembly)

**Files:**
- Create: `cmd/glovebox/main.go`
- Modify: `main.go`

Wire: config loading -> rule loading -> detector registry -> watcher -> worker pool -> router -> audit logger -> metrics server. Handle SIGTERM graceful shutdown.

- [ ] **Step 1: Implement main.go** wiring all components
- [ ] **Step 2: Manual smoke test** with test directories
- [ ] **Step 3: Commit**: `feat: wire main entry point with all components`

---

### Task 29: Helm Chart
**Bead:** `glovebox-7ub` (depends on: `glovebox-ol3`)

**Files:**
- Create: `charts/glovebox/Chart.yaml`
- Create: `charts/glovebox/values.yaml`
- Create: `charts/glovebox/templates/deployment.yaml`
- Create: `charts/glovebox/templates/configmap.yaml`
- Create: `charts/glovebox/templates/service.yaml`
- Create: `charts/glovebox/templates/networkpolicy.yaml`
- Create: `charts/glovebox/templates/pvc.yaml`

- [ ] **Step 1: Create Chart.yaml and values.yaml**
- [ ] **Step 2: Create all template files** (one K8s object per file per CLAUDE.md)
- [ ] **Step 3: Verify**: `helm lint charts/glovebox/`
- [ ] **Step 4: Verify**: `helm template glovebox charts/glovebox/ | kubectl apply --dry-run=client -f -`
- [ ] **Step 5: Commit**: `feat: Helm chart with deployment, network policy, and PVCs`

---

## Layer 5: Testing

---

### Task 30: Integration Tests
**Bead:** `glovebox-zbd`

**Files:**
- Create: `integration_test.go` (build tag: `//go:build integration`)

End-to-end tests using real filesystem. See bead exit criteria for test cases.

Run: `go test -tags=integration -v ./...`

- [ ] **Step 1: Write integration tests**
- [ ] **Step 2: Run and iterate until passing**
- [ ] **Step 3: Commit**: `test: end-to-end integration tests`

---

### Task 31: Container Tests
**Bead:** `glovebox-8cn`

**Files:**
- Create: `container_test.sh`

Build image, run with mounted test dirs, verify processing + metrics + graceful shutdown.

- [ ] **Step 1: Write container test script**
- [ ] **Step 2: Run and iterate until passing**
- [ ] **Step 3: Commit**: `test: container-level verification tests`
