#!/usr/bin/env bash
# input: canonical E2E database topology, retry-upload e2e dependencies, and Quay MinIO/mc images
# output: deterministic e2e orchestration, background retry of sealed UPLOAD_FAILED rows without the manual API, retention that keeps an expired unuploaded sealed file until it uploads and is then purged, checksum match on the files API, MinIO mismatch proof, and verification logs
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
# Docker Hub minio/minio and minio/mc return pull denied (verified 2026-09-21).
# Official community MinIO is source-only; dl.min.io historical binaries return 410.
# Quay still serves the last public RELEASE images.
MINIO_IMAGE="${MINIO_IMAGE:-quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z}"
MC_IMAGE="${MC_IMAGE:-quay.io/minio/mc:RELEASE.2025-08-13T08-35-41Z}"

CHECKPOINT_HTTP_CODE=""
CHECKPOINT_HTTP_BODY=""

source "$ROOT_DIR/scripts/e2e/lib-migration.sh"
MYSQL57_PORT="$E2E_MYSQL57_PORT"
META_DSN="$(e2e_meta_dsn direct)"

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
  # MinIO can answer /minio/health/live and then reject mc with unauthorized
  # for a short window. Retry until the bucket is listable. A lasting auth
  # failure still fails this scenario.
  local attempt
  for attempt in $(seq 1 30); do
    if docker run --rm --network host \
      -e MC_HOST_local="http://${MINIO_USER}:${MINIO_PASS}@127.0.0.1:${MINIO_PORT}" \
      "$MC_IMAGE" ls "local/${MINIO_BUCKET}" >/dev/null 2>&1; then
      return 0
    fi
    docker run --rm --network host \
      -e MC_HOST_local="http://${MINIO_USER}:${MINIO_PASS}@127.0.0.1:${MINIO_PORT}" \
      "$MC_IMAGE" mb -p "local/${MINIO_BUCKET}" >/dev/null 2>&1 || true
    sleep 1
  done
  echo "minio bucket not usable after start (unauthorized or not created)" >&2
  return 1
}

retention_skip_count() {
  local name="$1"
  curl -fsS "$API/api/tasks/$TASK_ID/events?limit=200" | jq --arg n "$name" '[.[] | select(.type=="RETENTION_SKIPPED_NOT_UPLOADED" and ((.message // "") | contains($n)) and ((.message // "") | contains("UPLOAD_FAILED")))] | length'
}

retention_blocked_gauge() {
  local id="$1"
  curl -fsS "$API/metrics" | awk -v id="$id" '
    index($0, "binlog_server_retention_blocked_files{task_id=\"" id "\"}") { print $NF; found=1 }
    END { if (!found) print "" }
  '
}

wait_retention_kept() {
  local path="$1"
  local name="$2"
  local _
  for _ in $(seq 1 90); do
    if [[ ! -e "$path" ]]; then
      echo "retention deleted the only copy: $path" >&2
      return 1
    fi
    local state
    state="$(curl -fsS "$API/api/tasks/$TASK_ID" | jq -r '.state // empty')"
    if [[ "$state" == "RETRY_BACKOFF" || "$state" == "FAILED" ]]; then
      echo "task entered $state during retention skip" >&2
      curl -fsS "$API/api/tasks/$TASK_ID" >&2 || true
      return 1
    fi
    if [[ "$(retention_skip_count "$name")" == "1" && "$state" == "RUNNING" ]]; then
      local gauge
      gauge="$(retention_blocked_gauge "$TASK_ID")"
      if [[ "$gauge" =~ ^[0-9]+$ && "$gauge" -ge 1 ]]; then
        return 0
      fi
    fi
    sleep 1
  done
  echo "retention skip was not visible for $name" >&2
  curl -fsS "$API/api/tasks/$TASK_ID/events?limit=200" >&2 || true
  curl -fsS "$API/metrics" >&2 || true
  return 1
}

wait_retention_purged() {
  local path="$1"
  local name="$2"
  local _
  for _ in $(seq 1 90); do
    local listed gauge
    listed="$(curl -fsS "$API/api/tasks/$TASK_ID/files?limit=200" | jq --arg n "$name" 'if type=="array" then any(.[]; .file_name==$n) else true end')"
    gauge="$(retention_blocked_gauge "$TASK_ID")"
    if [[ ! -e "$path" && "$listed" == "false" && "$gauge" == "0" ]]; then
      local state
      state="$(curl -fsS "$API/api/tasks/$TASK_ID" | jq -r '.state // empty')"
      if [[ "$state" == "RETRY_BACKOFF" || "$state" == "FAILED" ]]; then
        echo "task entered $state while purging an uploaded file" >&2
        return 1
      fi
      return 0
    fi
    sleep 1
  done
  echo "uploaded file was not purged: path=$path exists=$([[ -e "$path" ]] && echo yes || echo no) gauge=$(retention_blocked_gauge "$TASK_ID")" >&2
  curl -fsS "$API/api/tasks/$TASK_ID/files?limit=200" >&2 || true
  return 1
}

start_minio() {
  docker rm -f "$MINIO_NAME" >/dev/null 2>&1 || true
  docker pull "$MINIO_IMAGE"
  docker pull "$MC_IMAGE"
  docker run -d --name "$MINIO_NAME" \
    -p "${MINIO_PORT}:9000" \
    -p "${MINIO_CONSOLE_PORT}:9001" \
    -e "MINIO_ROOT_USER=${MINIO_USER}" \
    -e "MINIO_ROOT_PASSWORD=${MINIO_PASS}" \
    "$MINIO_IMAGE" server /data --console-address ":9001" >/dev/null
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
{"name":"$name","cluster_key":"$name","source":{"host":"$E2E_SOURCE_HOST","port":$MYSQL57_PORT,"user":"$E2E_SOURCE_USER","password":"$E2E_SOURCE_PASS","flavor":"mysql","server_id":$sid},"start":{"mode":"LATEST"},"storage":{"retention_days":7}}
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

wait_background_uploaded_record() {
  local task_id="$1"
  # 15s background interval, plus an in-flight attempt that can sit in the upload timeout while MinIO is down.
  for _ in {1..90}; do
    if curl -fsS "$API/api/tasks/$task_id/files?limit=200" | jq -e 'def open: (.state == "OPEN") or ((.file_name // "") | contains(".open.e")) or ((.file_path // "") | contains(".open.e")); if type=="array" then any(.[]; (open | not) and .upload_state=="UPLOADED") and all(.[]; open or .upload_state != "UPLOAD_FAILED") and all(.[]; (open | not) or .upload_state != "UPLOADED") else false end' >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "background retry did not upload sealed UPLOAD_FAILED rows: task_id=$task_id" >&2
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

echo "[retry-upload] age the failed sealed file past retention and rotate again"
FAILED_ROW="$(curl -fsS "$API/api/tasks/$TASK_ID/files?limit=200" | jq -r 'if type=="array" then [ .[] | select(.upload_state=="UPLOAD_FAILED" and ((.state // "") != "OPEN") and (((.file_name // "") | contains(".open.e")) | not) and (((.file_path // "") | contains(".open.e")) | not)) ] | .[0] | "\(.file_name)\t\(.file_path)" else empty end')"
FAILED_NAME="${FAILED_ROW%%$'\t'*}"
FAILED_PATH="${FAILED_ROW#*$'\t'}"
if [[ -z "$FAILED_NAME" || "$FAILED_NAME" == "null" ]]; then
  echo "no sealed UPLOAD_FAILED file to age" >&2
  exit 1
fi
if [[ -z "$FAILED_PATH" || "$FAILED_PATH" == "null" || "$FAILED_PATH" == "$FAILED_NAME" ]]; then
  FAILED_PATH="$DATA_DIR/$TASK_ID/$FAILED_NAME"
fi
if [[ ! -f "$FAILED_PATH" ]]; then
  echo "sealed file missing before retention: $FAILED_PATH" >&2
  exit 1
fi
touch -d '10 days ago' "$FAILED_PATH"
flush_binary_logs
wait_retention_kept "$FAILED_PATH" "$FAILED_NAME"

echo "[retry-upload] another rotate does not repeat the retention event"
checkpoint_fetch "$TASK_ID"
DEDUP_FILE="$(checkpoint_file)"
flush_binary_logs
rotated=""
for _ in $(seq 1 90); do
  checkpoint_fetch "$TASK_ID"
  current="$(checkpoint_file)"
  if [[ "$CHECKPOINT_HTTP_CODE" == "200" && -n "$current" && "$current" != "$DEDUP_FILE" ]]; then
    epoch="$(curl -fsS "$API/api/tasks/$TASK_ID" | jq -r '.epoch // 0')"
    if [[ -f "$DATA_DIR/$TASK_ID/${current}.open.e${epoch}" ]]; then
      rotated=1
      break
    fi
  fi
  sleep 1
done
if [[ -z "$rotated" ]]; then
  echo "second rotate did not open a new file from $DEDUP_FILE" >&2
  exit 1
fi
if [[ ! -f "$FAILED_PATH" ]]; then
  echo "retention deleted $FAILED_PATH on the second rotate" >&2
  exit 1
fi
SKIP_COUNT="$(retention_skip_count "$FAILED_NAME")"
if [[ "$SKIP_COUNT" != "1" ]]; then
  echo "expected one retention skip event, got $SKIP_COUNT" >&2
  curl -fsS "$API/api/tasks/$TASK_ID/events?limit=200" >&2 || true
  exit 1
fi

echo "[retry-upload] verify checkpoint still progresses while upload fails"
write_source_data "retry-progress-${RUN_TAG}"
wait_checkpoint_progress "$TASK_ID" "$BASE_FILE" "$BASE_POS"

echo "[retry-upload] recover minio and wait for background retry"
docker start "$MINIO_NAME" >/dev/null
wait_minio_live
ensure_minio_bucket

wait_background_uploaded_record "$TASK_ID"
if ! grep -q "background upload retry" "$SERVER_LOG"; then
  echo "background upload retry did not run" >&2
  exit 1
fi

echo "[retry-upload] uploaded expired file is purged on the next retention pass"
UPLOADED_STATE="$(curl -fsS "$API/api/tasks/$TASK_ID/files?limit=200" | jq -r --arg n "$FAILED_NAME" 'if type=="array" then ([.[] | select(.file_name==$n) | .upload_state] | .[0] // "") else "" end')"
if [[ "$UPLOADED_STATE" != "UPLOADED" ]]; then
  echo "aged file was not uploaded before purge: state=$UPLOADED_STATE" >&2
  curl -fsS "$API/api/tasks/$TASK_ID/files?limit=200" >&2 || true
  exit 1
fi
if [[ ! -f "$FAILED_PATH" ]]; then
  echo "aged file disappeared before the uploaded purge: $FAILED_PATH" >&2
  exit 1
fi
touch -d '10 days ago' "$FAILED_PATH"
flush_binary_logs
wait_retention_purged "$FAILED_PATH" "$FAILED_NAME"

echo "[retry-upload] uploaded sealed segment reports checksum match"
if ! curl -fsS "$API/api/tasks/$TASK_ID/files?limit=200" | jq -e 'if type=="array" then any(.[]; .upload_state=="UPLOADED" and .checksum=="match" and (.file_name | contains(".open.e") | not)) and all(.[]; .upload_state != "UPLOADED" or .checksum == "match") else false end' >/dev/null; then
  echo "uploaded file checksum is not match" >&2
  curl -fsS "$API/api/tasks/$TASK_ID/files?limit=200" >&2 || true
  exit 1
fi

echo "[retry-upload] real object mismatch is checksum mismatch and does not fail the upload caller"
(
  cd "$ROOT_DIR"
  BINLOG_E2E_MINIO_ENDPOINT="127.0.0.1:${MINIO_PORT}" \
  BINLOG_E2E_MINIO_BUCKET="$MINIO_BUCKET" \
  BINLOG_E2E_MINIO_ACCESS_KEY="$MINIO_USER" \
  BINLOG_E2E_MINIO_SECRET_KEY="$MINIO_PASS" \
    go test ./internal/tasks -count=1 -timeout 180s -run 'TestApplySealedUpload_MinIOChecksum$'
)

echo "[retry-upload] verify replication still progresses after retry"
wait_checkpoint_ready "$TASK_ID"
POST_RETRY_FILE="$(checkpoint_file)"
POST_RETRY_POS="$(checkpoint_pos)"
write_source_data "retry-after-${RUN_TAG}"
wait_checkpoint_progress "$TASK_ID" "$POST_RETRY_FILE" "$POST_RETRY_POS"

echo "[retry-upload] success"
