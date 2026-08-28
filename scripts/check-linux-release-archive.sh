#!/usr/bin/env bash
# input: Linux release tar.gz archive path plus local tar/mktemp tooling and the binary compatibility checker script
# output: archive asset checks plus non-zero exit when required binaries/migrations or Linux compatibility are missing
# pos: release artifact guardrail verifying packaged Linux archives are operationally complete and glibc-safe
# note: if this file changes, update this header and module README.md.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 dist/binlog-server_<version>_linux_amd64.tar.gz" >&2
  exit 1
fi

ARCHIVE_PATH="$1"
ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CHECKER="$ROOT_DIR/scripts/check-linux-compat.sh"

if [[ ! -f "$ARCHIVE_PATH" ]]; then
  echo "[compat-archive] error: archive not found: $ARCHIVE_PATH" >&2
  exit 1
fi

if [[ ! -x "$CHECKER" ]]; then
  echo "[compat-archive] error: missing executable checker: $CHECKER" >&2
  exit 1
fi

work_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$work_dir"
}
trap cleanup EXIT

echo "[compat-archive] inspect archive: $ARCHIVE_PATH"
tar -xzf "$ARCHIVE_PATH" -C "$work_dir"

bin_path="$(find "$work_dir" -type f -name binlog-server -print -quit)"
if [[ -z "$bin_path" ]]; then
  echo "[compat-archive] error: binlog-server executable not found in archive" >&2
  exit 1
fi

migrate_path="$(find "$work_dir" -type f -name migrate -print -quit)"
if [[ -z "$migrate_path" ]]; then
  echo "[compat-archive] error: migrate executable not found in archive" >&2
  exit 1
fi
if [[ ! -x "$migrate_path" ]]; then
  echo "[compat-archive] error: packaged migrate is not executable" >&2
  exit 1
fi

up_migration="$(find "$work_dir" -type f -path '*/migrations/*.up.sql' -print -quit)"
if [[ -z "$up_migration" ]]; then
  echo "[compat-archive] error: up migration SQL not found in archive" >&2
  exit 1
fi

down_migration="$(find "$work_dir" -type f -path '*/migrations/*.down.sql' -print -quit)"
if [[ -z "$down_migration" ]]; then
  echo "[compat-archive] error: down migration SQL not found in archive" >&2
  exit 1
fi

echo "[compat-archive] extracted binary: $bin_path"
"$CHECKER" "$bin_path"

echo "[compat-archive] OK: release archive is complete and embeds a glibc-safe Linux binary"
