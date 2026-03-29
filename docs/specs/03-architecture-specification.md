# OpenClaw Home Agent — Architecture Specification

**Version 1.0 — March 2026**

*This document specifies how the system delivers the features defined in the Product Specification and Feature & Behavior Specification. It covers component design, deployment topology, data flow, security enforcement, and operational concerns for both Phase 1 (Intel NUC / Kubernetes) and Phase 2 (Mac Studio M5 / native macOS).*

---

## 1. System Overview

The system consists of six primary components running on a single host:

1. **Four domain agents** — persistent OpenClaw agent processes (messaging, media, calendar, itinerary)
2. **One review agent** — persistent OpenClaw agent process (glovebox quarantine review)
3. **Glovebox service** — deterministic content scanning process (not an OpenClaw agent)
4. **Connectors** — lightweight processes that bridge external sources to the glovebox
5. **Supporting services** — embedding server, monitoring stack, secrets provider
6. **Shared filesystem** — the coordination and state layer

All components run on the same physical host. There is no distributed deployment or inter-host networking.

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Host Machine                                │
│                                                                     │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐           │
│  │Connector │  │Connector │  │Connector │  │Connector │           │
│  │ (Email)  │  │  (SMS)   │  │(Webhook) │  │ (UniFi)  │           │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘           │
│       │              │              │              │                │
│       └──────────────┴──────┬───────┴──────────────┘                │
│                             ▼                                       │
│                    ┌─────────────────┐                               │
│                    │    Glovebox     │                               │
│                    │ (scan & route)  │                               │
│                    └───┬────┬────┬───┘                               │
│                        │    │    │                                   │
│              ┌─────────┘    │    └──────────┐                       │
│              ▼              ▼               ▼                       │
│     ┌──────────────┐ ┌───────────┐  ┌─────────────┐               │
│     │Agent Workspace│ │Quarantine │  │  Audit Log  │               │
│     └──────┬───────┘ └─────┬─────┘  └─────────────┘               │
│            │               │                                        │
│            ▼               ▼                                        │
│  ┌────────────────────────────────────────────┐                    │
│  │           Agent Processes                   │                    │
│  │  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐ ┌──────┐│                    │
│  │  │Msg  │ │Media│ │Cal  │ │Itin │ │Review││                    │
│  │  └─────┘ └─────┘ └─────┘ └─────┘ └──────┘│                    │
│  └────────────────────────────────────────────┘                    │
│                        │                                            │
│              ┌─────────┴──────────┐                                │
│              ▼                    ▼                                 │
│     ┌──────────────┐    ┌──────────────┐                           │
│     │Shared FS     │    │ Supporting   │                           │
│     │(coordination)│    │ Services     │                           │
│     └──────────────┘    └──────────────┘                           │
└─────────────────────────────────────────────────────────────────────┘
```

## 2. Phase 1 Deployment: Intel NUC / Talos Linux / Kubernetes

### 2.1 Platform

- **Hardware:** Intel NUC (specific model TBD based on available inventory)
- **OS:** Talos Linux — immutable, API-managed, minimal attack surface
- **Orchestration:** Kubernetes (single-node cluster managed by `talosctl`)
- **CNI:** Cilium with default-deny egress network policies
- **GitOps:** ArgoCD watches a Git repository for desired state and reconciles cluster configuration

### 2.2 Workload Deployment

Each component runs as a Kubernetes workload:

| Component | Workload Type | Replicas | Notes |
|-----------|--------------|----------|-------|
| Messaging Agent | Deployment | 1 | Persistent process via OpenClaw heartbeat |
| Media Agent | Deployment | 1 | |
| Calendar Agent | Deployment | 1 | |
| Itinerary Agent | Deployment | 1 | |
| Review Agent | Deployment | 1 | |
| Glovebox | Deployment | 1 | Deterministic Node.js/Python process |
| Email Connector | CronJob or Deployment | 1 | Polls IMAP on interval |
| Embedding Server | Deployment | 1 | llama.cpp + nomic-embed-text-v2-moe |
| Prometheus | StatefulSet | 1 | Via kube-prometheus-stack |
| Grafana | Deployment | 1 | |
| 1Password Connect | Deployment | 1 | Secrets provider for ESO |
| External Secrets Operator | Deployment | 1 | Syncs 1Password secrets to K8s secrets |

### 2.3 Storage

- **OpenEBS** provides persistent volumes for agent workspaces, shared filesystem, glovebox directories, and monitoring data.
- Volumes are backed by local storage on the NUC's disk.
- Volume mount paths map directly to the logical filesystem layout defined in Section 4.

### 2.4 Network Policies

Cilium NetworkPolicies enforce per-agent egress rules:

- **Messaging Agent:** Allowed egress to Gmail API endpoints, configured IMAP servers. Denied all other egress.
- **Media Agent:** Allowed egress to Plex/Jellyfin API (local network). Denied external egress.
- **Calendar Agent:** Allowed egress to Google Calendar API endpoints, UniFi controller (local network). Denied all other egress.
- **Itinerary Agent:** Allowed egress to Anthropic API (for inference), general web (for search). This is the broadest egress profile and is the most carefully monitored.
- **Review Agent:** No egress. Filesystem only.
- **Glovebox:** No egress. Filesystem only.
- **Connectors:** Allowed egress to their specific external source (e.g., IMAP server, webhook endpoint).

Default policy: deny all egress and ingress not explicitly allowed.

### 2.5 Secrets

- 1Password Connect runs as a cluster service, exposing secrets via its API.
- External Secrets Operator (ESO) watches ExternalSecret resources and creates Kubernetes Secrets from 1Password items.
- Each agent's Deployment references its required secrets via standard Kubernetes secret volume mounts or environment variable injection.
- Secrets are never written to the Git repository, agent configuration files, or the shared filesystem.
- Secret rotation is handled in 1Password; ESO polls for changes on a configurable interval.

### 2.6 CI/CD

- GitLab CI/CD (self-hosted or cloud) runs lint, test, and build pipelines on push to the GitOps repository.
- ArgoCD detects changes in the repository and applies them to the cluster.
- Agent system prompts and tool configurations are versioned in Git alongside Kubernetes manifests.

## 3. Phase 2 Deployment: Mac Studio M5 / Native macOS

### 3.1 Platform

- **Hardware:** Mac Studio M5 with 64GB+ unified memory
- **OS:** macOS (current release at time of deployment)
- **Process Management:** launchd (LaunchDaemons for system-level, LaunchAgents for user-level)
- **OpenClaw:** Installed natively via npm. Not containerized — full access to macOS APIs, TCC permissions, Accessibility frameworks.

### 3.2 Why Native (Not Containerized)

Phase 2 capabilities require macOS-native access that Docker cannot provide:

- TCC (Transparency, Consent, and Control) permissions for Notifications, Accessibility, Screen Recording, Microphone, Automation
- AppleScript and Shortcuts execution for iMessage, system-level automation, desktop control
- Playwright browser automation with full GPU rendering and access to the desktop display
- Voice telephony integration via macOS audio stack
- Local LLM inference via vllm-mlx, leveraging Apple Silicon unified memory architecture

Docker on macOS interposes a Linux VM, breaking access to all of the above.

### 3.3 Workload Deployment

Each agent runs as a launchd-managed process:

```xml
<!-- Example: com.openclaw.agent.messaging.plist -->
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.openclaw.agent.messaging</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/op</string>
        <string>run</string>
        <string>--</string>
        <string>/usr/local/bin/node</string>
        <string>/opt/openclaw/bin/openclaw</string>
        <string>agent</string>
        <string>start</string>
        <string>--config</string>
        <string>/opt/openclaw/agents/messaging/config.json</string>
    </array>
    <key>KeepAlive</key>
    <true/>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/var/log/openclaw/messaging.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/openclaw/messaging.err</string>
</dict>
</plist>
```

Secrets are injected via `op run` (1Password CLI), which populates environment variables from 1Password items at process launch time. This replaces the Connect + ESO pattern from Phase 1.

### 3.4 Security on macOS

Since there is no container or network-level isolation in Phase 2:

- **Dedicated user account:** OpenClaw runs under a dedicated non-admin macOS user. This user has no sudo privileges and cannot modify system files.
- **Filesystem ACLs:** Each agent's workspace directory is owned by the openclaw user. Read/write boundaries are enforced via POSIX permissions and macOS ACLs.
- **TCC restrictions:** The openclaw user is granted only the TCC permissions required for its specific capabilities (Accessibility for desktop automation, Automation for AppleScript). Permissions are granted granularly, not blanket.
- **Application firewall:** macOS application firewall rules restrict per-process outbound connectivity. This replaces Cilium network policies.
- **FileVault:** Full-disk encryption is enabled on the Mac Studio.

### 3.5 Local Inference

- **vllm-mlx** runs as a separate launchd service, exposing an OpenAI-compatible API on localhost.
- Agents can be configured to use the local inference endpoint instead of (or in addition to) the Anthropic cloud API.
- The glovebox classifier (Phase 2 addition) uses the local inference endpoint exclusively — classification requests never leave the local network.
- Model selection prioritizes models that fit within available unified memory alongside all running agent processes. Target: a capable 7B–13B parameter model for classification, with the option for a larger model for agent inference if memory allows.

### 3.6 Apple Ecosystem Integrations

- **iMessage:** Read/send via AppleScript commands executed by the messaging agent. Requires Automation TCC permission for Messages.app.
- **Shortcuts:** Agents can trigger Shortcuts via the `shortcuts` CLI tool. Complex automation flows (multi-step home automation, device control) are built in Shortcuts and exposed to agents as callable actions.
- **Desktop Automation:** Playwright controls browser windows for web-based task automation. AppleScript controls native applications when needed.
- **Voice (Exploratory):** Microphone access for voice interaction, speech-to-text via macOS or Whisper, text-to-speech via macOS `say` or a local model. Scoped as exploratory — not a launch requirement for Phase 2.

## 4. Filesystem Layout

This layout is consistent across both phases. In Phase 1, paths are mounted as Kubernetes volumes. In Phase 2, they are directories under `/opt/openclaw/` or `~/.openclaw/` on the Mac.

```
~/.openclaw/
├── agents/
│   ├── messaging/
│   │   ├── config.json          # Agent configuration (tools, heartbeat, model)
│   │   ├── system-prompt.md     # Agent system prompt
│   │   └── workspace/           # Private memory and state files
│   ├── media/
│   │   ├── config.json
│   │   ├── system-prompt.md
│   │   └── workspace/
│   ├── calendar/
│   │   ├── config.json
│   │   ├── system-prompt.md
│   │   └── workspace/
│   ├── itinerary/
│   │   ├── config.json
│   │   ├── system-prompt.md
│   │   └── workspace/
│   └── review/
│       ├── config.json
│       ├── system-prompt.md
│       └── workspace/
├── glovebox/
│   ├── config.json              # Heuristic rules, thresholds, routing map
│   ├── staging/                 # Connectors write here. Only glovebox reads.
│   ├── quarantine/              # Items pending human review
│   │   └── <timestamp>-<hash>/
│   │       ├── content.md       # Sanitized content representation
│   │       └── metadata.json    # Source, signals, verdict details
│   └── audit/
│       ├── pass.jsonl           # Append-only log of passed items
│       └── rejected.jsonl       # Append-only log of rejected/quarantined items
├── shared/
│   ├── messaging/               # Messaging agent writes here, all agents read
│   ├── media/                   # Media agent writes here, all agents read
│   ├── calendar/                # Calendar agent writes here, all agents read
│   ├── itinerary/               # Itinerary agent writes here, all agents read
│   └── glovebox-notify.md       # Glovebox writes quarantine notices here
├── connectors/
│   ├── email/
│   │   └── config.json          # IMAP/Gmail config, polling interval
│   ├── unifi/
│   │   └── config.json          # UniFi controller address, credentials ref
│   └── webhook/
│       └── config.json          # Webhook listener config
├── backup/
│   └── config.json              # Backup schedule, targets (Synology, B2)
└── logs/
    ├── agent-messaging.log
    ├── agent-media.log
    ├── agent-calendar.log
    ├── agent-itinerary.log
    ├── agent-review.log
    └── glovebox.log
```

### 4.1 Permission Matrix

| Component | Own Workspace (R/W) | Other Workspaces | Shared (Read) | Shared (Write own subdir) | Glovebox Staging | Quarantine | Audit Log |
|-----------|---------------------|-----------------|---------------|--------------------------|-----------------|------------|-----------|
| Messaging Agent | ✓ | ✗ | ✓ | ✓ | ✗ | ✗ | ✗ |
| Media Agent | ✓ | ✗ | ✓ | ✓ | ✗ | ✗ | ✗ |
| Calendar Agent | ✓ | ✗ | ✓ | ✓ | ✗ | ✗ | ✗ |
| Itinerary Agent | ✓ | ✗ | ✓ | ✓ | ✗ | ✗ | ✗ |
| Review Agent | ✓ | Write (release) | ✓ | ✓ | ✗ | R/W | ✗ |
| Glovebox | ✗ | Write (routing) | ✗ | Write (notify) | R/W | R/W | Append |
| Connectors | ✗ | ✗ | ✗ | ✗ | Write | ✗ | ✗ |

## 5. Glovebox Architecture

### 5.1 Process Design

The glovebox is a standalone process — not an OpenClaw agent. It is deterministic, stateless (beyond its configuration), and has no LLM dependency in Phase 1.

**Language:** Node.js or Python (decision deferred to implementation). Criteria: filesystem watching performance, regex/string matching ergonomics, ease of adding the LLM classifier in Phase 2.

**Execution model:** The glovebox watches the staging directory for new items (via filesystem events or polling). On detecting a new item, it processes it synchronously: read → scan → verdict → route/quarantine → log. Items are processed one at a time in FIFO order.

### 5.2 Heuristic Engine

The heuristic engine is a configurable set of pattern-matching rules, each producing a signal with a name and weight:

```json
{
  "rules": [
    {
      "name": "instruction_override",
      "patterns": ["ignore previous", "disregard your instructions", "new directive", "forget your instructions"],
      "weight": 1.0,
      "match_type": "substring_case_insensitive"
    },
    {
      "name": "role_reassignment",
      "patterns": ["you are now", "act as", "pretend you are", "your new role"],
      "weight": 1.0,
      "match_type": "substring_case_insensitive"
    },
    {
      "name": "tool_invocation_syntax",
      "patterns": ["<tool>", "<function_call>", "exec:", "bash:", "```shell"],
      "weight": 0.8,
      "match_type": "substring"
    },
    {
      "name": "suspicious_encoding",
      "patterns": [],
      "weight": 0.7,
      "match_type": "custom_detector",
      "detector": "encoding_anomaly"
    },
    {
      "name": "prompt_template_structure",
      "patterns": [],
      "weight": 0.6,
      "match_type": "custom_detector",
      "detector": "template_structure"
    },
    {
      "name": "non_english_content",
      "patterns": [],
      "weight": 0.0,
      "match_type": "custom_detector",
      "detector": "language_detection",
      "behavior": "weight_booster",
      "boost_factor": 1.5
    }
  ],
  "quarantine_threshold": 0.8
}
```

**Signal compounding:** Non-English detection does not contribute its own weight. Instead, when detected, it multiplies all other signal weights by its boost factor. Non-English alone never triggers quarantine.

**Custom detectors** are functions (not regex) that analyze structural properties: base64 block detection, unicode anomaly counting, template structure heuristics, language classification (using a lightweight model like `franc` or `cld3`).

### 5.3 Connector Interface

Connectors are simple, single-purpose processes. Each connector:

1. Authenticates to its external source (IMAP, API, webhook listener)
2. Retrieves new content since the last poll
3. Writes each item to the glovebox staging directory as a subdirectory containing:
   - `content.raw` — the original content (email body, webhook payload, etc.)
   - `metadata.json` — structured metadata: `{ source, sender, subject, timestamp, destination_agent, content_type }`
4. Exits (for CronJob connectors) or returns to polling (for persistent connectors)

Connectors make the routing decision at delivery time via the `destination_agent` field. This is source-based routing — the email connector knows email goes to the messaging agent. The glovebox does not make routing decisions.

Connectors have write access only to the staging directory. They cannot read from staging, quarantine, agent workspaces, or any other system directory.

## 6. LLM Inference Strategy

### 6.1 Phase 1: Cloud Inference

- All agent inference uses the Anthropic API (Claude).
- Each agent includes its API key via Kubernetes secret injection.
- The embedding server runs locally (llama.cpp + nomic-embed-text-v2-moe) for memory retrieval. This is the only model running on the NUC.
- Cost is managed by tuning heartbeat intervals and designing system prompts that minimize unnecessary inference calls (e.g., silent heartbeat ticks should not invoke the LLM if there is nothing to process).

### 6.2 Phase 2: Hybrid Inference

- Agent inference can use local models (via vllm-mlx on localhost) or the Anthropic API, configurable per agent.
- The glovebox classifier exclusively uses local inference — classification requests never leave the network.
- The local embedding server continues to run for memory retrieval.
- Model selection for local inference: target a model that fits in unified memory alongside all running processes. With 64GB+ unified memory, a 7B–13B parameter model for classification and a larger model for agent inference should be feasible, but actual model selection is deferred to Phase 2 implementation based on available models and benchmarks at that time.

## 7. Monitoring & Observability

### 7.1 Metrics

The monitoring stack collects:

- **Agent metrics:** heartbeat tick count, actions taken per tick, LLM API call count and latency, tool invocations by type, errors and retries.
- **Glovebox metrics:** items processed, pass/quarantine/reject counts, heuristic signal frequency by rule, processing latency.
- **System metrics:** CPU, memory, disk usage, network egress by destination.
- **Connector metrics:** poll success/failure, items fetched per poll, authentication errors.

### 7.2 Alerting

Alerts fire for:

- Agent process down (not restarted by supervisor within 60 seconds)
- Glovebox processing queue depth exceeding threshold (items accumulating faster than processing)
- Quarantine queue depth exceeding threshold (items awaiting human review for too long)
- API authentication failures (expired tokens, revoked credentials)
- Disk usage approaching capacity
- Unexpected egress (network request to a non-allowlisted destination)

### 7.3 Dashboards

Grafana dashboards provide:

- System overview: all component health at a glance
- Per-agent activity: what each agent is doing, how often, success rate
- Glovebox funnel: items in → pass/quarantine/reject breakdown over time
- Security view: egress traffic, blocked requests, heuristic signal trends

### 7.4 Phase 1 Stack

- Prometheus (via kube-prometheus-stack Helm chart) for metrics collection
- Grafana for dashboards and alerting
- Prometheus AlertManager for alert routing (email, webhook, or Slack notification)

### 7.5 Phase 2 Stack

- Prometheus continues to run, either in a Docker container or directly on macOS
- Agents export metrics via a lightweight HTTP endpoint or structured log output
- Grafana dashboard configuration migrates from Phase 1 unchanged

## 8. Backup & Recovery Architecture

### 8.1 Backup Targets

| Target | What | Frequency | Tool |
|--------|------|-----------|------|
| Synology NAS (local) | All workspace files, configs, shared FS, audit logs | Daily (full), hourly (incremental) | rclone or rsync |
| Backblaze B2 (off-site) | Same as Synology | Daily | rclone |

### 8.2 What Is NOT Backed Up

- Glovebox staging directory (transient by design — items are processed and moved)
- Container images / node_modules (rebuilt from source on recovery)
- Secrets (live in 1Password, restored by re-linking ESO or `op run`)
- Monitoring time-series data (Prometheus) — acceptable to lose on rebuild; dashboards are in Git

### 8.3 Recovery Procedure

1. Provision clean hardware and OS (Talos for Phase 1, macOS for Phase 2)
2. Clone the GitOps repository
3. For Phase 1: `talosctl apply-config`, ArgoCD bootstrap, ESO connects to 1Password
4. For Phase 2: `npm install openclaw`, create launchd plists, configure `op run`
5. Restore workspace and shared filesystem data from Backblaze B2 via rclone
6. Verify agent startup and heartbeat operation
7. Verify connector authentication and glovebox processing

Target: under 2 hours from bare hardware to operational system.

## 9. Migration: Phase 1 → Phase 2

### 9.1 Principle: Parallel Operation, Not Cutover

The NUC stays live throughout migration. The Mac Studio is brought up alongside it. Channels are migrated one at a time, with the NUC as fallback.

### 9.2 Migration Layers

**Data layer (low risk):** Restore workspace and shared filesystem from Backblaze B2 to the Mac. Verify file integrity. The filesystem layout is identical across phases — no transformation needed.

**Secrets layer (medium risk):** Replace 1Password Connect + ESO with 1Password CLI + `op run`. Each agent's secret references need to be reconfigured for the new injection method. Test each agent's authentication independently before cutover.

**Integration layer (new work, not migration):** Phase 2 capabilities (iMessage, Shortcuts, desktop automation, local inference, voice) are net-new configuration on the Mac. There is no legacy state to carry over. These are enabled incrementally after core agent functionality is verified.

### 9.3 Cutover Sequence

1. Mac Studio operational with all Phase 1 agents running and verified
2. Migrate connectors one at a time (email first, then others)
3. Verify glovebox processing on Mac
4. Disable corresponding connectors on NUC
5. Run in parallel for minimum 2 weeks
6. Decommission NUC agents (keep NUC available for other homelab use)
7. Begin Phase 2 feature enablement on Mac

## 10. Open Questions & Deferred Decisions

| Question | Deferred To | Notes |
|----------|-------------|-------|
| Glovebox implementation language (Node.js vs Python) | Implementation | Both are viable; choose based on developer preference and library ecosystem |
| Specific local model for Phase 2 classifier | Phase 2 | Depends on available models and benchmarks at deployment time |
| Inter-agent messaging (sessions_send) | Phase 2 | Only if shared-file coordination proves insufficient |
| Voice interaction architecture | Phase 2 | Exploratory; not a launch requirement |
| Multi-user identity and permission model | Phase 2 | Single-user only in Phase 1 |
| IVR/phone automation approach | Phase 2 | Requires voice telephony integration; design TBD |
| Cognee knowledge graph configuration | Phase 2 | Depends on local model capability for graph extraction |
| Specific Helm charts and version pinning | Implementation | Lock versions in GitOps repo during implementation |
