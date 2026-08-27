#!/usr/bin/env bash
# input: isolated metadata DSN plus local tooling and worker crash-recovery dependencies
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-worker-crash-recovery-$(date +%s)}"
WORKER_SHARED_DIR="${E2E_WORKER_SHARED_DIR:-$DATA_DIR/worker-shared}"
WORKER1_ID="${E2E_WORKER1_ID:-e2e-worker-crash-1}"
WORKER2_ID="${E2E_WORKER2_ID:-e2e-worker-crash-2}"
WORKER1_HEALTH_ADDR="${E2E_WORKER1_HEALTH_ADDR:-127.0.0.1:18081}"
WORKER2_HEALTH_ADDR="${E2E_WORKER2_HEALTH_ADDR:-127.0.0.1:18082}"

RUN_TAG="$(date +%s)"
CONTROL_LOG="${E2E_CONTROL_LOG:-/tmp/binlog-server-e2e-crash-control-${RUN_TAG}.log}"
WORKER1_LOG="${E2E_WORKER1_LOG:-/tmp/binlog-server-e2e-crash-worker1-${RUN_TAG}.log}"
WORKER2_LOG="${E2E_WORKER2_LOG:-/tmp/binlog-server-e2e-crash-worker2-${RUN_TAG}.log}"

CONTROL_PID=""
WORKER1_PID=""
WORKER2_PID=""
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
need_cmd awk
e2e_ensure_meta_schema "$ROOT_DIR" "$META_DSN"

kill_pid() {
  local pid="$1"
  if [[ -z "$pid" ]]; then
    return 0
  fi
  kill "$pid" >/dev/null 2>&1 || true
  wait "$pid" >/dev/null 2>&1 || true
}

cleanup() {
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
  BINLOG_SERVER_LISTEN_ADDR="127.0.0.1:18080" \
  BINLOG_SERVER_DATA_DIR="$cp_data_dir" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$CONTROL_LOG" 2>&1 &
  CONTROL_PID=$!
}

start_worker() {
  local worker_id="$1"
  local health_addr="$2"
  local log_file="$3"

  mkdir -p "$WORKER_SHARED_DIR"
  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="worker" \
  BINLOG_SERVER_CLUSTER_WORKER_ID="$worker_id" \
  BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR="$health_addr" \
  BINLOG_SERVER_DATA_DIR="$WORKER_SHARED_DIR" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$log_file" 2>&1 &
  echo "$!"
}

wait_worker_online() {
  local worker_id="$1"
  for _ in {1..90}; do
    local workers
    workers="$(curl -fsS "$API/api/workers?limit=200")"
    if printf '%s' "$workers" | jq -e --arg wid "$worker_id" '.[] | select(.worker_id == $wid and .online == true and (.last_seen_at | tostring | length > 0))' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "worker did not become online in time: worker_id=$worker_id" >&2
  curl -fsS "$API/api/workers?limit=200" >&2 || true
  return 1
}

create_task() {
  local sid
  sid=$((390000 + RANDOM % 10000))

  local resp
  resp="$(curl -fsS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"e2e-worker-crash-${RUN_TAG}\",\"cluster_key\":\"e2e-worker-crash-${RUN_TAG}\",\"source\":{\"host\":\"127.0.0.1\",\"port\":13306,\"user\":\"repl\",\"password\":\"replpass\",\"flavor\":\"mysql\",\"server_id\":${sid}},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")"

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
  http_code="$(curl -sS -o /tmp/e2e-worker-crash-start-${RUN_TAG}.resp -w '%{http_code}' -X POST "$API/api/tasks/$task_id/start")"
  if [[ "$http_code" != "204" ]]; then
    echo "start task failed: http=$http_code body=$(cat /tmp/e2e-worker-crash-start-${RUN_TAG}.resp)" >&2
    return 1
  fi
}

stop_task() {
  local task_id="$1"
  local http_code
  http_code="$(curl -sS -o /tmp/e2e-worker-crash-stop-${RUN_TAG}.resp -w '%{http_code}' -X POST "$API/api/tasks/$task_id/stop")"
  if [[ "$http_code" != "204" ]]; then
    echo "stop task failed: http=$http_code body=$(cat /tmp/e2e-worker-crash-stop-${RUN_TAG}.resp)" >&2
    return 1
  fi
}

task_state() {
  local task_id="$1"
  curl -fsS "$API/api/tasks/$task_id" | jq -r '.state // empty'
}

task_epoch() {
  local task_id="$1"
  curl -fsS "$API/api/tasks/$task_id" | jq -r '.epoch // 0'
}

wait_task_state() {
  local task_id="$1"
  local expected="$2"
  for _ in {1..120}; do
    if [[ "$(task_state "$task_id")" == "$expected" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task did not become $expected in time: task_id=$task_id" >&2
  curl -fsS "$API/api/tasks/$task_id" >&2 || true
  return 1
}

wait_task_epoch_gt() {
  local task_id="$1"
  local old_epoch="$2"
  for _ in {1..120}; do
    local current
    current="$(task_epoch "$task_id")"
    if [[ "$current" =~ ^[0-9]+$ && "$old_epoch" =~ ^[0-9]+$ && "$current" -gt "$old_epoch" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task epoch did not increase in time: task_id=$task_id old_epoch=$old_epoch current=$(task_epoch "$task_id")" >&2
  return 1
}

wait_takeover_running() {
  local task_id="$1"
  local old_epoch="$2"
  local re_dispatch=0

  for _ in {1..240}; do
    local state current_epoch
    state="$(task_state "$task_id")"
    current_epoch="$(task_epoch "$task_id")"

    if [[ "$state" == "RUNNING" && "$current_epoch" =~ ^[0-9]+$ && "$old_epoch" =~ ^[0-9]+$ && "$current_epoch" -gt "$old_epoch" ]]; then
      return 0
    fi

    # worker2 首次恢复若因 lease 未过期导致 STOPPED，则通过 control-plane 重新 dispatch。
    if [[ "$state" == "STOPPED" && "$re_dispatch" -eq 0 ]]; then
      echo "[worker-crash] task is STOPPED during takeover; dispatch start once via control-plane"
      start_task "$task_id"
      re_dispatch=1
    fi
    sleep 1
  done

  echo "task did not recover to RUNNING with newer epoch in time: task_id=$task_id state=$(task_state "$task_id") epoch=$(task_epoch "$task_id") old_epoch=$old_epoch" >&2
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

wait_checkpoint_file_changed() {
  local task_id="$1"
  local before_file="$2"
  for _ in {1..180}; do
    checkpoint_fetch "$task_id"
    if [[ "$CHECKPOINT_HTTP_CODE" != "200" ]]; then
      sleep 1
      continue
    fi
    local current_file
    current_file="$(checkpoint_file)"
    if [[ -n "$current_file" && "$current_file" != "$before_file" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "checkpoint file did not rotate from $before_file in time: task_id=$task_id body=$CHECKPOINT_HTTP_BODY" >&2
  return 1
}

write_source_data() {
  local value="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot \
    -e "INSERT INTO binlog_e2e_57.t1(v) VALUES('${value}-1'),('${value}-2'),('${value}-3');"
}

flush_binary_logs() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot -e "FLUSH BINARY LOGS;"
}

wait_open_file() {
  local task_dir="$1"
  for _ in {1..120}; do
    if [[ -d "$task_dir" ]]; then
      local path
      path="$(find "$task_dir" -maxdepth 1 -type f -name '*.open.e*' | head -n 1)"
      if [[ -n "$path" ]]; then
        printf '%s' "$path"
        return 0
      fi
    fi
    sleep 1
  done
  echo "open file not found in time: task_dir=$task_dir" >&2
  return 1
}

wait_no_open_files() {
  local task_dir="$1"
  for _ in {1..120}; do
    local count
    count="$(find "$task_dir" -maxdepth 1 -type f -name '*.open.e*' | wc -l | tr -d ' ')"
    if [[ "$count" == "0" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "open files still exist after waiting: task_dir=$task_dir" >&2
  find "$task_dir" -maxdepth 1 -type f -name '*.open.e*' >&2 || true
  return 1
}

log_open_files() {
  local task_dir="$1"
  find "$task_dir" -maxdepth 1 -type f -name '*.open.e*' -print || true
}

assert_no_old_epoch_open_files() {
  local task_dir="$1"
  local old_epoch="$2"
  if find "$task_dir" -maxdepth 1 -type f -name "*.open.e${old_epoch}" | grep -q .; then
    echo "found stale old-epoch open files: epoch=$old_epoch task_dir=$task_dir" >&2
    find "$task_dir" -maxdepth 1 -type f -name "*.open.e${old_epoch}" >&2 || true
    return 1
  fi
}

assert_open_files_only_current_epoch() {
  local task_dir="$1"
  local current_epoch="$2"
  local file base suffix epoch_text
  while IFS= read -r file; do
    base="$(basename "$file")"
    suffix="${base##*.open.e}"
    if [[ "$suffix" == "$base" ]]; then
      echo "unexpected open file name format: $base" >&2
      return 1
    fi
    epoch_text="$suffix"
    if [[ ! "$epoch_text" =~ ^[0-9]+$ ]]; then
      echo "unexpected open file epoch suffix: $base" >&2
      return 1
    fi
    if [[ "$epoch_text" != "$current_epoch" ]]; then
      echo "found non-current-epoch open file: file=$base current_epoch=$current_epoch" >&2
      return 1
    fi
  done < <(find "$task_dir" -maxdepth 1 -type f -name '*.open.e*' | sort)
}

collect_sealed_files() {
  local task_dir="$1"
  local file
  while IFS= read -r file; do
    file="$(basename "$file")"
    if [[ "$file" =~ ^mysql-bin\.[0-9]{6}$ ]]; then
      printf '%s\n' "$file"
    fi
  done < <(find "$task_dir" -maxdepth 1 -type f | sort)
}

verify_unique_sealed_files() {
  local files=("$@")
  local i j
  for ((i=0; i<${#files[@]}; i++)); do
    for ((j=i+1; j<${#files[@]}; j++)); do
      if [[ "${files[$i]}" == "${files[$j]}" ]]; then
        echo "duplicate sealed file detected: ${files[$i]}" >&2
        return 1
      fi
    done
  done
}

assert_no_abnormal_sealed_files() {
  local files=("$@")
  local base
  for base in "${files[@]}"; do
    if [[ ! "$base" =~ ^mysql-bin\.[0-9]{6}$ ]]; then
      echo "found abnormal sealed file name (unexpected suffix): $base" >&2
      return 1
    fi
  done
}

md5_local() {
  local file="$1"
  if command -v md5sum >/dev/null 2>&1; then
    md5sum "$file" | awk '{print $1}'
    return
  fi
  if command -v md5 >/dev/null 2>&1; then
    md5 -q "$file"
    return
  fi
  echo "missing md5 tool (md5sum/md5)" >&2
  exit 1
}

md5_in_container() {
  local file="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 sh -lc '
set -e
f="$1"
if command -v md5sum >/dev/null 2>&1; then
  md5sum "$f" | awk "{print \$1}"
elif command -v md5 >/dev/null 2>&1; then
  md5 -q "$f"
else
  openssl md5 "$f" | awk -F "= " "{print \$2}"
fi
' sh "/var/lib/mysql/$file" | tr -d '\r'
}

echo "[worker-crash] start control-plane + worker1"
start_control_plane
wait_control_plane_ready
WORKER1_PID="$(start_worker "$WORKER1_ID" "$WORKER1_HEALTH_ADDR" "$WORKER1_LOG")"
wait_worker_online "$WORKER1_ID"
echo "[worker-crash] control-plane pid=$CONTROL_PID worker1 pid=$WORKER1_PID"

echo "[worker-crash] create + start task"
TASK_ID="$(create_task)"
TASK_DIR="$WORKER_SHARED_DIR/$TASK_ID"
start_task "$TASK_ID"
wait_task_state "$TASK_ID" "RUNNING"
OLD_EPOCH="$(task_epoch "$TASK_ID")"
if [[ ! "$OLD_EPOCH" =~ ^[0-9]+$ || "$OLD_EPOCH" -le 0 ]]; then
  echo "invalid initial epoch before crash: $OLD_EPOCH" >&2
  exit 1
fi
echo "[worker-crash] task running task_id=$TASK_ID old_epoch=$OLD_EPOCH"

echo "[worker-crash] write source data and rotate once"
write_source_data "worker-crash-before-rotate-${RUN_TAG}"
flush_binary_logs
wait_checkpoint_ready "$TASK_ID"
BASE_FILE="$(checkpoint_file)"
BASE_POS="$(checkpoint_pos)"
echo "[worker-crash] checkpoint after first rotate: ${BASE_FILE}:${BASE_POS}"

echo "[worker-crash] write more data and wait OPEN file, then crash worker1"
write_source_data "worker-crash-open-window-${RUN_TAG}"
OPEN_FILE="$(wait_open_file "$TASK_DIR")"
echo "[worker-crash] open file before crash: $OPEN_FILE"
kill -9 "$WORKER1_PID" >/dev/null 2>&1 || true
wait "$WORKER1_PID" >/dev/null 2>&1 || true
WORKER1_PID=""
sleep 1

echo "[worker-crash] start worker2 takeover"
WORKER2_PID="$(start_worker "$WORKER2_ID" "$WORKER2_HEALTH_ADDR" "$WORKER2_LOG")"
wait_worker_online "$WORKER2_ID"
wait_takeover_running "$TASK_ID" "$OLD_EPOCH"
NEW_EPOCH="$(task_epoch "$TASK_ID")"
echo "[worker-crash] worker2 takeover complete new_epoch=$NEW_EPOCH worker2 pid=$WORKER2_PID"

echo "[worker-crash] write data after takeover and verify checkpoint keeps moving"
write_source_data "worker-crash-after-takeover-${RUN_TAG}"
flush_binary_logs
wait_checkpoint_file_changed "$TASK_ID" "$BASE_FILE"
wait_checkpoint_progress "$TASK_ID" "$BASE_FILE" "$BASE_POS"
wait_checkpoint_ready "$TASK_ID"
AFTER_FILE="$(checkpoint_file)"
AFTER_POS="$(checkpoint_pos)"
echo "[worker-crash] checkpoint after takeover: ${AFTER_FILE}:${AFTER_POS}"

echo "[worker-crash] stop task and wait settle for strict file checks"
stop_task "$TASK_ID"
wait_task_state "$TASK_ID" "STOPPED"
assert_no_old_epoch_open_files "$TASK_DIR" "$OLD_EPOCH"
assert_open_files_only_current_epoch "$TASK_DIR" "$NEW_EPOCH"
echo "[worker-crash] open files after stop (current epoch allowed):"
log_open_files "$TASK_DIR"

SEALED_FILES=()
while IFS= read -r file; do
  if [[ -n "$file" ]]; then
    SEALED_FILES+=("$file")
  fi
done < <(collect_sealed_files "$TASK_DIR")
if [[ "${#SEALED_FILES[@]}" -lt 1 ]]; then
  echo "no sealed files found in task dir: $TASK_DIR" >&2
  exit 1
fi
assert_no_abnormal_sealed_files "${SEALED_FILES[@]}"
verify_unique_sealed_files "${SEALED_FILES[@]}"
echo "[worker-crash] sealed files count=${#SEALED_FILES[@]} files=${SEALED_FILES[*]}"

SAMPLE_COUNT=2
if [[ "${#SEALED_FILES[@]}" -lt 2 ]]; then
  SAMPLE_COUNT=1
fi

echo "[worker-crash] md5 compare source vs backup (sample_count=$SAMPLE_COUNT)"
for ((i=0; i<SAMPLE_COUNT; i++)); do
  idx=$((${#SEALED_FILES[@]} - 1 - i))
  file="${SEALED_FILES[$idx]}"
  src_md5="$(md5_in_container "$file")"
  bak_md5="$(md5_local "$TASK_DIR/$file")"
  echo "[worker-crash] md5 file=$file src=$src_md5 bak=$bak_md5"
  if [[ "$src_md5" != "$bak_md5" ]]; then
    echo "md5 mismatch for file=$file" >&2
    exit 1
  fi
done

echo "[worker-crash] success"
