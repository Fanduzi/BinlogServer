#!/usr/bin/env bash
# input: meta MySQL, a source MySQL, and a data directory
# output: proof that a sealed UPLOADED segment stays in the catalog and in the printed restore command after a later open epoch of the same source file name
# pos: integration coverage for one catalog row per durable epoch
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-epoch-segments-$(date +%s)}"
RUN_TAG="$(date +%s)"
SERVER_LOG="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-epoch-segments-${RUN_TAG}.log}"
SERVER_PID=""
SOURCE_FILE="mysql-bin.000176"

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

start_server() {
  mkdir -p "$DATA_DIR"
  BINLOG_SERVER_DATA_DIR="$DATA_DIR" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$SERVER_LOG" 2>&1 &
  SERVER_PID=$!
  wait_api_ready
}

meta_sql() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary \
    mysql -uroot -proot binlog_meta -Nse "$1" | tr -d '\r'
}

echo "[epoch-segments] start server"
start_server

name="e2e-epoch-segments-${RUN_TAG}"
resp="$(curl -sS -X POST "$API/api/tasks" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"$name\",\"cluster_key\":\"$name\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":$MYSQL57_PORT,\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":417600},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")"
task_id="$(printf '%s' "$resp" | jq -r '.id // empty')"
if [[ -z "$task_id" || "$task_id" == "null" ]]; then
  echo "create task failed: $resp" >&2
  exit 1
fi
echo "[epoch-segments] task $task_id"

kill_server

task_dir="$DATA_DIR/$task_id"
mkdir -p "$task_dir"
sealed_path="$task_dir/$SOURCE_FILE"
go run "$ROOT_DIR/scripts/e2e/gen-timed-segment.go" "$sealed_path" \
  "2020-01-01T00:00:00Z" "2020-01-01T00:10:00Z"
sealed_size="$(wc -c <"$sealed_path" | tr -d ' ')"

meta_sql "INSERT INTO backup_checkpoints (task_id, file_name, pos, gtid_set, updated_at) VALUES ('${task_id}', '${SOURCE_FILE}', 44, '', UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE file_name=VALUES(file_name), pos=VALUES(pos), updated_at=VALUES(updated_at)"
meta_sql "INSERT INTO binlog_files (task_id, file_name, source_file, file_path, epoch, state, checksum, size_bytes, start_pos, end_pos, created_at, sealed_at, object_key, upload_state, upload_error, uploaded_at) VALUES ('${task_id}', '${SOURCE_FILE}', '${SOURCE_FILE}', '${sealed_path}', 0, 'SEALED', 'match', ${sealed_size}, 4, 44, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'e2e/legacy/${SOURCE_FILE}', 'UPLOADED', '', UTC_TIMESTAMP(6))"

echo "[epoch-segments] restart and open the next epoch"
start_server
curl -sS -X POST "$API/api/tasks/${task_id}/start" >/dev/null || true

# A fresh standalone lease is epoch 1, so the new segment is name.open.e1.
open_path="$task_dir/${SOURCE_FILE}.open.e1"
for _ in {1..60}; do
  if [[ -f "$open_path" ]]; then
    break
  fi
  sleep 1
done
if [[ ! -f "$open_path" ]]; then
  echo "open epoch was not created" >&2
  cat "$SERVER_LOG" >&2 || true
  exit 1
fi

kill_server
meta_sql "UPDATE backup_tasks SET state='STOPPED', epoch=0 WHERE id='${task_id}'"
go run "$ROOT_DIR/scripts/e2e/gen-timed-segment.go" "$open_path" \
  "2020-01-01T00:20:00Z" "2020-01-01T00:30:00Z"

echo "[epoch-segments] catalog and restore command"
start_server

api_get() {
  local url="$1" body_file http_code
  body_file="$(mktemp)"
  http_code="$(curl -sS -o "$body_file" -w '%{http_code}' "$url")" || true
  if [[ "$http_code" != "200" ]]; then
    echo "GET $url HTTP $http_code" >&2
    cat "$body_file" >&2 || true
    echo >&2
    cat "$SERVER_LOG" >&2 || true
    rm -f "$body_file"
    exit 1
  fi
  cat "$body_file"
  rm -f "$body_file"
}

files="$(api_get "$API/api/tasks/${task_id}/files")"
sealed_state="$(printf '%s' "$files" | jq -r --arg p "$sealed_path" '.[] | select(.file_path==$p) | "\(.epoch // 0) \(.state) \(.upload_state) \(.checksum) \(.object_key)"')"
open_state="$(printf '%s' "$files" | jq -r --arg p "$open_path" '.[] | select(.file_path==$p) | "\(.epoch) \(.state) \(.file_name)"')"
if [[ "$sealed_state" != "0 SEALED UPLOADED match e2e/legacy/${SOURCE_FILE}" ]]; then
  echo "sealed catalog row changed: $sealed_state" >&2
  echo "$files" >&2
  exit 1
fi
if [[ "$open_state" != "1 OPEN ${SOURCE_FILE}" ]]; then
  echo "open catalog row: $open_state" >&2
  echo "$files" >&2
  exit 1
fi

replay="$(api_get "$API/api/tasks/${task_id}/replay")"
printf '%s' "$replay" | jq -e --arg sealed "$sealed_path" --arg open "$open_path" '
  (.paths | index($sealed) != null) and (.paths | index($open) != null) and
  ((.paths | index($sealed)) < (.paths | index($open)))
' >/dev/null

pitr="$(api_get "$API/api/tasks/${task_id}/replay?start_datetime=2020-01-01%2000:05:00&stop_datetime=2020-01-01%2000:25:00")"
printf '%s' "$pitr" | jq -e --arg sealed "$sealed_path" --arg open "$open_path" '
  (.paths | index($sealed) != null) and (.paths | index($open) != null) and
  ((.command | contains($sealed)) and (.command | contains($open)))
' >/dev/null

rows="$(meta_sql "SELECT COUNT(*) FROM binlog_files WHERE task_id='${task_id}' AND file_name='${SOURCE_FILE}'")"
rows="$(printf '%s' "$rows" | tr -d '[:space:]')"
if [[ "$rows" != "2" ]]; then
  echo "catalog rows=$rows want 2" >&2
  exit 1
fi

echo "[epoch-segments] success: sealed epoch 0 and open epoch 1 are both in the restore command"
