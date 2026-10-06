#!/usr/bin/env bash
# input: all-in-one binlog-server with metadata, and a closed source port
# output: proof two mid-streak restarts (SIGTERM then kill -9) still reach FAILED at ten SOURCE_UNREACHABLE failures, with epoch unchanged and retry_attempt not reset to 1
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
RUN_TAG="$(date +%s)"
NAME="e2e-unreachable-giveup-${RUN_TAG}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd jq
need_cmd docker

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
[[ "$id" =~ ^[0-9]+$ ]] || fail "create response has no numeric id: $id"
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

current_pid() {
  if [[ -n "$new_pid" ]] && kill -0 "$new_pid" 2>/dev/null; then
    printf '%s' "$new_pid"
    return 0
  fi
  printf '%s' "${E2E_SERVER_PID:-}"
}

meta_budget() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary \
    mysql -uroot -proot binlog_meta -Nse \
    "SELECT retry_attempt, consecutive_source_failures FROM backup_tasks WHERE id='${id}'" \
    | tr -d '\r'
}

restart_server() {
  local signal="$1"
  local pid
  pid="$(current_pid)"
  local data="${E2E_DATA_DIR:-}"
  local meta="${BINLOG_SERVER_META_DSN:-}"
  local log="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-suite.log}"
  [[ -n "$pid" ]] || fail "no server pid to restart"
  [[ -n "$data" ]] || fail "E2E_DATA_DIR is required to restart"
  [[ -n "$meta" ]] || fail "BINLOG_SERVER_META_DSN is required to restart"
  if [[ "$signal" == "KILL" ]]; then
    kill -9 "$pid" >/dev/null 2>&1 || fail "kill -9 $pid failed"
  else
    kill "$pid" >/dev/null 2>&1 || fail "SIGTERM $pid failed"
  fi
  local gone=0
  for _ in $(seq 1 30); do
    if ! kill -0 "$pid" 2>/dev/null; then
      gone=1
      break
    fi
    sleep 1
  done
  [[ "$gone" == "1" ]] || fail "server still running after $signal"
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
      echo "[unreachable-giveup] restarted pid=$new_pid signal=$signal"
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
restarts=0
state=""
err=""
epoch=0
owner=""
# Set after a restart until the next failure proves the stored attempt continued.
expect_epoch=""
attempt_before=""
# 1+2+4+8+16+30s sleeps, plus two process restarts. 400s covers that.
for _ in $(seq 1 400); do
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
  if [[ -n "$expect_epoch" && "$state" == "RETRY_BACKOFF" ]]; then
    [[ "$epoch" == "$expect_epoch" ]] || fail "same-owner restart changed epoch $expect_epoch -> $epoch"
    budget="$(meta_budget)" || fail "meta budget query failed"
    attempt="$(printf '%s' "$budget" | awk '{print $1}')"
    consecutive="$(printf '%s' "$budget" | awk '{print $2}')"
    if [[ -n "$attempt" && "$attempt" -gt "$attempt_before" ]]; then
      [[ "$attempt" != "1" ]] || fail "retry_attempt reset to 1 after restart (was $attempt_before)"
      echo "[unreachable-giveup] continued attempt=$attempt consecutive=$consecutive epoch=$epoch"
      expect_epoch=""
    fi
  fi
  if [[ "$state" == "RETRY_BACKOFF" && -z "$expect_epoch" ]]; then
    n="$(unreachable_count)"
    do_restart=""
    if [[ "$restarts" -eq 0 && "$n" -ge 4 && "$n" -le 6 ]]; then
      do_restart="TERM"
    elif [[ "$restarts" -eq 1 && "$n" -ge 7 && "$n" -le 8 ]]; then
      do_restart="KILL"
    fi
    if [[ -n "$do_restart" ]]; then
      budget="$(meta_budget)" || fail "meta budget query failed"
      attempt_before="$(printf '%s' "$budget" | awk '{print $1}')"
      # The event can be visible one poll before the task row. Wait for the column.
      if [[ -z "$attempt_before" || "$attempt_before" -lt "$n" ]]; then
        do_restart=""
      fi
    fi
    if [[ -n "$do_restart" ]]; then
      [[ "$attempt_before" != "1" ]] || fail "retry_attempt is 1 at $n failures"
      expect_epoch="$epoch"
      [[ "$expect_epoch" != "0" ]] || fail "epoch is 0 during RETRY_BACKOFF"
      echo "[unreachable-giveup] task=$id failures=$n attempt=$attempt_before epoch=$epoch signal=$do_restart"
      restart_server "$do_restart"
      restarts=$((restarts + 1))
    fi
  fi
  if [[ "$state" == "FAILED" ]]; then
    [[ "$restarts" == "2" ]] || fail "FAILED after $restarts restarts, want 2"
    [[ -z "$expect_epoch" ]] || fail "FAILED before the post-restart attempt was observed"
    [[ "$err" == SOURCE_UNREACHABLE:* ]] || fail "last_error=$err"
    [[ "$saw_backoff" == "1" ]] || fail "FAILED without staying in RETRY_BACKOFF"
    [[ "$epoch" == "0" && -z "$owner" ]] || fail "lease not released epoch=$epoch owner=$owner"
    total="$(unreachable_count)"
    [[ "$total" == "10" ]] || fail "SOURCE_UNREACHABLE runner errors=$total, want 10"
    budget="$(meta_budget)" || fail "meta budget query failed"
    consecutive="$(printf '%s' "$budget" | awk '{print $2}')"
    [[ "$consecutive" == "10" ]] || fail "consecutive_source_failures=$consecutive, want 10"
    echo "[unreachable-giveup] task=$id FAILED after $restarts restarts total=$total"
    exit 0
  fi
  sleep 1
done

fail "did not reach FAILED (state=$state epoch=$epoch restarts=$restarts error=$err)"
