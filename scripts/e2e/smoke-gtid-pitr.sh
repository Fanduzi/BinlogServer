#!/usr/bin/env bash
# input: mysql80 with GTID on, the suite API, the task data directory, and the Percona 8.0 mysqlbinlog client
# output: a fresh MySQL 8.0 whose rows stop before one GTID, for both an open segment and the sealed rename of that segment, plus the plain-text 400 sentences
# pos: docker coverage for stopping replay before one MySQL GTID when several transactions share a second
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"
RUN_TAG="$(date +%s)"
DB="binlog_pitr_${RUN_TAG}"
STAMP=1704067200
RESTORE_OPEN="binlog-e2e-gtid-pitr-open"
RESTORE_SEALED="binlog-e2e-gtid-pitr-sealed"

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
  docker rm -f "$RESTORE_OPEN" "$RESTORE_SEALED" >/dev/null 2>&1 || true
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

stop_pos_of() {
  local cmd="$1"
  local pos
  pos="$(printf '%s\n' "$cmd" | grep -oE -- '--stop-position=[0-9]+' || true)"
  pos="${pos%%$'\n'*}"
  pos="${pos#--stop-position=}"
  if [[ ! "$pos" =~ ^[0-9]+$ ]]; then
    echo "missing stop-position in: $cmd" >&2
    exit 1
  fi
  printf '%s' "$pos"
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

apply_replay() {
  local json="$1"
  local restore="$2"
  local cmd stop_pos
  local -a args=()
  local -a start_arg=()
  cmd="$(jq -r '.command' <<<"$json")"
  stop_pos="$(stop_pos_of "$cmd")"
  if printf '%s\n' "$cmd" | grep -q -- '--start-datetime='; then
    local raw
    raw="$(printf '%s\n' "$cmd" | grep -oE -- "--start-datetime='[^']*'" | head -1)"
    start_arg=("$raw")
  fi
  local i=0 p abs dest
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    abs="$(abs_path "$p")"
    dest="/tmp/gtid-pitr-${restore}-${i}.bin"
    docker compose -f "$COMPOSE_FILE" cp "$abs" "percona80:${dest}" </dev/null
    args+=("$dest")
    i=$((i + 1))
  done < <(jq -r '.paths[]' <<<"$json")
  if [[ "$i" -lt 1 ]]; then
    echo "replay has no paths: $json" >&2
    exit 1
  fi
  echo "[gtid-pitr] apply $restore stop-position=$stop_pos files=${#args[@]}"
  local log
  log="$(mktemp)"
  if ! docker compose -f "$COMPOSE_FILE" exec -T -e TZ=UTC percona80 \
    mysqlbinlog ${start_arg[@]+"${start_arg[@]}"} --stop-position="$stop_pos" "${args[@]}" \
    | docker exec -i "$restore" mysql -uroot -proot --binary-mode >"$log" 2>&1; then
    echo "apply failed" >&2
    cat "$log" >&2
    rm -f "$log"
    exit 1
  fi
  rm -f "$log"
}

assert_rows() {
  local restore="$1"
  local got
  got="$(docker exec "$restore" mysql -uroot -proot --batch --raw -Nse "SELECT id, v FROM ${DB}.t ORDER BY id" | tr -d '\r')"
  local want
  want="$(printf '1\tgood-1\n2\tgood-2\n3\tgood-3')"
  if [[ "$got" != "$want" ]]; then
    echo "$restore rows [$got] want [$want]" >&2
    exit 1
  fi
  local extra
  extra="$(docker exec "$restore" mysql -uroot -proot --batch --raw -Nse "SELECT COUNT(*) FROM ${DB}.t WHERE id IN (1,2,3); SELECT COUNT(*) FROM ${DB}.t WHERE id >= 4;" | tr -d '\r')"
  if [[ "$extra" != $'3\n0' ]]; then
    echo "$restore counts [$extra]" >&2
    exit 1
  fi
}

dump_paths() {
  local json="$1"
  local i=0 p abs dest piece dump=""
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    abs="$(abs_path "$p")"
    dest="/tmp/gtid-pitr-dump-${i}.bin"
    docker compose -f "$COMPOSE_FILE" cp "$abs" "percona80:${dest}" </dev/null
    piece="$(docker compose -f "$COMPOSE_FILE" exec -T percona80 mysqlbinlog -v "$dest" </dev/null | tr -d '\r')"
    dump+="$piece"$'\n'
    i=$((i + 1))
  done < <(jq -r '.paths[]' <<<"$json")
  printf '%s' "$dump"
}

gtid_header() {
  local dump="$1"
  local gtid="$2"
  awk -v g="$gtid" '
    /^#[0-9]{6} / { hdr = $0 }
    index($0, g) && /GTID_NEXT/ { print hdr; exit }
  ' <<<"$dump"
}

clock_of_header() {
  local hdr="$1"
  local ymd time
  if [[ -z "$hdr" ]]; then
    echo "empty binlog header" >&2
    exit 1
  fi
  ymd="$(printf '%s\n' "$hdr" | awk '{ print substr($1, 2) }')"
  time="$(printf '%s\n' "$hdr" | awk '{ print $2 }')"
  if [[ ${#time} -eq 7 ]]; then
    time="0${time}"
  fi
  printf '20%s-%s-%s %s' "${ymd:0:2}" "${ymd:2:2}" "${ymd:4:2}" "$time"
}

echo "[gtid-pitr] compression off so each transaction is its own GTID event"
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"
before="$(mysql80 "SELECT @@GLOBAL.gtid_executed;" | tr -d ' \n')"
schema="$(mysql80_script "
SET @b = @@GLOBAL.gtid_executed;
CREATE DATABASE ${DB};
SELECT CONCAT('db\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SET @b = @@GLOBAL.gtid_executed;
CREATE TABLE ${DB}.t (id INT PRIMARY KEY, v VARCHAR(64) NOT NULL);
SELECT CONCAT('table\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
")"
table_gtid="$(sql_field table "$schema")"
echo "[gtid-pitr] schema gtid $table_gtid"

name="e2e-gtid-pitr-${RUN_TAG}"
body="$(jq -n \
  --arg name "$name" \
  --arg gtid "$before" \
  --arg host "$E2E_SOURCE_HOST" \
  --arg user "$E2E_SOURCE_USER" \
  --arg pass "$E2E_SOURCE_PASS" \
  --argjson port "$E2E_MYSQL80_PORT" \
  '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:310181},start:{mode:"GTID",gtid_set:$gtid},storage:{retention_days:7}}')"
resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
task_id="$(printf '%s' "$resp" | jq -r '.id // empty')"
if [[ -z "$task_id" || "$task_id" == "null" ]]; then
  echo "create task failed: $resp" >&2
  exit 1
fi
curl -fsS -X POST "$API/api/tasks/$task_id/start" >/dev/null
wait_state "$task_id" "RUNNING"
wait_gtid "$table_gtid"
task_dir="$(wait_task_dir)"
schema_open="$(one_open_name "$task_dir")"
echo "[gtid-pitr] schema open $schema_open"
mysql80 "FLUSH BINARY LOGS;"
schema_sealed="$(wait_sealed "$task_dir" "$schema_open")"
echo "[gtid-pitr] schema sealed $schema_sealed"

dml="$(mysql80_script "
SET TIMESTAMP=${STAMP};
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (1,'good-1');
SELECT CONCAT('g1\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (2,'good-2');
SELECT CONCAT('g2\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (3,'good-3');
SELECT CONCAT('g3\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SET @b = @@GLOBAL.gtid_executed;
DELETE FROM ${DB}.t;
SELECT CONCAT('bad\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (4,'after-bad');
SELECT CONCAT('g4\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (5,'after-bad-2');
SELECT CONCAT('g5\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
SET TIMESTAMP=$((STAMP + 1));
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (6,'next-second');
SELECT CONCAT('later\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
")"
bad="$(sql_field bad "$dml")"
g1="$(sql_field g1 "$dml")"
g4="$(sql_field g4 "$dml")"
g5="$(sql_field g5 "$dml")"
later="$(sql_field later "$dml")"
echo "[gtid-pitr] bad gtid $bad later $later"
wait_gtid "$later"

open_json="$(fetch_replay --data-urlencode "stop_gtid=${bad}" --data-urlencode "limit=1")"
open_cmd="$(jq -r '.command' <<<"$open_json")"
open_pos="$(stop_pos_of "$open_cmd")"
open_count="$(jq '.paths | length' <<<"$open_json")"
if [[ "$open_count" -lt 2 ]]; then
  echo "limit=1 dropped earlier segments: $open_json" >&2
  exit 1
fi
if ! jq -e 'any(.paths[]; test("\\.open\\.e"))' <<<"$open_json" >/dev/null; then
  echo "open replay has no .open.e segment: $open_json" >&2
  exit 1
fi
if ! jq -e --arg sealed "$schema_sealed" 'any(.paths[]; endswith($sealed))' <<<"$open_json" >/dev/null; then
  echo "open replay dropped sealed schema $schema_sealed: $open_json" >&2
  exit 1
fi
if printf '%s\n' "$open_cmd" | grep -q -- '--stop-datetime='; then
  echo "gtid command used stop-datetime: $open_cmd" >&2
  exit 1
fi
if ! printf '%s\n' "$open_cmd" | grep -q 'TZ=UTC mysqlbinlog'; then
  echo "command client: $open_cmd" >&2
  exit 1
fi
dml_open="$(jq -r '.paths[]' <<<"$open_json" | awk '/\.open\.e/ { base=$0 } END { n=split(base, a, "/"); print a[n] }')"
echo "[gtid-pitr] open stop-position=$open_pos file=$dml_open"

dump="$(dump_paths "$open_json")"
c1="$(clock_of_header "$(gtid_header "$dump" "$g1")")"
cbad="$(clock_of_header "$(gtid_header "$dump" "$bad")")"
c4="$(clock_of_header "$(gtid_header "$dump" "$g4")")"
c5="$(clock_of_header "$(gtid_header "$dump" "$g5")")"
clater="$(clock_of_header "$(gtid_header "$dump" "$later")")"
if [[ "$c1" != "$cbad" || "$c1" != "$c4" || "$c1" != "$c5" ]]; then
  echo "same-second transactions diverged: g1=$c1 bad=$cbad g4=$c4 g5=$c5" >&2
  exit 1
fi
if [[ "$clater" == "$c1" ]]; then
  echo "transaction after the bad GTID stayed in $c1" >&2
  exit 1
fi
echo "[gtid-pitr] shared second $c1 later $clater"

start_restore "$RESTORE_OPEN" 310183
apply_replay "$open_json" "$RESTORE_OPEN"
assert_rows "$RESTORE_OPEN"
echo "[gtid-pitr] open segment restore kept the three rows"

mysql80 "FLUSH BINARY LOGS;"
dml_sealed="$(wait_sealed "$task_dir" "$dml_open")"
rot="$(mysql80_script "
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (7,'after-rotate');
SELECT CONCAT('rot\t', GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b));
")"
rot_gtid="$(sql_field rot "$rot")"
wait_gtid "$rot_gtid"
newest="$(mysql80 "SHOW BINARY LOGS;" | awk 'END { print $1 }')"
sealed_json="$(fetch_replay --data-urlencode "stop_gtid=${bad}")"
sealed_cmd="$(jq -r '.command' <<<"$sealed_json")"
sealed_pos="$(stop_pos_of "$sealed_cmd")"
if [[ "$sealed_pos" != "$open_pos" ]]; then
  echo "seal changed stop-position from $open_pos to $sealed_pos" >&2
  exit 1
fi
if jq -e 'any(.paths[]; test("\\.open\\.e"))' <<<"$sealed_json" >/dev/null; then
  echo "sealed replay still lists an open segment: $sealed_json" >&2
  exit 1
fi
if ! jq -e --arg sealed "$dml_sealed" 'any(.paths[]; endswith($sealed))' <<<"$sealed_json" >/dev/null; then
  echo "sealed replay missing $dml_sealed: $sealed_json" >&2
  exit 1
fi
if jq -e --arg newest "$newest" 'any(.paths[]; endswith($newest) or contains($newest + ".open"))' <<<"$sealed_json" >/dev/null; then
  echo "sealed replay includes later file $newest: $sealed_json" >&2
  exit 1
fi
archive="$(mktemp)"
archive_code="$(curl -sS -G -o "$archive" -w '%{http_code}' "$API/api/tasks/$task_id/replay/archive" --data-urlencode "stop_gtid=${bad}")"
if [[ "$archive_code" != "200" ]]; then
  echo "archive status $archive_code" >&2
  exit 1
fi
got_members="$(tar -tf "$archive" | sed 's#^\./##' | sort)"
want_members="$(jq -r '.paths[] | split("/") | .[-1]' <<<"$sealed_json" | sort)"
rm -f "$archive"
if [[ "$got_members" != "$want_members" ]]; then
  echo "archive members [$got_members] want [$want_members]" >&2
  exit 1
fi
echo "[gtid-pitr] sealed $dml_sealed stop-position=$sealed_pos excludes $newest"

start_restore "$RESTORE_SEALED" 310184
apply_replay "$sealed_json" "$RESTORE_SEALED"
assert_rows "$RESTORE_SEALED"
echo "[gtid-pitr] sealed segment restore kept the three rows"

after="$(date -u -d "$cbad UTC + 1 second" '+%Y-%m-%d %H:%M:%S')"
equal_json="$(fetch_replay --data-urlencode "stop_gtid=${bad}" --data-urlencode "start_datetime=${cbad}")"
equal_cmd="$(jq -r '.command' <<<"$equal_json")"
if ! printf '%s\n' "$equal_cmd" | grep -q -- "--start-datetime='${cbad}'" || ! printf '%s\n' "$equal_cmd" | grep -q -- "--stop-position=${sealed_pos}"; then
  echo "start equal to the GTID second: $equal_cmd" >&2
  exit 1
fi

assert_replay_status "$task_id" 400 "invalid stop_gtid" --data-urlencode "stop_gtid=not-a-gtid"
assert_replay_status "$task_id" 400 "invalid stop_gtid" --data-urlencode "stop_gtid="
assert_replay_status "$task_id" 400 "invalid stop_gtid" --data-urlencode "stop_gtid=0-1-10"
assert_replay_status "$task_id" 400 "stop_gtid is not in this task's backed-up range" --data-urlencode "stop_gtid=${bad%:*}:999999999"
assert_replay_status "$task_id" 400 "stop_gtid is not in this task's backed-up range" --data-urlencode "stop_gtid=00000000-0000-0000-0000-000000000099:1"
assert_replay_status "$task_id" 400 "stop_datetime and stop_gtid cannot both be set" \
  --data-urlencode "stop_gtid=${bad}" \
  --data-urlencode "stop_datetime=${cbad}"
assert_replay_status "$task_id" 400 "start_datetime is after stop_gtid" \
  --data-urlencode "stop_gtid=${bad}" \
  --data-urlencode "start_datetime=${after}"
assert_replay_status missing 400 "invalid stop_gtid" --data-urlencode "stop_gtid=not-a-gtid"
assert_replay_status missing 404 "task not found" --data-urlencode "stop_gtid=${bad}"

maria_name="e2e-gtid-pitr-maria-${RUN_TAG}"
maria_body="$(jq -n \
  --arg name "$maria_name" \
  --arg host "$E2E_SOURCE_HOST" \
  --arg user "$E2E_SOURCE_USER" \
  --arg pass "$E2E_SOURCE_PASS" \
  --argjson port "$E2E_MYSQL80_PORT" \
  '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mariadb",server_id:310182},start:{mode:"LATEST"},storage:{retention_days:7}}')"
maria_resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$maria_body")"
maria_id="$(printf '%s' "$maria_resp" | jq -r '.id // empty')"
if [[ -z "$maria_id" || "$maria_id" == "null" ]]; then
  echo "create mariadb task failed: $maria_resp" >&2
  exit 1
fi
assert_replay_status "$maria_id" 400 "stop_gtid is not supported for this flavor" --data-urlencode "stop_gtid=${bad}"
assert_replay_status "$maria_id" 400 "stop_gtid is not supported for this flavor" --data-urlencode "stop_gtid=0-1-10"

echo "[gtid-pitr] restored rows 1-3 once; bad GTID and later writes stayed out"
