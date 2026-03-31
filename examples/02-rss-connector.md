# Configuring Glovebox with an RSS Connector

*2026-03-31T07:16:21Z by Showboat 0.6.1*
<!-- showboat-id: 23ba4eb9-cff8-4afc-9ca4-eb67f22b4f97 -->

This demo shows how to run glovebox with the RSS connector scanning a real RSS feed. The connector polls the feed, stages entries, and glovebox scans them for prompt injections before delivering to an agent workspace.

Build both binaries:

```bash
go build -o /tmp/demo-glovebox . && go build -o /tmp/demo-rss ./connectors/rss/ && echo 'built glovebox + rss-connector'
```

```output
built glovebox + rss-connector
```

Create the directory structure:

```bash
mkdir -p /tmp/demo/{staging,staging-tmp,quarantine,audit,failed,agents/media/workspace/inbox,shared/glovebox-notifications,rss-state}
```

```output
```

Create the glovebox config:

```bash
cat > /tmp/demo/glovebox-config.json << 'CONF'
{
  "staging_dir": "/tmp/demo/staging",
  "quarantine_dir": "/tmp/demo/quarantine",
  "audit_dir": "/tmp/demo/audit",
  "failed_dir": "/tmp/demo/failed",
  "agents_dir": "/tmp/demo/agents",
  "shared_dir": "/tmp/demo/shared",
  "agent_allowlist": ["media"],
  "metrics_port": 19092,
  "watch_mode": "fsnotify",
  "poll_interval_seconds": 2,
  "rules_file": "/tmp/demo/rules.json",
  "scan_workers": 2,
  "scan_timeout_seconds": 10,
  "scan_chunk_size_bytes": 262144
}
CONF
cp configs/default-rules.json /tmp/demo/rules.json
echo "glovebox config created"
```

```output
glovebox config created
```

Create the RSS connector config pointing at the Engadget news feed:

```bash
cat > /tmp/demo/rss-config.json << 'CONF'
{
  "rules": [
    {"match": "feed:engadget", "destination": "media", "tags": {"category": "tech-news"}},
    {"match": "*", "destination": "media"}
  ],
  "feeds": [
    {"name": "engadget", "url": "https://www.engadget.com/rss.xml"}
  ],
  "fetch_links": false,
  "link_policy": {"default": "safe"}
}
CONF
echo "rss-config.json created"
```

```output
rss-config.json created
```

Start glovebox, then the RSS connector:

```bash
/tmp/demo-glovebox --config /tmp/demo/glovebox-config.json &>/tmp/demo/glovebox.log &
sleep 1
GLOVEBOX_CONNECTOR_CONFIG=/tmp/demo/rss-config.json \
GLOVEBOX_STAGING_DIR=/tmp/demo/staging \
GLOVEBOX_STATE_DIR=/tmp/demo/rss-state \
/tmp/demo-rss &>/tmp/demo/rss.log &
sleep 8
echo "RSS connector log:"
grep -E "starting|poll|ready" /tmp/demo/rss.log
echo ""
DELIVERED=$(ls /tmp/demo/agents/media/workspace/inbox/ 2>/dev/null | wc -l)
QUARANTINED=$(ls /tmp/demo/quarantine/ 2>/dev/null | wc -l)
echo "Delivered: $DELIVERED items"
echo "Quarantined: $QUARANTINED items"
```

```output
RSS connector log:
2026/03/31 00:16:30 INFO starting connector connector=rss
2026/03/31 00:16:30 INFO running initial poll connector=rss
2026/03/31 00:16:30 ERROR health server connector=rss error="listen tcp :8080: bind: address already in use"
2026/03/31 00:16:31 INFO initial poll complete, connector ready connector=rss

Delivered: 47 items
Quarantined: 0 items
```

Inspect a delivered item:

```bash
FIRST=$(ls /tmp/demo/agents/media/workspace/inbox/ | head -1)
echo "Subject: $(python3 -c "import json; print(json.load(open(\"/tmp/demo/agents/media/workspace/inbox/$FIRST/metadata.json\"))[\"subject\"])")"
echo ""
echo "Content (first 200 chars):"
head -c 200 "/tmp/demo/agents/media/workspace/inbox/$FIRST/content.raw"
```

```output
Subject: Dispatch is coming to Xbox this summer

Content (first 200 chars):
Dispatch is coming to Xbox this summer

Dispatch was one of 2025â€™s standout titles and one of the best narrative games in years, which made its no-show on Xbox all the more puzzling. Luckily, thatâ€```
```

Check the checkpoint (RSS connector remembers where it left off):

```bash
cat /tmp/demo/rss-state/state.json | python3 -m json.tool
```

```output
{
    "last:engadget": "be8ee9de-e368-42e7-98ef-c453317c9d28"
}
```

Clean up:

```bash
kill %1 %2 2>/dev/null; rm -rf /tmp/demo /tmp/demo-glovebox /tmp/demo-rss; echo 'cleaned up'
```

```output
cleaned up
```
