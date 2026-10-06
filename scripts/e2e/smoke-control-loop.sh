#!/usr/bin/env bash
# input: mysql57 source, a scratch metadata database at schema 3, and the current binlog-server binary
# output: one Binlog Dump after a password change during Stop then Start, zero dumps when state is STOPPED, and a v0.5.45-shaped RUNNING row that stays running after upgrade reconcile
# pos: acceptance check for the desired-run control loop, issue 193, and the spec_revision=0 upgrade reconcile
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-control-loop-$(date +%s)}"
RUN_TAG="$(date +%s)"
CONTROL_LOG="${E2E_CONTROL_LOG:-/tmp/binlog-server-e2e-ctl-loop-control-${RUN_TAG}.log}"
WORKER_LOG="${E2E_WORKER_LOG:-/tmp/binlog-server-e2e-ctl-loop-worker-${RUN_TAG}.log}"
ALL_LOG="${E2E_ALL_LOG:-/tmp/binlog-server-e2e-ctl-loop-all-${RUN_TAG}.log}"
WORKER_ID="${E2E_WORKER_ID:-e2e-ctl-worker}"
WORKER_HEALTH_ADDR="${E2E_WORKER_HEALTH_ADDR:-127.0.0.1:18081}"
SCRATCH_DB="binlog_meta_ctlloop"
DUMP_USER="e2ectl${RUN_TAG: -6}"
UPG_USER="e2eupg${RUN_TAG: -6}"
PASS1="ctlpass1"
PASS2="ctlpass2"

CONTROL_PID=""
WORKER_PID=""
ALL_PID=""

source "$ROOT_DIR/scripts/e2e/lib-migration.sh"
MYSQL57_PORT="$E2E_MYSQL57_PORT"
META_DSN="$(e2e_meta_dsn direct)"
SCRATCH_DSN="$(printf '%s' "$META_DSN" | sed -E "s#^(.*@tcp\\([^)]+\\)/)[^/?]+#\\1${SCRATCH_DB}#")"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq
need_cmd go

kill_pid() {
  local pid="$1"
  if [[ -n "$pid" ]]; then
    kill "$pid" >/dev/null 2>&1 || true
    wait "$pid" >/dev/null 2>&1 || true
  fi
}

cleanup() {
  kill_pid "$ALL_PID"
  kill_pid "$WORKER_PID"
  kill_pid "$CONTROL_PID"
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary \
    mysql -uroot -proot -e "DROP DATABASE IF EXISTS ${SCRATCH_DB};" >/dev/null 2>&1 || true
}
trap cleanup EXIT

meta_exec() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary \
    mysql -uroot -proot "$SCRATCH_DB" -Nse "$1" | tr -d '\r'
}

source_exec() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 \
    mysql -uroot -proot -e "$1"
}

dump_count() {
  local user="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 \
    mysql -N -uroot -proot -e "SELECT COUNT(*) FROM information_schema.processlist WHERE USER='${user}' AND COMMAND LIKE 'Binlog Dump%'" | tr -d '\r'
}

wait_http() {
  local url="$1"
  local log="$2"
  for _ in {1..120}; do
    if curl -fsS "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "not ready: $url" >&2
  cat "$log" >&2 || true
  return 1
}

start_control_plane() {
  mkdir -p "$DATA_DIR/control-plane"
  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="control-plane" \
  BINLOG_SERVER_LISTEN_ADDR="127.0.0.1:18080" \
  BINLOG_SERVER_DATA_DIR="$DATA_DIR/control-plane" \
  BINLOG_SERVER_META_DSN="$SCRATCH_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$CONTROL_LOG" 2>&1 &
  CONTROL_PID=$!
  wait_http "$API/healthz" "$CONTROL_LOG"
}

start_worker() {
  mkdir -p "$DATA_DIR/worker"
  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="worker" \
  BINLOG_SERVER_CLUSTER_WORKER_ID="$WORKER_ID" \
  BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR="$WORKER_HEALTH_ADDR" \
  BINLOG_SERVER_DATA_DIR="$DATA_DIR/worker" \
  BINLOG_SERVER_META_DSN="$SCRATCH_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$WORKER_LOG" 2>&1 &
  WORKER_PID=$!
  wait_http "http://$WORKER_HEALTH_ADDR/healthz" "$WORKER_LOG"
}

stop_pair() {
  kill_pid "$WORKER_PID"
  WORKER_PID=""
  kill_pid "$CONTROL_PID"
  CONTROL_PID=""
}

start_all_in_one() {
  mkdir -p "$DATA_DIR/all"
  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="all-in-one" \
  BINLOG_SERVER_CLUSTER_WORKER_ID="e2e-ctl-all" \
  BINLOG_SERVER_LISTEN_ADDR="127.0.0.1:18080" \
  BINLOG_SERVER_DATA_DIR="$DATA_DIR/all" \
  BINLOG_SERVER_META_DSN="$SCRATCH_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$ALL_LOG" 2>&1 &
  ALL_PID=$!
  wait_http "$API/healthz" "$ALL_LOG"
}

task_json() {
  curl -fsS "$API/api/tasks/$1"
}

task_state() {
  task_json "$1" | jq -r '.state // empty'
}

wait_state() {
  local id="$1" want="$2"
  for _ in {1..90}; do
    if [[ "$(task_state "$id")" == "$want" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task $id did not become $want; body=$(task_json "$id")" >&2
  return 1
}

wait_dumps() {
  local user="$1" want="$2"
  for _ in {1..60}; do
    if [[ "$(dump_count "$user")" == "$want" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "user $user dumps=$(dump_count "$user") want=$want" >&2
  return 1
}

echo "[control-loop] scratch database $SCRATCH_DB"
docker compose -f "$COMPOSE_FILE" exec -T meta-primary \
  mysql -uroot -proot -e "DROP DATABASE IF EXISTS ${SCRATCH_DB}; CREATE DATABASE ${SCRATCH_DB} DEFAULT CHARACTER SET utf8mb4; GRANT ALL PRIVILEGES ON ${SCRATCH_DB}.* TO 'meta'@'%'; FLUSH PRIVILEGES;"
(
  cd "$ROOT_DIR"
  META_DSN="$SCRATCH_DSN" go run ./cmd/migrate up
)

source_exec "CREATE USER IF NOT EXISTS '${DUMP_USER}'@'%' IDENTIFIED BY '${PASS1}'; GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '${DUMP_USER}'@'%'; CREATE USER IF NOT EXISTS '${UPG_USER}'@'%' IDENTIFIED BY '${PASS1}'; GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '${UPG_USER}'@'%'; FLUSH PRIVILEGES;"

start_control_plane
start_worker

SID=$((370000 + RUN_TAG % 10000))
CREATE_BODY="$(jq -n \
  --arg name "e2e-ctl-${RUN_TAG}" \
  --arg user "$DUMP_USER" \
  --arg pass "$PASS1" \
  --argjson port "$MYSQL57_PORT" \
  --argjson sid "$SID" \
  '{name:$name,cluster_key:$name,source:{host:"127.0.0.1",port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:{mode:"LATEST"},storage:{retention_days:7}}')"
CREATED="$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$CREATE_BODY")"
TASK_ID="$(printf '%s' "$CREATED" | jq -r '.id // empty')"
if [[ -z "$TASK_ID" || "$TASK_ID" == "null" ]]; then
  echo "create failed: $CREATED" >&2
  exit 1
fi

HTTP="$(curl -sS -o /tmp/e2e-ctl-start.resp -w '%{http_code}' -X POST "$API/api/tasks/$TASK_ID/start")"
if [[ "$HTTP" != "204" ]]; then
  echo "start failed http=$HTTP body=$(cat /tmp/e2e-ctl-start.resp)" >&2
  exit 1
fi
wait_state "$TASK_ID" "RUNNING"
wait_dumps "$DUMP_USER" 1
EPOCH="$(task_json "$TASK_ID" | jq -r '.epoch // 0')"
if [[ "$EPOCH" == "0" || "$EPOCH" == "null" ]]; then
  echo "expected an epoch, body=$(task_json "$TASK_ID")" >&2
  exit 1
fi

HTTP="$(curl -sS -o /tmp/e2e-ctl-stop.resp -w '%{http_code}' -X POST "$API/api/tasks/$TASK_ID/stop")"
if [[ "$HTTP" != "204" ]]; then
  echo "stop failed http=$HTTP body=$(cat /tmp/e2e-ctl-stop.resp)" >&2
  exit 1
fi

# Change the password while Stop is in flight when we can still see STOPPING.
# A fast Close may already be STOPPED; the following Start is still one dump.
SEEN=""
for _ in {1..2000}; do
  SEEN="$(task_state "$TASK_ID")"
  if [[ "$SEEN" == "STOPPING" || "$SEEN" == "STOPPED" ]]; then
    break
  fi
done
if [[ "$SEEN" != "STOPPING" && "$SEEN" != "STOPPED" ]]; then
  echo "stop did not leave RUNNING; state=$SEEN" >&2
  exit 1
fi

source_exec "ALTER USER '${DUMP_USER}'@'%' IDENTIFIED BY '${PASS2}'; FLUSH PRIVILEGES;"
PUT_BODY="$(jq -n \
  --arg name "e2e-ctl-${RUN_TAG}" \
  --arg user "$DUMP_USER" \
  --arg pass "$PASS2" \
  --argjson port "$MYSQL57_PORT" \
  --argjson sid "$SID" \
  '{name:$name,cluster_key:$name,source:{host:"127.0.0.1",port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:{mode:"LATEST"},storage:{retention_days:7}}')"
HTTP="$(curl -sS -o /tmp/e2e-ctl-put.resp -w '%{http_code}' -X PUT "$API/api/tasks/$TASK_ID" -H 'Content-Type: application/json' -d "$PUT_BODY")"
if [[ "$HTTP" != "200" ]]; then
  echo "update failed http=$HTTP body=$(cat /tmp/e2e-ctl-put.resp)" >&2
  exit 1
fi
HTTP="$(curl -sS -o /tmp/e2e-ctl-restart.resp -w '%{http_code}' -X POST "$API/api/tasks/$TASK_ID/start")"
if [[ "$HTTP" != "204" ]]; then
  echo "restart failed http=$HTTP body=$(cat /tmp/e2e-ctl-restart.resp) state=$(task_state "$TASK_ID")" >&2
  cat "$WORKER_LOG" >&2 || true
  exit 1
fi
wait_state "$TASK_ID" "RUNNING"
wait_dumps "$DUMP_USER" 1
AFTER="$(task_json "$TASK_ID")"
AFTER_EPOCH="$(printf '%s' "$AFTER" | jq -r '.epoch // 0')"
AFTER_USER="$(printf '%s' "$AFTER" | jq -r '.source.user // empty')"
if [[ "$SEEN" == "STOPPING" && "$AFTER_EPOCH" != "$EPOCH" ]]; then
  echo "epoch changed during in-flight stop $EPOCH -> $AFTER_EPOCH" >&2
  exit 1
fi
if [[ "$AFTER_USER" != "$DUMP_USER" ]]; then
  echo "user=$AFTER_USER want=$DUMP_USER" >&2
  exit 1
fi

HTTP="$(curl -sS -o /tmp/e2e-ctl-stop2.resp -w '%{http_code}' -X POST "$API/api/tasks/$TASK_ID/stop")"
if [[ "$HTTP" != "204" ]]; then
  echo "final stop failed http=$HTTP body=$(cat /tmp/e2e-ctl-stop2.resp)" >&2
  exit 1
fi
for _ in {1..90}; do
  BODY="$(task_json "$TASK_ID")"
  STATE="$(printf '%s' "$BODY" | jq -r '.state // empty')"
  if [[ "$STATE" == "STOPPED" ]]; then
    DUMPS="$(dump_count "$DUMP_USER")"
    OWNER="$(printf '%s' "$BODY" | jq -r '.owner_worker_id // empty')"
    if [[ "$DUMPS" != "0" || -n "$OWNER" ]]; then
      echo "STOPPED with dumps=$DUMPS owner=$OWNER" >&2
      exit 1
    fi
    break
  fi
  sleep 0.5
done
if [[ "$(task_state "$TASK_ID")" != "STOPPED" ]]; then
  echo "final state=$(task_state "$TASK_ID")" >&2
  exit 1
fi

echo "[control-loop] password change during stop left one dump, then zero"

stop_pair

UPG_SID=$((SID + 1))
SOURCE_JSON="$(jq -cn \
  --arg user "$UPG_USER" \
  --arg pass "$PASS1" \
  --argjson port "$MYSQL57_PORT" \
  --argjson sid "$UPG_SID" \
  '{host:"127.0.0.1",port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid}')"
START_JSON='{"mode":"LATEST"}'
STORAGE_JSON='{"retention_days":7}'
# v0.5.45-shaped row: RUNNING while desired_run stays at the column default STOP, revisions 0.
meta_exec "INSERT INTO backup_tasks (id, name, cluster_key, state, last_error, owner_worker_id, epoch, run_id, source_json, start_json, storage_json, updated_at, desired_run, spec_revision, applied_spec_revision, failed_spec_revision, retry_attempt, consecutive_source_failures) VALUES ('900001', 'e2e-upg-${RUN_TAG}', 'e2e-upg-${RUN_TAG}', 'RUNNING', '', '', 0, '', '${SOURCE_JSON}', '${START_JSON}', '${STORAGE_JSON}', UTC_TIMESTAMP(6), 'STOP', 0, 0, 0, 0, 0)"

start_all_in_one
wait_state "900001" "RUNNING"
wait_dumps "$UPG_USER" 1
ROW="$(meta_exec "SELECT CONCAT(desired_run, ',', spec_revision, ',', state) FROM backup_tasks WHERE id='900001'")"
if [[ "$ROW" != "RUN,0,RUNNING" ]]; then
  echo "upgrade row=$ROW, want RUN,0,RUNNING" >&2
  cat "$ALL_LOG" >&2 || true
  exit 1
fi
if [[ "$(task_state "900001")" == "STOPPED" ]]; then
  echo "upgrade task was stopped" >&2
  exit 1
fi

echo "[control-loop] upgraded RUNNING task stayed running"
