#!/usr/bin/env bash
# input: local tooling (docker/curl/jq/go) and e2e environment/service dependencies
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module AGENTS.md.
set -euo pipefail

API="${E2E_API:-http://127.0.0.1:18080}"
RUN_TAG="$(date +%s)"
BASE_CLUSTER_KEY="e2e-invalid-input-base-${RUN_TAG}"
UPDATED_CLUSTER_KEY="e2e-invalid-input-updated-${RUN_TAG}"
BASE_NAME="e2e-invalid-input-base-${RUN_TAG}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd jq

assert_post_400() {
  local body="$1"
  local resp_file
  resp_file="$(mktemp)"
  local code
  code="$(curl -sS -o "$resp_file" -w '%{http_code}' -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
  if [[ "$code" != "400" ]]; then
    echo "expected POST /api/tasks => 400, got $code body=$(cat "$resp_file")" >&2
    rm -f "$resp_file"
    exit 1
  fi
  rm -f "$resp_file"
}

assert_put_400() {
  local task_id="$1"
  local body="$2"
  local resp_file
  resp_file="$(mktemp)"
  local code
  code="$(curl -sS -o "$resp_file" -w '%{http_code}' -X PUT "$API/api/tasks/$task_id" -H 'Content-Type: application/json' -d "$body")"
  if [[ "$code" != "400" ]]; then
    echo "expected PUT /api/tasks/$task_id => 400, got $code body=$(cat "$resp_file")" >&2
    rm -f "$resp_file"
    exit 1
  fi
  rm -f "$resp_file"
}

assert_task_cluster_key() {
  local task_id="$1"
  local expected="$2"
  local resp
  if ! resp="$(curl -fsS "$API/api/tasks/$task_id")"; then
    echo "failed to query task $task_id after invalid update" >&2
    exit 1
  fi
  local actual
  actual="$(printf '%s' "$resp" | jq -r '.cluster_key // empty')"
  if [[ "$actual" != "$expected" ]]; then
    echo "task $task_id cluster_key changed unexpectedly: expected=$expected actual=$actual" >&2
    exit 1
  fi
}

assert_task_baseline_unchanged() {
  local task_id="$1"
  local resp
  if ! resp="$(curl -fsS "$API/api/tasks/$task_id")"; then
    echo "failed to query task $task_id after invalid update" >&2
    exit 1
  fi

  local actual_name actual_key actual_host actual_port actual_user actual_start actual_retention
  actual_name="$(printf '%s' "$resp" | jq -r '.name // empty')"
  actual_key="$(printf '%s' "$resp" | jq -r '.cluster_key // empty')"
  actual_host="$(printf '%s' "$resp" | jq -r '.source.host // empty')"
  actual_port="$(printf '%s' "$resp" | jq -r '.source.port // 0')"
  actual_user="$(printf '%s' "$resp" | jq -r '.source.user // empty')"
  actual_start="$(printf '%s' "$resp" | jq -r '.start.mode // empty')"
  actual_retention="$(printf '%s' "$resp" | jq -r '.storage.retention_days // 0')"

  [[ "$actual_name" == "$BASE_NAME" ]] || { echo "task $task_id name changed unexpectedly: $actual_name" >&2; exit 1; }
  [[ "$actual_key" == "$BASE_CLUSTER_KEY" ]] || { echo "task $task_id cluster_key changed unexpectedly: $actual_key" >&2; exit 1; }
  [[ "$actual_host" == "127.0.0.1" ]] || { echo "task $task_id source.host changed unexpectedly: $actual_host" >&2; exit 1; }
  [[ "$actual_port" == "13306" ]] || { echo "task $task_id source.port changed unexpectedly: $actual_port" >&2; exit 1; }
  [[ "$actual_user" == "repl" ]] || { echo "task $task_id source.user changed unexpectedly: $actual_user" >&2; exit 1; }
  [[ "$actual_start" == "LATEST" ]] || { echo "task $task_id start.mode changed unexpectedly: $actual_start" >&2; exit 1; }
  [[ "$actual_retention" == "7" ]] || { echo "task $task_id storage.retention_days changed unexpectedly: $actual_retention" >&2; exit 1; }
}

create_valid_task() {
  local sid
  sid=$((400000 + RANDOM % 10000))
  local name="$BASE_NAME"
  local payload
  payload="$(cat <<JSON
{"name":"$name","cluster_key":"$BASE_CLUSTER_KEY","source":{"host":"127.0.0.1","port":13306,"user":"repl","password":"replpass","flavor":"mysql","server_id":$sid},"start":{"mode":"LATEST"},"storage":{"retention_days":7}}
JSON
)"
  local resp
  if ! resp="$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$payload")"; then
    echo "failed to create valid task for invalid-input scenario" >&2
    exit 1
  fi
  local task_id
  task_id="$(printf '%s' "$resp" | jq -r '.id // empty')"
  if [[ -z "$task_id" || "$task_id" == "null" ]]; then
    echo "invalid create response: $resp" >&2
    exit 1
  fi
  printf '%s' "$task_id"
}

echo "[invalid-inputs] verify create rejects invalid cluster_key"
assert_post_400 '{"name":"invalid-a","cluster_key":"../bad","source":{"host":"127.0.0.1","port":13306,"user":"repl","flavor":"mysql","server_id":410001}}'

echo "[invalid-inputs] verify create rejects invalid source.host"
assert_post_400 '{"name":"invalid-b","cluster_key":"invalid-b","source":{"host":"bad host","port":13306,"user":"repl","flavor":"mysql","server_id":410002}}'

echo "[invalid-inputs] create one valid task for update checks"
TASK_ID="$(create_valid_task)"

echo "[invalid-inputs] verify update rejects invalid start.mode"
assert_put_400 "$TASK_ID" "{\"name\":\"updated-name\",\"cluster_key\":\"$UPDATED_CLUSTER_KEY\",\"start\":{\"mode\":\"BAD_MODE\"}}"
assert_task_cluster_key "$TASK_ID" "$BASE_CLUSTER_KEY"
assert_task_baseline_unchanged "$TASK_ID"

echo "[invalid-inputs] verify update rejects invalid storage.retention_days"
assert_put_400 "$TASK_ID" "{\"name\":\"updated-name\",\"cluster_key\":\"$UPDATED_CLUSTER_KEY\",\"storage\":{\"retention_days\":0}}"
assert_task_cluster_key "$TASK_ID" "$BASE_CLUSTER_KEY"
assert_task_baseline_unchanged "$TASK_ID"

echo "[invalid-inputs] success"
