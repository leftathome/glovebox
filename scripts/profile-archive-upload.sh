#!/usr/bin/env bash
# g499: profile glovebox archive PATCH peak memory vs upload size.
# Streams a $SIZE-byte upload against a containerized glovebox with
# GODEBUG=gctrace=1; reports peak Go heap (from gctrace) + cgroup memory.peak.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

SIZE="${SIZE:-12884901888}"
IMAGE="glovebox-archive:smoke"
NAME="gbox-prof-$$"
TOKEN="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
MP=19090; IP=9091
TMP="$(mktemp -d)"; DATA="$TMP/data"; ARCHIVE="$TMP/a.mbox"
cleanup(){ docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$TMP"; }
trap cleanup EXIT

docker build -q -t "$IMAGE" -f Dockerfile . >/dev/null
mkdir -p "$DATA"/glovebox/{staging,quarantine,audit,failed,archives,.tmp-archives} "$DATA"/agents "$DATA"/shared
chmod -R 777 "$DATA"
cat > "$DATA/config.json" <<EOF
{ "metrics_port": $MP, "staging_dir": "/data/glovebox/staging",
  "quarantine_dir": "/data/glovebox/quarantine", "audit_dir": "/data/glovebox/audit",
  "failed_dir": "/data/glovebox/failed", "agents_dir": "/data/agents", "shared_dir": "/data/shared",
  "rules_file": "/etc/glovebox/rules.json",
  "ingest": { "enabled": true, "auth": { "enabled": true, "source": "env" },
              "archives": { "enabled": true, "staging_root": "/data/glovebox" } } }
EOF
truncate -s "$SIZE" "$ARCHIVE"
SHA="$(sha256sum "$ARCHIVE" | awk '{print $1}')"

docker run -d --name "$NAME" --network=host -v "$DATA":/data \
  -e GLOVEBOX_INGEST_TOKEN="$TOKEN" -e GLOVEBOX_INGEST_SOURCE_ID=recognizer \
  -e GODEBUG=gctrace=1 "$IMAGE" --config /data/config.json >/dev/null
CID="$(docker inspect -f '{{.Id}}' "$NAME")"
for i in $(seq 1 30); do curl -sf "http://127.0.0.1:$MP/metrics" >/dev/null 2>&1 && break; sleep 1; done

# Locate the container's cgroup v2 dir (podman/libpod layout).
CGDIR=""
for d in "/sys/fs/cgroup/machine.slice/libpod-$CID.scope/container" \
         "/sys/fs/cgroup/machine.slice/libpod-$CID.scope" \
         "/sys/fs/cgroup/system.slice/docker-$CID.scope"; do
  [ -r "$d/memory.current" ] && { CGDIR="$d"; break; }
done
# Sample anon (heap+runtime) vs file (page cache) every 0.4s during the upload.
SMAX="$TMP/smax"; : > "$SMAX"
( anonmax=0; filemax=0; curmax=0
  while :; do
    cur=$(cat "$CGDIR/memory.current" 2>/dev/null || echo 0)
    anon=$(awk '/^anon /{print $2}' "$CGDIR/memory.stat" 2>/dev/null || echo 0)
    file=$(awk '/^file /{print $2}' "$CGDIR/memory.stat" 2>/dev/null || echo 0)
    [ "${cur:-0}" -gt "$curmax" ] && curmax=$cur
    [ "${anon:-0}" -gt "$anonmax" ] && anonmax=$anon
    [ "${file:-0}" -gt "$filemax" ] && filemax=$file
    echo "$curmax $anonmax $filemax" > "$SMAX"
    sleep 0.4
  done ) &
SAMPLER=$!

b64(){ printf '%s' "$1" | base64 -w0; }
META="archive_id $(b64 "prof-$$"),archive_filename $(b64 a.mbox),subtree_relative_path $(b64 .),media_type $(b64 archive/mbox),matcher_id $(b64 prof/mbox),provider $(b64 prof),sha256 $(b64 "$SHA"),size_bytes $(b64 "$SIZE")"
LOC="$(curl -sS -D - -o /dev/null -X POST "http://127.0.0.1:$IP/v1/archives" \
  -H "Authorization: Bearer $TOKEN" -H "Tus-Resumable: 1.0.0" \
  -H "Upload-Length: $SIZE" -H "Upload-Metadata: $META" | grep -i '^location:' | tr -d '\r' | awk '{print $2}')"
T0=$(date +%s)
CODE="$(curl -sS -o /dev/null -w '%{http_code}' -X PATCH "http://127.0.0.1:$IP$LOC" \
  -H "Authorization: Bearer $TOKEN" -H "Tus-Resumable: 1.0.0" -H "Upload-Offset: 0" \
  -H "Content-Type: application/offset+octet-stream" -H "Expect:" --upload-file "$ARCHIVE")"
T1=$(date +%s)
kill "$SAMPLER" 2>/dev/null || true; wait "$SAMPLER" 2>/dev/null || true
sleep 1  # let a final GC line flush
read CUR_MAX ANON_MAX FILE_MAX < "$SMAX" 2>/dev/null || { CUR_MAX=0; ANON_MAX=0; FILE_MAX=0; }
mib(){ awk "BEGIN{printf \"%.0f\", ${1:-0}/1048576}"; }
PEAK_TOTAL_MIB="$(mib "$CUR_MAX")"; PEAK_ANON_MIB="$(mib "$ANON_MAX")"; PEAK_FILE_MIB="$(mib "$FILE_MAX")"
PEAK_FROM_FILE="NA"; [ -r "$CGDIR/memory.peak" ] && PEAK_FROM_FILE="$(mib "$(cat "$CGDIR/memory.peak")")"

# Peak Go heap from gctrace lines "A->B->C MB": B is heap-in-use at end of mark
# (the cycle peak). Also capture max "N MB goal". Use [-] to dodge grep's flag parse.
LOGF="$TMP/logs.txt"; docker logs "$NAME" > "$LOGF" 2>&1
PEAK_HEAP_MB="$(grep -oE '[0-9]+[-]>[0-9]+[-]>[0-9]+ MB' "$LOGF" | awk -F'[->]+' '{print $2}' | sort -n | tail -1)"
PEAK_GOAL_MB="$(grep -oE '[0-9]+ MB goal' "$LOGF" | awk '{print $1}' | sort -n | tail -1)"
NGC="$(grep -cE '^gc [0-9]+ @' "$LOGF" || true)"

printf 'RESULT size_gib=%.1f http=%s secs=%s ngc=%s go_heap_mb=%s | peak_total_mib=%s peak_anon_mib=%s peak_file_cache_mib=%s mem_peak_mib=%s\n' \
  "$(awk "BEGIN{print $SIZE/1073741824}")" "$CODE" "$((T1-T0))" "${NGC:-0}" "${PEAK_HEAP_MB:-NA}" \
  "$PEAK_TOTAL_MIB" "$PEAK_ANON_MIB" "$PEAK_FILE_MIB" "$PEAK_FROM_FILE"
