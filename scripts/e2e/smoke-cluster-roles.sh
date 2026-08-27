#!/usr/bin/env bash
# input: isolated metadata DSN plus local tooling and cluster-role e2e dependencies
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-cluster-roles-$(date +%s)}"
WORKER_ID="${E2E_WORKER_ID:-e2e-worker-1}"
WORKER_OFFLINE_WAIT_SEC="${E2E_WORKER_OFFLINE_WAIT_SEC:-20}"
WORKER_HEALTH_ADDR="${E2E_WORKER_HEALTH_ADDR:-127.0.0.1:18081}"

RUN_TAG="$(date +%s)"
CONTROL_LOG="${E2E_CONTROL_LOG:-/tmp/binlog-server-e2e-control-${RUN_TAG}.log}"
WORKER_LOG="${E2E_WORKER_LOG:-/tmp/binlog-server-e2e-worker-${RUN_TAG}.log}"

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

stop_worker() {
  if [[ -z "$WORKER_PID" ]]; then
    return 0
  fi
  kill "$WORKER_PID" >/dev/null 2>&1 || true
  wait "$WORKER_PID" >/dev/null 2>&1 || true
  WORKER_PID=""
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

wait_worker_offline() {
  for _ in {1..90}; do
    local workers
    workers="$(curl -fsS "$API/api/workers?limit=200")"
    if printf '%s' "$workers" | jq -e --arg wid "$WORKER_ID" '.[] | select(.worker_id == $wid and .online == false)' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "worker did not become offline in time: worker_id=$WORKER_ID" >&2
  curl -fsS "$API/api/workers?limit=200" >&2 || true
  return 1
}

wait_worker_health_ready() {
  local health_api="http://$WORKER_HEALTH_ADDR"
  for _ in {1..60}; do
    if curl -fsS "$health_api/healthz" >/dev/null 2>&1 && curl -fsS "$health_api/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "worker health probe not ready in time: addr=$WORKER_HEALTH_ADDR" >&2
  cat "$WORKER_LOG" >&2 || true
  return 1
}

assert_worker_health_api_blocked() {
  local health_api="http://$WORKER_HEALTH_ADDR"
  local http_code
  http_code="$(curl -sS -o /tmp/e2e-worker-health-api-${RUN_TAG}.resp -w '%{http_code}' "$health_api/api/tasks")"
  if [[ "$http_code" != "404" ]]; then
    echo "expected worker health probe to block /api/*, got http=$http_code body=$(cat /tmp/e2e-worker-health-api-${RUN_TAG}.resp)" >&2
    return 1
  fi
}

create_task() {
  local sid
  sid=$((360000 + RANDOM % 10000))

  local resp
  resp="$(curl -fsS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"e2e-cluster-roles-${RUN_TAG}\",\"cluster_key\":\"e2e-cluster-roles-${RUN_TAG}\",\"source\":{\"host\":\"127.0.0.1\",\"port\":13306,\"user\":\"repl\",\"password\":\"replpass\",\"flavor\":\"mysql\",\"server_id\":${sid}},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")"

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
  http_code="$(curl -sS -o /tmp/e2e-cluster-start-${RUN_TAG}.resp -w '%{http_code}' -X POST "$API/api/tasks/$task_id/start")"
  if [[ "$http_code" != "204" ]]; then
    echo "start task failed: http=$http_code body=$(cat /tmp/e2e-cluster-start-${RUN_TAG}.resp)" >&2
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

echo "[cluster-roles] start control-plane process"
start_control_plane
wait_control_plane_ready
echo "[cluster-roles] control-plane ready pid=$CONTROL_PID"

echo "[cluster-roles] start worker process"
start_worker
wait_worker_online
wait_worker_health_ready
assert_worker_health_api_blocked
echo "[cluster-roles] worker online worker_id=$WORKER_ID pid=$WORKER_PID"

echo "[cluster-roles] create + start task via control-plane api"
TASK_ID="$(create_task)"
start_task "$TASK_ID"

# 在线 worker 应该在常驻状态下直接接管 STARTING 任务，无需重启。
wait_task_running "$TASK_ID"
echo "[cluster-roles] task running task_id=$TASK_ID"

wait_checkpoint_ready "$TASK_ID"
BEFORE_FILE="$(checkpoint_file)"
BEFORE_POS="$(checkpoint_pos)"
echo "[cluster-roles] checkpoint before write: ${BEFORE_FILE}:${BEFORE_POS}"

echo "[cluster-roles] write source data and verify checkpoint progress"
write_source_data "cluster-roles-before-failover-${RUN_TAG}"
wait_checkpoint_progress "$TASK_ID" "$BEFORE_FILE" "$BEFORE_POS"
wait_checkpoint_ready "$TASK_ID"
AFTER_FILE="$(checkpoint_file)"
AFTER_POS="$(checkpoint_pos)"
echo "[cluster-roles] checkpoint after write: ${AFTER_FILE}:${AFTER_POS}"

echo "[cluster-roles] stop worker and wait offline detection"
stop_worker
sleep "$WORKER_OFFLINE_WAIT_SEC"
wait_worker_offline
echo "[cluster-roles] worker offline confirmed"

wait_checkpoint_ready "$TASK_ID"
RECOVER_BEFORE_FILE="$(checkpoint_file)"
RECOVER_BEFORE_POS="$(checkpoint_pos)"

echo "[cluster-roles] restart worker and verify recovery"
start_worker
wait_worker_online
wait_worker_health_ready
wait_task_running "$TASK_ID"
write_source_data "cluster-roles-after-recover-${RUN_TAG}"
wait_checkpoint_progress "$TASK_ID" "$RECOVER_BEFORE_FILE" "$RECOVER_BEFORE_POS"
wait_checkpoint_ready "$TASK_ID"
RECOVER_AFTER_FILE="$(checkpoint_file)"
RECOVER_AFTER_POS="$(checkpoint_pos)"
echo "[cluster-roles] checkpoint after recovery: ${RECOVER_AFTER_FILE}:${RECOVER_AFTER_POS}"

echo "[cluster-roles] success"
