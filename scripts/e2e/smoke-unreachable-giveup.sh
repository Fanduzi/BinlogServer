#!/usr/bin/env bash
# input: all-in-one binlog-server with metadata, and a closed source port
# output: proof four SOURCE_UNREACHABLE failures, a process restart, then FAILED at ten total with the lease released
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
RUN_TAG="$(date +%s)"
NAME="e2e-unreachable-giveup-${RUN_TAG}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd jq

fail() {
  echo "[unreachable-giveup] $*" >&2
  exit 1
}

payload="$(jq -n --arg name "$NAME" '{name:$name,cluster_key:$name,source:{host:"127.0.0.1",port:1,user:"repl",password:"replpass",flavor:"mysql",server_id:319901},start:{mode:"LATEST"},storage:{retention_days:7}}')"
create_file="$(mktemp)"
create_code="$(curl -sS -o "$create_file" -w '%{http_code}' -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$payload")"
[[ "$create_code" == "201" ]] || fail "create failed http=$create_code body=$(cat "$create_file")"
id="$(jq -r '.id // empty' "$create_file")"
rm -f "$create_file"
[[ -n "$id" && "$id" != "null" ]] || fail "create response has no id"
start_code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/api/tasks/$id/start")"
[[ "$start_code" == "204" ]] || fail "start failed http=$start_code"

unreachable_count() {
  curl -fsS "$API/api/tasks/$id/events" | jq '[.[] | select(.type == "TASK_RUNNER_ERROR" and ((.detail // "") | startswith("SOURCE_UNREACHABLE")))] | length'
}

new_pid=""
cleanup_restart() {
  if [[ -n "$new_pid" ]]; then
    kill "$new_pid" >/dev/null 2>&1 || true
    wait "$new_pid" >/dev/null 2>&1 || true
  fi
}
trap cleanup_restart EXIT

restart_server() {
  local pid="${E2E_SERVER_PID:-}"
  local data="${E2E_DATA_DIR:-}"
  local meta="${BINLOG_SERVER_META_DSN:-}"
  local log="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-suite.log}"
  [[ -n "$pid" ]] || fail "E2E_SERVER_PID is required to restart"
  [[ -n "$data" ]] || fail "E2E_DATA_DIR is required to restart"
  [[ -n "$meta" ]] || fail "BINLOG_SERVER_META_DSN is required to restart"
  kill "$pid" >/dev/null 2>&1 || fail "SIGTERM $pid failed"
  local gone=0
  for _ in $(seq 1 30); do
    if ! kill -0 "$pid" 2>/dev/null; then
      gone=1
      break
    fi
    sleep 1
  done
  [[ "$gone" == "1" ]] || fail "server still running after SIGTERM"
  for _ in $(seq 1 30); do
    if ! curl -fsS "$API/healthz" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  BINLOG_SERVER_DATA_DIR="$data" BINLOG_SERVER_META_DSN="$meta" \
    nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >>"$log" 2>&1 &
  new_pid=$!
  for _ in $(seq 1 120); do
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      echo "[unreachable-giveup] restarted pid=$new_pid"
      return 0
    fi
    if ! kill -0 "$new_pid" 2>/dev/null; then
      fail "restarted server exited early"
    fi
    sleep 1
  done
  fail "restarted server not ready"
}

saw_backoff=0
restarted=0
state=""
err=""
epoch=0
owner=""
# Four failures, a restart, then six more. Backoff after restart starts again at 1s.
for _ in $(seq 1 240); do
  if ! body="$(curl -fsS "$API/api/tasks/$id" 2>/dev/null)"; then
    sleep 1
    continue
  fi
  state="$(printf '%s' "$body" | jq -r '.state // empty')"
  err="$(printf '%s' "$body" | jq -r '.last_error // empty')"
  epoch="$(printf '%s' "$body" | jq -r '.epoch // 0')"
  owner="$(printf '%s' "$body" | jq -r '.owner_worker_id // empty')"
  if [[ "$state" == "RETRY_BACKOFF" ]]; then
    saw_backoff=1
  fi
  if [[ "$restarted" == "0" && "$state" == "RETRY_BACKOFF" ]]; then
    n="$(unreachable_count)"
    if [[ "$n" -eq 4 ]]; then
      echo "[unreachable-giveup] task=$id failures=4, restarting"
      restart_server
      restarted=1
    fi
  fi
  if [[ "$state" == "FAILED" ]]; then
    [[ "$restarted" == "1" ]] || fail "FAILED before the mid-streak restart"
    [[ "$err" == SOURCE_UNREACHABLE:* ]] || fail "last_error=$err"
    [[ "$saw_backoff" == "1" ]] || fail "FAILED without staying in RETRY_BACKOFF"
    [[ "$epoch" == "0" && -z "$owner" ]] || fail "lease not released epoch=$epoch owner=$owner"
    total="$(unreachable_count)"
    [[ "$total" == "10" ]] || fail "SOURCE_UNREACHABLE runner errors=$total, want 10 (not 14)"
    echo "[unreachable-giveup] task=$id FAILED after restart total=$total"
    exit 0
  fi
  sleep 1
done

fail "did not reach FAILED (state=$state epoch=$epoch restarted=$restarted error=$err)"
