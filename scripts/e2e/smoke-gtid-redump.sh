#!/usr/bin/env bash
# input: mysql80 with GTID on, the suite API, the task data directory, the Percona 8.0 mysqlbinlog client, and E2E_SERVER_PID, E2E_SERVER_LOG, and BINLOG_SERVER_META_DSN to restart the suite server
# output: GTID and LATEST tasks that survive several Binlog Dump kills, a kill storm during large multi-event transactions, one rotation, a kill -9 of the server mid-load and a restart on the same data dir, and an out-of-order GTID_NEXT hole plus a second UUID, without ever reaching FAILED or STREAM_REGRESSION, with every new transaction stored once, a window that matches the files, and a replay of each task from an empty database whose checksum matches the source
# pos: CI coverage for a dump reconnect, process restart, or upgrade that must resume from what the open segment already holds, and for legal GTID holes and order
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
# Large transactions span many row events, so a dump kill or a kill -9 is
# likely to land inside one.
BIG_ROWS=3000
SERVER_PID_NOW="${E2E_SERVER_PID:-}"
RESTARTED_PID=""
REPL_USER="rd${RUN_TAG}"
REPL_PASS="redumppass"

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
  docker rm -f -v "$RESTORE" >/dev/null 2>&1 || true
  if [[ -n "${GTID_TASK:-}" ]]; then
    curl -fsS -X POST "$API/api/tasks/$GTID_TASK/stop" >/dev/null 2>&1 || true
  fi
  if [[ -n "${LATEST_TASK:-}" ]]; then
    curl -fsS -X POST "$API/api/tasks/$LATEST_TASK/stop" >/dev/null 2>&1 || true
  fi
  mysql80 "DROP USER IF EXISTS '${REPL_USER}'@'%';" >/dev/null 2>&1 || true
  if [[ -n "$RESTARTED_PID" ]]; then
    kill "$RESTARTED_PID" >/dev/null 2>&1 || true
    wait "$RESTARTED_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

task_json() {
  curl -fsS "$API/api/tasks/$1"
}

wait_state() {
  local id="$1"
  local want="$2"
  local tries="${3:-60}"
  local i resp st
  for i in $(seq 1 "$tries"); do
    if ! resp="$(task_json "$id" 2>/dev/null)"; then
      sleep 0.5
      continue
    fi
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
  mysql80 "SELECT COUNT(*) FROM information_schema.processlist WHERE USER='${REPL_USER}' AND COMMAND LIKE 'Binlog Dump%';"
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
  ids="$(mysql80 "SELECT ID FROM information_schema.processlist WHERE USER='${REPL_USER}' AND COMMAND LIKE 'Binlog Dump%';")"
  for id in $ids; do
    [[ -z "$id" ]] && continue
    mysql80 "KILL ${id};" >/dev/null || true
    n=$((n + 1))
  done
  printf '%s' "$n"
}

# big_txn commits BIG_ROWS rows and one update as one transaction.
big_txn() {
  local k="$1"
  local base=$((100000 + k * 10000))
  mysql80 "SET SESSION cte_max_recursion_depth=$((BIG_ROWS + 10)); BEGIN; INSERT INTO ${DB}.t(id,v) WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<${BIG_ROWS}) SELECT ${base}+n, CONCAT('big-${RUN_TAG}-', ${base}+n) FROM s; UPDATE ${DB}.t SET v=CONCAT(v,'-u') WHERE id=${base}+1; COMMIT;" >/dev/null
}

# kill_storm kills every dump of this scenario a few times while big
# transactions stream, so some reconnects start inside a transaction.
kill_storm() {
  local i got total=0
  for i in 1 2 3 4; do
    sleep 0.4
    got="$(kill_dumps)"
    total=$((total + got))
  done
  echo "$total" >"$1"
}

# restart_server kill -9s the suite server and starts it again on the same
# data dir and metadata DSN. Flushed bytes ahead of the checkpoint stay in
# the open segment, as after a crash or an upgrade.
restart_server() {
  local pid="$SERVER_PID_NOW"
  local meta="${BINLOG_SERVER_META_DSN:-}"
  local log="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-suite.log}"
  kill -9 "$pid" >/dev/null 2>&1 || { echo "kill -9 $pid failed" >&2; return 1; }
  local gone=0
  for _ in $(seq 1 30); do
    if ! kill -0 "$pid" 2>/dev/null; then
      gone=1
      break
    fi
    sleep 1
  done
  [[ "$gone" == "1" ]] || { echo "server $pid still running after kill -9" >&2; return 1; }
  for _ in $(seq 1 30); do
    if ! curl -fsS "$API/healthz" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  echo "[gtid-redump] killed server pid=$pid mid-load"
  # Transactions committed while the server is down must arrive after the restart.
  mysql80 "INSERT INTO ${DB}.t(id,v) VALUES (900001, 'down-${RUN_TAG}-1');"
  big_txn 7
  mysql80 "INSERT INTO ${DB}.t(id,v) VALUES (900002, 'down-${RUN_TAG}-2');"
  BINLOG_SERVER_DATA_DIR="$DATA_DIR" BINLOG_SERVER_META_DSN="$meta" \
    nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >>"$log" 2>&1 &
  RESTARTED_PID=$!
  SERVER_PID_NOW="$RESTARTED_PID"
  for _ in $(seq 1 120); do
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      echo "[gtid-redump] restarted server pid=$RESTARTED_PID"
      return 0
    fi
    if ! kill -0 "$RESTARTED_PID" 2>/dev/null; then
      echo "restarted server exited early" >&2
      tail -n 50 "$log" >&2 || true
      return 1
    fi
    sleep 1
  done
  echo "restarted server not ready" >&2
  return 1
}

# gtid_next_gap commits uuid:max+3 before max+1 and max+2, and one
# transaction under a UUID this source never generated. A replica with
# parallel workers and replica_preserve_commit_order=OFF writes the same
# order. Neither is a regression.
gtid_next_gap() {
  local uuid executed max k foreign
  uuid="$(mysql80 "SELECT @@server_uuid;")"
  executed="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
  max="$(printf '%s' "$executed" | tr ',' '\n' | grep -i "^${uuid}:" | sed -E 's/.*[-:]([0-9]+)$/\1/')"
  if [[ -z "$max" ]]; then
    echo "cannot read the last GNO of $uuid from $executed" >&2
    return 1
  fi
  k=$((max + 3))
  mysql80 "SET GTID_NEXT='${uuid}:${k}'; INSERT INTO ${DB}.t(id,v) VALUES (800001, 'gap-${RUN_TAG}-${k}'); SET GTID_NEXT='AUTOMATIC';"
  mysql80 "INSERT INTO ${DB}.t(id,v) VALUES (800002, 'gap-${RUN_TAG}-fill1');"
  mysql80 "INSERT INTO ${DB}.t(id,v) VALUES (800003, 'gap-${RUN_TAG}-fill2');"
  mysql80 "INSERT INTO ${DB}.t(id,v) VALUES (800004, 'gap-${RUN_TAG}-after');"
  foreign="$(printf 'f0e1d2c3-0000-4000-8000-%012d' "$RUN_TAG")"
  mysql80 "SET GTID_NEXT='${foreign}:1'; INSERT INTO ${DB}.t(id,v) VALUES (800005, 'uuid-${RUN_TAG}'); SET GTID_NEXT='AUTOMATIC';"
  echo "[gtid-redump] committed ${uuid}:${k} before $((max + 1))-$((max + 2)), and ${foreign}:1"
}

assert_never_regressed() {
  local id="$1"
  local n
  n="$(curl -fsS "$API/api/tasks/$id/events?limit=500" | jq '[.[] | select(((.detail // "") + " " + (.message // "")) | test("STREAM_REGRESSION|STORAGE_INCONSISTENT"))] | length')"
  if [[ "$n" != "0" ]]; then
    echo "task $id recorded $n STREAM_REGRESSION or STORAGE_INCONSISTENT events" >&2
    curl -fsS "$API/api/tasks/$id/events?limit=500" >&2 || true
    return 1
  fi
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
  docker rm -f -v "$RESTORE" >/dev/null 2>&1 || true
  # Own network namespace so this server does not bind the host's 3306.
  # MYSQL_ROOT_HOST=% is what lets docker exec reach it over TCP as root@127.0.0.1.
  docker run -d --name "$RESTORE" \
    -e MYSQL_ROOT_PASSWORD=root \
    -e MYSQL_ROOT_HOST=% \
    mysql:8.0 \
    --gtid-mode=ON \
    --enforce-gtid-consistency=ON \
    --server-id=19080 \
    --log-bin=mysql-bin \
    --binlog-format=ROW >/dev/null
  # The image answers SELECT 1 on a temporary init server, then restarts.
  # Wait until RESET sticks and gtid_executed is empty.
  local i got errf
  errf="$(mktemp)"
  for i in $(seq 1 90); do
    if docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "SELECT 1" >/dev/null 2>"$errf"; then
      if docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "RESET MASTER;" >/dev/null 2>"$errf" \
        || docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "RESET BINARY LOGS AND GTIDS;" >/dev/null 2>"$errf"; then
        got="$(docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "SELECT @@port = 3306 AND @@GLOBAL.gtid_executed = ''" 2>>"$errf" | tr -d '[:space:]' || true)"
        if [[ "$got" == "1" ]]; then
          rm -f "$errf"
          return 0
        fi
      fi
    fi
    sleep 1
  done
  echo "restore $RESTORE did not become ready" >&2
  cat "$errf" >&2 || true
  rm -f "$errf"
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
    if [[ "$p" != /* ]]; then
      p="$ROOT_DIR/${p#./}"
    fi
    dest="/tmp/redump-${id}-${i}.bin"
    docker compose -f "$COMPOSE_FILE" cp "$p" "percona80:${dest}" </dev/null
    paths+=("$dest")
    i=$((i + 1))
  done < <(jq -r '.paths[]' <<<"$json")
  if [[ "$i" -lt 1 ]]; then
    echo "replay has no paths: $json" >&2
    return 1
  fi
  # RESET clears GTIDs and leaves user databases. Drop first so the next replay can create it again.
  docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "DROP DATABASE IF EXISTS ${DB};" >/dev/null
  docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "RESET MASTER;" >/dev/null 2>&1 \
    || docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "RESET BINARY LOGS AND GTIDS;" >/dev/null
  # mysql:8.0 has no mysqlbinlog. The suite's pipefail makes either side fail the scenario.
  local log
  log="$(mktemp)"
  if ! docker compose -f "$COMPOSE_FILE" exec -T percona80 mysqlbinlog "${paths[@]}" \
    | docker exec -i "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp --binary-mode >"$log" 2>&1; then
    echo "replay of task $id failed" >&2
    cat "$log" >&2 || true
    rm -f "$log"
    return 1
  fi
  rm -f "$log"
}

echo "[gtid-redump] compression off so GTID and XID stay outside a transaction payload"
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"
mysql80 "CREATE USER '${REPL_USER}'@'%' IDENTIFIED BY '${REPL_PASS}';"
mysql80 "GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '${REPL_USER}'@'%';"

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
      --arg user "$REPL_USER" \
      --arg pass "$REPL_PASS" \
      --argjson port "$E2E_MYSQL80_PORT" \
      --argjson sid "$sid" \
      '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:{mode:"GTID",gtid_set:$gtid},storage:{retention_days:7}}'
  else
    jq -n \
      --arg name "$name" \
      --arg host "$E2E_SOURCE_HOST" \
      --arg user "$REPL_USER" \
      --arg pass "$REPL_PASS" \
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

mysql80 "CREATE DATABASE ${DB};"
mysql80 "CREATE TABLE ${DB}.t (id INT PRIMARY KEY, v VARCHAR(64) NOT NULL);"

killed=0
restarted=0
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
  if [[ "$n" == 20 ]]; then
    wait_dumps
    storm_out="$(mktemp)"
    kill_storm "$storm_out" &
    storm_pid=$!
    for k in 1 2 3 4; do
      big_txn "$k"
    done
    wait "$storm_pid"
    got="$(cat "$storm_out")"
    rm -f "$storm_out"
    killed=$((killed + got))
    echo "[gtid-redump] killed $got dump threads during 4 transactions of ${BIG_ROWS} rows (total $killed)"
    wait_state "$GTID_TASK" RUNNING 240
    wait_state "$LATEST_TASK" RUNNING 240
  fi
  if [[ "$n" == 24 ]]; then
    if [[ -z "$SERVER_PID_NOW" || -z "${BINLOG_SERVER_META_DSN:-}" ]]; then
      echo "[gtid-redump] E2E_SERVER_PID and BINLOG_SERVER_META_DSN are required for the restart step; run it through run-suite.sh" >&2
      exit 1
    fi
    wait_dumps
    big_txn 5 &
    loader=$!
    sleep 0.3
    restart_server
    wait "$loader"
    big_txn 6
    restarted=1
    # A new process may wait for the old lease before it runs the tasks again.
    wait_state "$GTID_TASK" RUNNING 360
    wait_state "$LATEST_TASK" RUNNING 360
    wait_dumps
    echo "[gtid-redump] both tasks running again after kill -9"
  fi
  if [[ "$n" == 32 ]]; then
    gtid_next_gap
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
  assert_never_regressed "$id"
  st="$(task_json "$id" | jq -r '.state // empty')"
  if [[ "$st" != "RUNNING" ]]; then
    echo "task $id is $st after the run: $(task_json "$id")" >&2
    exit 1
  fi
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
if [[ -n "${E2E_SERVER_LOG:-}" ]]; then
  grep -a -F "open tail" "$E2E_SERVER_LOG" | grep -a -F "task=${GTID_TASK} " | tail -n 3 | sed 's/^/[gtid-redump] evidence: /' || true
fi
echo "[gtid-redump] success: $killed dump kills, a kill storm during ${BIG_ROWS}-row transactions, $restarted kill -9 restart, one rotation, an out-of-order GTID_NEXT hole and a second UUID, both modes replay once"
