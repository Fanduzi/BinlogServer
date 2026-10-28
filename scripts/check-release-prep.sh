#!/usr/bin/env bash
# input: a release version (vX.Y.Z) and an optional git ref (default: the working tree)
# output: one line per check from cmd/release-check (internal/releaseprep.Main); exit 1 when a release surface is missing or stale, 2 on a usage, read or build error
# pos: entry point for `make release-check`; release.yml runs the same command in its release-prep job
# note: if this file changes, update this header and scripts/README.md. The checks live in internal/releaseprep.
set -euo pipefail
cd "$(dirname "$0")/.."
# Build, then run: `go run` turns every non-zero exit into 1, and the 2 for a
# usage or read error must reach release.yml as 2.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
go build -o "$tmp/release-check" ./cmd/release-check || exit 2
"$tmp/release-check" "$@" && rc=0 || rc=$?
exit "$rc"
