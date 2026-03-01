#!/usr/bin/env bash
# input: local tooling (docker/curl/jq/go) and e2e environment/service dependencies
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module AGENTS.md.

e2e_default_meta_dsn() {
  local port="${1:-13306}"
  local host="${E2E_META_HOST:-127.0.0.1}"
  local user="${E2E_META_USER:-meta}"
  local pass="${E2E_META_PASS:-metapass}"
  local db="${E2E_META_DB:-binlog_meta}"
  printf '%s:%s@tcp(%s:%s)/%s?parseTime=true' "$user" "$pass" "$host" "$port" "$db"
}

e2e_mask_dsn() {
  local dsn="$1"
  # 掩码格式示例: user:***@tcp(host:port)/db?...
  printf '%s' "$dsn" | sed -E 's#^([^:@/]+):[^@]*@#\1:***@#'
}

e2e_ensure_meta_schema() {
  local root_dir="$1"
  local dsn="${2:-$(e2e_default_meta_dsn 13306)}"

  command -v go >/dev/null 2>&1 || { echo "missing command: go" >&2; exit 1; }
  echo "[e2e] migrate meta schema dsn=$(e2e_mask_dsn "$dsn")"
  (
    cd "$root_dir"
    META_DSN="$dsn" go run ./cmd/migrate up
  )
}
