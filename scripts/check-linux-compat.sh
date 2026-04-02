#!/usr/bin/env bash
# input: target Linux binary path plus local inspection tools such as file/readelf/ldd when available
# output: compatibility inspection logs and non-zero exit when a Linux binary carries dynamic libc dependencies
# pos: release guardrail ensuring distributed Linux binaries stay portable across older glibc environments
# note: if this file changes, update this header and module README.md.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 /path/to/linux-binary" >&2
  exit 1
fi

BIN_PATH="$1"

if [[ ! -f "$BIN_PATH" ]]; then
  echo "[compat] error: binary not found: $BIN_PATH" >&2
  exit 1
fi

if ! command -v file >/dev/null 2>&1; then
  echo "[compat] error: missing required tool: file" >&2
  exit 1
fi

file_output="$(file "$BIN_PATH")"
echo "[compat] file: $file_output"

if [[ "$file_output" != *"ELF"* ]]; then
  echo "[compat] error: expected a Linux ELF binary" >&2
  exit 1
fi

if [[ "$file_output" != *"statically linked"* ]]; then
  echo "[compat] error: binary is not statically linked; this risks newer glibc requirements" >&2
  exit 1
fi

if command -v readelf >/dev/null 2>&1; then
  dynamic_output="$(readelf -d "$BIN_PATH" 2>&1 || true)"
  if grep -q '(NEEDED)' <<<"$dynamic_output"; then
    echo "[compat] error: dynamic shared-library dependency detected" >&2
    echo "$dynamic_output" >&2
    exit 1
  fi
  echo "[compat] readelf: no NEEDED shared-library entries"
else
  echo "[compat] readelf unavailable; skipped dynamic section inspection"
fi

if command -v ldd >/dev/null 2>&1; then
  ldd_output="$(ldd "$BIN_PATH" 2>&1 || true)"
  echo "[compat] ldd: $ldd_output"
  if [[ "$ldd_output" != *"not a dynamic executable"* && "$ldd_output" != *"statically linked"* ]]; then
    echo "[compat] error: ldd indicates dynamic linkage" >&2
    exit 1
  fi
else
  echo "[compat] ldd unavailable; skipped runtime linker inspection"
fi

echo "[compat] OK: binary is suitable for older glibc hosts because it has no dynamic libc dependency"
