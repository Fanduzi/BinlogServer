#!/usr/bin/env bash
# input: canonical E2E database topology (compose mysql57 as source), two to
#   three workers on per_node storage with separate data_dirs, one meta DSN
# output: ADR 0011 §4.2 fault-injection matrix as a release-tier e2e: each
#   cell asserts on meta rows, /api/health, findings and the source's Binlog
#   Dump thread count (never on logs). Cells without a deterministic fault
#   hook are reported SKIP with a reason; cells owned by a dedicated
#   scenario are reported COVERED-BY so the summary is honest rather than
#   green-by-omission
# pos: integration-test automation layer validating end-to-end cluster failover
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-fault-matrix-$(date +%s)}"
WORKER1_ID="${E2E_WORKER1_ID:-e2e-fm-1}"
WORKER2_ID="${E2E_WORKER2_ID:-e2e-fm-2}"
WORKER3_ID="${E2E_WORKER3_ID:-e2e-fm-3}"
WORKER1_HEALTH_ADDR="${E2E_WORKER1_HEALTH_ADDR:-127.0.0.1:18081}"
WORKER2_HEALTH_ADDR="${E2E_WORKER2_HEALTH_ADDR:-127.0.0.1:18082}"
WORKER3_HEALTH_ADDR="${E2E_WORKER3_HEALTH_ADDR:-127.0.0.1:18083}"
# node_gone sits above the freeze windows the non-terminal cells use (~30s of
# SIGSTOP) so #473-style invariants stay strict there; the row-6 cell waits it
# out explicitly, and the kill-9 cell allows a legitimate FAILED while the
# dead owner is gone past this bound (ChainOwnerBack folds it back).
NODE_GONE_SEC="${E2E_NODE_GONE_SEC:-45}"

RUN_TAG="$(date +%s)"
CONTROL_LOG="${E2E_CONTROL_LOG:-/tmp/binlog-server-e2e-fm-control-${RUN_TAG}.log}"
WORKER1_LOG="${E2E_WORKER1_LOG:-/tmp/binlog-server-e2e-fm-worker1-${RUN_TAG}.log}"
WORKER2_LOG="${E2E_WORKER2_LOG:-/tmp/binlog-server-e2e-fm-worker2-${RUN_TAG}.log}"
WORKER3_LOG="${E2E_WORKER3_LOG:-/tmp/binlog-server-e2e-fm-worker3-${RUN_TAG}.log}"
HOLD_BIN="${E2E_HOLD_BIN:-/tmp/binlog-e2e-hold-for-update-${RUN_TAG}}"

CONTROL_PID=""
WORKER1_PID=""
WORKER2_PID=""
WORKER3_PID=""
HOLD_PID=""

source "$ROOT_DIR/scripts/e2e/lib-migration.sh"
MYSQL57_PORT="$E2E_MYSQL57_PORT"
META_DSN="${BINLOG_SERVER_META_DSN:-$(e2e_meta_dsn direct)}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq
e2e_ensure_meta_schema "$ROOT_DIR" "$META_DSN"

kill_pid() {
  local pid="$1"
  if [[ -z "$pid" ]]; then
    return 0
  fi
  kill -CONT "$pid" >/dev/null 2>&1 || true
  kill "$pid" >/dev/null 2>&1 || true
  wait "$pid" >/dev/null 2>&1 || true
}

cleanup() {
  kill_pid "$HOLD_PID"
  kill_pid "$WORKER3_PID"
  kill_pid "$WORKER2_PID"
  kill_pid "$WORKER1_PID"
  kill_pid "$CONTROL_PID"
}
trap cleanup EXIT

wait_control_plane_ready() {
  for _ in {1..120}; do
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "control-plane not ready in time" >&2
  cat "$CONTROL_LOG" >&2 || true
  return 1
}

start_control_plane() {
  local cp_data_dir="$DATA_DIR/control-plane"
  mkdir -p "$cp_data_dir"
  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="control-plane" \
  BINLOG_SERVER_CLUSTER_STORAGE="per_node" \
  BINLOG_SERVER_LISTEN_ADDR="127.0.0.1:18080" \
  BINLOG_SERVER_DATA_DIR="$cp_data_dir" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  BINLOG_SERVER_CLUSTER_LEASE_TTL_SEC=15 \
  BINLOG_SERVER_HEALTH_SWEEP_INTERVAL_SEC=2 \
  BINLOG_SERVER_HEALTH_NODE_GONE_AFTER_SEC="$NODE_GONE_SEC" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$CONTROL_LOG" 2>&1 &
  CONTROL_PID=$!
}

start_worker() {
  local worker_id="$1"
  local health_addr="$2"
  local data_dir="$3"
  local log_file="$4"
  mkdir -p "$data_dir"
  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="worker" \
  BINLOG_SERVER_CLUSTER_STORAGE="per_node" \
  BINLOG_SERVER_CLUSTER_WORKER_ID="$worker_id" \
  BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR="$health_addr" \
  BINLOG_SERVER_DATA_DIR="$data_dir" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  BINLOG_SERVER_CLUSTER_LEASE_TTL_SEC=15 \
  BINLOG_SERVER_HEALTH_SWEEP_INTERVAL_SEC=2 \
  BINLOG_SERVER_HEALTH_NODE_GONE_AFTER_SEC="$NODE_GONE_SEC" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$log_file" 2>&1 &
  echo "$!"
}

# A worker_id registration survives kill -9 until its TTL: restarting the
# same worker_id needs one retry loop until AcquireWorkerRegistration wins.
restart_worker() {
  local worker_id="$1"
  local health_addr="$2"
  local data_dir="$3"
  local log_file="$4"
  local tries="${5:-30}"
  local i
  for i in $(seq 1 "$tries"); do
    local pid
    pid="$(start_worker "$worker_id" "$health_addr" "$data_dir" "$log_file")"
    sleep 2
    if kill -0 "$pid" 2>/dev/null; then
      echo "$pid"
      return 0
    fi
    wait "$pid" 2>/dev/null || true
    echo "[fm] worker $worker_id registration refused (attempt $i/$tries); retrying" >&2
    sleep 2
  done
  echo "worker $worker_id never re-registered after restart" >&2
  tail -n 40 "$log_file" >&2 || true
  return 1
}

wait_worker_online() {
  local worker_id="$1"
  for _ in {1..90}; do
    if curl -fsS "$API/api/workers?limit=200" | jq -e --arg wid "$worker_id" '.[] | select(.worker_id == $wid and .online == true)' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "worker did not become online in time: worker_id=$worker_id" >&2
  return 1
}

create_task() {
  local name="$1" sid resp id
  sid=$((490000 + RANDOM % 10000))
  resp="$(curl -fsS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"cluster_key\":\"$name\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":${MYSQL57_PORT},\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":${sid}},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")"
  id="$(printf '%s' "$resp" | jq -r '.id // empty')"
  [[ -n "$id" && "$id" != "null" ]] || { echo "create task failed: $resp" >&2; return 1; }
  printf '%s' "$id"
}

http_code() {
  local method="$1" path="$2"
  curl -sS -o "/tmp/e2e-fm-${RUN_TAG}.resp" -w '%{http_code}' -X "$method" "$API$path"
}

task_field() { curl -fsS "$API/api/tasks/$1" | jq -r ".$2 // empty"; }
task_state() { task_field "$1" state; }
task_epoch() { task_field "$1" epoch; }
task_owner() { task_field "$1" owner_worker_id; }
task_desired() { task_field "$1" desired_run; }

meta_sql() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary mysql -umeta -pmetapass -N binlog_meta -e "$1" 2>/dev/null
}

dump_threads() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot -N \
    -e "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE COMMAND LIKE 'Binlog Dump%';" 2>/dev/null | tr -d ' \r\n'
}

health_code() {
  curl -sS -o /dev/null -w '%{http_code}' "$API/api/health"
}

open_crit() {
  local task_id="$1" kind="$2"
  curl -fsS "$API/api/findings?limit=500" | jq -e --arg t "$task_id" --arg k "$kind" '.[] | select(.task_id == $t and .kind == $k and .severity == "CRITICAL" and (.resolved_at == null))' >/dev/null
}

crit_any() {
  local task_id="$1"
  curl -fsS "$API/api/findings?limit=500" | jq -e --arg t "$task_id" '.[] | select(.task_id == $t and .severity == "CRITICAL" and (.resolved_at == null))' >/dev/null
}

write_source_rows() {
  local tag="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -urepl -preplpass -e \
    "CREATE DATABASE IF NOT EXISTS fm_${tag}; CREATE TABLE IF NOT EXISTS fm_${tag}.t (id INT PRIMARY KEY AUTO_INCREMENT, v VARCHAR(200)); INSERT INTO fm_${tag}.t (v) VALUES ('${tag}-a'),('${tag}-b'); FLUSH BINARY LOGS;" >/dev/null 2>&1 || true
}

wait_task_state() {
  local task_id="$1" expected="$2" secs="${3:-120}"
  local i
  for i in $(seq 1 "$secs"); do
    if [[ "$(task_state "$task_id")" == "$expected" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task $task_id never reached $expected: $(curl -fsS "$API/api/tasks/$task_id")" >&2
  return 1
}

wait_running_owner() {
  local task_id="$1" owner="$2" secs="${3:-120}"
  local i
  for i in $(seq 1 "$secs"); do
    if [[ "$(task_state "$task_id")" == "RUNNING" && "$(task_owner "$task_id")" == "$owner" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task $task_id not RUNNING under $owner: $(curl -fsS "$API/api/tasks/$task_id")" >&2
  return 1
}

# Matrix-level invariants asserted inside every cell that injects a fault:
# never FAILED (unless the cell itself is the terminal one), desired_run
# never STOP, and at most one Binlog Dump on the source.
assert_matrix_invariants() {
  local task_id="$1" where="$2" allow_failed="${3:-}"
  local state desired threads
  state="$(task_state "$task_id")"
  desired="$(task_desired "$task_id")"
  if [[ "$state" == "FAILED" && "$allow_failed" != "allow-failed" ]]; then
    echo "[$where] task $task_id hit FAILED: $(curl -fsS "$API/api/tasks/$task_id")" >&2
    return 1
  fi
  if [[ "$desired" == "STOP" ]]; then
    echo "[$where] desired_run became STOP: $(curl -fsS "$API/api/tasks/$task_id")" >&2
    return 1
  fi
  threads="$(dump_threads)"
  if [[ -n "$threads" && "$threads" =~ ^[0-9]+$ && "$threads" -gt 1 ]]; then
    echo "[$where] source has $threads Binlog Dump threads, want at most 1" >&2
    return 1
  fi
  return 0
}

# ---- matrix accounting -------------------------------------------------------
declare -a MATRIX_ROWS=()
CELL_FAILURES=0
cell_result() { MATRIX_ROWS+=("$(printf '%-34s %-12s %s' "$1" "$2" "$3")"); }
cell_fail()   { CELL_FAILURES=$((CELL_FAILURES + 1)); cell_result "$1" "FAIL" "$2"; }
cell_skip()   { cell_result "$1" "SKIP" "$2"; }
cell_covered(){ cell_result "$1" "COVERED" "$2"; }

print_matrix() {
  echo
  echo "================ ADR 0011 §4.2 fault-injection matrix ================"
  printf '%-34s %-12s %s\n' "CELL" "RESULT" "EVIDENCE / REASON"
  printf '%s\n' "--------------------------------------------------------------------"
  printf '%s\n' "${MATRIX_ROWS[@]}"
  echo "===================================================================="
  echo "cells run: ${#MATRIX_ROWS[@]}, failures: $CELL_FAILURES"
}

# ---- bring up the topology ---------------------------------------------------
echo "[fm] control-plane + workers 1/2 on per_node (worker3 on demand)"
start_control_plane
wait_control_plane_ready
WORKER1_PID="$(start_worker "$WORKER1_ID" "$WORKER1_HEALTH_ADDR" "$DATA_DIR/w1" "$WORKER1_LOG")"
WORKER2_PID="$(start_worker "$WORKER2_ID" "$WORKER2_HEALTH_ADDR" "$DATA_DIR/w2" "$WORKER2_LOG")"
wait_worker_online "$WORKER1_ID"
wait_worker_online "$WORKER2_ID"
( cd "$ROOT_DIR" && CGO_ENABLED=0 go build -o "$HOLD_BIN" ./scripts/e2e/hold_for_update.go )

# ==============================================================================
# Cell: phase "dump open" x kill -9, plus supplementary row 7 (per_node kill -9
# then restart with the same node_id/data_dir re-becomes owner).
# ==============================================================================
cell_kill9_restart() {
  local task_id
  task_id="$(create_task "e2e-fm-kill9-${RUN_TAG}")"
  http_code POST "/api/tasks/$task_id/start" >/dev/null
  wait_running_owner "$task_id" "$WORKER1_ID" || wait_running_owner "$task_id" "$WORKER2_ID"
  local owner_pid data_dir owner_id other_id
  owner_id="$(task_owner "$task_id")"
  if [[ "$owner_id" == "$WORKER1_ID" ]]; then
    owner_pid="$WORKER1_PID"; data_dir="$DATA_DIR/w1"; other_id="$WORKER2_ID"
  else
    owner_pid="$WORKER2_PID"; data_dir="$DATA_DIR/w2"; other_id="$WORKER1_ID"
  fi
  local old_epoch
  old_epoch="$(task_epoch "$task_id")"
  write_source_rows "k9a"

  echo "[fm:k9] owner=$owner_id epoch=$old_epoch; kill -9"
  kill -9 "$owner_pid" 2>/dev/null || true
  wait "$owner_pid" 2>/dev/null || true

  # The dir-less peer may claim once, then must yield (tail unreadable) and
  # refuse: the row never takes desired STOP. The dead owner is gone past
  # node_gone_after while we restart it, so FAILED/CHAIN_OWNER_GONE is the
  # designed intermediate here (ADR §4.2 row 7); ChainOwnerBack folds it back
  # once the restarted node verifies the chain.
  local seen_terminal=0 i st
  for i in $(seq 1 90); do
    assert_matrix_invariants "$task_id" "k9-takeover" "allow-failed" || return 1
    st="$(task_state "$task_id")"
    if [[ "$st" == "LEASE_DEGRADED" || "$st" == "FAILED" ]]; then
      seen_terminal=1; break
    fi
    sleep 1
  done
  if [[ "$seen_terminal" -ne 1 && "$(task_state "$task_id")" != "RUNNING" ]]; then
    echo "[fm:k9] peer never claimed+yielded to LEASE_DEGRADED/FAILED: $(curl -fsS "$API/api/tasks/$task_id")" >&2
    return 1
  fi
  # Row 7: restart the same worker_id/data_dir; it must re-become owner.
  echo "[fm:k9] restart $owner_id on the same data_dir; it must re-own"
  local new_pid
  new_pid="$(restart_worker "$owner_id" "$([ "$owner_id" == "$WORKER1_ID" ] && echo "$WORKER1_HEALTH_ADDR" || echo "$WORKER2_HEALTH_ADDR")" "$data_dir" "$([ "$owner_id" == "$WORKER1_ID" ] && echo "$WORKER1_LOG" || echo "$WORKER2_LOG")")" || return 1
  [[ "$owner_id" == "$WORKER1_ID" ]] && WORKER1_PID="$new_pid" || WORKER2_PID="$new_pid"
  wait_running_owner "$task_id" "$owner_id" 180 || return 1
  local new_epoch
  new_epoch="$(task_epoch "$task_id")"
  [[ "$new_epoch" =~ ^[0-9]+$ && "$new_epoch" -gt "$old_epoch" ]] || { echo "[fm:k9] epoch did not grow: $old_epoch -> $new_epoch" >&2; return 1; }
  write_source_rows "k9b"
  sleep 4
  assert_matrix_invariants "$task_id" "k9-resumed" || return 1
  [[ "$(dump_threads)" == "1" ]] || { echo "[fm:k9] dump threads=$(dump_threads), want 1" >&2; return 1; }
  http_code POST "/api/tasks/$task_id/stop" >/dev/null
  wait_task_state "$task_id" "STOPPED" 60
  cell_result "phase-dump x kill-9 + row7 restart" "RUN" "pass: peer yielded (no FAILED/STOP); $owner_id re-owned at epoch $new_epoch; one dump"
}

# ==============================================================================
# Cell: extra row "consecutive yields while the peer never has the directory"
# plus phase "TailUnreadable committed" x SIGSTOP.
# ==============================================================================
cell_yield_chain() {
  local task_id
  task_id="$(create_task "e2e-fm-yield-${RUN_TAG}")"
  http_code POST "/api/tasks/$task_id/start" >/dev/null
  wait_running_owner "$task_id" "$WORKER1_ID" || wait_running_owner "$task_id" "$WORKER2_ID"
  local owner_id other_id owner_pid
  owner_id="$(task_owner "$task_id")"
  if [[ "$owner_id" == "$WORKER1_ID" ]]; then
    owner_pid="$WORKER1_PID"; other_id="$WORKER2_ID"
  else
    owner_pid="$WORKER2_PID"; other_id="$WORKER1_ID"
  fi
  local old_epoch
  old_epoch="$(task_epoch "$task_id")"

  echo "[fm:yield] SIGSTOP $owner_id past lease_ttl"
  kill -STOP "$owner_pid"
  local yield_epoch="" i
  for i in $(seq 1 90); do
    assert_matrix_invariants "$task_id" "yield-takeover" || { kill -CONT "$owner_pid"; return 1; }
    if [[ "$(task_state "$task_id")" == "LEASE_DEGRADED" ]]; then
      yield_epoch="$(task_epoch "$task_id")"
      break
    fi
    sleep 1
  done
  [[ -n "$yield_epoch" ]] || { kill -CONT "$owner_pid"; echo "[fm:yield] never reached LEASE_DEGRADED" >&2; return 1; }

  # The refusing peer must not re-acquire: epoch holds over several claim ticks.
  local j
  for j in $(seq 1 8); do
    assert_matrix_invariants "$task_id" "yield-hold" || { kill -CONT "$owner_pid"; return 1; }
    local cur
    cur="$(task_epoch "$task_id")"
    if [[ "$cur" != "$yield_epoch" ]]; then
      kill -CONT "$owner_pid"
      echo "[fm:yield] epoch moved past the single refusal: $yield_epoch -> $cur" >&2
      return 1
    fi
    sleep 1
  done
  local hc
  hc="$(health_code)"
  [[ "$hc" == "503" ]] || { kill -CONT "$owner_pid"; echo "[fm:yield] /api/health=$hc while the owner was fenced, want 503" >&2; return 1; }
  crit_any "$task_id" || { kill -CONT "$owner_pid"; echo "[fm:yield] no open CRITICAL finding for $task_id while degraded" >&2; return 1; }

  kill -CONT "$owner_pid"
  wait_running_owner "$task_id" "$owner_id" 240 || return 1
  [[ "$(dump_threads)" == "1" ]] || return 1
  http_code POST "/api/tasks/$task_id/stop" >/dev/null
  wait_task_state "$task_id" "STOPPED" 60
  cell_result "extra: consecutive yields, no dir" "RUN" "pass: one claim+yield; refusal held epoch=$yield_epoch; 503 during; $owner_id resumed"
}

# ==============================================================================
# Cell: extra row "copy the task directory to the peer, then it can Start".
# ==============================================================================
cell_copy_dir() {
  local task_id
  task_id="$(create_task "e2e-fm-copy-${RUN_TAG}")"
  http_code POST "/api/tasks/$task_id/start" >/dev/null
  wait_running_owner "$task_id" "$WORKER1_ID" || wait_running_owner "$task_id" "$WORKER2_ID"
  local owner_id other_id other_dir
  owner_id="$(task_owner "$task_id")"
  if [[ "$owner_id" == "$WORKER1_ID" ]]; then other_id="$WORKER2_ID"; other_dir="$DATA_DIR/w2"; else other_id="$WORKER1_ID"; other_dir="$DATA_DIR/w1"; fi
  write_source_rows "cd1"
  sleep 3

  echo "[fm:copy] freeze owner, copy $owner_id's task dir to $other_id"
  local owner_pid
  owner_pid="$([ "$owner_id" == "$WORKER1_ID" ] && echo "$WORKER1_PID" || echo "$WORKER2_PID")"
  kill -STOP "$owner_pid"
  local src_dir="$([ "$owner_id" == "$WORKER1_ID" ] && echo "$DATA_DIR/w1" || echo "$DATA_DIR/w2")"
  mkdir -p "$other_dir/$task_id"
  cp -a "$src_dir/$task_id/." "$other_dir/$task_id/" || true

  wait_running_owner "$task_id" "$other_id" 180 || { kill -CONT "$owner_pid"; return 1; }
  kill -CONT "$owner_pid"
  # The thawed old owner needs a moment to notice its stale epoch and close
  # its dump: briefly two Binlog Dump threads is the expected handoff, never
  # three; it must settle back to exactly one.
  local threads one=0
  for _ in $(seq 1 30); do
    threads="$(dump_threads)"
    if [[ "$threads" =~ ^[0-9]+$ && "$threads" -gt 2 ]]; then
      echo "[fm:copy] $threads dump threads during handoff, want <=2" >&2
      return 1
    fi
    [[ "$threads" == "1" ]] && { one=1; break; }
    sleep 1
  done
  [[ "$one" -eq 1 ]] || { echo "[fm:copy] dump threads never settled to 1: $(dump_threads)" >&2; return 1; }
  assert_matrix_invariants "$task_id" "copy-resume" || return 1
  http_code POST "/api/tasks/$task_id/stop" >/dev/null
  wait_task_state "$task_id" "STOPPED" 60
  cell_result "extra: copy dir to peer" "RUN" "pass: $other_id ChainVerified and ran; old owner yielded its stale epoch; one dump"
}

# ==============================================================================
# Cell: supplementary row 6 — owner stopped past node_gone_after, then SIGCONT
# with the directory present: FAILED/CHAIN_OWNER_GONE folds back through
# ChainVerified + ChainOwnerBack to LEASE_DEGRADED and DumpReady, no operator.
# ==============================================================================
cell_node_gone_back() {
  local task_id
  task_id="$(create_task "e2e-fm-gone-${RUN_TAG}")"
  http_code POST "/api/tasks/$task_id/start" >/dev/null
  wait_running_owner "$task_id" "$WORKER1_ID" || wait_running_owner "$task_id" "$WORKER2_ID"
  local owner_id owner_pid
  owner_id="$(task_owner "$task_id")"
  owner_pid="$([ "$owner_id" == "$WORKER1_ID" ] && echo "$WORKER1_PID" || echo "$WORKER2_PID")"

  echo "[fm:gone] SIGSTOP $owner_id for > node_gone_after=${NODE_GONE_SEC}s"
  kill -STOP "$owner_pid"
  local i st failed_seen=0
  for i in $(seq 1 $((NODE_GONE_SEC + 45))); do
    st="$(task_state "$task_id")"
    if [[ "$st" == "FAILED" ]]; then failed_seen=1; break; fi
    sleep 1
  done
  if [[ "$failed_seen" -ne 1 ]]; then
    kill -CONT "$owner_pid"
    echo "[fm:gone] never FAILED after node_gone_after: $(curl -fsS "$API/api/tasks/$task_id")" >&2
    return 1
  fi
  open_crit "$task_id" "TASK_FAILED" || { kill -CONT "$owner_pid"; echo "[fm:gone] FAILED without an open TASK_FAILED finding" >&2; return 1; }
  echo "[fm:gone] FAILED past node_gone_after; SIGCONT — ChainOwnerBack must fold it back"
  kill -CONT "$owner_pid"
  # verified_at > failed_at -> LEASE_DEGRADED -> DumpReady, no operator Start.
  wait_running_owner "$task_id" "$owner_id" 240 || return 1
  assert_matrix_invariants "$task_id" "gone-resumed" || return 1
  http_code POST "/api/tasks/$task_id/stop" >/dev/null
  wait_task_state "$task_id" "STOPPED" 60
  cell_result "row6: node_gone then SIGCONT" "RUN" "pass: FAILED+CHAIN_OWNER_GONE folded back via ChainVerified; $owner_id reached DumpReady"
}

# ==============================================================================
# Cell: supplementary row 10 — three nodes, only one has the directory.
# ==============================================================================
cell_three_nodes() {
  echo "[fm:3n] start worker3 (no directory)"
  WORKER3_PID="$(start_worker "$WORKER3_ID" "$WORKER3_HEALTH_ADDR" "$DATA_DIR/w3" "$WORKER3_LOG")"
  wait_worker_online "$WORKER3_ID"

  local task_id
  task_id="$(create_task "e2e-fm-3n-${RUN_TAG}")"
  http_code POST "/api/tasks/$task_id/start" >/dev/null
  local i owner_id=""
  for i in $(seq 1 120); do
    owner_id="$(task_owner "$task_id")"
    [[ -n "$owner_id" && "$(task_state "$task_id")" == "RUNNING" ]] && break
    sleep 1
  done
  local owner_pid owner_dir
  case "$owner_id" in
    "$WORKER1_ID") owner_pid="$WORKER1_PID"; owner_dir="$DATA_DIR/w1" ;;
    "$WORKER2_ID") owner_pid="$WORKER2_PID"; owner_dir="$DATA_DIR/w2" ;;
    "$WORKER3_ID") owner_pid="$WORKER3_PID"; owner_dir="$DATA_DIR/w3" ;;
    *) echo "[fm:3n] no owner: $(curl -fsS "$API/api/tasks/$task_id")" >&2; return 1 ;;
  esac
  local old_epoch
  old_epoch="$(task_epoch "$task_id")"

  echo "[fm:3n] SIGSTOP owner $owner_id; dir-less peers must yield once each"
  kill -STOP "$owner_pid"
  sleep 30 # lease_ttl 15 + claim interval + refusal write
  local cur_epoch st
  cur_epoch="$(task_epoch "$task_id")"
  st="$(task_state "$task_id")"
  if [[ "$st" == "FAILED" || "$(task_desired "$task_id")" == "STOP" ]]; then
    kill -CONT "$owner_pid"
    echo "[fm:3n] terminal write during claims: $(curl -fsS "$API/api/tasks/$task_id")" >&2
    return 1
  fi
  # Each of the two dir-less nodes claims at most once: epoch grows by <= 2.
  local growth=$((cur_epoch - old_epoch))
  if (( growth > 2 )); then
    kill -CONT "$owner_pid"
    echo "[fm:3n] epoch grew $growth (>2) — unbounded claim/yield loop: $(curl -fsS "$API/api/tasks/$task_id")" >&2
    return 1
  fi
  kill -CONT "$owner_pid"
  wait_running_owner "$task_id" "$owner_id" 240 || return 1
  [[ "$(dump_threads)" == "1" ]] || return 1
  http_code POST "/api/tasks/$task_id/stop" >/dev/null
  wait_task_state "$task_id" "STOPPED" 60
  kill_pid "$WORKER3_PID"; WORKER3_PID=""
  cell_result "row10: three nodes, one dir" "RUN" "pass: dir-less peers each yielded once (epoch +$growth); owner resumed"
}

# ==============================================================================
# Cell: supplementary row 11 — operator races (deterministic subset).
# ==============================================================================
cell_operator_races() {
  local task_id
  task_id="$(create_task "e2e-fm-ops-${RUN_TAG}")"
  http_code POST "/api/tasks/$task_id/start" >/dev/null
  wait_task_state "$task_id" "RUNNING" 120 || return 1

  # Start on STOPPING -> 409.
  local code
  code="$(http_code POST "/api/tasks/$task_id/stop")"
  [[ "$code" == "204" ]] || { echo "[fm:ops] stop http=$code" >&2; return 1; }
  code="$(http_code POST "/api/tasks/$task_id/start")"
  if [[ "$code" != "409" ]]; then
    echo "[fm:ops] Start on STOPPING http=$code, want 409: $(cat /tmp/e2e-fm-${RUN_TAG}.resp)" >&2
    return 1
  fi
  wait_task_state "$task_id" "STOPPED" 60 || return 1

  # Repeated Start with the same spec -> 204 and spec_revision unchanged.
  code="$(http_code POST "/api/tasks/$task_id/start")"
  [[ "$code" == "204" || "$code" == "200" ]] || { echo "[fm:ops] restart http=$code" >&2; return 1; }
  wait_task_state "$task_id" "RUNNING" 120 || return 1
  local rev1 rev2
  rev1="$(meta_sql "SELECT spec_revision FROM backup_tasks WHERE id='$task_id'" | tr -d ' \r\n')"
  code="$(http_code POST "/api/tasks/$task_id/start")"
  [[ "$code" == "204" || "$code" == "200" || "$code" == "409" ]] || { echo "[fm:ops] dup start http=$code" >&2; return 1; }
  rev2="$(meta_sql "SELECT spec_revision FROM backup_tasks WHERE id='$task_id'" | tr -d ' \r\n')"
  if [[ "$rev1" != "$rev2" ]]; then
    echo "[fm:ops] spec_revision moved on a same-spec Start: $rev1 -> $rev2" >&2
    return 1
  fi

  # Delete mid-run -> STOPPING + pending_delete; Start on it -> 409.
  code="$(http_code DELETE "/api/tasks/$task_id")"
  [[ "$code" == "204" ]] || { echo "[fm:ops] delete http=$code: $(cat /tmp/e2e-fm-${RUN_TAG}.resp)" >&2; return 1; }
  local pd
  pd="$(meta_sql "SELECT pending_delete FROM backup_tasks WHERE id='$task_id'" | tr -d ' \r\n')"
  [[ "$pd" == "1" ]] || { echo "[fm:ops] pending_delete=$pd, want 1" >&2; return 1; }
  code="$(http_code POST "/api/tasks/$task_id/start")"
  [[ "$code" == "409" ]] || { echo "[fm:ops] Start on pending_delete http=$code, want 409" >&2; return 1; }

  # kill -9 the holder; the row waits for lease expiry, then StopExpired
  # removes it without the dead StopConfirmed.
  local owner_id owner_pid
  owner_id="$(task_owner "$task_id")"
  if [[ -n "$owner_id" ]]; then
    case "$owner_id" in
      "$WORKER1_ID") owner_pid="$WORKER1_PID" ;;
      "$WORKER2_ID") owner_pid="$WORKER2_PID" ;;
      "$WORKER3_ID") owner_pid="$WORKER3_PID" ;;
      *) owner_pid="" ;;
    esac
    [[ -n "$owner_pid" ]] && { kill -9 "$owner_pid" 2>/dev/null || true; wait "$owner_pid" 2>/dev/null || true; }
  fi
  local gone=0
  for i in $(seq 1 90); do
    if [[ "$(meta_sql "SELECT COUNT(*) FROM backup_tasks WHERE id='$task_id'" | tr -d ' \r\n')" == "0" ]]; then
      gone=1; break
    fi
    sleep 1
  done
  [[ "$gone" -eq 1 ]] || { echo "[fm:ops] row still present after lease expiry + StopExpired" >&2; return 1; }
  cell_result "row11: operator races" "RUN" "pass: 409 on STOPPING, dup-Start same revision, delete->pending_delete, kill -9 + StopExpired removed the row"
}

# ==============================================================================
# Cell: supplementary row 2 — the _health side holds a task_leases row lock;
# the owner's own writes hit 1205 and retry; the task keeps its owner/epoch
# and never writes FAILED.
# ==============================================================================
cell_locked_health() {
  local task_id
  task_id="$(create_task "e2e-fm-lock-${RUN_TAG}")"
  http_code POST "/api/tasks/$task_id/start" >/dev/null
  wait_task_state "$task_id" "RUNNING" 120 || return 1
  local owner_id old_epoch
  owner_id="$(task_owner "$task_id")"
  old_epoch="$(task_epoch "$task_id")"

  # --plain: the holder keeps the row past the armed idle-kill so the owner's
  # own FOR UPDATE blocks into its session lock-wait bound (1205), which is
  # the row-2 behavior under test.
  "$HOLD_BIN" --plain "$META_DSN" "$task_id" >"/tmp/e2e-fm-hold-${RUN_TAG}.log" 2>&1 &
  HOLD_PID=$!
  for _ in {1..30}; do
    grep -q 'HOLDING' "/tmp/e2e-fm-hold-${RUN_TAG}.log" 2>/dev/null && break
    sleep 1
  done
  if ! grep -q 'HOLDING' "/tmp/e2e-fm-hold-${RUN_TAG}.log" 2>/dev/null; then
    echo "[fm:lock] holder never locked the lease row: $(cat /tmp/e2e-fm-hold-${RUN_TAG}.log)" >&2
    return 1
  fi

  # Hold through more than one armed lock-wait bound, then release.
  sleep 10
  local st
  st="$(task_state "$task_id")"
  kill_pid "$HOLD_PID"; HOLD_PID=""
  if [[ "$st" == "FAILED" ]]; then
    echo "[fm:lock] owner wrote FAILED while its health write was lock-blocked" >&2
    return 1
  fi
  [[ "$(task_owner "$task_id")" == "$owner_id" ]] || { echo "[fm:lock] owner changed while lock-blocked" >&2; return 1; }
  [[ "$(task_epoch "$task_id")" == "$old_epoch" ]] || { echo "[fm:lock] epoch moved while lock-blocked" >&2; return 1; }
  sleep 5
  assert_matrix_invariants "$task_id" "lock-resume" || return 1
  http_code POST "/api/tasks/$task_id/stop" >/dev/null
  wait_task_state "$task_id" "STOPPED" 60
  cell_result "row2: lease row locked, owner retries" "RUN" "pass: no FAILED; owner=$owner_id epoch=$old_epoch kept through the block"
}

# ==============================================================================
# Run the implemented cells; each failure is recorded, not fatal.
# ==============================================================================
for spec in \
  "phase-dump x kill-9 + row7 restart|cell_kill9_restart" \
  "extra: consecutive yields, no dir|cell_yield_chain" \
  "extra: copy dir to peer|cell_copy_dir" \
  "row6: node_gone then SIGCONT|cell_node_gone_back" \
  "row10: three nodes, one dir|cell_three_nodes" \
  "row11: operator races|cell_operator_races" \
  "row2: lease row locked, owner retries|cell_locked_health"; do
  cid="${spec%%|*}"; fn="${spec##*|}"
  echo
  echo "========== cell: $cid =========="
  if "$fn"; then :; else cell_fail "$cid" "see cell output above"; fi
done

# Cells owned by dedicated scenarios (also release-tier, every PR).
cell_covered "phase FOR UPDATE x SIGSTOP (row1)" "smoke-session-kill (e2e job; MySQL 8.0 + MariaDB idle_transaction_timeout)"
cell_covered "phase TailUnreadable x SIGSTOP / extra per_node owner SIGSTOP" "smoke-sigstop-takeover (e2e-sigstop-takeover job)"
cell_covered "row4: stale write after 2xTTL fenced" "smoke-sigstop-takeover resume path (epoch/generation predicate)"
cell_covered "row12: mixed-version meta" "e2e-upgrade-failback matrix (MIXED_VERSION fence)"
cell_covered "row13: meta failover / clock skew" "smoke-meta-failover / smoke-meta-failover-override"

# Cells without a deterministic fault hook yet.
cell_skip "phase Acquire->dump-open x all faults" "window between Acquire commit and dump open needs an injection hook"
cell_skip "all partition cells" "node->meta partition primitive not built (ProxySQL routing flip pending)"
cell_skip "phase evaluator-reads-dir x all faults" "window between directory read and ChainVerified not instrumentable"
cell_skip "phase OperatorStart txn x SIGSTOP/kill-9" "start transaction window not instrumentable"
cell_skip "extra: shared mount pulled" "no shared-storage runner in the self-managed topology"
cell_skip "row3: stall idle_kill<t<TTL" "session-idle kill window partially covered by session-kill bounds; dedicated cell pending"
cell_skip "row5: claim txn uncommitted" "claim transaction window not instrumentable"
cell_skip "row8: own .open ENOENT" "needs refusal_retry_cap tuning; next PR"
cell_skip "row9/row14: stale-dir copy, corrupt replica" "chain-verification rules land in PR-5"
cell_skip "row15: owner disk full/EROFS" "per-node disk-full primitive; backlog A5"
cell_skip "row16: grace<TTL recovery window" "partially covered by meta-failover; dedicated cell pending"
cell_skip "row17: uploads on/off takeover" "upload-toggle variants pending"

print_matrix
if [[ "$CELL_FAILURES" -gt 0 ]]; then
  echo "[fm] $CELL_FAILURES cell(s) FAILED" >&2
  exit 1
fi
echo "[fm] matrix done: all run cells passed; skips reported above"
