#!/usr/bin/env bash
set -euo pipefail
go generate ./internal/sanitizeapi/
if ! git diff --quiet -- internal/sanitizeapi/sanitizeapi.gen.go; then
  echo "ERROR: generated code is stale. Run 'go generate ./...' and commit." >&2
  git --no-pager diff -- internal/sanitizeapi/sanitizeapi.gen.go >&2
  exit 1
fi
echo "codegen up to date."
