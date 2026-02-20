#!/usr/bin/env bash
set -euo pipefail

API="${E2E_API:-http://127.0.0.1:18080}"
RUN_TAG="$(date +%s)"

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

create_valid_task() {
  local sid
  sid=$((400000 + RANDOM % 10000))
  local name="e2e-invalid-input-base-${RUN_TAG}"
  local cluster_key="e2e-invalid-input-base-${RUN_TAG}"
  local payload
  payload="$(cat <<JSON
{"name":"$name","cluster_key":"$cluster_key","source":{"host":"127.0.0.1","port":13306,"user":"repl","password":"replpass","flavor":"mysql","server_id":$sid},"start":{"mode":"LATEST"},"storage":{"retention_days":7}}
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
assert_put_400 "$TASK_ID" '{"name":"updated-name","cluster_key":"e2e-invalid-input-base","start":{"mode":"BAD_MODE"}}'

echo "[invalid-inputs] verify update rejects invalid storage.retention_days"
assert_put_400 "$TASK_ID" '{"name":"updated-name","cluster_key":"e2e-invalid-input-base","storage":{"retention_days":0}}'

echo "[invalid-inputs] success"
