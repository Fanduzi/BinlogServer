#!/usr/bin/env bash
# input: mysql80 with GTID on, the suite API, the task data directory, and the Percona 8.0 mysqlbinlog client
# output: a MySQL 8.0 restored from a midpoint dump, then rolled forward with the returned command to exactly the rows before the stop, for both stop_gtid and stop_datetime, plus the empty note and the plain-text 400 sentences
# pos: docker coverage for replaying from a restored backup's executed GTID set
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"
RUN_TAG="$(date +%s)"
DB="binlog_exec_${RUN_TAG}"
JUNK="binlog_exec_junk_${RUN_TAG}"
STAMP=1704067200
STOP_CLOCK="2024-01-01 00:00:01"
RESTORE_GTID="binlog-e2e-gtid-exec-gtid"
RESTORE_TIME="binlog-e2e-gtid-exec-time"
DUMP=""

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq
need_cmd tar
need_cmd find

mysql80() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot --batch --raw -Nse "$1" | tr -d '\r'
}

mysql80_script() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot --batch --raw -N <<<"$1" | tr -d '\r'
}

restore_compression() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -e "SET GLOBAL binlog_transaction_compression=ON;" >/dev/null 2>&1 || true
}

cleanup() {
  docker rm -f "$RESTORE_GTID" "$RESTORE_TIME" >/dev/null 2>&1 || true
  if [[ -n "$DUMP" ]]; then
    rm -f "$DUMP"
  fi
  restore_compression
}
trap cleanup EXIT

gtid_subset() {
  local inner="$1"
  local outer="$2"
  inner="$(printf '%s' "$inner" | tr -d ' \r\n')"
  outer="$(printf '%s' "$outer" | tr -d ' \r\n')"
  mysql80 "SELECT GTID_SUBSET('${inner}', '${outer}');"
}

abs_path() {
  local p="$1"
  if [[ "$p" == /* ]]; then
    printf '%s' "$p"
  else
    printf '%s' "$ROOT_DIR/${p#./}"
  fi
}

sql_field() {
  local key="$1"
  local text="$2"
  local line
  line="$(printf '%s\n' "$text" | awk -F '\t' -v k="$key" '$1 == k { print $2; exit }')"
  line="$(printf '%s' "$line" | tr -d ' \r\n')"
  if [[ ! "$line" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}:[1-9][0-9]*$ ]]; then
    echo "gtid field $key missing in: $text" >&2
    exit 1
  fi
  printf '%s' "$line"
}

set_field() {
  local key="$1"
  local text="$2"
  local line
  line="$(printf '%s\n' "$text" | awk -F '\t' -v k="$key" '$1 == k { print substr($0, index($0, "\t") + 1); exit }')"
  line="$(printf '%s' "$line" | tr -d ' \r\n')"
  if [[ -z "$line" ]]; then
    echo "set field $key missing in: $text" >&2
    exit 1
  fi
  printf '%s' "$line"
}

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
    sleep 0.5
  done
  echo "task $id did not reach $want: $(task_json "$id")" >&2
  return 1
}

wait_gtid() {
  local want="$1"
  local i cp gtid
  for i in $(seq 1 90); do
    cp="$(curl -fsS "$API/api/tasks/$task_id/checkpoint")"
    gtid="$(printf '%s' "$cp" | jq -r '.gtid_set // empty')"
    if [[ -n "$gtid" && "$(gtid_subset "$want" "$gtid")" == "1" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "checkpoint does not include $want: $(curl -fsS "$API/api/tasks/$task_id/checkpoint")" >&2
  return 1
}

wait_task_dir() {
  local i path
  for i in $(seq 1 60); do
    path="$(curl -fsS "$API/api/tasks/$task_id/files?limit=50" | jq -r 'if type=="array" then ([.[] | .file_path // empty] | map(select(length>0)) | .[0] // "") else "" end')"
    if [[ -n "$path" ]]; then
      path="$(abs_path "$path")"
      if [[ -f "$path" ]]; then
        dirname "$path"
        return 0
      fi
    fi
    sleep 0.5
  done
  echo "task directory not ready" >&2
  curl -fsS "$API/api/tasks/$task_id/files?limit=50" >&2 || true
  return 1
}

one_open_name() {
  local dir="$1"
  local names
  names="$(find "$dir" -maxdepth 1 -type f -name '*.open.e*' -printf '%f\n' | sort)"
  if [[ -z "$names" || "$(printf '%s\n' "$names" | wc -l)" -ne 1 ]]; then
    echo "expected one open segment in $dir, got: ${names:-<none>}" >&2
    find "$dir" -maxdepth 1 -type f -printf '%f\n' >&2 || true
    return 1
  fi
  printf '%s' "$names"
}

wait_sealed() {
  local dir="$1"
  local open_name="$2"
  local sealed="${open_name%%.open.e*}"
  local i
  for i in $(seq 1 60); do
    if [[ -f "$dir/$sealed" && ! -e "$dir/$open_name" ]]; then
      printf '%s' "$sealed"
      return 0
    fi
    sleep 0.5
  done
  echo "did not seal $open_name in $dir" >&2
  find "$dir" -maxdepth 1 -type f -printf '%f\n' >&2 || true
  return 1
}

assert_replay_status() {
  local id="$1"
  local want_code="$2"
  local want_body="$3"
  shift 3
  local tmp code got
  tmp="$(mktemp)"
  code="$(curl -sS -G -o "$tmp" -w '%{http_code}' "$API/api/tasks/${id}/replay" "$@")"
  got="$(tr -d '\r\n' <"$tmp")"
  rm -f "$tmp"
  if [[ "$code" != "$want_code" || "$got" != "$want_body" ]]; then
    echo "replay $id status=$code body=[$got] want $want_code [$want_body]" >&2
    exit 1
  fi
}

fetch_replay() {
  local tmp code
  tmp="$(mktemp)"
  code="$(curl -sS -G -o "$tmp" -w '%{http_code}' "$API/api/tasks/$task_id/replay" "$@")"
  if [[ "$code" != "200" ]]; then
    echo "replay status $code $(cat "$tmp")" >&2
    rm -f "$tmp"
    exit 1
  fi
  cat "$tmp"
  rm -f "$tmp"
}

exclude_of() {
  local cmd="$1"
  local raw
  raw="$(printf '%s\n' "$cmd" | grep -oE -- "--exclude-gtids=[^ \\\\]+" | head -1 || true)"
  raw="${raw#--exclude-gtids=}"
  raw="${raw//\'/}"
  if [[ -z "$raw" ]]; then
    echo "missing exclude-gtids in: $cmd" >&2
    exit 1
  fi
  printf '%s' "$raw"
}

start_restore() {
  local name="$1"
  local sid="$2"
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" \
    -e MYSQL_ROOT_PASSWORD=root \
    -e MYSQL_ROOT_HOST=% \
    mysql:8.0 \
    --gtid-mode=ON \
    --enforce-gtid-consistency=ON \
    --server-id="$sid" \
    --log-bin=mysql-bin \
    --binlog-format=ROW \
    --default-time-zone=+00:00 >/dev/null
  local i
  for i in $(seq 1 90); do
    if docker exec "$name" mysql -uroot -proot -Nse "SELECT 1" >/dev/null 2>&1; then
      if ! docker exec "$name" mysql -uroot -proot -e "RESET MASTER;" >/dev/null 2>&1 \
        && ! docker exec "$name" mysql -uroot -proot -e "RESET BINARY LOGS AND GTIDS;" >/dev/null 2>&1; then
        echo "restore $name could not reset gtid state" >&2
        return 1
      fi
      return 0
    fi
    sleep 1
  done
  echo "restore $name not ready" >&2
  docker logs "$name" >&2 || true
  return 1
}

load_dump() {
  local name="$1"
  if ! docker exec -i "$name" mysql -uroot -proot --binary-mode <"$DUMP"; then
    echo "dump load failed on $name" >&2
    exit 1
  fi
}

apply_command() {
  local json="$1"
  local restore="$2"
  local cmd i=0 p abs dest sql log
  cmd="$(jq -r '.command' <<<"$json")"
  if [[ -z "$cmd" || "$cmd" == "null" ]]; then
    echo "replay command empty: $json" >&2
    exit 1
  fi
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    abs="$(abs_path "$p")"
    dest="/tmp/gtid-exec-${restore}-${i}.bin"
    docker compose -f "$COMPOSE_FILE" cp "$abs" "percona80:${dest}" </dev/null
    cmd="${cmd//"$p"/"$dest"}"
    i=$((i + 1))
  done < <(jq -r '.paths[]' <<<"$json")
  if [[ "$i" -lt 1 ]]; then
    echo "replay has no paths: $json" >&2
    exit 1
  fi
  echo "[gtid-executed] apply $restore files=$i"
  sql="$(mktemp)"
  log="$(mktemp)"
  if ! printf '%s\n' "$cmd" | docker compose -f "$COMPOSE_FILE" exec -T percona80 bash -s >"$sql" 2>"$log"; then
    echo "mysqlbinlog failed for $restore" >&2
    cat "$log" >&2
    rm -f "$sql" "$log"
    exit 1
  fi
  if ! docker exec -i "$restore" mysql -uroot -proot --binary-mode <"$sql" >"$log" 2>&1; then
    echo "apply failed for $restore" >&2
    cat "$log" >&2
    rm -f "$sql" "$log"
    exit 1
  fi
  rm -f "$sql" "$log"
}

assert_rows() {
  local restore="$1"
  local got
  got="$(docker exec "$restore" mysql -uroot -proot --batch --raw -Nse "SELECT id, v FROM ${DB}.t ORDER BY id" | tr -d '\r')"
  local want
  want="$(printf '1\trow-1\n2\trow-2\n3\trow-3\n4\trow-4')"
  if [[ "$got" != "$want" ]]; then
    echo "$restore rows [$got] want [$want]" >&2
    exit 1
  fi
  local extra
  extra="$(docker exec "$restore" mysql -uroot -proot --batch --raw -Nse "SELECT COUNT(*) FROM ${DB}.t; SELECT COUNT(*) FROM ${DB}.t WHERE id >= 5;" | tr -d '\r')"
  if [[ "$extra" != $'4\n0' ]]; then
    echo "$restore counts [$extra]" >&2
    exit 1
  fi
}

echo "[gtid-executed] compression off so each transaction is its own GTID event"
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"
history="$(mysql80_script "
CREATE DATABASE ${JUNK};
CREATE TABLE ${JUNK}.t (id INT PRIMARY KEY);
INSERT INTO ${JUNK}.t VALUES (1);
SELECT CONCAT('old\t', @@GLOBAL.gtid_executed);
INSERT INTO ${JUNK}.t VALUES (2);
INSERT INTO ${JUNK}.t VALUES (3);
INSERT INTO ${JUNK}.t VALUES (4);
SELECT CONCAT('start\t', @@GLOBAL.gtid_executed);
")"
old_set="$(set_field old "$history")"
start_point="$(set_field start "$history")"
if [[ "$(gtid_subset "$old_set" "$start_point")" != "1" || "$(gtid_subset "$start_point" "$old_set")" == "1" ]]; then
  echo "expected old_set to be a strict prefix of start_point: old=$old_set start=$start_point" >&2
  exit 1
fi
echo "[gtid-executed] start point recorded"

name="e2e-gtid-executed-${RUN_TAG}"
body="$(jq -n \
  --arg name "$name" \
  --arg gtid "$start_point" \
  --arg host "$E2E_SOURCE_HOST" \
  --arg user "$E2E_SOURCE_USER" \
  --arg pass "$E2E_SOURCE_PASS" \
  --argjson port "$E2E_MYSQL80_PORT" \
  '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:310185},start:{mode:"GTID",gtid_set:$gtid},storage:{retention_days:7}}')"
resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
task_id="$(printf '%s' "$resp" | jq -r '.id // empty')"
if [[ -z "$task_id" || "$task_id" == "null" ]]; then
  echo "create task failed: $resp" >&2
  exit 1
fi
curl -fsS -X POST "$API/api/tasks/$task_id/start" >/dev/null
wait_state "$task_id" "RUNNING"

schema="$(mysql80_script "
CREATE DATABASE ${DB};
CREATE TABLE ${DB}.t (id INT PRIMARY KEY, v VARCHAR(64) NOT NULL);
INSERT INTO ${DB}.t VALUES (1,'row-1');
INSERT INTO ${DB}.t VALUES (2,'row-2');
SELECT CONCAT('row2\t', @@GLOBAL.gtid_executed);
")"
row2="$(set_field row2 "$schema")"
wait_gtid "$row2"
task_dir="$(wait_task_dir)"
mid_open="$(one_open_name "$task_dir")"
DUMP="$(mktemp)"
docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysqldump -uroot -proot --single-transaction --set-gtid-purged=ON --databases "$DB" >"$DUMP"
backup_set="$(mysql80 "SELECT @@GLOBAL.gtid_executed;" | tr -d ' \n')"
echo "[gtid-executed] dump at $backup_set"
mysql80 "FLUSH BINARY LOGS;"
mid_sealed="$(wait_sealed "$task_dir" "$mid_open")"
echo "[gtid-executed] midpoint sealed $mid_sealed"

dml="$(mysql80_script "
SET TIMESTAMP=${STAMP};
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (3,'row-3');
SELECT CONCAT('g3\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (4,'row-4');
SELECT CONCAT('g4\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SELECT CONCAT('through4\t', @@GLOBAL.gtid_executed);
SET TIMESTAMP=$((STAMP + 5));
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (5,'row-5');
SELECT CONCAT('g5\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (6,'row-6');
SELECT CONCAT('g6\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
")"
g5="$(sql_field g5 "$dml")"
g6="$(sql_field g6 "$dml")"
through4="$(set_field through4 "$dml")"
echo "[gtid-executed] stop gtid $g5"
wait_gtid "$g6"

gtid_json="$(fetch_replay --data-urlencode "start_gtid_set=${backup_set}" --data-urlencode "stop_gtid=${g5}")"
gtid_cmd="$(jq -r '.command' <<<"$gtid_json")"
if jq -e --arg sealed "$mid_sealed" 'any(.paths[]; endswith($sealed))' <<<"$gtid_json" >/dev/null; then
  echo "midpoint segment still in paths: $gtid_json" >&2
  exit 1
fi
if [[ "$(jq '.paths | length' <<<"$gtid_json")" -lt 1 ]]; then
  echo "gtid replay has no paths: $gtid_json" >&2
  exit 1
fi
if ! printf '%s\n' "$gtid_cmd" | grep -q '^TZ=UTC mysqlbinlog' \
  || ! printf '%s\n' "$gtid_cmd" | grep -q -- '--exclude-gtids=' \
  || ! printf '%s\n' "$gtid_cmd" | grep -q -- '--stop-position=' \
  || printf '%s\n' "$gtid_cmd" | grep -q -- '--stop-datetime=' \
  || printf '%s\n' "$gtid_cmd" | grep -q -- '--start-datetime='; then
  echo "gtid command shape: $gtid_cmd" >&2
  exit 1
fi
exclude="$(exclude_of "$gtid_cmd")"
if [[ "$(gtid_subset "$backup_set" "$exclude")" != "1" || "$(gtid_subset "$exclude" "$backup_set")" != "1" ]]; then
  echo "exclude [$exclude] is not the backup set [$backup_set]" >&2
  exit 1
fi
archive="$(mktemp)"
archive_code="$(curl -sS -G -o "$archive" -w '%{http_code}' "$API/api/tasks/$task_id/replay/archive" \
  --data-urlencode "start_gtid_set=${backup_set}" \
  --data-urlencode "stop_gtid=${g5}")"
if [[ "$archive_code" != "200" ]]; then
  echo "archive status $archive_code" >&2
  exit 1
fi
mapfile -t members < <(tar -tf "$archive" | sort)
mapfile -t want_members < <(jq -r '.paths[]' <<<"$gtid_json" | xargs -n1 basename | sort)
if [[ "$(printf '%s\n' "${members[@]}")" != "$(printf '%s\n' "${want_members[@]}")" ]]; then
  echo "archive members [${members[*]}] want [${want_members[*]}]" >&2
  exit 1
fi
rm -f "$archive"

echo "[gtid-executed] restore dump then pipe stop_gtid command"
start_restore "$RESTORE_GTID" 310187
load_dump "$RESTORE_GTID"
apply_command "$gtid_json" "$RESTORE_GTID"
assert_rows "$RESTORE_GTID"

time_json="$(fetch_replay --data-urlencode "start_gtid_set=${backup_set}" --data-urlencode "stop_datetime=${STOP_CLOCK}")"
time_cmd="$(jq -r '.command' <<<"$time_json")"
if ! printf '%s\n' "$time_cmd" | grep -q -- "--exclude-gtids=" \
  || ! printf '%s\n' "$time_cmd" | grep -q -- "--stop-datetime='${STOP_CLOCK}'" \
  || printf '%s\n' "$time_cmd" | grep -q -- '--stop-position=' \
  || printf '%s\n' "$time_cmd" | grep -q -- '--start-datetime='; then
  echo "datetime command shape: $time_cmd" >&2
  exit 1
fi
echo "[gtid-executed] restore dump then pipe stop_datetime command"
start_restore "$RESTORE_TIME" 310188
load_dump "$RESTORE_TIME"
apply_command "$time_json" "$RESTORE_TIME"
assert_rows "$RESTORE_TIME"

empty_json="$(fetch_replay --data-urlencode "start_gtid_set=${through4}" --data-urlencode "stop_gtid=${g5}")"
empty_note="$(jq -r '.note // empty' <<<"$empty_json")"
empty_cmd="$(jq -r '.command' <<<"$empty_json")"
empty_paths="$(jq '.paths | length' <<<"$empty_json")"
if [[ "$empty_paths" != "0" || -n "$empty_cmd" || "$empty_note" != "every transaction up to the stop is already in start_gtid_set" ]]; then
  echo "covered window: $empty_json" >&2
  exit 1
fi

assert_replay_status "$task_id" 400 "start_gtid_set has a gap before this task's backed-up range" \
  --data-urlencode "start_gtid_set=${old_set}" \
  --data-urlencode "stop_gtid=${g5}"
start_json="$(fetch_replay --data-urlencode "start_gtid_set=${start_point}" --data-urlencode "stop_gtid=${g5}")"
if [[ "$(jq -r '.command' <<<"$start_json")" == "" ]]; then
  echo "exact continuation was empty: $start_json" >&2
  exit 1
fi
assert_replay_status "$task_id" 400 "invalid start_gtid_set" \
  --data-urlencode "start_gtid_set=not-a-set" \
  --data-urlencode "stop_gtid=${g5}"
assert_replay_status "$task_id" 400 "start_gtid_set and start_datetime cannot both be set" \
  --data-urlencode "start_gtid_set=${backup_set}" \
  --data-urlencode "start_datetime=${STOP_CLOCK}" \
  --data-urlencode "stop_gtid=${g5}"
assert_replay_status "$task_id" 400 "stop_datetime or stop_gtid is required" \
  --data-urlencode "start_gtid_set=${backup_set}"
assert_replay_status missing 400 "invalid start_gtid_set" --data-urlencode "start_gtid_set=not-a-set" --data-urlencode "stop_gtid=${g5}"

maria_name="e2e-gtid-executed-maria-${RUN_TAG}"
maria_body="$(jq -n \
  --arg name "$maria_name" \
  --arg host "$E2E_SOURCE_HOST" \
  --arg user "$E2E_SOURCE_USER" \
  --arg pass "$E2E_SOURCE_PASS" \
  --argjson port "$E2E_MYSQL80_PORT" \
  '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mariadb",server_id:310186},start:{mode:"LATEST"},storage:{retention_days:7}}')"
maria_resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$maria_body")"
maria_id="$(printf '%s' "$maria_resp" | jq -r '.id // empty')"
if [[ -z "$maria_id" || "$maria_id" == "null" ]]; then
  echo "create mariadb task failed: $maria_resp" >&2
  exit 1
fi
assert_replay_status "$maria_id" 400 "start_gtid_set is not supported for this flavor" \
  --data-urlencode "start_gtid_set=${backup_set}" \
  --data-urlencode "stop_gtid=${g5}"

echo "[gtid-executed] restored rows 1-4 once; stop transaction and later writes stayed out"
