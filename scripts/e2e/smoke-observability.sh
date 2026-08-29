#!/usr/bin/env bash
# input: local tooling, observability dependencies, and the canonical E2E database topology
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
MYSQL57_PORT="$E2E_MYSQL57_PORT"
RUN_TAG="$(date +%s)"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

fetch_metrics() {
  curl -fsS "$API/metrics"
}

assert_core_metrics_exist() {
  local body="$1"
  local names=(
    "binlog_server_task_state_count"
    "binlog_server_replication_lag_seconds"
    "binlog_server_checkpoint_age_seconds"
    "binlog_server_worker_online"
    "binlog_server_upload_failures_total"
  )
  local name
  for name in "${names[@]}"; do
    if ! printf '%s\n' "$body" | grep -q "$name"; then
      echo "missing core metric: $name" >&2
      exit 1
    fi
  done
}

metric_value_with_labels() {
  local body="$1"
  local metric="$2"
  local labels="$3"
  local value
  value="$(printf '%s\n' "$body" | awk -v m="$metric" -v l="$labels" '
    {
      key=$1
      if (l == "" && key == m) {
        print $2
        found=1
        exit
      }
      if (l != "" && key == (m "{" l "}")) {
        print $2
        found=1
        exit
      }
    }
    END {
      if (!found) {
        print ""
      }
    }
  ')"
  printf '%s' "$value"
}

metric_value_or_zero() {
  local body="$1"
  local metric="$2"
  local labels="$3"
  local value
  value="$(metric_value_with_labels "$body" "$metric" "$labels")"
  if [[ -z "$value" ]]; then
    echo "0"
    return
  fi
  echo "$value"
}

float_gt() {
  local left="$1"
  local right="$2"
  awk -v l="$left" -v r="$right" 'BEGIN { if (l > r) exit 0; exit 1 }'
}

create_task() {
  local sid
  sid=$((380000 + RANDOM % 10000))
  local resp
  resp="$(curl -fsS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"e2e-observability-${RUN_TAG}\",\"cluster_key\":\"e2e-observability-${RUN_TAG}\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":${MYSQL57_PORT},\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":${sid}},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")"
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
  http_code="$(curl -sS -o /tmp/e2e-observability-start-${RUN_TAG}.resp -w '%{http_code}' -X POST "$API/api/tasks/$task_id/start")"
  if [[ "$http_code" != "204" ]]; then
    echo "start task failed: http=$http_code body=$(cat /tmp/e2e-observability-start-${RUN_TAG}.resp)" >&2
    exit 1
  fi
}

stop_task() {
  local task_id="$1"
  local http_code
  http_code="$(curl -sS -o /tmp/e2e-observability-stop-${RUN_TAG}.resp -w '%{http_code}' -X POST "$API/api/tasks/$task_id/stop")"
  if [[ "$http_code" != "204" ]]; then
    echo "stop task failed: http=$http_code body=$(cat /tmp/e2e-observability-stop-${RUN_TAG}.resp)" >&2
    exit 1
  fi
}

wait_task_state() {
  local task_id="$1"
  local expected="$2"
  for _ in {1..120}; do
    local state
    state="$(curl -fsS "$API/api/tasks/$task_id" | jq -r '.state // empty')"
    if [[ "$state" == "$expected" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task did not become $expected in time: task_id=$task_id" >&2
  curl -fsS "$API/api/tasks/$task_id" >&2 || true
  exit 1
}

write_source_data() {
  local value="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot \
    -e "INSERT INTO binlog_e2e_57.t1(v) VALUES('${value}');"
}

wait_checkpoint_ready() {
  local task_id="$1"
  for _ in {1..120}; do
    local raw http_code body
    raw="$(curl -sS -w $'\n%{http_code}' "$API/api/tasks/$task_id/checkpoint")"
    http_code="${raw##*$'\n'}"
    body="${raw%$'\n'*}"
    if [[ "$http_code" == "200" ]] && printf '%s' "$body" | jq -e '.file != null and (.file | tostring | length > 0)' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "checkpoint not ready in time: task_id=$task_id" >&2
  exit 1
}

echo "[observability] create task"
TASK_ID="$(create_task)"

echo "[observability] verify /metrics exposes core metric names"
METRICS_BEFORE_START="$(fetch_metrics)"
assert_core_metrics_exist "$METRICS_BEFORE_START"

CREATED_BEFORE="$(metric_value_or_zero "$METRICS_BEFORE_START" "binlog_server_task_state_count" "state=\"CREATED\"")"
RUNNING_BEFORE="$(metric_value_or_zero "$METRICS_BEFORE_START" "binlog_server_task_state_count" "state=\"RUNNING\"")"
STOPPED_BEFORE="$(metric_value_or_zero "$METRICS_BEFORE_START" "binlog_server_task_state_count" "state=\"STOPPED\"")"
echo "[observability] state before start: created=$CREATED_BEFORE running=$RUNNING_BEFORE stopped=$STOPPED_BEFORE"

echo "[observability] start task and wait RUNNING"
start_task "$TASK_ID"
wait_task_state "$TASK_ID" "RUNNING"

METRICS_RUNNING="$(fetch_metrics)"
CREATED_AFTER_START="$(metric_value_or_zero "$METRICS_RUNNING" "binlog_server_task_state_count" "state=\"CREATED\"")"
RUNNING_AFTER_START="$(metric_value_or_zero "$METRICS_RUNNING" "binlog_server_task_state_count" "state=\"RUNNING\"")"
echo "[observability] state after start: created=$CREATED_AFTER_START running=$RUNNING_AFTER_START"

if ! float_gt "$RUNNING_AFTER_START" "$RUNNING_BEFORE"; then
  echo "expected RUNNING task_state_count to increase: before=$RUNNING_BEFORE after=$RUNNING_AFTER_START" >&2
  exit 1
fi

echo "[observability] write source data and wait checkpoint"
write_source_data "observability-${RUN_TAG}"
wait_checkpoint_ready "$TASK_ID"

echo "[observability] stop task and wait STOPPED"
stop_task "$TASK_ID"
wait_task_state "$TASK_ID" "STOPPED"

METRICS_STOPPED="$(fetch_metrics)"
STOPPED_AFTER="$(metric_value_or_zero "$METRICS_STOPPED" "binlog_server_task_state_count" "state=\"STOPPED\"")"
echo "[observability] state after stop: stopped=$STOPPED_AFTER"

if ! float_gt "$STOPPED_AFTER" "$STOPPED_BEFORE"; then
  echo "expected STOPPED task_state_count to increase: before=$STOPPED_BEFORE after=$STOPPED_AFTER" >&2
  exit 1
fi

AGE_1="$(metric_value_with_labels "$METRICS_STOPPED" "binlog_server_checkpoint_age_seconds" "task_id=\"${TASK_ID}\"")"
if [[ -z "$AGE_1" ]]; then
  echo "expected checkpoint_age metric for task_id=$TASK_ID after task is stopped" >&2
  exit 1
fi
echo "[observability] checkpoint_age first=$AGE_1"

sleep 3
METRICS_AGE_2="$(fetch_metrics)"
AGE_2="$(metric_value_with_labels "$METRICS_AGE_2" "binlog_server_checkpoint_age_seconds" "task_id=\"${TASK_ID}\"")"
if [[ -z "$AGE_2" ]]; then
  echo "expected checkpoint_age metric for task_id=$TASK_ID on second scrape" >&2
  exit 1
fi
echo "[observability] checkpoint_age second=$AGE_2"

if ! float_gt "$AGE_2" "$AGE_1"; then
  echo "expected checkpoint_age_seconds to increase: first=$AGE_1 second=$AGE_2" >&2
  exit 1
fi

echo "[observability] success"
