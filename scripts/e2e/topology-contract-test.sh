#!/usr/bin/env bash
# input: canonical E2E topology module and Compose adapter defaults
# output: fast contract assertions for defaults, overrides, topology selection, and validation
# pos: no-Docker regression check for the E2E database-topology boundary
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TOPOLOGY_LIB="$ROOT_DIR/scripts/e2e/lib-topology.sh"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"

fail() {
  echo "[topology-contract] $*" >&2
  exit 1
}

assert_eq() {
  local expected="$1"
  local actual="$2"
  local label="$3"
  [[ "$actual" == "$expected" ]] || fail "$label: expected '$expected', got '$actual'"
}

compose_default() {
  local name="$1"
  local marker="\${${name}:-"
  local line value
  while IFS= read -r line; do
    if [[ "$line" == *"$marker"* ]]; then
      value="${line#*"$marker"}"
      printf '%s' "${value%%\}*}"
      return 0
    fi
  done <"$COMPOSE_FILE"
  return 1
}

topology_vars=(
  E2E_SOURCE_HOST E2E_SOURCE_USER E2E_SOURCE_PASS
  E2E_MYSQL57_PORT E2E_MYSQL80_PORT E2E_PERCONA57_PORT E2E_PERCONA80_PORT
  E2E_META_HOST E2E_META_USER E2E_META_PASS E2E_META_DB
  E2E_META_PRIMARY_PORT E2E_META_REPLICA_PORT
  E2E_META_PROXYSQL_ADMIN_PORT E2E_META_PROXYSQL_PORT E2E_META_DSN
)
for name in "${topology_vars[@]}"; do
  unset "$name"
done
source "$TOPOLOGY_LIB"

assert_eq "127.0.0.1" "$E2E_SOURCE_HOST" "default source host"
assert_eq "repl" "$E2E_SOURCE_USER" "default source user"
assert_eq "replpass" "$E2E_SOURCE_PASS" "default source password"
assert_eq "meta:metapass@tcp(127.0.0.1:13316)/binlog_meta?parseTime=true" "$(e2e_meta_dsn direct)" "direct metadata DSN"
assert_eq "meta:metapass@tcp(127.0.0.1:16036)/binlog_meta?parseTime=true" "$(e2e_meta_dsn failover)" "failover metadata DSN"

port_vars=(
  E2E_MYSQL57_PORT E2E_MYSQL80_PORT E2E_PERCONA57_PORT E2E_PERCONA80_PORT
  E2E_META_PRIMARY_PORT E2E_META_REPLICA_PORT
  E2E_META_PROXYSQL_ADMIN_PORT E2E_META_PROXYSQL_PORT
)
for name in "${port_vars[@]}"; do
  assert_eq "${!name}" "$(compose_default "$name")" "$name Compose fallback"
done

override_dsn="$(
  E2E_META_HOST=db.example E2E_META_USER=user E2E_META_PASS=pass E2E_META_DB=custom \
  E2E_META_PRIMARY_PORT=23016 bash -c 'source "$1"; e2e_meta_dsn direct' _ "$TOPOLOGY_LIB"
)"
assert_eq "user:pass@tcp(db.example:23016)/custom?parseTime=true" "$override_dsn" "component override"

full_dsn="custom:secret@tcp(metadata.example:3306)/custom?parseTime=true"
actual_dsn="$(E2E_META_DSN="$full_dsn" bash -c 'source "$1"; e2e_meta_dsn failover' _ "$TOPOLOGY_LIB")"
assert_eq "$full_dsn" "$actual_dsn" "full DSN precedence"

for bad_port in 0 65536 abc; do
  if E2E_MYSQL57_PORT="$bad_port" bash -c 'source "$1"' _ "$TOPOLOGY_LIB" >/dev/null 2>&1; then
    fail "invalid port accepted: $bad_port"
  fi
done
if E2E_SOURCE_HOST= bash -c 'source "$1"' _ "$TOPOLOGY_LIB" >/dev/null 2>&1; then
  fail "empty required value accepted"
fi
if bash -c 'source "$1"; e2e_meta_dsn unknown' _ "$TOPOLOGY_LIB" >/dev/null 2>&1; then
  fail "unknown metadata topology accepted"
fi

echo "[topology-contract] PASS"
