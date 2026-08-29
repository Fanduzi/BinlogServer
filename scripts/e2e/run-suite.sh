#!/usr/bin/env bash
# input: local tooling, canonical E2E database topology, scenarios, and profile selection
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$ROOT_DIR/scripts/e2e/lib-migration.sh"
API="http://127.0.0.1:18080"
PROFILE="quick"
SCENARIOS_RAW=""
KEEP_ENV=0
SERVER_PID=""
SERVER_LOG="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-suite.log}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-suite-$(date +%s)}"
META_DSN=""

usage() {
  cat <<'EOF'
Usage:
  ./scripts/e2e/run-suite.sh [--profile quick|full] [--scenarios a,b,c] [--keep-env]

Profiles:
  quick  -> smoke,compression
  full   -> smoke,compression,orchestrator,semisync,meta-failover

Options:
  --profile <name>     选择预设场景集（默认 quick）
  --scenarios <list>   逗号分隔，自定义场景（覆盖 profile）
  --keep-env           结束后不自动 down，便于排障
  -h, --help           显示帮助

Scenarios:
  smoke
  compression
  orchestrator
  semisync
  meta-failover
  meta-failover-override
  smoke-observability
  smoke-cluster-roles
  smoke-control-plane-failover
  smoke-worker-crash-recovery
  smoke-invalid-inputs
  smoke-retry-upload
  smoke-scale
EOF
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --profile)
        PROFILE="${2:-}"
        shift 2
        ;;
      --scenarios)
        SCENARIOS_RAW="${2:-}"
        shift 2
        ;;
      --keep-env)
        KEEP_ENV=1
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        echo "unknown arg: $1" >&2
        usage
        exit 1
        ;;
    esac
  done
}

build_scenarios() {
  if [[ -n "$SCENARIOS_RAW" ]]; then
    IFS=',' read -r -a scenarios <<<"$SCENARIOS_RAW"
    echo "${scenarios[@]}"
    return
  fi

  case "$PROFILE" in
    quick)
      echo "smoke compression"
      ;;
    full)
      echo "smoke compression orchestrator semisync meta-failover"
      ;;
    *)
      echo "unsupported profile: $PROFILE" >&2
      exit 1
      ;;
  esac
}

wait_server_ready() {
  for i in {1..120}; do
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      return 0
    fi
    if [[ $i -eq 120 ]]; then
      echo "binlog-server not ready in time" >&2
      cat "$SERVER_LOG" >&2 || true
      return 1
    fi
    sleep 1
  done
  return 1
}

start_suite_server() {
  if [[ -n "$SERVER_PID" ]]; then
    return 0
  fi
  BINLOG_SERVER_DATA_DIR="$DATA_DIR" BINLOG_SERVER_META_DSN="$META_DSN" nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$SERVER_LOG" 2>&1 &
  SERVER_PID=$!
  wait_server_ready
  echo "[suite] binlog-server ready pid=$SERVER_PID"
}

stop_suite_server() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" >/dev/null 2>&1 || true
    SERVER_PID=""
  fi
}

is_self_managed_scenario() {
  local name="$1"
  case "$name" in
    smoke-cluster-roles|smoke-control-plane-failover|smoke-worker-crash-recovery|smoke-retry-upload)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

run_scenario() {
  local name="$1"
  case "$name" in
    smoke)
      "$ROOT_DIR/scripts/e2e/smoke.sh"
      ;;
    compression)
      E2E_DATA_DIR="$DATA_DIR" "$ROOT_DIR/scripts/e2e/smoke-compression.sh"
      ;;
    orchestrator)
      "$ROOT_DIR/scripts/e2e/smoke-orchestrator.sh"
      ;;
    semisync)
      "$ROOT_DIR/scripts/e2e/smoke-semisync.sh"
      ;;
    meta-failover)
      "$ROOT_DIR/scripts/e2e/smoke-meta-failover.sh"
      ;;
    meta-failover-override)
      "$ROOT_DIR/scripts/e2e/smoke-meta-failover-override.sh"
      ;;
    smoke-observability)
      "$ROOT_DIR/scripts/e2e/smoke-observability.sh"
      ;;
    smoke-cluster-roles)
      E2E_DATA_DIR="$DATA_DIR" "$ROOT_DIR/scripts/e2e/smoke-cluster-roles.sh"
      ;;
    smoke-control-plane-failover)
      E2E_DATA_DIR="$DATA_DIR" "$ROOT_DIR/scripts/e2e/smoke-control-plane-failover.sh"
      ;;
    smoke-worker-crash-recovery)
      E2E_DATA_DIR="$DATA_DIR" "$ROOT_DIR/scripts/e2e/smoke-worker-crash-recovery.sh"
      ;;
    smoke-invalid-inputs)
      "$ROOT_DIR/scripts/e2e/smoke-invalid-inputs.sh"
      ;;
    smoke-retry-upload)
      E2E_DATA_DIR="$DATA_DIR" "$ROOT_DIR/scripts/e2e/smoke-retry-upload.sh"
      ;;
    smoke-scale)
      E2E_DATA_DIR="$DATA_DIR" E2E_SERVER_PID="$SERVER_PID" E2E_SERVER_LOG="$SERVER_LOG" "$ROOT_DIR/scripts/e2e/smoke-scale.sh"
      ;;
    *)
      echo "unsupported scenario: $name" >&2
      return 1
      ;;
  esac
}

has_scenario() {
  local target="$1"
  shift
  local item
  for item in "$@"; do
    if [[ "$item" == "$target" ]]; then
      return 0
    fi
  done
  return 1
}

cleanup() {
  stop_suite_server
  if [[ "$KEEP_ENV" -eq 0 ]]; then
    "$ROOT_DIR/scripts/e2e/down.sh" >/dev/null 2>&1 || true
  fi
}

main() {
  parse_args "$@"

  need_cmd curl
  need_cmd docker
  need_cmd jq
  need_cmd go

  scenarios=()
  while IFS= read -r s; do
    scenarios+=("$s")
  done < <(build_scenarios | tr ' ' '\n' | sed '/^$/d')
  if [[ "${#scenarios[@]}" -eq 0 ]]; then
    echo "no scenarios selected" >&2
    exit 1
  fi

  trap cleanup EXIT

  echo "[suite] profile=$PROFILE scenarios=${scenarios[*]}"
  echo "[suite] data_dir=$DATA_DIR"

  "$ROOT_DIR/scripts/e2e/down.sh" >/dev/null 2>&1 || true
  "$ROOT_DIR/scripts/e2e/up.sh"

  if has_scenario "meta-failover" "${scenarios[@]}" || has_scenario "meta-failover-override" "${scenarios[@]}"; then
    META_DSN="$(e2e_meta_dsn failover)"
    "$ROOT_DIR/scripts/e2e/setup-meta-replication.sh"
  elif [[ -z "$META_DSN" ]]; then
    META_DSN="$(e2e_meta_dsn direct)"
  fi
  e2e_ensure_meta_schema "$ROOT_DIR" "$META_DSN"

  mkdir -p "$DATA_DIR"

  for s in "${scenarios[@]}"; do
    if is_self_managed_scenario "$s"; then
      stop_suite_server
    else
      start_suite_server
    fi
    echo "[suite] scenario=$s"
    run_scenario "$s"
    echo "[suite] scenario=$s done"
  done

  echo "[suite] all scenarios passed"
}

main "$@"
