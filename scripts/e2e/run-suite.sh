#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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
  smoke-cluster-roles
  smoke-control-plane-failover
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
    smoke-cluster-roles)
      E2E_DATA_DIR="$DATA_DIR" "$ROOT_DIR/scripts/e2e/smoke-cluster-roles.sh"
      ;;
    smoke-control-plane-failover)
      E2E_DATA_DIR="$DATA_DIR" "$ROOT_DIR/scripts/e2e/smoke-control-plane-failover.sh"
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
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" >/dev/null 2>&1 || true
  fi
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
    META_DSN="meta:metapass@tcp(127.0.0.1:16036)/binlog_meta?parseTime=true"
    "$ROOT_DIR/scripts/e2e/setup-meta-replication.sh"
  fi

  mkdir -p "$DATA_DIR"
  if has_scenario "smoke-cluster-roles" "${scenarios[@]}" || has_scenario "smoke-control-plane-failover" "${scenarios[@]}"; then
    if [[ "${#scenarios[@]}" -ne 1 ]]; then
      echo "scenario smoke-cluster-roles/smoke-control-plane-failover must run alone" >&2
      exit 1
    fi
  else
    if [[ -n "$META_DSN" ]]; then
      BINLOG_SERVER_DATA_DIR="$DATA_DIR" BINLOG_SERVER_META_DSN="$META_DSN" nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$SERVER_LOG" 2>&1 &
    else
      BINLOG_SERVER_DATA_DIR="$DATA_DIR" nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$SERVER_LOG" 2>&1 &
    fi
    SERVER_PID=$!
    wait_server_ready
    echo "[suite] binlog-server ready pid=$SERVER_PID"
  fi

  for s in "${scenarios[@]}"; do
    echo "[suite] scenario=$s"
    run_scenario "$s"
    echo "[suite] scenario=$s done"
  done

  echo "[suite] all scenarios passed"
}

main "$@"
