#!/usr/bin/env bash
# Runs the record journal's database tests against the PostgreSQL production
# runs: the CloudNativePG image at the same major.minor as the databases-app
# Cluster (17.10 on 2026-10-16; bump JOURNAL_PG_IMAGE with the cluster).
# The image has no Docker entrypoint, so the container runs initdb itself.
#
# Usage: scripts/journal-test-db.sh [go test package patterns...]
#        (default ./internal/journal/...)
set -euo pipefail

IMAGE="${JOURNAL_PG_IMAGE:-ghcr.io/cloudnative-pg/postgresql:17.10}"
ENGINE="${CONTAINER_ENGINE:-$(command -v podman || command -v docker || true)}"
PORT="${JOURNAL_PG_PORT:-}"
NAME="glovebox-journal-pg-$$"

if [ -z "$ENGINE" ]; then
  echo "need podman or docker" >&2
  exit 2
fi

# port_free is true when nothing accepts connections on 127.0.0.1:$1.
port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
if [ -z "$PORT" ]; then
  for p in $(seq 55432 55532); do
    if port_free "$p"; then PORT="$p"; break; fi
  done
  if [ -z "$PORT" ]; then
    echo "no free port in 55432-55532; set JOURNAL_PG_PORT" >&2
    exit 2
  fi
elif ! port_free "$PORT"; then
  echo "JOURNAL_PG_PORT=$PORT is already in use" >&2
  exit 2
fi

# The trap is set before the container starts, so a failure at any later
# point still removes it.
trap '"$ENGINE" rm -f "$NAME" >/dev/null 2>&1 || true' EXIT

# JOURNAL_PG_HOST_NETWORK=1 runs the container on the host network instead of
# publishing a port (for sandboxes that do not route published ports).
if [ "${JOURNAL_PG_HOST_NETWORK:-}" = 1 ]; then
  NET=(--network host); PGOPTS="-p ${PORT} -c listen_addresses=127.0.0.1"
else
  NET=(-p "127.0.0.1:${PORT}:5432"); PGOPTS="-c listen_addresses=*"
fi
"$ENGINE" run -d --rm --name "$NAME" "${NET[@]}" "$IMAGE" \
  /bin/sh -c "initdb -D /tmp/pg -U postgres --auth=trust >/dev/null && exec postgres -D /tmp/pg ${PGOPTS} -c fsync=off" >/dev/null

export JOURNAL_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:${PORT}/postgres?sslmode=disable"
export JOURNAL_TEST_REQUIRE_DB=1
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.27.1}"
if [ "$#" -eq 0 ]; then
  set -- ./internal/journal/...
fi
go test -count=1 -race "$@"
