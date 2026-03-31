# Starting Glovebox Interactively

*2026-03-31T07:09:34Z by Showboat 0.6.1*
<!-- showboat-id: c17b44b2-c3ff-4f3d-b3ee-5fd0e4992a80 -->

This demo shows how to build and run glovebox from source. Glovebox is a content scanning service that watches a staging directory, scans incoming content for prompt injection attacks, and routes clean items to agent workspaces.

First, build the glovebox binary:

```bash
go build -o /tmp/demo-glovebox .
```

```output
```

Create the directory structure glovebox needs:

```bash
mkdir -p /tmp/demo/{staging,quarantine,audit,failed,agents/messaging/workspace/inbox,shared/glovebox-notifications}
```

```output
```

Create a config file pointing to our demo directories:

```bash
cat > /tmp/demo/config.json << 'CONF'
{
  "staging_dir": "/tmp/demo/staging",
  "quarantine_dir": "/tmp/demo/quarantine",
  "audit_dir": "/tmp/demo/audit",
  "failed_dir": "/tmp/demo/failed",
  "agents_dir": "/tmp/demo/agents",
  "shared_dir": "/tmp/demo/shared",
  "agent_allowlist": ["messaging"],
  "metrics_port": 19091,
  "watch_mode": "fsnotify",
  "poll_interval_seconds": 2,
  "rules_file": "/tmp/demo/rules.json",
  "scan_workers": 2,
  "scan_timeout_seconds": 10,
  "scan_chunk_size_bytes": 262144
}
CONF
echo "config.json created"
```

```output
config.json created
```

Use the default scanning rules:

```bash
cp configs/default-rules.json /tmp/demo/rules.json && echo 'rules.json copied'
```

```output
rules.json copied
```

Start glovebox in the background and verify it is running:

```bash
/tmp/demo-glovebox --config /tmp/demo/config.json &>/tmp/demo/glovebox.log & sleep 1 && curl -s http://localhost:19091/metrics | head -3
```

```output
# HELP go_gc_duration_seconds A summary of the wall-time pause (stop-the-world) duration in garbage collection cycles.
# TYPE go_gc_duration_seconds summary
go_gc_duration_seconds{quantile="0"} 0
```

Manually stage a test item to see glovebox scan it:

```bash
ITEM=/tmp/demo/staging/test-item-001
mkdir -p "$ITEM"
echo "Hello from the test item" > "$ITEM/content.raw"
cat > "$ITEM/metadata.json" << 'META'
{
  "source": "manual",
  "sender": "demo",
  "subject": "Test item",
  "timestamp": "2026-03-30T12:00:00Z",
  "destination_agent": "messaging",
  "content_type": "text/plain",
  "ordered": false,
  "auth_failure": false
}
META
sleep 3
echo "Delivered items:"
ls /tmp/demo/agents/messaging/workspace/inbox/ 2>/dev/null | head -5
echo ""
echo "Audit log:"
cat /tmp/demo/audit/pass.jsonl 2>/dev/null | tail -1 | python3 -m json.tool 2>/dev/null | head -10
```

```output
Delivered items:
test-item-001

Audit log:
{
    "timestamp": "2026-03-31T07:14:50Z",
    "source": "manual",
    "sender": "demo",
    "content_hash": "607dd2566f1c2f99c236a7d24f73491f78319b2b5824c61220887bb80e152a85",
    "content_length": 25,
    "signals": null,
    "total_score": 0,
    "verdict": "pass",
    "destination": "messaging",
```

Clean up:

```bash
kill %1 2>/dev/null; rm -rf /tmp/demo /tmp/demo-glovebox; echo 'cleaned up'
```

```output
cleaned up
```
