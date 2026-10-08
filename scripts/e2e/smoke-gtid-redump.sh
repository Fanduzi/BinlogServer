#!/usr/bin/env bash
# input: mysql80 with GTID on, the suite API, and the task data directory
# output: GTID and LATEST tasks that survive several Binlog Dump kills and one rotation, with every new transaction stored once and a window that matches the files
# pos: CI coverage for a dump reconnect that must resume from the flushed GTID set or file position
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"
RUN_TAG="$(date +%s)"
DB="redump_${RUN_TAG}"
RESTORE="binlog-e2e-redump-${RUN_TAG}"
ROWS=40

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

mysql80() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -Nse "$1" | tr -d '\r'
}

restore_compression() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -e "SET GLOBAL binlog_transaction_compression=ON;" >/dev/null 2>&1 || true
}

cleanup() {
  restore_compression
  docker rm -f "$RESTORE" >/dev/null 2>&1 || true
  if [[ -n "${GTID_TASK:-}" ]]; then
    curl -fsS -X POST "$API/api/tasks/$GTID_TASK/stop" >/dev/null 2>&1 || true
  fi
  if [[ -n "${LATEST_TASK:-}" ]]; then
    curl -fsS -X POST "$API/api/tasks/$LATEST_TASK/stop" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

task_json() {
  curl -fsS "$API/api/tasks/$1"
}

wait_state() {
  local id="$1"
  local want="$2"
  local i resp st
  for i in $(seq 1 60); do
    resp="$(task_json "$id")"
    st="$(printf '%s' "$resp" | jq -r '.state // empty')"
    if [[ "$st" == "$want" ]]; then
      return 0
    fi
    if [[ "$st" == "FAILED" ]]; then
      echo "task $id failed: $resp" >&2
      return 1
    fi
    sleep 0.5
  done
  echo "task $id did not reach $want: $(task_json "$id")" >&2
  return 1
}

dump_count() {
  mysql80 "SELECT COUNT(*) FROM information_schema.processlist WHERE USER='repl' AND COMMAND LIKE 'Binlog Dump%';"
}

wait_dumps() {
  local i n
  for i in $(seq 1 40); do
    n="$(dump_count)"
    if [[ "$n" -ge 2 ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "expected 2 binlog dumps, found ${n:-0}" >&2
  mysql80 "SHOW PROCESSLIST;" >&2 || true
  return 1
}

kill_dumps() {
  local ids id n=0
  ids="$(mysql80 "SELECT ID FROM information_schema.processlist WHERE USER='repl' AND COMMAND LIKE 'Binlog Dump%';")"
  for id in $ids; do
    [[ -z "$id" ]] && continue
    mysql80 "KILL ${id};" >/dev/null || true
    n=$((n + 1))
  done
  printf '%s' "$n"
}

signature() {
  local which="$1"
  local sql="SELECT COUNT(*), COALESCE(BIT_XOR(CAST(CRC32(CONCAT(id,'|',v)) AS UNSIGNED)),0) FROM ${DB}.t;"
  if [[ "$which" == "source" ]]; then
    mysql80 "$sql"
    return
  fi
  docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "$sql" | tr -d '\r'
}

gtid_subset() {
  local inner="$1"
  local outer="$2"
  inner="$(printf '%s' "$inner" | tr -d ' \r\n')"
  outer="$(printf '%s' "$outer" | tr -d ' \r\n')"
  mysql80 "SELECT GTID_SUBSET('${inner}', '${outer}');"
}

gtid_subtract_subset() {
  local left="$1"
  local right="$2"
  local outer="$3"
  left="$(printf '%s' "$left" | tr -d ' \r\n')"
  right="$(printf '%s' "$right" | tr -d ' \r\n')"
  outer="$(printf '%s' "$outer" | tr -d ' \r\n')"
  mysql80 "SELECT GTID_SUBSET(GTID_SUBTRACT('${left}', '${right}'), '${outer}');"
}

assert_window() {
  local id="$1"
  local seed="$2"
  local executed="$3"
  local win continuous covered
  win="$(curl -fsS "$API/api/tasks/$id/window")"
  continuous="$(printf '%s' "$win" | jq -r '.continuous')"
  covered="$(printf '%s' "$win" | jq -r '.gtid_set // empty')"
  if [[ "$continuous" != "true" || -z "$covered" ]]; then
    echo "window for $id is not a continuous GTID chain: $win" >&2
    return 1
  fi
  if [[ "$(gtid_subtract_subset "$executed" "$seed" "$covered")" != "1" ]]; then
    echo "window for $id is missing transactions committed after $seed: window=$covered executed=$executed" >&2
    return 1
  fi
  if [[ "$(gtid_subset "$covered" "$executed")" != "1" ]]; then
    echo "window for $id claims a GTID the source has not executed: window=$covered executed=$executed" >&2
    return 1
  fi
}

assert_checkpoint_matches_window() {
  local id="$1"
  local seed="$2"
  local cp gtid
  cp="$(curl -fsS "$API/api/tasks/$id/checkpoint")"
  gtid="$(printf '%s' "$cp" | jq -r '.gtid_set // empty')"
  if [[ -z "$gtid" ]]; then
    echo "GTID checkpoint is empty: $cp" >&2
    return 1
  fi
  local covered
  covered="$(curl -fsS "$API/api/tasks/$id/window" | jq -r '.gtid_set // empty')"
  if [[ "$(gtid_subtract_subset "$gtid" "$seed" "$covered")" != "1" ]]; then
    echo "checkpoint claims a transaction the files do not contain: checkpoint=$gtid window=$covered seed=$seed" >&2
    return 1
  fi
}

start_restore() {
  docker rm -f "$RESTORE" >/dev/null 2>&1 || true
  docker run -d --name "$RESTORE" \
    --network host \
    -e MYSQL_ROOT_PASSWORD=root \
    -e MYSQL_ALLOW_EMPTY_PASSWORD=no \
    mysql:8.0 \
    --gtid-mode=ON \
    --enforce-gtid-consistency=ON \
    --server-id=19080 \
    --log-bin=mysql-bin \
    --binlog-format=ROW >/dev/null
  local i
  for i in $(seq 1 90); do
    if docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "SELECT 1" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "restore $RESTORE did not become ready" >&2
  docker logs "$RESTORE" >&2 || true
  return 1
}

apply_replay() {
  local id="$1"
  local json i=0 dest
  local -a paths=()
  json="$(curl -fsS "$API/api/tasks/$id/replay")"
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    dest="/tmp/redump-${id}-${i}.bin"
    docker cp "$p" "$RESTORE:${dest}"
    paths+=("$dest")
    i=$((i + 1))
  done < <(jq -r '.paths[]' <<<"$json")
  if [[ "$i" -lt 1 ]]; then
    echo "replay has no paths: $json" >&2
    return 1
  fi
  docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "RESET MASTER;" >/dev/null 2>&1 \
    || docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "RESET BINARY LOGS AND GTIDS;" >/dev/null
  if ! docker exec "$RESTORE" sh -c "mysqlbinlog ${paths[*]} | mysql -uroot -proot -h127.0.0.1 --protocol=tcp"; then
    echo "replay of task $id failed" >&2
    return 1
  fi
}

echo "[gtid-redump] compression off so GTID and XID stay outside a transaction payload"
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"

seed="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
if [[ -z "$seed" ]]; then
  echo "empty GTID_EXECUTED" >&2
  exit 1
fi

create_body() {
  local name="$1"
  local mode="$2"
  local sid="$3"
  local gtid="$4"
  if [[ "$mode" == "GTID" ]]; then
    jq -n \
      --arg name "$name" \
      --arg gtid "$gtid" \
      --arg host "$E2E_SOURCE_HOST" \
      --arg user "$E2E_SOURCE_USER" \
      --arg pass "$E2E_SOURCE_PASS" \
      --argjson port "$E2E_MYSQL80_PORT" \
      --argjson sid "$sid" \
      '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:{mode:"GTID",gtid_set:$gtid},storage:{retention_days:7}}'
  else
    jq -n \
      --arg name "$name" \
      --arg host "$E2E_SOURCE_HOST" \
      --arg user "$E2E_SOURCE_USER" \
      --arg pass "$E2E_SOURCE_PASS" \
      --argjson port "$E2E_MYSQL80_PORT" \
      --argjson sid "$sid" \
      '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:{mode:"LATEST"},storage:{retention_days:7}}'
  fi
}

post_task() {
  local body="$1"
  local resp id
  resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
  id="$(printf '%s' "$resp" | jq -r '.id // empty')"
  if [[ -z "$id" || "$id" == "null" ]]; then
    echo "create task failed: $resp" >&2
    exit 1
  fi
  curl -fsS -X POST "$API/api/tasks/$id/start" >/dev/null
  printf '%s' "$id"
}

GTID_TASK="$(post_task "$(create_body "e2e-gtid-redump-${RUN_TAG}" GTID 19041 "$seed")")"
LATEST_TASK="$(post_task "$(create_body "e2e-latest-redump-${RUN_TAG}" LATEST 19042 "")")"
wait_state "$GTID_TASK" RUNNING
wait_state "$LATEST_TASK" RUNNING
wait_dumps
echo "[gtid-redump] gtid=$GTID_TASK latest=$LATEST_TASK running"

mysql80 "CREATE DATABASE ${DB}; CREATE TABLE ${DB}.t (id INT PRIMARY KEY, v VARCHAR(64) NOT NULL);"

killed=0
n=1
while [[ "$n" -le "$ROWS" ]]; do
  mysql80 "INSERT INTO ${DB}.t(id,v) VALUES (${n}, 'redump-${RUN_TAG}-${n}');"
  if [[ "$n" == 12 ]]; then
    mysql80 "FLUSH BINARY LOGS;"
    echo "[gtid-redump] rotated source binlog"
  fi
  if [[ "$n" == 8 || "$n" == 16 || "$n" == 28 ]]; then
    wait_dumps
    got="$(kill_dumps)"
    killed=$((killed + got))
    echo "[gtid-redump] killed $got dump threads at row $n (total $killed)"
    wait_state "$GTID_TASK" RUNNING
    wait_state "$LATEST_TASK" RUNNING
  fi
  n=$((n + 1))
done
if [[ "$killed" -lt 3 ]]; then
  echo "killed only $killed binlog dump threads; need several reconnects" >&2
  exit 1
fi

executed="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
for id in "$GTID_TASK" "$LATEST_TASK"; do
  found=0
  for _ in $(seq 1 60); do
    if grep -a -R -F -q "redump-${RUN_TAG}-${ROWS}" "$DATA_DIR/$id" 2>/dev/null; then
      found=1
      break
    fi
    st="$(task_json "$id" | jq -r '.state // empty')"
    if [[ "$st" == "FAILED" ]]; then
      echo "task $id failed while catching up: $(task_json "$id")" >&2
      exit 1
    fi
    sleep 0.5
  done
  if [[ "$found" != "1" ]]; then
    echo "row $ROWS did not land in $DATA_DIR/$id" >&2
    exit 1
  fi
  alert="$(task_json "$id" | jq -r '.storage_alert.code // empty')"
  if [[ -n "$alert" ]]; then
    echo "task $id reported storage_alert during a healthy run: $(task_json "$id")" >&2
    exit 1
  fi
  assert_window "$id" "$seed" "$executed"
done
assert_checkpoint_matches_window "$GTID_TASK" "$seed"
echo "[gtid-redump] both tasks stored every row; window matches the source"

start_restore
source_sig="$(signature source)"
for id in "$GTID_TASK" "$LATEST_TASK"; do
  apply_replay "$id"
  got_sig="$(signature restore)"
  if [[ "$got_sig" != "$source_sig" ]]; then
    echo "replay of $id signature $got_sig != source $source_sig" >&2
    exit 1
  fi
  echo "[gtid-redump] replay $id matches $source_sig"
done
echo "[gtid-redump] success: $killed dump kills, one rotation, both modes replay once"
