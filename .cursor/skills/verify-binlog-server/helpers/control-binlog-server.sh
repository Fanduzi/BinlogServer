#!/usr/bin/env bash
# input: BINLOG_VERIFY_* isolation variables, BINLOG_SERVER_BIN, optional migrate binary and meta DSN
# output: one isolated binlog-server process, pid file, start.env, doctor and curl results; cleanup removes only that instance
# pos: verification control helper for the verify-binlog-server skill
# note: if this file changes, update this header and module README.md.
set -euo pipefail

# Kill only the pid recorded for this run. Never pkill, killall, or match by process name.

HELPER_NAME="control-binlog-server"

usage() {
  cat <<EOF
Usage: ${HELPER_NAME} <launch|doctor|curl|cleanup|env-print>

  launch
  doctor
  curl [--] METHOD PATH [curl-args...]
  cleanup
  env-print

Isolation env (defaults shown):
  BINLOG_VERIFY_RUN_ID     local
  BINLOG_VERIFY_LISTEN     127.0.0.1:18080
  BINLOG_VERIFY_BASE       /tmp/binlog-server-verify-\$BINLOG_VERIFY_RUN_ID
  BINLOG_VERIFY_EVIDENCE   /tmp/binlog-server-verify-evidence-\$BINLOG_VERIFY_RUN_ID
  BINLOG_SERVER_BIN        released binlog-server binary (required for launch and doctor)
  BINLOG_VERIFY_META_DSN   optional; launch runs migrate up when set
  BINLOG_VERIFY_MIGRATE_BIN  optional; default is ./migrate next to the server binary
  BINLOG_VERIFY_MIGRATIONS   optional; default is ./migrations next to the server binary
EOF
}

trim_slash() {
  local p="${1%/}"
  printf '%s' "${p:-/}"
}

resolve_env() {
  RUN_ID="${BINLOG_VERIFY_RUN_ID:-local}"
  LISTEN="${BINLOG_VERIFY_LISTEN:-127.0.0.1:18080}"
  BASE="$(trim_slash "${BINLOG_VERIFY_BASE:-/tmp/binlog-server-verify-${RUN_ID}}")"
  EVIDENCE="$(trim_slash "${BINLOG_VERIFY_EVIDENCE:-/tmp/binlog-server-verify-evidence-${RUN_ID}}")"
  BIN="${BINLOG_SERVER_BIN:-}"
  META_DSN="${BINLOG_VERIFY_META_DSN:-}"
  PID_FILE="${BASE}/binlog-server.pid"
  DATA_DIR="${BASE}/data"
  LOG_DIR="${BASE}/logs"

  local host port curl_host
  host="${LISTEN%:*}"
  port="${LISTEN##*:}"
  if [[ "$host" == "$LISTEN" || -z "$host" ]]; then
    host="127.0.0.1"
  fi
  case "$host" in
    0.0.0.0 | "*" | "[::]" | "::") curl_host="127.0.0.1" ;;
    *) curl_host="$host" ;;
  esac
  BASE_URL="http://${curl_host}:${port}"
}

require_bin() {
  if [[ -z "$BIN" || ! -x "$BIN" ]]; then
    echo "BINLOG_SERVER_BIN must be an executable released binary (got: ${BIN:-empty})" >&2
    exit 1
  fi
  BIN="$(readlink -f "$BIN")"
}

read_pid() {
  if [[ ! -f "$PID_FILE" ]]; then
    echo "missing pid file: $PID_FILE" >&2
    return 1
  fi
  local pid
  pid="$(tr -d '[:space:]' <"$PID_FILE")"
  if [[ ! "$pid" =~ ^[0-9]+$ ]]; then
    echo "pid file is not a pid: $PID_FILE" >&2
    return 1
  fi
  printf '%s' "$pid"
}

stop_recorded_pid() {
  local pid
  if [[ ! -f "$PID_FILE" ]]; then
    return 0
  fi
  pid="$(read_pid)"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    local _
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
      if ! kill -0 "$pid" 2>/dev/null; then
        break
      fi
      sleep 0.2
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -KILL "$pid" 2>/dev/null || true
    fi
  fi
  rm -f "$PID_FILE"
}

write_start_env() {
  {
    printf 'BINLOG_VERIFY_RUN_ID=%q\n' "$RUN_ID"
    printf 'BINLOG_VERIFY_LISTEN=%q\n' "$LISTEN"
    printf 'BINLOG_VERIFY_BASE=%q\n' "$BASE"
    printf 'BINLOG_VERIFY_EVIDENCE=%q\n' "$EVIDENCE"
    printf 'BINLOG_SERVER_BIN=%q\n' "$BIN"
    printf 'BINLOG_SERVER_LISTEN_ADDR=%q\n' "$LISTEN"
    printf 'BINLOG_SERVER_DATA_DIR=%q\n' "$DATA_DIR"
    printf 'BINLOG_SERVER_LOG_FILE=%q\n' "${LOG_DIR}/binlog-server.log"
    if [[ -n "$META_DSN" ]]; then
      printf 'BINLOG_VERIFY_META_DSN=%q\n' "$META_DSN"
      printf 'BINLOG_SERVER_META_DSN=%q\n' "$META_DSN"
    fi
  } >"${BASE}/start.env"
}

maybe_migrate() {
  if [[ -z "$META_DSN" ]]; then
    return 0
  fi
  local migrate_bin migrations
  migrate_bin="${BINLOG_VERIFY_MIGRATE_BIN:-$(dirname "$BIN")/migrate}"
  migrations="${BINLOG_VERIFY_MIGRATIONS:-$(dirname "$BIN")/migrations}"
  if [[ ! -x "$migrate_bin" ]]; then
    echo "migrate binary is not executable: $migrate_bin" >&2
    exit 1
  fi
  if [[ ! -d "$migrations" ]]; then
    echo "migrations directory not found: $migrations" >&2
    exit 1
  fi
  migrate_bin="$(readlink -f "$migrate_bin")"
  migrations="$(readlink -f "$migrations")"
  echo "migrate up against BINLOG_VERIFY_META_DSN"
  META_DSN="$META_DSN" "$migrate_bin" up --dsn "$META_DSN" --path "$migrations"
}

cmd_launch() {
  require_bin
  if [[ -f "$PID_FILE" ]]; then
    local existing
    existing="$(read_pid)" || true
    if [[ -n "${existing:-}" ]] && kill -0 "$existing" 2>/dev/null; then
      echo "already running pid ${existing} (pid file ${PID_FILE})" >&2
      exit 1
    fi
  fi

  mkdir -p "$BASE" "$DATA_DIR" "$LOG_DIR"
  maybe_migrate
  write_start_env

  local -a server_env
  server_env=(
    "PATH=${PATH}"
    "HOME=${HOME}"
    "BINLOG_SERVER_LISTEN_ADDR=${LISTEN}"
    "BINLOG_SERVER_DATA_DIR=${DATA_DIR}"
    "BINLOG_SERVER_LOG_FILE=${LOG_DIR}/binlog-server.log"
  )
  if [[ -n "$META_DSN" ]]; then
    server_env+=("BINLOG_SERVER_META_DSN=${META_DSN}")
  fi

  (
    cd "$BASE"
    env -i "${server_env[@]}" \
      nohup "$BIN" >>"${LOG_DIR}/stdout.log" 2>>"${LOG_DIR}/stderr.log" &
    echo $! >"$PID_FILE"
  )

  local pid
  pid="$(read_pid)"
  local _
  for _ in $(seq 1 60); do
    if curl -fsS "${BASE_URL}/healthz" 2>/dev/null | grep -qx 'ok'; then
      echo "launched pid ${pid} ${BASE_URL}"
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "binlog-server exited before /healthz returned ok" >&2
      tail -n 80 "${LOG_DIR}/stderr.log" >&2 || true
      exit 1
    fi
    sleep 0.5
  done
  echo "timed out waiting for ${BASE_URL}/healthz" >&2
  tail -n 80 "${LOG_DIR}/stderr.log" >&2 || true
  exit 1
}

port_owned_by_pid() {
  local pid="$1"
  if ! command -v ss >/dev/null 2>&1; then
    echo "port-owner: ss missing, skipped"
    return 0
  fi
  local out port
  port="${LISTEN##*:}"
  out="$(ss -H -ltnp 2>/dev/null || true)"
  if [[ -z "$out" ]]; then
    echo "port-owner: ss returned nothing, skipped"
    return 0
  fi
  if ! grep -q 'pid=' <<<"$out"; then
    echo "port-owner: ss did not report process owners, skipped"
    return 0
  fi
  local matches
  matches="$(grep -E ":${port}([^0-9]|$)" <<<"$out" || true)"
  if [[ -z "$matches" ]]; then
    echo "port-owner: nothing listening on port ${port}" >&2
    return 1
  fi
  if grep -q "pid=${pid}," <<<"$matches"; then
    echo "port-owner: pid ${pid} owns ${LISTEN}"
    return 0
  fi
  echo "port-owner: ${LISTEN} is not owned by pid ${pid}" >&2
  printf '%s\n' "$matches" >&2
  return 1
}

cmd_doctor() {
  require_bin
  local status=0
  local version
  version="$("$BIN" --version 2>/dev/null | head -n 1 || true)"
  if [[ -n "$version" ]]; then
    echo "version: ${version}"
  else
    echo "version: FAILED to read ${BIN} --version" >&2
    status=1
  fi

  local pid=""
  if pid="$(read_pid)"; then
    if kill -0 "$pid" 2>/dev/null; then
      echo "pid: ${pid} alive"
    else
      echo "pid: ${pid} not alive" >&2
      status=1
    fi
  else
    status=1
  fi

  local healthz=""
  if healthz="$(curl -fsS "${BASE_URL}/healthz" 2>/dev/null)" && printf '%s\n' "$healthz" | grep -qx 'ok'; then
    echo "healthz: ok"
  else
    echo "healthz: FAILED (${BASE_URL}/healthz)" >&2
    status=1
  fi

  local api_health=""
  if api_health="$(curl -fsS "${BASE_URL}/api/health" 2>/dev/null)" && printf '%s' "$api_health" | grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"'; then
    echo "api-health: ok"
  else
    echo "api-health: FAILED (${BASE_URL}/api/health)" >&2
    status=1
  fi

  local ui_code=""
  ui_code="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE_URL}/ui/" || true)"
  if [[ "$ui_code" == "200" ]]; then
    echo "ui: 200"
  else
    echo "ui: FAILED status ${ui_code:-none}" >&2
    status=1
  fi

  if [[ -n "$pid" ]] && ! port_owned_by_pid "$pid"; then
    status=1
  fi

  if [[ "$status" -ne 0 ]]; then
    exit "$status"
  fi
}

cmd_curl() {
  if [[ "${1:-}" == "--" ]]; then
    shift
  fi
  local method="${1:-}" path="${2:-}"
  if [[ -z "$method" || -z "$path" ]]; then
    echo "usage: ${HELPER_NAME} curl [--] METHOD PATH [curl-args...]" >&2
    exit 2
  fi
  shift 2
  case "$path" in
    /*) ;;
    *) path="/${path}" ;;
  esac
  curl -sS -X "$method" "${BASE_URL}${path}" "$@"
}

remove_base_keep_evidence() {
  local base evidence
  base="$(trim_slash "$BASE")"
  evidence="$(trim_slash "$EVIDENCE")"
  if [[ "$evidence" == "$base" ]]; then
    echo "refusing cleanup: BINLOG_VERIFY_EVIDENCE is the same path as BINLOG_VERIFY_BASE" >&2
    exit 1
  fi

  local saved=""
  if [[ -e "$evidence" && "$evidence" == "$base"/* ]]; then
    saved="$(mktemp -d /tmp/binlog-verify-evidence.XXXXXX)"
    mv "$evidence" "${saved}/kept"
  fi
  rm -rf "$base"
  if [[ -n "$saved" ]]; then
    mkdir -p "$(dirname "$evidence")"
    mv "${saved}/kept" "$evidence"
    rmdir "$saved"
  fi
}

cmd_cleanup() {
  stop_recorded_pid
  remove_base_keep_evidence
  echo "cleaned ${BASE}; evidence kept at ${EVIDENCE}"
}

cmd_env_print() {
  printf 'BINLOG_VERIFY_RUN_ID=%s\n' "$RUN_ID"
  printf 'BINLOG_VERIFY_LISTEN=%s\n' "$LISTEN"
  printf 'BINLOG_VERIFY_BASE=%s\n' "$BASE"
  printf 'BINLOG_VERIFY_EVIDENCE=%s\n' "$EVIDENCE"
  printf 'BINLOG_SERVER_BIN=%s\n' "${BIN:-}"
  if [[ -n "$META_DSN" ]]; then
    printf 'BINLOG_VERIFY_META_DSN=%s\n' "$META_DSN"
  else
    printf 'BINLOG_VERIFY_META_DSN=\n'
  fi
  printf 'BASE_URL=%s\n' "$BASE_URL"
  printf 'PID_FILE=%s\n' "$PID_FILE"
}

main() {
  local cmd="${1:-}"
  if [[ -z "$cmd" || "$cmd" == "-h" || "$cmd" == "--help" ]]; then
    usage
    exit 2
  fi
  shift
  resolve_env
  case "$cmd" in
    launch)
      [[ $# -eq 0 ]] || { usage; exit 2; }
      cmd_launch
      ;;
    doctor)
      [[ $# -eq 0 ]] || { usage; exit 2; }
      cmd_doctor
      ;;
    curl) cmd_curl "$@" ;;
    cleanup)
      [[ $# -eq 0 ]] || { usage; exit 2; }
      cmd_cleanup
      ;;
    env-print)
      [[ $# -eq 0 ]] || { usage; exit 2; }
      cmd_env_print
      ;;
    *)
      usage
      exit 2
      ;;
  esac
}

main "$@"
