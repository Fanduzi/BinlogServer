#!/usr/bin/env bash
# input: mysql80 with GTID on, the suite API, the task data directory, and meta-primary binlog_files
# output: a continuous recoverable window, then a missing-source-file break in GET /window and binlog_server_recovery_breaks after a middle sealed segment is removed
# pos: docker coverage that the retained-chain window matches replay and survives a real catalog gap
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"
RUN_TAG="$(date +%s)"
DB="binlog_window_${RUN_TAG}"
task_id=""

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq
need_cmd date

mysql80() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot --batch --raw -Nse "$1" | tr -d '\r'
}

meta_sql() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary \
    mysql -uroot -proot binlog_meta -Nse "$1" | tr -d '\r'
}

restore_compression() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -e "SET GLOBAL binlog_transaction_compression=ON;" >/dev/null 2>&1 || true
}

cleanup() {
  restore_compression
}
trap cleanup EXIT

fail() {
  echo "[recovery-window] $*" >&2
  exit 1
}

abs_path() {
  local p="$1"
  if [[ "$p" == /* ]]; then
    printf '%s' "$p"
  else
    printf '%s' "$ROOT_DIR/${p#./}"
  fi
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
  fail "task $id did not reach $want: $(task_json "$id")"
}

gtid_subset() {
  local inner="$1"
  local outer="$2"
  inner="$(printf '%s' "$inner" | tr -d ' \r\n')"
  outer="$(printf '%s' "$outer" | tr -d ' \r\n')"
  mysql80 "SELECT GTID_SUBSET('${inner}', '${outer}');"
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
  fail "checkpoint does not include $want: $(curl -fsS "$API/api/tasks/$task_id/checkpoint")"
}

wait_sealed_count() {
  local want="$1"
  local i n
  for i in $(seq 1 60); do
    n="$(curl -fsS "$API/api/tasks/$task_id/files?limit=50" | jq '[.[] | select(.state == "SEALED")] | length')"
    if [[ "$n" -ge "$want" ]]; then
      return 0
    fi
    sleep 0.5
  done
  fail "expected at least $want sealed files: $(curl -fsS "$API/api/tasks/$task_id/files?limit=50")"
}

get_window() {
  local tmp code
  tmp="$(mktemp)"
  code="$(curl -sS -o "$tmp" -w '%{http_code}' "$API/api/tasks/$task_id/window")"
  if [[ "$code" != "200" ]]; then
    echo "window status $code $(cat "$tmp")" >&2
    rm -f "$tmp"
    exit 1
  fi
  cat "$tmp"
  rm -f "$tmp"
}

replay_at() {
  local stop="$1"
  local tmp code
  tmp="$(mktemp)"
  code="$(curl -sS -G -o "$tmp" -w '%{http_code}' "$API/api/tasks/$task_id/replay" --data-urlencode "stop_datetime=$stop")"
  if [[ "$code" != "200" ]]; then
    echo "replay status $code stop=$stop $(cat "$tmp")" >&2
    rm -f "$tmp"
    exit 1
  fi
  cat "$tmp"
  rm -f "$tmp"
}

clock_shift() {
  local instant="$1"
  local shift="$2"
  date -u -d "${instant} ${shift}" '+%Y-%m-%d %H:%M:%S'
}

metric_value() {
  local name="$1"
  local id="$2"
  curl -fsS "$API/metrics" | awk -v n="$name" -v id="$id" '
    index($1, n "{") == 1 && index($0, "task_id=\"" id "\"") { print $2; exit }
  '
}

echo "[recovery-window] compression off so each insert is its own event"
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"
mysql80 "CREATE DATABASE ${DB}; CREATE TABLE ${DB}.t (id INT PRIMARY KEY, v VARCHAR(32) NOT NULL);"
before="$(mysql80 "SELECT @@GLOBAL.gtid_executed;" | tr -d ' \n')"
name="e2e-recovery-window-${RUN_TAG}"
body="$(jq -n \
  --arg name "$name" \
  --arg gtid "$before" \
  --arg host "$E2E_SOURCE_HOST" \
  --arg user "$E2E_SOURCE_USER" \
  --arg pass "$E2E_SOURCE_PASS" \
  --argjson port "$E2E_MYSQL80_PORT" \
  '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:310182},start:{mode:"GTID",gtid_set:$gtid},storage:{retention_days:7}}')"
resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
task_id="$(printf '%s' "$resp" | jq -r '.id // empty')"
if [[ -z "$task_id" || "$task_id" == "null" ]]; then
  fail "create task failed: $resp"
fi
curl -fsS -X POST "$API/api/tasks/$task_id/start" >/dev/null
wait_state "$task_id" "RUNNING"
wait_gtid "$before"

for n in 1 2 3; do
  mysql80 "INSERT INTO ${DB}.t VALUES (${n}, 'row-${n}'); FLUSH BINARY LOGS;"
done
wait_sealed_count 3
curl -fsS -X POST "$API/api/tasks/$task_id/stop" >/dev/null
wait_state "$task_id" "STOPPED"
wait_sealed_count 3

window="$(get_window)"
printf '%s\n' "$window" > /tmp/recovery-window-continuous.json
echo "[recovery-window] continuous $(jq -c '{continuous,earliest,latest,gtid_set,breaks}' <<<"$window")"
continuous="$(jq -r '.continuous' <<<"$window")"
earliest="$(jq -r '.earliest // empty' <<<"$window")"
latest="$(jq -r '.latest // empty' <<<"$window")"
gtid_set="$(jq -r '.gtid_set // empty' <<<"$window")"
break_count="$(jq '.breaks | length' <<<"$window")"
[[ "$continuous" == "true" ]] || fail "expected a continuous window: $window"
[[ -n "$earliest" && -n "$latest" ]] || fail "expected earliest and latest: $window"
[[ -n "$gtid_set" ]] || fail "expected gtid_set: $window"
[[ "$break_count" == "0" ]] || fail "expected no breaks: $window"

inside="$(clock_shift "$latest" "+ 1 second")"
inside_replay="$(replay_at "$inside")"
inside_paths="$(jq '.paths | length' <<<"$inside_replay")"
inside_cmd="$(jq -r '.command // empty' <<<"$inside_replay")"
[[ "$inside_paths" -ge 1 && -n "$inside_cmd" ]] || fail "stop after latest should replay: $inside_replay"

before_stop="$(clock_shift "$earliest" "- 1 second")"
before_replay="$(replay_at "$before_stop")"
before_paths="$(jq '.paths | length' <<<"$before_replay")"
before_cmd="$(jq -r '.command // empty' <<<"$before_replay")"
[[ "$before_paths" == "0" && -z "$before_cmd" ]] || fail "stop before earliest should be empty: $before_replay"

files_json="$(curl -fsS "$API/api/tasks/$task_id/files?limit=50")"
sealed_count="$(jq '[.[] | select(.state == "SEALED")] | length' <<<"$files_json")"
[[ "$sealed_count" -ge 3 ]] || fail "need 3 sealed files, got $sealed_count: $files_json"
middle="$(jq -c '[.[] | select(.state == "SEALED")] | sort_by(.file_name) | .[1]' <<<"$files_json")"
source_name="$(jq -r '.file_name' <<<"$middle")"
file_path="$(jq -r '.file_path' <<<"$middle")"
[[ -n "$source_name" && "$source_name" != "null" && -n "$file_path" && "$file_path" != "null" ]] || fail "middle sealed file missing: $middle"
disk="$(abs_path "$file_path")"
rm -f "$disk"
[[ ! -e "$disk" ]] || fail "could not remove $disk"
deleted="$(meta_sql "DELETE FROM binlog_files WHERE task_id='${task_id}' AND source_file='${source_name}' AND state='SEALED'; SELECT ROW_COUNT();")"
left="$(meta_sql "SELECT COUNT(*) FROM binlog_files WHERE task_id='${task_id}' AND source_file='${source_name}' AND state='SEALED';")"
[[ "$left" == "0" ]] || fail "catalog row for $source_name still present (delete output: $deleted)"
echo "[recovery-window] removed middle sealed $source_name"

broken="$(get_window)"
printf '%s\n' "$broken" > /tmp/recovery-window-broken.json
echo "[recovery-window] broken $(jq -c '{continuous,earliest,latest,gtid_set,breaks}' <<<"$broken")"
[[ "$(jq -r '.continuous' <<<"$broken")" == "false" ]] || fail "expected a break: $broken"
jq -e --arg name "$source_name" 'any(.breaks[]?; (.reason | contains("missing source file")) and (.reason | contains($name)))' <<<"$broken" >/dev/null \
  || fail "expected missing source file $source_name: $broken"

breaks_metric="$(metric_value binlog_server_recovery_breaks "$task_id")"
age_metric="$(metric_value binlog_server_recovery_earliest_age_seconds "$task_id")"
[[ -n "$breaks_metric" ]] || fail "missing binlog_server_recovery_breaks for $task_id"
awk -v v="$breaks_metric" 'BEGIN { exit !(v+0 >= 1) }' || fail "breaks metric $breaks_metric"
[[ -n "$age_metric" ]] || fail "missing binlog_server_recovery_earliest_age_seconds for $task_id"
awk -v v="$age_metric" 'BEGIN { exit !(v+0 >= 0) }' || fail "earliest age metric $age_metric"

late="$(replay_at "$inside")"
jq -e --arg name "$source_name" '[.paths[]? | split("/") | last] | index($name) == null' <<<"$late" >/dev/null \
  || fail "replay still lists removed $source_name: $late"
if jq -e 'any(.breaks[]?; .reason | contains("gtid hole"))' <<<"$broken" >/dev/null; then
  echo "[recovery-window] gtid hole reported"
fi
echo "[recovery-window] ok task=$task_id removed=$source_name breaks=$breaks_metric earliest_age_seconds=$age_metric"
