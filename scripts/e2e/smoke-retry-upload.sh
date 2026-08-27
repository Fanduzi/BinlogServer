#!/usr/bin/env bash
# input: isolated metadata DSN plus local tooling and retry-upload e2e dependencies
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-retry-upload-$(date +%s)}"
RUN_TAG="$(date +%s)"

SERVER_LOG="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-retry-upload-${RUN_TAG}.log}"
SERVER_PID=""

MINIO_NAME="binlog-e2e-minio-retry-${RUN_TAG}"
MINIO_PORT=19000
MINIO_CONSOLE_PORT=19001
MINIO_USER="minioadmin"
MINIO_PASS="minioadmin"
MINIO_BUCKET="e2e-retry-upload"

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
need_cmd go
e2e_ensure_meta_schema "$ROOT_DIR" "$META_DSN"

kill_server() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" >/dev/null 2>&1 || true
    SERVER_PID=""
  fi
}

cleanup() {
  kill_server
  docker rm -f "$MINIO_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_api_ready() {
  for _ in {1..120}; do
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "binlog-server not ready in time" >&2
  cat "$SERVER_LOG" >&2 || true
  return 1
}

start_server_with_upload() {
  mkdir -p "$DATA_DIR"
  BINLOG_SERVER_DATA_DIR="$DATA_DIR" \
  BINLOG_SERVER_UPLOAD_ENDPOINT="127.0.0.1:${MINIO_PORT}" \
  BINLOG_SERVER_UPLOAD_BUCKET="$MINIO_BUCKET" \
  BINLOG_SERVER_UPLOAD_ACCESS_KEY="$MINIO_USER" \
  BINLOG_SERVER_UPLOAD_SECRET_KEY="$MINIO_PASS" \
  BINLOG_SERVER_UPLOAD_REGION="us-east-1" \
  BINLOG_SERVER_UPLOAD_PREFIX="e2e/retry-upload" \
  BINLOG_SERVER_UPLOAD_USE_SSL="false" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$SERVER_LOG" 2>&1 &
  SERVER_PID=$!
  wait_api_ready
}

wait_minio_live() {
  for _ in {1..60}; do
    if curl -fsS "http://127.0.0.1:${MINIO_PORT}/minio/health/live" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "minio not ready in time" >&2
  return 1
}

ensure_minio_bucket() {
  docker run --rm --network host \
    -e MC_HOST_local="http://${MINIO_USER}:${MINIO_PASS}@127.0.0.1:${MINIO_PORT}" \
    minio/mc mb -p "local/${MINIO_BUCKET}" >/dev/null 2>&1 || true
}

start_minio() {
  docker rm -f "$MINIO_NAME" >/dev/null 2>&1 || true
  docker run -d --name "$MINIO_NAME" \
    -p "${MINIO_PORT}:9000" \
    -p "${MINIO_CONSOLE_PORT}:9001" \
    -e "MINIO_ROOT_USER=${MINIO_USER}" \
    -e "MINIO_ROOT_PASSWORD=${MINIO_PASS}" \
    minio/minio server /data --console-address ":9001" >/dev/null
  wait_minio_live
  ensure_minio_bucket
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
  echo "checkpoint not ready in time: task_id=$task_id body=$CHECKPOINT_HTTP_BODY" >&2
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
  echo "checkpoint did not progress from ${before_file}:${before_pos}" >&2
  return 1
}

create_task() {
  local sid
  sid=$((420000 + RANDOM % 10000))
  local name="e2e-retry-upload-${RUN_TAG}"
  local payload
  payload="$(cat <<JSON
{"name":"$name","cluster_key":"$name","source":{"host":"127.0.0.1","port":13306,"user":"repl","password":"replpass","flavor":"mysql","server_id":$sid},"start":{"mode":"LATEST"},"storage":{"retention_days":7}}
JSON
)"
  local resp
  if ! resp="$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$payload")"; then
    echo "create task failed" >&2
    exit 1
  fi
  local task_id
  task_id="$(printf '%s' "$resp" | jq -r '.id // empty')"
  if [[ -z "$task_id" || "$task_id" == "null" ]]; then
    echo "invalid create response: $resp" >&2
    exit 1
  fi
  local code
  code="$(curl -sS -o /tmp/e2e-retry-upload-start-${RUN_TAG}.resp -w '%{http_code}' -X POST "$API/api/tasks/$task_id/start")"
  if [[ "$code" != "204" ]]; then
    echo "start task failed: http=$code body=$(cat /tmp/e2e-retry-upload-start-${RUN_TAG}.resp)" >&2
    exit 1
  fi
  printf '%s' "$task_id"
}

wait_task_running() {
  local task_id="$1"
  for _ in {1..120}; do
    local state
    state="$(curl -fsS "$API/api/tasks/$task_id" | jq -r '.state // empty')"
    if [[ "$state" == "RUNNING" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task not running in time: $task_id" >&2
  return 1
}

write_source_data() {
  local value="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot \
    -e "INSERT INTO binlog_e2e_57.t1(v) VALUES('${value}-1'),('${value}-2');"
}

flush_binary_logs() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot -e "FLUSH BINARY LOGS;"
}

wait_failed_upload_record() {
  local task_id="$1"
  for _ in {1..120}; do
    if curl -fsS "$API/api/tasks/$task_id/files?limit=200" | jq -e 'if type=="array" then any(.[]; .upload_state=="UPLOAD_FAILED" and (.file_name | contains(".open.e") | not)) else false end' >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "failed upload record not found in time: task_id=$task_id" >&2
  curl -fsS "$API/api/tasks/$task_id/files?limit=200" >&2 || true
  return 1
}

wait_uploaded_record() {
  local task_id="$1"
  for _ in {1..60}; do
    if curl -fsS "$API/api/tasks/$task_id/files?limit=200" | jq -e 'if type=="array" then any(.[]; .upload_state=="UPLOADED") else false end' >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "uploaded file record not found in time after retry: task_id=$task_id" >&2
  curl -fsS "$API/api/tasks/$task_id/files?limit=200" >&2 || true
  return 1
}

echo "[retry-upload] start minio + bucket"
start_minio

echo "[retry-upload] start binlog-server with upload enabled"
start_server_with_upload

echo "[retry-upload] create and start task"
TASK_ID="$(create_task)"
wait_task_running "$TASK_ID"
wait_checkpoint_ready "$TASK_ID"

BASE_FILE="$(checkpoint_file)"
BASE_POS="$(checkpoint_pos)"

echo "[retry-upload] stop minio to force upload failure"
docker stop "$MINIO_NAME" >/dev/null

echo "[retry-upload] write + rotate to produce UPLOAD_FAILED records"
write_source_data "retry-fail-${RUN_TAG}"
flush_binary_logs
wait_failed_upload_record "$TASK_ID"

echo "[retry-upload] verify checkpoint still progresses while upload fails"
write_source_data "retry-progress-${RUN_TAG}"
wait_checkpoint_progress "$TASK_ID" "$BASE_FILE" "$BASE_POS"

echo "[retry-upload] recover minio then trigger manual retry"
docker start "$MINIO_NAME" >/dev/null
wait_minio_live
ensure_minio_bucket

RETRY_RESP="$(curl -fsS -X POST "$API/api/tasks/$TASK_ID/files/retry-upload?limit=100")"
echo "[retry-upload] retry result: $RETRY_RESP"
SUCCEEDED="$(printf '%s' "$RETRY_RESP" | jq -r '.succeeded // 0')"
if [[ ! "$SUCCEEDED" =~ ^[0-9]+$ ]] || (( SUCCEEDED < 1 )); then
  echo "expected retry succeeded >= 1, got $SUCCEEDED" >&2
  exit 1
fi
wait_uploaded_record "$TASK_ID"

echo "[retry-upload] verify replication still progresses after retry"
wait_checkpoint_ready "$TASK_ID"
POST_RETRY_FILE="$(checkpoint_file)"
POST_RETRY_POS="$(checkpoint_pos)"
write_source_data "retry-after-${RUN_TAG}"
wait_checkpoint_progress "$TASK_ID" "$POST_RETRY_FILE" "$POST_RETRY_POS"

echo "[retry-upload] success"
