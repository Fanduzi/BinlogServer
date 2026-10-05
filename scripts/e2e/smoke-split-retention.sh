#!/usr/bin/env bash
# input: canonical E2E database topology, meta DSN, and Quay MinIO/mc images
# output: proof that a shorter bucket retention is rejected, a retention_days-only task still purges object catalog and disk together, and a longer bucket retention deletes only the local file while download replay and replay/archive still serve the object
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-split-retention-$(date +%s)}"
RUN_TAG="$(date +%s)"

SERVER_LOG="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-split-retention-${RUN_TAG}.log}"
SERVER_PID=""

MINIO_NAME="binlog-e2e-minio-split-${RUN_TAG}"
MINIO_PORT=19010
MINIO_CONSOLE_PORT=19011
MINIO_USER="minioadmin"
MINIO_PASS="minioadmin"
MINIO_BUCKET="e2e-split-retention"
# Docker Hub minio/minio and minio/mc return pull denied. Quay still serves the last public RELEASE images.
MINIO_IMAGE="${MINIO_IMAGE:-quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z}"
MC_IMAGE="${MC_IMAGE:-quay.io/minio/mc:RELEASE.2025-08-13T08-35-41Z}"

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
need_cmd tar
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
  BINLOG_SERVER_UPLOAD_PREFIX="e2e/split-retention" \
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

mc_cmd() {
  docker run --rm --network host \
    -e MC_HOST_local="http://${MINIO_USER}:${MINIO_PASS}@127.0.0.1:${MINIO_PORT}" \
    "$MC_IMAGE" "$@"
}

ensure_minio_bucket() {
  local attempt
  for attempt in $(seq 1 30); do
    if mc_cmd ls "local/${MINIO_BUCKET}" >/dev/null 2>&1; then
      return 0
    fi
    mc_cmd mb -p "local/${MINIO_BUCKET}" >/dev/null 2>&1 || true
    sleep 1
  done
  echo "minio bucket not usable after start" >&2
  return 1
}

object_exists() {
  mc_cmd stat "local/${MINIO_BUCKET}/$1" >/dev/null 2>&1
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

task_payload() {
  local name="$1"
  local sid="$2"
  local storage="$3"
  cat <<JSON
{"name":"$name","cluster_key":"$name","source":{"host":"$E2E_SOURCE_HOST","port":$MYSQL57_PORT,"user":"$E2E_SOURCE_USER","password":"$E2E_SOURCE_PASS","flavor":"mysql","server_id":$sid},"start":{"mode":"LATEST"},"storage":$storage}
JSON
}

create_task() {
  local name="$1"
  local sid="$2"
  local storage="$3"
  local resp
  if ! resp="$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$(task_payload "$name" "$sid" "$storage")")"; then
    echo "create task failed: $name" >&2
    exit 1
  fi
  printf '%s' "$resp"
}

start_task() {
  local task_id="$1"
  local code
  code="$(curl -sS -o "/tmp/e2e-split-start-${task_id}.resp" -w '%{http_code}' -X POST "$API/api/tasks/$task_id/start")"
  if [[ "$code" != "204" ]]; then
    echo "start task failed: http=$code body=$(cat "/tmp/e2e-split-start-${task_id}.resp")" >&2
    exit 1
  fi
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
  curl -fsS "$API/api/tasks/$task_id" >&2 || true
  return 1
}

wait_checkpoint_ready() {
  local task_id="$1"
  for _ in {1..120}; do
    if curl -fsS "$API/api/tasks/$task_id/checkpoint" | jq -e '.file != null and (.file | tostring | length > 0) and (.pos // 0) >= 0' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "checkpoint not ready in time: $task_id" >&2
  curl -fsS "$API/api/tasks/$task_id/checkpoint" >&2 || true
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

files_json() {
  curl -fsS "$API/api/tasks/$1/files?limit=200"
}

uploaded_count() {
  files_json "$1" | jq '[.[] | select(.upload_state=="UPLOADED" and ((.state // "") != "OPEN") and (((.file_name // "") | contains(".open.e")) | not))] | length'
}

wait_uploaded_count() {
  local task_id="$1"
  local want="$2"
  local _
  for _ in $(seq 1 120); do
    local n
    n="$(uploaded_count "$task_id")"
    if [[ "$n" =~ ^[0-9]+$ && "$n" -ge "$want" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "wanted $want uploaded sealed files for $task_id, got $(uploaded_count "$task_id" || true)" >&2
  files_json "$task_id" >&2 || true
  return 1
}

nth_uploaded_name() {
  local task_id="$1"
  local index="$2"
  files_json "$task_id" | jq -r --argjson i "$index" '[.[] | select(.upload_state=="UPLOADED" and ((.state // "") != "OPEN") and (((.file_name // "") | contains(".open.e")) | not)) | .file_name] | sort | .[$i] // empty'
}

file_field() {
  local task_id="$1"
  local name="$2"
  local field="$3"
  files_json "$task_id" | jq -r --arg n "$name" --arg f "$field" '([.[] | select(.file_name==$n) | .[$f]] | .[0] // "")'
}

file_listed() {
  local task_id="$1"
  local name="$2"
  files_json "$task_id" | jq -r --arg n "$name" 'any(.[]; .file_name==$n)'
}

require_local_path() {
  local task_id="$1"
  local name="$2"
  local path
  path="$(file_field "$task_id" "$name" file_path)"
  if [[ -z "$path" || "$path" == "null" || "$path" == "$name" || ! -f "$path" ]]; then
    path="$DATA_DIR/$task_id/$name"
  fi
  if [[ ! -f "$path" ]]; then
    echo "sealed file missing: $name path=$path" >&2
    files_json "$task_id" >&2 || true
    exit 1
  fi
  printf '%s' "$path"
}

wait_full_purge() {
  local task_id="$1"
  local path="$2"
  local name="$3"
  local key="$4"
  local _
  for _ in $(seq 1 90); do
    local listed state
    listed="$(file_listed "$task_id" "$name")"
    state="$(curl -fsS "$API/api/tasks/$task_id" | jq -r '.state // empty')"
    if [[ "$state" == "RETRY_BACKOFF" || "$state" == "FAILED" ]]; then
      echo "task $task_id entered $state during full purge of $name" >&2
      return 1
    fi
    if [[ ! -e "$path" && "$listed" == "false" ]] && ! object_exists "$key"; then
      return 0
    fi
    sleep 1
  done
  echo "full purge did not finish: task=$task_id name=$name path_exists=$([[ -e "$path" ]] && echo yes || echo no) listed=$(file_listed "$task_id" "$name") object=$(object_exists "$key" && echo yes || echo no)" >&2
  files_json "$task_id" >&2 || true
  return 1
}

wait_local_only_purge() {
  local task_id="$1"
  local path="$2"
  local name="$3"
  local _
  for _ in $(seq 1 90); do
    local state location upload object
    state="$(curl -fsS "$API/api/tasks/$task_id" | jq -r '.state // empty')"
    if [[ "$state" == "RETRY_BACKOFF" || "$state" == "FAILED" ]]; then
      echo "task $task_id entered $state during local-only purge of $name" >&2
      return 1
    fi
    location="$(file_field "$task_id" "$name" location)"
    upload="$(file_field "$task_id" "$name" upload_state)"
    object="$(file_field "$task_id" "$name" object_key)"
    if [[ ! -e "$path" && "$location" == "bucket" && "$upload" == "UPLOADED" && -n "$object" && "$object" != "null" ]]; then
      printf '%s' "$object"
      return 0
    fi
    sleep 1
  done
  echo "local-only purge was not visible for $name" >&2
  files_json "$task_id" >&2 || true
  return 1
}

assert_replay_bucket() {
  local path="$1"
  local label="$2"
  shift 2
  local body
  if ! body="$(curl -fsS -G "$@" --data-urlencode "limit=200")"; then
    echo "$label request failed for $path" >&2
    return 1
  fi
  if ! printf '%s' "$body" | jq -e --arg p "$path" '
    (.paths // []) as $ps | (.locations // []) as $ls |
    ($ps | index($p)) as $i | $i != null and $ls[$i] == "bucket"
  ' >/dev/null; then
    echo "$label did not mark $path as bucket: $body" >&2
    return 1
  fi
}

assert_archive_member() {
  local task_id="$1"
  local name="$2"
  local expect="$3"
  local archive="/tmp/e2e-split-archive-${RUN_TAG}.tar"
  local member="/tmp/e2e-split-member-${RUN_TAG}.bin"
  curl -fsS "$API/api/tasks/$task_id/replay/archive?limit=200" -o "$archive"
  if ! tar -xOf "$archive" "$name" >"$member"; then
    echo "replay archive has no member $name" >&2
    tar -tf "$archive" >&2 || true
    return 1
  fi
  if ! cmp -s "$expect" "$member"; then
    echo "replay archive member $name does not match the pre-delete file" >&2
    return 1
  fi
}

echo "[split-retention] start minio + bucket"
start_minio

echo "[split-retention] start binlog-server with upload and catalog"
start_server_with_upload

echo "[split-retention] bucket retention shorter than local is rejected"
SHORT_BODY="/tmp/e2e-split-short-${RUN_TAG}.resp"
SHORT_CODE="$(curl -sS -o "$SHORT_BODY" -w '%{http_code}' -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$(task_payload "e2e-split-short-${RUN_TAG}" $((450000 + RANDOM % 10000)) '{"retention_days":7,"local_retention_days":10,"bucket_retention_days":3}')")"
if [[ "$SHORT_CODE" != "400" ]] || ! grep -q "shorter than local retention" "$SHORT_BODY"; then
  echo "expected 400 shorter than local retention, got http=$SHORT_CODE body=$(cat "$SHORT_BODY")" >&2
  exit 1
fi

echo "[split-retention] retention_days alone keeps the v0.5.28 response and full purge"
LEGACY_RESP="$(create_task "e2e-split-legacy-${RUN_TAG}" $((460000 + RANDOM % 10000)) '{"retention_days":7}')"
if printf '%s' "$LEGACY_RESP" | grep -q 'local_retention_days\|bucket_retention_days'; then
  echo "single-key create response included a split retention field: $LEGACY_RESP" >&2
  exit 1
fi
LEGACY_ID="$(printf '%s' "$LEGACY_RESP" | jq -r '.id // empty')"
if [[ -z "$LEGACY_ID" || "$LEGACY_ID" == "null" ]]; then
  echo "invalid legacy create response: $LEGACY_RESP" >&2
  exit 1
fi
start_task "$LEGACY_ID"
wait_task_running "$LEGACY_ID"
wait_checkpoint_ready "$LEGACY_ID"
write_source_data "legacy-${RUN_TAG}"
flush_binary_logs
wait_uploaded_count "$LEGACY_ID" 1
LEGACY_NAME="$(nth_uploaded_name "$LEGACY_ID" 0)"
LEGACY_PATH="$(require_local_path "$LEGACY_ID" "$LEGACY_NAME")"
LEGACY_KEY="$(file_field "$LEGACY_ID" "$LEGACY_NAME" object_key)"
if [[ -z "$LEGACY_KEY" || "$LEGACY_KEY" == "null" ]]; then
  echo "legacy uploaded file has no object key" >&2
  exit 1
fi
if ! object_exists "$LEGACY_KEY"; then
  echo "legacy object missing before purge: $LEGACY_KEY" >&2
  exit 1
fi
touch -d '10 days ago' "$LEGACY_PATH"
flush_binary_logs
wait_full_purge "$LEGACY_ID" "$LEGACY_PATH" "$LEGACY_NAME" "$LEGACY_KEY"
LEGACY_STATE="$(curl -fsS "$API/api/tasks/$LEGACY_ID" | jq -r '.state // empty')"
if [[ "$LEGACY_STATE" != "RUNNING" ]]; then
  echo "legacy task left RUNNING after full purge: $LEGACY_STATE" >&2
  exit 1
fi

echo "[split-retention] local retention drops the disk file and keeps the object"
SPLIT_RESP="$(create_task "e2e-split-keep-${RUN_TAG}" $((470000 + RANDOM % 10000)) '{"retention_days":7,"local_retention_days":1,"bucket_retention_days":30}')"
if ! printf '%s' "$SPLIT_RESP" | jq -e '.storage.local_retention_days == 1 and .storage.bucket_retention_days == 30 and .storage.retention_days == 7' >/dev/null; then
  echo "split create did not echo both retentions: $SPLIT_RESP" >&2
  exit 1
fi
SPLIT_ID="$(printf '%s' "$SPLIT_RESP" | jq -r '.id')"
start_task "$SPLIT_ID"
wait_task_running "$SPLIT_ID"
wait_checkpoint_ready "$SPLIT_ID"
write_source_data "mid-${RUN_TAG}"
flush_binary_logs
wait_uploaded_count "$SPLIT_ID" 1
write_source_data "old-${RUN_TAG}"
flush_binary_logs
wait_uploaded_count "$SPLIT_ID" 2
MID_NAME="$(nth_uploaded_name "$SPLIT_ID" 0)"
OLD_NAME="$(nth_uploaded_name "$SPLIT_ID" 1)"
if [[ -z "$MID_NAME" || -z "$OLD_NAME" || "$MID_NAME" == "$OLD_NAME" ]]; then
  echo "need two distinct uploaded files, got mid=$MID_NAME old=$OLD_NAME" >&2
  files_json "$SPLIT_ID" >&2 || true
  exit 1
fi
MID_PATH="$(require_local_path "$SPLIT_ID" "$MID_NAME")"
OLD_PATH="$(require_local_path "$SPLIT_ID" "$OLD_NAME")"
OLD_KEY="$(file_field "$SPLIT_ID" "$OLD_NAME" object_key)"
MID_COPY="/tmp/e2e-split-mid-${RUN_TAG}.bin"
cp "$MID_PATH" "$MID_COPY"
touch -d '2 days ago' "$MID_PATH"
touch -d '40 days ago' "$OLD_PATH"
flush_binary_logs
MID_KEY="$(wait_local_only_purge "$SPLIT_ID" "$MID_PATH" "$MID_NAME")"
if ! object_exists "$MID_KEY"; then
  echo "bucket object disappeared with the local file: $MID_KEY" >&2
  exit 1
fi
DOWNLOAD="/tmp/e2e-split-download-${RUN_TAG}.bin"
curl -fsS "$API/api/tasks/$SPLIT_ID/files/$MID_NAME" -o "$DOWNLOAD"
if ! cmp -s "$MID_COPY" "$DOWNLOAD"; then
  echo "downloaded bucket segment does not match the pre-delete file" >&2
  exit 1
fi
assert_replay_bucket "$MID_PATH" "replay" "$API/api/tasks/$SPLIT_ID/replay"
STOP_AT="$(date -u -d 'tomorrow' +'%Y-%m-%d %H:%M:%S')"
assert_replay_bucket "$MID_PATH" "replay stop_datetime" "$API/api/tasks/$SPLIT_ID/replay" --data-urlencode "stop_datetime=${STOP_AT}"
assert_archive_member "$SPLIT_ID" "$MID_NAME" "$MID_COPY"
wait_full_purge "$SPLIT_ID" "$OLD_PATH" "$OLD_NAME" "$OLD_KEY"
SPLIT_STATE="$(curl -fsS "$API/api/tasks/$SPLIT_ID" | jq -r '.state // empty')"
if [[ "$SPLIT_STATE" != "RUNNING" ]]; then
  echo "split task left RUNNING: $SPLIT_STATE" >&2
  exit 1
fi
if [[ "$(file_field "$SPLIT_ID" "$MID_NAME" location)" != "bucket" ]]; then
  echo "bucket-only segment disappeared while purging the older file" >&2
  files_json "$SPLIT_ID" >&2 || true
  exit 1
fi

echo "[split-retention] success"
