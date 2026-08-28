#!/usr/bin/env bash
# input: isolated metadata DSN, optional E2E_MYSQL57_PORT override, and control-plane failover dependencies
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
MYSQL57_PORT="${E2E_MYSQL57_PORT:-13306}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-control-plane-failover-$(date +%s)}"
WORKER_ID="${E2E_WORKER_ID:-e2e-worker-1}"
WORKER_HEALTH_ADDR="${E2E_WORKER_HEALTH_ADDR:-127.0.0.1:18081}"

RUN_TAG="$(date +%s)"
CONTROL_LOG="${E2E_CONTROL_LOG:-/tmp/binlog-server-e2e-control-failover-${RUN_TAG}.log}"
WORKER_LOG="${E2E_WORKER_LOG:-/tmp/binlog-server-e2e-worker-failover-${RUN_TAG}.log}"

CONTROL_PID=""
WORKER_PID=""
CHECKPOINT_HTTP_CODE=""
CHECKPOINT_HTTP_BODY=""

source "$ROOT_DIR/scripts/e2e/lib-migration.sh"
META_DSN="${E2E_META_DSN:-$(e2e_default_meta_dsn 13316)}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq
e2e_ensure_meta_schema "$ROOT_DIR" "$META_DSN"

cleanup() {
  if [[ -n "$WORKER_PID" ]]; then
    kill "$WORKER_PID" >/dev/null 2>&1 || true
    wait "$WORKER_PID" >/dev/null 2>&1 || true
  fi
  if [[ -n "$CONTROL_PID" ]]; then
    kill "$CONTROL_PID" >/dev/null 2>&1 || true
    wait "$CONTROL_PID" >/dev/null 2>&1 || true
  fi
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
  BINLOG_SERVER_LISTEN_ADDR="127.0.0.1:18080" \
  BINLOG_SERVER_DATA_DIR="$cp_data_dir" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$CONTROL_LOG" 2>&1 &
  CONTROL_PID=$!
}

stop_control_plane() {
  if [[ -z "$CONTROL_PID" ]]; then
    return 0
  fi
  kill "$CONTROL_PID" >/dev/null 2>&1 || true
  wait "$CONTROL_PID" >/dev/null 2>&1 || true
  CONTROL_PID=""
}

start_worker() {
  local worker_data_dir="$DATA_DIR/worker"
  mkdir -p "$worker_data_dir"

  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="worker" \
  BINLOG_SERVER_CLUSTER_WORKER_ID="$WORKER_ID" \
  BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR="$WORKER_HEALTH_ADDR" \
  BINLOG_SERVER_DATA_DIR="$worker_data_dir" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$WORKER_LOG" 2>&1 &
  WORKER_PID=$!
}

wait_worker_online() {
  for _ in {1..60}; do
    local workers
    workers="$(curl -fsS "$API/api/workers?limit=200")"
    if printf '%s' "$workers" | jq -e --arg wid "$WORKER_ID" '.[] | select(.worker_id == $wid and .online == true and (.last_seen_at | tostring | length > 0))' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "worker did not become online in time: worker_id=$WORKER_ID" >&2
  curl -fsS "$API/api/workers?limit=200" >&2 || true
  cat "$WORKER_LOG" >&2 || true
  return 1
}

wait_worker_health_ready() {
  local health_api="http://$WORKER_HEALTH_ADDR"
  for _ in {1..60}; do
    if curl -fsS "$health_api/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "worker health probe not ready in time: addr=$WORKER_HEALTH_ADDR" >&2
  cat "$WORKER_LOG" >&2 || true
  return 1
}

create_task() {
  local sid
  sid=$((370000 + RANDOM % 10000))

  local resp
  resp="$(curl -fsS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"e2e-control-plane-failover-${RUN_TAG}\",\"cluster_key\":\"e2e-control-plane-failover-${RUN_TAG}\",\"source\":{\"host\":\"127.0.0.1\",\"port\":${MYSQL57_PORT},\"user\":\"repl\",\"password\":\"replpass\",\"flavor\":\"mysql\",\"server_id\":${sid}},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")"

  local id
  id="$(printf '%s' "$resp" | jq -r '.id // empty')"
  if [[ -z "$id" || "$id" == "null" ]]; then
    echo "create task failed: $resp" >&2
    exit 1
  fi
  printf '%s' "$id"
}

start_task() {
  local task_id="$1"
  local http_code
  http_code="$(curl -sS -o /tmp/e2e-control-failover-start-${RUN_TAG}.resp -w '%{http_code}' -X POST "$API/api/tasks/$task_id/start")"
  if [[ "$http_code" != "204" ]]; then
    echo "start task failed: http=$http_code body=$(cat /tmp/e2e-control-failover-start-${RUN_TAG}.resp)" >&2
    return 1
  fi
}

task_state() {
  local task_id="$1"
  curl -fsS "$API/api/tasks/$task_id" | jq -r '.state // empty'
}

wait_task_running() {
  local task_id="$1"
  for _ in {1..120}; do
    if [[ "$(task_state "$task_id")" == "RUNNING" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task did not become RUNNING in time: task_id=$task_id" >&2
  curl -fsS "$API/api/tasks/$task_id" >&2 || true
  return 1
}

checkpoint_fetch() {
  local task_id="$1"
  local raw
  raw="$(curl -sS -w $'\n%{http_code}' "$API/api/tasks/$task_id/checkpoint")"
  CHECKPOINT_HTTP_CODE="${raw##*$'\n'}"
  CHECKPOINT_HTTP_BODY="${raw%$'\n'*}"
}

wait_checkpoint_ready() {
  local task_id="$1"
  for _ in {1..120}; do
    checkpoint_fetch "$task_id"
    if [[ "$CHECKPOINT_HTTP_CODE" == "200" ]] && printf '%s' "$CHECKPOINT_HTTP_BODY" | jq -e '.file != null and (.file | tostring | length > 0) and (.pos // 0) >= 0' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "checkpoint not ready in time: task_id=$task_id http=$CHECKPOINT_HTTP_CODE body=$CHECKPOINT_HTTP_BODY" >&2
  return 1
}

checkpoint_file() {
  printf '%s' "$CHECKPOINT_HTTP_BODY" | jq -r '.file // ""'
}

checkpoint_pos() {
  printf '%s' "$CHECKPOINT_HTTP_BODY" | jq -r '.pos // 0'
}

wait_checkpoint_progress() {
  local task_id="$1"
  local before_file="$2"
  local before_pos="$3"

  for _ in {1..180}; do
    checkpoint_fetch "$task_id"
    if [[ "$CHECKPOINT_HTTP_CODE" != "200" ]]; then
      sleep 1
      continue
    fi

    local current_file current_pos
    current_file="$(checkpoint_file)"
    current_pos="$(checkpoint_pos)"
    if [[ "$current_file" != "$before_file" ]]; then
      return 0
    fi
    if [[ "$current_pos" =~ ^[0-9]+$ && "$before_pos" =~ ^[0-9]+$ && "$current_pos" -gt "$before_pos" ]]; then
      return 0
    fi
    sleep 1
  done

  echo "checkpoint did not progress: before=${before_file}:${before_pos} after=${CHECKPOINT_HTTP_BODY}" >&2
  return 1
}

write_source_data() {
  local value="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot \
    -e "INSERT INTO binlog_e2e_57.t1(v) VALUES('${value}');"
}

echo "[control-failover] start control-plane + worker"
start_control_plane
wait_control_plane_ready
echo "[control-failover] control-plane ready pid=$CONTROL_PID"

start_worker
wait_worker_health_ready
echo "[control-failover] worker ready pid=$WORKER_PID"

echo "[control-failover] create + start task"
TASK_ID="$(create_task)"
start_task "$TASK_ID"
wait_task_running "$TASK_ID"
wait_worker_online
echo "[control-failover] task running task_id=$TASK_ID"

wait_checkpoint_ready "$TASK_ID"
A_FILE="$(checkpoint_file)"
A_POS="$(checkpoint_pos)"
echo "[control-failover] checkpoint A: ${A_FILE}:${A_POS}"

echo "[control-failover] write data before control-plane crash"
write_source_data "control-failover-before-crash-${RUN_TAG}"
wait_checkpoint_progress "$TASK_ID" "$A_FILE" "$A_POS"
wait_checkpoint_ready "$TASK_ID"
B_FILE="$(checkpoint_file)"
B_POS="$(checkpoint_pos)"
echo "[control-failover] checkpoint B: ${B_FILE}:${B_POS}"

echo "[control-failover] stop control-plane and keep worker running"
stop_control_plane
sleep 2
wait_worker_health_ready

echo "[control-failover] write data while control-plane is down"
write_source_data "control-failover-during-cp-down-${RUN_TAG}"

echo "[control-failover] restart control-plane"
start_control_plane
wait_control_plane_ready
echo "[control-failover] control-plane restarted pid=$CONTROL_PID"

wait_checkpoint_progress "$TASK_ID" "$B_FILE" "$B_POS"
wait_checkpoint_ready "$TASK_ID"
C_FILE="$(checkpoint_file)"
C_POS="$(checkpoint_pos)"
echo "[control-failover] checkpoint C: ${C_FILE}:${C_POS}"

echo "[control-failover] verify control-plane api after restart"
curl -fsS "$API/healthz" >/dev/null
curl -fsS "$API/api/tasks/$TASK_ID" >/dev/null

echo "[control-failover] verify worker online visibility after control-plane recovery"
wait_worker_online

echo "[control-failover] success"
