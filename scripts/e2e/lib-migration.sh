#!/usr/bin/env bash
# input: canonical E2E database topology, metadata DSN, and local Go tooling
# output: metadata DSN masking and schema-migration helpers
# pos: shared metadata migration adapter for E2E orchestration and standalone scenarios
# note: if this file changes, update this header and module README.md.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-topology.sh"

e2e_mask_dsn() {
  local dsn="$1"
  # 掩码格式示例: user:***@tcp(host:port)/db?...
  printf '%s' "$dsn" | sed -E 's#^([^:@/]+):[^@]*@#\1:***@#'
}

e2e_ensure_meta_schema() {
  local root_dir="$1"
  local dsn="${2:-$(e2e_meta_dsn direct)}"

  command -v go >/dev/null 2>&1 || { echo "missing command: go" >&2; exit 1; }
  echo "[e2e] migrate meta schema dsn=$(e2e_mask_dsn "$dsn")"
  (
    cd "$root_dir"
    META_DSN="$dsn" go run ./cmd/migrate up
  )
}
