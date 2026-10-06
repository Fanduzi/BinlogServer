#!/usr/bin/env bash
# input: internal/meta/sqlcgen/*.go after sqlc generate
# output: the same files with an L3 header that sqlc itself does not emit
# pos: post-step of make sqlc-generate so generated files stay checkable
# note: if this file changes, update this header and module README.md.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dir="$root/internal/meta/sqlcgen"
header='// Package sqlcgen is the sqlc-generated metadata query package. Do not edit.
// input: internal/meta/sql/*.sql and the migrations schema
// output: typed query methods for leases, task runs, and worker heartbeats
// pos: generated query layer under internal/meta; refresh with make sqlc-generate
// note: if this file changes, update this header and module README.md.
'

shopt -s nullglob
for f in "$dir"/*.go; do
  if head -n 1 "$f" | grep -q '^// Package sqlcgen '; then
    continue
  fi
  tmp="$(mktemp)"
  printf '%s\n' "$header" | cat - "$f" >"$tmp"
  mv "$tmp" "$f"
done
