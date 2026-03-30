# Glovebox Deployment Guide

## 1. Overview

Glovebox is a deterministic content scanning service that sits between external
data connectors and the domain agents in an OpenClaw Home Agent deployment. A
complete deployment consists of:

- **Glovebox** -- the scanning service that watches a staging directory, scans
  incoming content against weighted pattern rules, and routes items to agent
  workspaces (PASS), quarantine (QUARANTINE), or discard (REJECT).
- **One or more connectors** -- processes that fetch content from external
  sources (IMAP, RSS, etc.) and write it to the shared staging directory.

The connectors and glovebox communicate exclusively through the filesystem: connectors
write items to `staging/`, glovebox picks them up, scans them, and moves them to
their final destination. There is no network communication between glovebox and
its connectors.

```
                                 +-------------------+
  +-----------+                  |    glovebox       |
  |   IMAP    |--+               |                   |     +----------------+
  +-----------+  |  staging/     | validate           |--->| agents/        |
                 +-------------->| pre-process        |    | (workspaces)   |
  +-----------+  |               | scan (worker pool) |    +----------------+
  |   RSS     |--+               | score & route      |
  +-----------+                  |                   |     +----------------+
                                 |                   |--->| quarantine/    |
  +-----------+                  +-------------------+    | (human review) |
  |  custom   |--+                                        +----------------+
  +-----------+
```

Container images are published to GitHub Container Registry:

| Image | Registry path |
|-------|---------------|
| Glovebox | `ghcr.io/leftathome/glovebox` |
| RSS connector | `ghcr.io/leftathome/glovebox-rss` |
| IMAP connector | `ghcr.io/leftathome/glovebox-imap` |

Images are tagged `latest` (from main), plus semver tags (`v1.2.3`, `v1.2`) on
release.

---

## 2. Docker Compose (Primary Deployment Method)

### 2.1 Complete docker-compose.yml

This example deploys glovebox with both the RSS and IMAP connectors. Save this
file as `docker-compose.yml` in your project directory.

```yaml
services:
  # ---------- Glovebox scanner ----------
  glovebox:
    image: ghcr.io/leftathome/glovebox:latest
    restart: unless-stopped
    volumes:
      - staging:/data/glovebox/staging
      - quarantine:/data/glovebox/quarantine
      - audit:/data/glovebox/audit
      - failed:/data/glovebox/failed
      - agents:/data/agents
      - shared:/data/shared
      - ./configs/glovebox-config.json:/etc/glovebox/config.json:ro
      - ./configs/rules.json:/etc/glovebox/rules.json:ro
    ports:
      - "9090:9090"   # Prometheus metrics
    # Distroless images have no shell utilities. Use curl from the host
    # or an orchestrator-level health check instead of in-container checks.

  # ---------- RSS connector ----------
  rss-connector:
    image: ghcr.io/leftathome/glovebox-rss:latest
    restart: unless-stopped
    environment:
      GLOVEBOX_STAGING_DIR: /data/glovebox/staging
      GLOVEBOX_STATE_DIR: /state
      GLOVEBOX_CONNECTOR_CONFIG: /etc/connector/config.json
    volumes:
      - staging:/data/glovebox/staging
      - rss-state:/state
      - ./configs/rss-config.json:/etc/connector/config.json:ro
    ports:
      - "8081:8080"   # Health + metrics

  # ---------- IMAP connector ----------
  imap-connector:
    image: ghcr.io/leftathome/glovebox-imap:latest
    restart: unless-stopped
    environment:
      GLOVEBOX_STAGING_DIR: /data/glovebox/staging
      GLOVEBOX_STATE_DIR: /state
      GLOVEBOX_CONNECTOR_CONFIG: /etc/connector/config.json
      IMAP_HOST: mail.example.com
      IMAP_PORT: "993"
      IMAP_USERNAME: user@example.com
      IMAP_PASSWORD: ${IMAP_PASSWORD}
      IMAP_TLS: "true"
    volumes:
      - staging:/data/glovebox/staging
      - imap-state:/state
      - ./configs/imap-config.json:/etc/connector/config.json:ro
    ports:
      - "8082:8080"   # Health + metrics

volumes:
  staging:
  quarantine:
  audit:
  failed:
  agents:
  shared:
  rss-state:
  imap-state:
```

### 2.2 Configuration Files

Create a `configs/` directory alongside your `docker-compose.yml` with these
files.

**configs/glovebox-config.json:**

```json
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

**configs/rules.json** -- copy from the repository's `configs/default-rules.json`
or customize (see Section 5.5 for the rules format).

**configs/rss-config.json:**

```json
{
  "routes": [
    { "match": "feed:home-assistant-blog", "destination": "media" },
    { "match": "*", "destination": "messaging" }
  ],
  "feeds": [
    {
      "name": "home-assistant-blog",
      "url": "https://www.home-assistant.io/atom.xml"
    },
    {
      "name": "ars-technica",
      "url": "https://feeds.arstechnica.com/arstechnica/technology-lab"
    }
  ],
  "fetch_links": false,
  "link_policy": {
    "default": "safe",
    "rules": []
  }
}
```

**configs/imap-config.json:**

```json
{
  "folders": [
    { "name": "INBOX" },
    { "name": "Notifications" }
  ],
  "routes": [
    { "match": "folder:INBOX", "destination": "messaging" },
    { "match": "folder:Notifications", "destination": "media" },
    { "match": "*", "destination": "messaging" }
  ]
}
```

### 2.3 IMAP Credentials

Never put passwords directly in `docker-compose.yml`. Use a `.env` file in the
same directory:

```
IMAP_PASSWORD=your-app-password-here
```

Docker Compose reads `.env` automatically and substitutes `${IMAP_PASSWORD}`.
Add `.env` to your `.gitignore`.

For production, use Docker secrets or an external secret manager (1Password,
Vault, etc.) to inject credentials at runtime.

### 2.4 Adding More Connectors

To add another connector (for example, a GitHub connector you built with the
scaffold generator):

1. Add a new service block to `docker-compose.yml`:

```yaml
  github-connector:
    image: ghcr.io/leftathome/glovebox-github:latest
    restart: unless-stopped
    environment:
      GLOVEBOX_STAGING_DIR: /data/glovebox/staging
      GLOVEBOX_STATE_DIR: /state
      GLOVEBOX_CONNECTOR_CONFIG: /etc/connector/config.json
      GITHUB_TOKEN: ${GITHUB_TOKEN}
    volumes:
      - staging:/data/glovebox/staging
      - github-state:/state
      - ./configs/github-config.json:/etc/connector/config.json:ro
    ports:
      - "8083:8080"
    depends_on:
      glovebox:
        condition: service_healthy
```

2. Add the state volume to the `volumes:` section:

```yaml
  github-state:
```

3. Create the connector config file at `configs/github-config.json`.

4. If the connector delivers to a new agent name, add it to the
   `agent_allowlist` in `configs/glovebox-config.json`.

The critical requirement is that every connector mounts the same `staging`
volume at the path specified by `GLOVEBOX_STAGING_DIR`.

### 2.5 Starting and Verifying

```sh
# Start the stack
docker compose up -d

# Verify all services are healthy
docker compose ps

# Check glovebox logs
docker compose logs glovebox

# Check connector logs
docker compose logs rss-connector
docker compose logs imap-connector

# Verify metrics are available
curl -s http://localhost:9090/metrics

# Check connector health
curl -s http://localhost:8081/healthz
curl -s http://localhost:8082/readyz
```

---

## 3. Kubernetes

This section provides guidance for deploying glovebox on Kubernetes. It is not a
complete Helm chart, but covers the key resources you need.

### 3.1 Shared Volume

Glovebox and all connectors must share the staging directory. In Kubernetes,
this requires a ReadWriteMany (RWX) PersistentVolume. Options include:

- **NFS** -- simplest for homelab; works well for the volume sizes involved
- **iSCSI** -- higher performance, but only supports ReadWriteOnce (RWO) unless
  your storage backend supports multi-attach
- **Longhorn / Rook-Ceph** -- if you run a distributed storage layer

For NFS, create a PV and PVC:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: glovebox-staging
  namespace: openclaw
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs-client
  resources:
    requests:
      storage: 5Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: glovebox-quarantine
  namespace: openclaw
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs-client
  resources:
    requests:
      storage: 2Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: glovebox-audit
  namespace: openclaw
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: nfs-client
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: glovebox-agents
  namespace: openclaw
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs-client
  resources:
    requests:
      storage: 10Gi
```

**Important:** If you use NFS, be aware that `fsnotify` does not work over NFS.
Set `watch_mode` to `"poll"` in the glovebox config (or leave the default
`poll_interval_seconds` as a fallback). Local storage (hostPath, Longhorn) does
support fsnotify.

### 3.2 IMAP Credentials Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: imap-credentials
  namespace: openclaw
type: Opaque
stringData:
  IMAP_HOST: mail.example.com
  IMAP_PORT: "993"
  IMAP_USERNAME: user@example.com
  IMAP_PASSWORD: your-app-password-here
  IMAP_TLS: "true"
```

Do not commit this file to version control. Create it with `kubectl create secret`
or inject it from an external secret manager (External Secrets Operator, Vault
CSI provider, etc.).

### 3.3 Glovebox Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: glovebox
  namespace: openclaw
  labels:
    app: glovebox
spec:
  replicas: 1
  selector:
    matchLabels:
      app: glovebox
  template:
    metadata:
      labels:
        app: glovebox
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        fsGroup: 65534
      containers:
        - name: glovebox
          image: ghcr.io/leftathome/glovebox:latest
          args: ["--config", "/etc/glovebox/config.json"]
          ports:
            - name: metrics
              containerPort: 9090
              protocol: TCP
          livenessProbe:
            httpGet:
              path: /metrics
              port: metrics
            initialDelaySeconds: 5
            periodSeconds: 30
          volumeMounts:
            - name: staging
              mountPath: /data/glovebox/staging
            - name: quarantine
              mountPath: /data/glovebox/quarantine
            - name: audit
              mountPath: /data/glovebox/audit
            - name: failed
              mountPath: /data/glovebox/failed
            - name: agents
              mountPath: /data/agents
            - name: shared
              mountPath: /data/shared
            - name: config
              mountPath: /etc/glovebox
          resources:
            requests:
              cpu: 100m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 256Mi
      volumes:
        - name: staging
          persistentVolumeClaim:
            claimName: glovebox-staging
        - name: quarantine
          persistentVolumeClaim:
            claimName: glovebox-quarantine
        - name: audit
          persistentVolumeClaim:
            claimName: glovebox-audit
        - name: agents
          persistentVolumeClaim:
            claimName: glovebox-agents
        - name: failed
          persistentVolumeClaim:
            claimName: glovebox-failed
        - name: shared
          emptyDir: {}
        - name: config
          configMap:
            name: glovebox-config
```

### 3.4 IMAP Connector Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: imap-connector
  namespace: openclaw
  labels:
    app: imap-connector
spec:
  replicas: 1
  selector:
    matchLabels:
      app: imap-connector
  template:
    metadata:
      labels:
        app: imap-connector
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        fsGroup: 65534
      containers:
        - name: imap-connector
          image: ghcr.io/leftathome/glovebox-imap:latest
          env:
            - name: GLOVEBOX_STAGING_DIR
              value: /data/glovebox/staging
            - name: GLOVEBOX_STATE_DIR
              value: /state
            - name: GLOVEBOX_CONNECTOR_CONFIG
              value: /etc/connector/config.json
          envFrom:
            - secretRef:
                name: imap-credentials
          ports:
            - name: health
              containerPort: 8080
              protocol: TCP
          livenessProbe:
            httpGet:
              path: /healthz
              port: health
            initialDelaySeconds: 5
            periodSeconds: 15
          readinessProbe:
            httpGet:
              path: /readyz
              port: health
            initialDelaySeconds: 10
            periodSeconds: 10
          volumeMounts:
            - name: staging
              mountPath: /data/glovebox/staging
            - name: state
              mountPath: /state
            - name: config
              mountPath: /etc/connector
          resources:
            requests:
              cpu: 50m
              memory: 32Mi
            limits:
              cpu: 200m
              memory: 128Mi
      volumes:
        - name: staging
          persistentVolumeClaim:
            claimName: glovebox-staging
        - name: state
          emptyDir: {}
        - name: config
          configMap:
            name: imap-connector-config
```

### 3.5 RSS Connector Deployment

Follow the same pattern as the IMAP connector, replacing:

- Image: `ghcr.io/leftathome/glovebox-rss:latest`
- ConfigMap: `rss-connector-config`
- No `envFrom` for secrets (RSS has no credentials)
- No `IMAP_*` environment variables

### 3.6 ConfigMaps

```sh
kubectl create configmap glovebox-config \
  --from-file=config.json=configs/glovebox-config.json \
  --from-file=rules.json=configs/rules.json \
  -n openclaw

kubectl create configmap imap-connector-config \
  --from-file=config.json=configs/imap-config.json \
  -n openclaw

kubectl create configmap rss-connector-config \
  --from-file=config.json=configs/rss-config.json \
  -n openclaw
```

---

## 4. Binary Install

### 4.1 Download

Download binaries from the
[GitHub Releases](https://github.com/leftathome/glovebox/releases) page, or
build from source:

```sh
git clone https://github.com/leftathome/glovebox.git
cd glovebox
CGO_ENABLED=0 go build -o /usr/local/bin/glovebox .
CGO_ENABLED=0 go build -o /usr/local/bin/glovebox-rss ./connectors/rss/
CGO_ENABLED=0 go build -o /usr/local/bin/glovebox-imap ./connectors/imap/
```

### 4.2 Directory Layout

```sh
sudo mkdir -p /etc/glovebox
sudo mkdir -p /var/lib/glovebox/{staging,quarantine,audit,failed}
sudo mkdir -p /var/lib/glovebox/agents
sudo mkdir -p /var/lib/glovebox/shared
sudo mkdir -p /var/lib/glovebox/connectors/rss/state
sudo mkdir -p /var/lib/glovebox/connectors/imap/state
```

Copy configuration files:

```sh
sudo cp configs/default-config.json /etc/glovebox/config.json
sudo cp configs/default-rules.json /etc/glovebox/rules.json
sudo cp connectors/rss/config.json /etc/glovebox/rss-config.json
sudo cp connectors/imap/config.json /etc/glovebox/imap-config.json
```

Edit `/etc/glovebox/config.json` to point to your local paths:

```json
{
  "staging_dir": "/var/lib/glovebox/staging",
  "quarantine_dir": "/var/lib/glovebox/quarantine",
  "audit_dir": "/var/lib/glovebox/audit",
  "failed_dir": "/var/lib/glovebox/failed",
  "agents_dir": "/var/lib/glovebox/agents",
  "shared_dir": "/var/lib/glovebox/shared",
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

### 4.3 File Permissions

Create a dedicated system user:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin glovebox
sudo chown -R glovebox:glovebox /var/lib/glovebox
sudo chown -R glovebox:glovebox /etc/glovebox
sudo chmod 750 /var/lib/glovebox/audit
```

The audit directory should be append-only for the glovebox user. On
ext4/xfs, you can enforce this with:

```sh
sudo chattr +a /var/lib/glovebox/audit
```

### 4.4 systemd Unit Files

**/etc/systemd/system/glovebox.service:**

```ini
[Unit]
Description=Glovebox content scanning service
After=network.target

[Service]
Type=simple
User=glovebox
Group=glovebox
ExecStart=/usr/local/bin/glovebox --config /etc/glovebox/config.json
Restart=on-failure
RestartSec=5

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/glovebox
ReadOnlyPaths=/etc/glovebox
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

**/etc/systemd/system/glovebox-rss.service:**

```ini
[Unit]
Description=Glovebox RSS connector
After=glovebox.service
Requires=glovebox.service

[Service]
Type=simple
User=glovebox
Group=glovebox
Environment=GLOVEBOX_STAGING_DIR=/var/lib/glovebox/staging
Environment=GLOVEBOX_STATE_DIR=/var/lib/glovebox/connectors/rss/state
Environment=GLOVEBOX_CONNECTOR_CONFIG=/etc/glovebox/rss-config.json
ExecStart=/usr/local/bin/glovebox-rss
Restart=on-failure
RestartSec=5

NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/glovebox/staging /var/lib/glovebox/connectors/rss/state
ReadOnlyPaths=/etc/glovebox
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

**/etc/systemd/system/glovebox-imap.service:**

```ini
[Unit]
Description=Glovebox IMAP connector
After=glovebox.service
Requires=glovebox.service

[Service]
Type=simple
User=glovebox
Group=glovebox
Environment=GLOVEBOX_STAGING_DIR=/var/lib/glovebox/staging
Environment=GLOVEBOX_STATE_DIR=/var/lib/glovebox/connectors/imap/state
Environment=GLOVEBOX_CONNECTOR_CONFIG=/etc/glovebox/imap-config.json
Environment=IMAP_HOST=mail.example.com
Environment=IMAP_PORT=993
Environment=IMAP_USERNAME=user@example.com
Environment=IMAP_TLS=true
EnvironmentFile=-/etc/glovebox/imap-credentials.env

ExecStart=/usr/local/bin/glovebox-imap
Restart=on-failure
RestartSec=5

NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/glovebox/staging /var/lib/glovebox/connectors/imap/state
ReadOnlyPaths=/etc/glovebox
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Store the IMAP password in `/etc/glovebox/imap-credentials.env`:

```
IMAP_PASSWORD=your-app-password-here
```

Restrict permissions on this file:

```sh
sudo chmod 600 /etc/glovebox/imap-credentials.env
sudo chown glovebox:glovebox /etc/glovebox/imap-credentials.env
```

Enable and start:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now glovebox
sudo systemctl enable --now glovebox-rss
sudo systemctl enable --now glovebox-imap
```

---

## 5. Configuration Reference

### 5.1 Glovebox Config

The glovebox scanner reads a JSON config file (default path:
`/etc/glovebox/config.json`, overridden with `--config`).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `staging_dir` | string | `/data/glovebox/staging` | Directory where connectors write incoming items. Glovebox watches this directory for new item subdirectories. |
| `quarantine_dir` | string | `/data/glovebox/quarantine` | Items that score above the quarantine threshold are moved here for human review. |
| `audit_dir` | string | `/data/glovebox/audit` | Append-only directory for JSONL audit log files recording every scan verdict. |
| `failed_dir` | string | `/data/glovebox/failed` | Items that fail structural validation (missing metadata, malformed content) are moved here. |
| `agents_dir` | string | `/data/agents` | Root directory containing agent workspace subdirectories. Clean items are delivered to `<agents_dir>/<agent>/workspace/inbox/`. |
| `shared_dir` | string | `/data/shared` | Shared directory for inter-service communication (notification placeholders, etc.). |
| `agent_allowlist` | []string | `["messaging", "media", "calendar", "itinerary"]` | List of valid agent names. Items routed to an agent not on this list are rejected. |
| `metrics_port` | int | `9090` | TCP port for the HTTP server that exposes the `/metrics` Prometheus endpoint. |
| `watch_mode` | string | `"fsnotify"` | How glovebox detects new items in staging. `"fsnotify"` uses inotify (Linux) or kqueue (macOS). Use `"poll"` if staging is on NFS or another filesystem that does not support inotify. |
| `poll_interval_seconds` | int | `5` | How often (in seconds) glovebox polls the staging directory when using poll mode, or as a fallback sweep interval in fsnotify mode. |
| `rules_file` | string | `/etc/glovebox/rules.json` | Path to the JSON file containing scanning rules and the quarantine threshold. |
| `scan_workers` | int | `4` | Number of parallel goroutines in the scan worker pool. Each worker processes one item at a time. |
| `scan_timeout_seconds` | int | `30` | Maximum time in seconds for scanning a single item. If a scan exceeds this timeout, the item is quarantined. |
| `scan_chunk_size_bytes` | int | `262144` (256 KB) | Size of read chunks for streaming scan of large content. The scanner reads content in chunks of this size to bound memory usage. |

### 5.2 Connector Config -- Common Fields

Every connector config file supports a `routes` array that determines which
agent workspace receives each item. Routes are evaluated in order; the first
match wins.

```json
{
  "routes": [
    { "match": "<pattern>", "destination": "<agent-name>" },
    { "match": "*", "destination": "<default-agent>" }
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `routes` | []object | Ordered list of routing rules. |
| `routes[].match` | string | Pattern to match against the item source. Use `*` as a catch-all. Prefix patterns are connector-specific (e.g., `feed:<name>` for RSS, `folder:<name>` for IMAP). |
| `routes[].destination` | string | Name of the agent workspace to deliver to. Must be in glovebox's `agent_allowlist`. |

### 5.3 RSS Connector Config

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `feeds` | []object | (required) | List of RSS/Atom feeds to poll. |
| `feeds[].name` | string | (required) | Identifier for this feed, used in route matching as `feed:<name>`. |
| `feeds[].url` | string | (required) | URL of the RSS or Atom feed. |
| `fetch_links` | bool | `false` | When true, the connector fetches the full HTML page linked by each feed entry and includes it as additional content for scanning. |
| `link_policy` | object | `{"default": "safe", "rules": []}` | Controls how linked content is handled. |
| `link_policy.default` | string | `"safe"` | Default link handling policy. |
| `link_policy.rules` | []object | `[]` | Per-domain or per-pattern link handling overrides. |

The RSS connector polls every 15 minutes by default.

**Environment variables:**

| Variable | Required | Description |
|----------|----------|-------------|
| `GLOVEBOX_STAGING_DIR` | Yes | Path to the shared staging directory. |
| `GLOVEBOX_STATE_DIR` | Yes | Path where the connector persists its checkpoint (last-seen item per feed). |
| `GLOVEBOX_CONNECTOR_CONFIG` | No | Path to the connector config JSON. Defaults to `/etc/connector/config.json`. |

### 5.4 IMAP Connector Config

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `folders` | []object | (required) | List of IMAP folders to monitor. |
| `folders[].name` | string | (required) | IMAP folder name (e.g., `"INBOX"`, `"Notifications"`). Used in route matching as `folder:<name>`. |

The IMAP connector polls every 5 minutes by default.

**Environment variables:**

| Variable | Required | Description |
|----------|----------|-------------|
| `GLOVEBOX_STAGING_DIR` | Yes | Path to the shared staging directory. |
| `GLOVEBOX_STATE_DIR` | Yes | Path where the connector persists its checkpoint (last-seen UID per folder). |
| `GLOVEBOX_CONNECTOR_CONFIG` | No | Path to the connector config JSON. Defaults to `/etc/connector/config.json`. |
| `IMAP_HOST` | Yes | IMAP server hostname (e.g., `mail.example.com`). |
| `IMAP_PORT` | No | IMAP server port. Defaults to `993` for TLS. |
| `IMAP_USERNAME` | Yes | IMAP login username. |
| `IMAP_PASSWORD` | Yes | IMAP login password or app-specific password. |
| `IMAP_TLS` | No | TLS is enabled by default (implicit TLS on port 993). Set to `"false"` to disable TLS and use port 143. |

### 5.5 Rules File

The rules file defines the scanning patterns and quarantine threshold. The
default is at `configs/default-rules.json` in the repository.

```json
{
  "rules": [
    {
      "name": "instruction_override",
      "patterns": [
        "(?i)ignore\\s+(\\w+\\s+)*previous",
        "(?i)disregard\\s+(\\w+\\s+)*your\\s+instructions"
      ],
      "weight": 1.0,
      "match_type": "regex"
    },
    {
      "name": "tool_invocation_syntax",
      "patterns": ["<tool>", "<function_call>", "exec:", "bash:"],
      "weight": 0.8,
      "match_type": "substring"
    },
    {
      "name": "suspicious_encoding",
      "patterns": [],
      "weight": 0.7,
      "match_type": "custom_detector",
      "detector": "encoding_anomaly"
    }
  ],
  "quarantine_threshold": 0.8
}
```

| Field | Type | Description |
|-------|------|-------------|
| `rules` | []object | Ordered list of scanning rules. All rules are evaluated against every item; scores are summed. |
| `rules[].name` | string | Human-readable identifier for the rule. Appears in audit logs. |
| `rules[].patterns` | []string | List of patterns to match. For `regex` rules, these are Go regular expressions. For `substring` rules, these are literal strings. Empty for `custom_detector` rules. |
| `rules[].weight` | float | Score contribution when this rule matches. Weights are summed across all matching rules to produce the final score. |
| `rules[].match_type` | string | One of `"regex"`, `"substring"`, `"substring_case_insensitive"`, or `"custom_detector"`. |
| `rules[].detector` | string | (Only for `custom_detector` type.) Name of the built-in detector function (e.g., `"encoding_anomaly"`, `"template_structure"`, `"language_detection"`). |
| `rules[].behavior` | string | (Optional.) Special behavior modifier. `"weight_booster"` multiplies other matching rule weights by `boost_factor` instead of adding its own score. |
| `rules[].boost_factor` | float | (Only for `weight_booster` behavior.) Multiplier applied to the accumulated score from other rules. |
| `quarantine_threshold` | float | Items whose total score meets or exceeds this value are quarantined. Default: `0.8`. |

**Scoring example:** An item matches `instruction_override` (weight 1.0) and
`tool_invocation_syntax` (weight 0.8). Total score = 1.8, which exceeds the
default threshold of 0.8. The item is quarantined.

---

## 6. Directory Layout

```
/data/
  glovebox/
    staging/           Incoming items from connectors (shared volume)
      <item-id>/
        content.raw    Raw content bytes (email body, RSS entry, etc.)
        metadata.json  Item metadata (source, timestamp, connector, route)
    quarantine/        Items that scored above the quarantine threshold
      <item-id>/
        content.raw
        metadata.json
        scan-result.json   Scan verdict with matched rules and score
    audit/             Append-only JSONL audit logs
      audit.jsonl      One JSON line per scanned item
    failed/            Items that failed structural validation
      <item-id>/       Malformed or incomplete items moved here
  agents/
    messaging/
      workspace/
        inbox/         Clean items delivered by glovebox
    media/
      workspace/
        inbox/
  shared/              Inter-service communication
    glovebox-notifications/
```

| Directory | Purpose |
|-----------|---------|
| `staging/` | Connectors write item subdirectories here using an atomic rename protocol. Glovebox watches this directory and picks up items for scanning. Items are removed from staging after processing. |
| `quarantine/` | Items that scored at or above the `quarantine_threshold` are moved here with their scan result. A human operator reviews these and either approves (moves to agent inbox) or discards them. |
| `audit/` | Append-only JSONL log of every scan verdict. Each line contains the item ID, source, score, matched rules, final verdict (PASS/QUARANTINE/REJECT), and timestamp. This directory should be append-only at the filesystem level. |
| `failed/` | Items that failed validation -- missing `content.raw`, malformed `metadata.json`, unauthorized connector, or other structural problems. Inspect these to diagnose connector bugs. |
| `agents/<name>/workspace/inbox/` | The delivery destination for clean items. Each agent has its own workspace. Glovebox only delivers to agents on the `agent_allowlist`. |
| `shared/` | Used for notification placeholders and inter-service coordination. |

---

## 7. Health Endpoints and Monitoring

### 7.1 Glovebox

Glovebox exposes a single HTTP server on the `metrics_port` (default `9090`):

| Endpoint | Description |
|----------|-------------|
| `/metrics` | Prometheus metrics endpoint (OpenTelemetry format). |

Glovebox does not currently expose dedicated `/healthz` or `/readyz` endpoints.
In Docker Compose, use the `/metrics` endpoint for health checks. In
Kubernetes, use a TCP liveness probe on the metrics port or check `/metrics`
with an HTTP probe.

### 7.2 Connectors

Every connector built with the connector framework exposes an HTTP server on
port `8080` (configurable via `HealthPort` in the connector options):

| Endpoint | Description |
|----------|-------------|
| `/healthz` | Liveness probe. Returns `200 OK` as soon as the HTTP server starts. If this fails, the process is dead and should be restarted. |
| `/readyz` | Readiness probe. Returns `503 Service Unavailable` until the connector has completed at least one successful poll cycle, then `200 OK`. Use this to determine when the connector is actively producing data. |
| `/metrics` | Prometheus metrics endpoint for connector-specific metrics. |

### 7.3 Key Metrics

**Glovebox metrics** (exposed on port 9090):

| Metric | Type | Description |
|--------|------|-------------|
| `glovebox_items_processed_total` | Counter | Total items scanned, labeled by verdict (pass, quarantine, reject). |
| `glovebox_processing_duration_seconds` | Histogram | Time spent scanning each item. |
| `glovebox_signals_triggered_total` | Counter | Total rule matches across all items. |
| `glovebox_staging_queue_depth` | Gauge | Number of items currently waiting in the staging directory. |
| `glovebox_quarantine_queue_depth` | Gauge | Number of items currently in quarantine. |
| `glovebox_pending_items` | Gauge | Number of items currently being scanned (in-flight). |
| `glovebox_scan_workers_busy` | Gauge | Number of scan workers currently occupied. |
| `glovebox_scan_timeouts_total` | Counter | Number of items quarantined due to scan timeout. |
| `glovebox_audit_failures_total` | Counter | Number of failed audit log writes. |
| `glovebox_failed_items` | Gauge | Number of items in the failed directory. |

**Connector metrics** (exposed on port 8080):

| Metric | Type | Description |
|--------|------|-------------|
| `connector_polls_total` | Counter | Total number of poll cycles executed. |
| `connector_items_produced_total` | Counter | Total items written to staging. |
| `connector_poll_duration_seconds` | Histogram | Time spent per poll cycle. |
| `connector_errors_total` | Counter | Total errors encountered during polling. |
| `connector_checkpoint_age_seconds` | Gauge | Time since the last checkpoint was saved. |
| `connector_items_dropped_total` | Counter | Items dropped (e.g., duplicates, filtered out). |

### 7.4 Recommended Alerts

| Alert | Condition | Severity |
|-------|-----------|----------|
| Staging queue backing up | `glovebox_staging_queue_depth > 50` for 5 minutes | Warning |
| Quarantine growing | `glovebox_quarantine_queue_depth > 20` | Warning (needs human review) |
| Scan timeouts | `rate(glovebox_scan_timeouts_total[5m]) > 0` | Warning |
| Audit log failures | `glovebox_audit_failures_total > 0` | Critical |
| All workers busy | `glovebox_scan_workers_busy == <scan_workers>` for 5 minutes | Warning |
| Connector not polling | `rate(connector_polls_total[30m]) == 0` | Critical |
| Connector errors | `rate(connector_errors_total[5m]) > 0.1` | Warning |
| Connector checkpoint stale | `connector_checkpoint_age_seconds > 3600` | Warning |

### 7.5 Prometheus Scrape Config

```yaml
scrape_configs:
  - job_name: glovebox
    static_configs:
      - targets: ["glovebox:9090"]
  - job_name: glovebox-connectors
    static_configs:
      - targets:
          - "rss-connector:8080"
          - "imap-connector:8080"
```

---

## 8. Troubleshooting

### 8.1 Items Stuck in Staging

**Symptom:** `glovebox_staging_queue_depth` is growing and items are not being
processed.

**Possible causes:**

1. **Glovebox is not running.** Check `docker compose ps` or
   `systemctl status glovebox`. Inspect logs for startup errors.

2. **Watch mode mismatch.** If staging is on NFS, `fsnotify` will not detect
   new files. Set `watch_mode` to `"poll"` in the glovebox config. The
   `poll_interval_seconds` fallback should still catch items, but with a delay
   equal to the poll interval.

3. **All scan workers are busy.** Check `glovebox_scan_workers_busy`. If it
   equals `scan_workers`, the worker pool is saturated. Increase `scan_workers`
   or investigate why individual scans are slow (check
   `glovebox_processing_duration_seconds`).

4. **Scan timeout loop.** If content is consistently timing out, it will be
   quarantined rather than stuck. But if the timeout mechanism itself has an
   issue, check the glovebox logs for error messages.

### 8.2 Items in Quarantine

**Symptom:** Items are accumulating in the quarantine directory.

**To inspect quarantined items:**

```sh
# List quarantined items
ls /data/glovebox/quarantine/

# View the scan result for a specific item
cat /data/glovebox/quarantine/<item-id>/scan-result.json

# View the original content
cat /data/glovebox/quarantine/<item-id>/content.raw

# View the item metadata (source, connector, timestamp)
cat /data/glovebox/quarantine/<item-id>/metadata.json
```

The `scan-result.json` file shows which rules matched and the total score. Use
this to determine if the quarantine was a true positive (actual prompt injection
attempt) or a false positive.

**To release a false positive:** Move the item directory to the appropriate
agent inbox:

```sh
mv /data/glovebox/quarantine/<item-id> \
   /data/agents/messaging/workspace/inbox/<item-id>
```

**To tune rules:** If you see repeated false positives from a specific rule,
lower its `weight` in the rules file or raise the `quarantine_threshold`. If
you see repeated true positives slipping through, increase the relevant rule
weight or lower the threshold.

### 8.3 Items in Failed

**Symptom:** Items are accumulating in the failed directory.

**Possible causes:**

1. **Connector bug.** The connector is writing malformed items (missing
   `content.raw` or `metadata.json`). Inspect the failed item directory and
   compare against the staging item schema.

2. **Invalid destination.** The item's route resolves to an agent name that is
   not in the `agent_allowlist`. Add the agent name to the allowlist or fix the
   connector's route configuration.

3. **Filesystem permissions.** Glovebox cannot write to the agent workspace
   directory. Check permissions on the agents directory.

### 8.4 Inspecting Audit Logs

The audit log is an append-only JSONL file at `<audit_dir>/audit.jsonl`. Each
line is a JSON object with the scan verdict for one item.

```sh
# View the last 10 audit entries
tail -10 /data/glovebox/audit/audit.jsonl

# Search for quarantined items
grep '"verdict":"QUARANTINE"' /data/glovebox/audit/audit.jsonl

# Search by source connector
grep '"connector":"imap"' /data/glovebox/audit/audit.jsonl

# Count verdicts
grep -c '"verdict":"PASS"' /data/glovebox/audit/audit.jsonl
grep -c '"verdict":"QUARANTINE"' /data/glovebox/audit/audit.jsonl
grep -c '"verdict":"REJECT"' /data/glovebox/audit/audit.jsonl
```

### 8.5 Connector Not Producing Items

**Symptom:** `connector_polls_total` is incrementing but
`connector_items_produced_total` is not.

1. **No new content.** The external source has no new items since the last
   checkpoint. This is normal for low-traffic feeds.

2. **Checkpoint is ahead of reality.** If you reset the external source (e.g.,
   re-imported an IMAP mailbox), the checkpoint may be ahead. Delete the
   checkpoint file in the connector's state directory and restart the connector.

3. **Authentication failure.** Check connector logs for authentication errors.
   For IMAP, verify that `IMAP_HOST`, `IMAP_USERNAME`, and `IMAP_PASSWORD` are
   correct. For app-specific passwords (Gmail, etc.), generate a new one if the
   old one has been revoked.

4. **Readiness probe failing.** If `/readyz` returns 503, the connector has not
   completed a successful poll. Check the connector logs for the root cause.
