#!/usr/bin/env bash
# input: mysql80, the suite API, and the task data directory
# output: proof that several mid-dump source restarts on the suite all-in-one leave RETRY_BACKOFF then RUNNING, never FAILED or SEGMENT_NOT_ON_WORKER, and that a LATEST task with no copied event yet still pulls rows committed during the outage; the pulled binlog stays checksum-clean with each GTID once
# pos: docker coverage for a mid-dump source outage
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"
RUN_TAG="$(date +%s)"
# Several restarts: the artificial rotate plus the claim loop's epoch bump
# only failed some of the time on tip aa2d08e2. Default is 5.
# The last cycle also starts a second LATEST task that has not copied a row,
# then inserts while binlog-server is stopped so that anchor is what resumes.
RESTARTS="${E2E_SOURCE_RESTARTS:-5}"
GAP1="outage-gap1-${RUN_TAG}"
GAP2="outage-gap2-${RUN_TAG}"
GAP3="outage-gap3-${RUN_TAG}"
PAUSED_PID=""

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

mysql80() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -Nse "$1" 2>/dev/null | tr -d '\r'
}

wait_mysql80() {
  local i
  for i in $(seq 1 60); do
    if docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysqladmin ping -h127.0.0.1 -proot >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "mysql80 did not accept connections" >&2
  return 1
}

restore_mysql80() {
  if [[ -n "${PAUSED_PID}" ]]; then
    kill -CONT "$PAUSED_PID" >/dev/null 2>&1 || true
    PAUSED_PID=""
  fi
  docker compose -f "$COMPOSE_FILE" start mysql80 >/dev/null 2>&1 || true
  if wait_mysql80; then
    docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -e "SET GLOBAL binlog_transaction_compression=ON;" >/dev/null 2>&1 || true
  fi
}
trap restore_mysql80 EXIT

server_pid() {
  if [[ -n "${E2E_SERVER_PID:-}" ]] && kill -0 "$E2E_SERVER_PID" 2>/dev/null; then
    printf '%s' "$E2E_SERVER_PID"
    return
  fi
  pgrep -n -f 'binlog-server-e2e' || true
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

# uuid_interval_end is the highest transaction number for this server.
# A LATEST task does not store gtid_set: events from the middle of a file
# are not a complete executed set. Continuity is the source GTID appearing
# once in mysqlbinlog, plus checkpoint file/pos matching SHOW MASTER STATUS.
uuid_interval_end() {
  local uuid="$1"
  local set="$2"
  local flat spec n
  flat="$(printf '%s' "$set" | tr -d ' \r\n')"
  case "$flat" in
    *"${uuid}:"*) ;;
    *)
      printf '0'
      return
      ;;
  esac
  spec="${flat#*"${uuid}:"}"
  spec="${spec%%,*}"
  n="$(printf '%s' "$spec" | grep -oE '[0-9]+' | sort -n | tail -1 || true)"
  printf '%s' "${n:-0}"
}

reject_segment_missing() {
  local resp="$1"
  local where="$2"
  local st err
  st="$(printf '%s' "$resp" | jq -r '.state // empty')"
  err="$(printf '%s' "$resp" | jq -r '.last_error // empty')"
  if [[ "$st" == "FAILED" || "$err" == *SEGMENT_NOT_ON_WORKER* ]]; then
    echo "$where: $resp" >&2
    exit 1
  fi
}

wait_backoff() {
  local id="$1"
  local where="$2"
  local resp st err
  for _ in $(seq 1 80); do
    resp="$(task_json "$id")"
    reject_segment_missing "$resp" "$where"
    st="$(printf '%s' "$resp" | jq -r '.state // empty')"
    err="$(printf '%s' "$resp" | jq -r '.last_error // empty')"
    if [[ "$st" == "RETRY_BACKOFF" && "$err" == SOURCE_UNREACHABLE:* ]]; then
      printf '%s' "$resp"
      return 0
    fi
    sleep 0.25
  done
  echo "$where stayed out of RETRY_BACKOFF: $(task_json "$id")" >&2
  return 1
}

wait_running() {
  local id="$1"
  local where="$2"
  local resp st err
  for _ in $(seq 1 60); do
    resp="$(task_json "$id")"
    reject_segment_missing "$resp" "$where"
    st="$(printf '%s' "$resp" | jq -r '.state // empty')"
    err="$(printf '%s' "$resp" | jq -r '.last_error // empty')"
    if [[ "$st" == "RUNNING" && -z "$err" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "$where did not return to RUNNING: $(task_json "$id")" >&2
  return 1
}

echo "[source-outage] compression off so row images stay readable"
wait_mysql80
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"

name="e2e-source-outage-${RUN_TAG}"
body="$(jq -n \
  --arg name "$name" \
  --arg host "$E2E_SOURCE_HOST" \
  --arg user "$E2E_SOURCE_USER" \
  --arg pass "$E2E_SOURCE_PASS" \
  --argjson port "$E2E_MYSQL80_PORT" \
  '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:310190},start:{mode:"LATEST"},storage:{retention_days:7}}')"
resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
task_id="$(printf '%s' "$resp" | jq -r '.id // empty')"
if [[ -z "$task_id" || "$task_id" == "null" ]]; then
  echo "create task failed: $resp" >&2
  exit 1
fi
curl -fsS -X POST "$API/api/tasks/$task_id/start" >/dev/null
wait_state "$task_id" "RUNNING"
echo "[source-outage] task $task_id running"

server_uuid="$(mysql80 "SELECT @@GLOBAL.server_uuid;")"
task_dir="$DATA_DIR/$task_id"
if ! [[ "$RESTARTS" =~ ^[1-9][0-9]*$ ]]; then
  echo "E2E_SOURCE_RESTARTS must be a positive integer, got $RESTARTS" >&2
  exit 1
fi

markers=()
gtids=()
before_file=""
before_pos=""
gap_id=""
gap1_gtid=""
gap2_gtid=""
gap3_gtid=""
for n in $(seq 1 "$RESTARTS"); do
  marker="outage-${RUN_TAG}-${n}"
  echo "[source-outage] cycle $n/$RESTARTS insert $marker"
  mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('${marker}');"
  executed="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
  gtid="$(uuid_interval_end "$server_uuid" "$executed")"
  if [[ "$gtid" == "0" ]]; then
    echo "source GTID_EXECUTED has no interval for $server_uuid: $executed" >&2
    exit 1
  fi
  found=0
  for _ in $(seq 1 60); do
    if [[ -d "$task_dir" ]] && grep -a -q -F "$marker" "$task_dir"/* 2>/dev/null; then
      found=1
      break
    fi
    sleep 0.5
  done
  if [[ "$found" != "1" ]]; then
    echo "backup did not store $marker under $task_dir" >&2
    exit 1
  fi
  before_cp="$(curl -fsS "$API/api/tasks/$task_id/checkpoint")"
  before_file="$(printf '%s' "$before_cp" | jq -r '.file // empty')"
  before_pos="$(printf '%s' "$before_cp" | jq -r '.pos // empty')"
  echo "[source-outage] cycle $n checkpoint before stop file=$before_file pos=$before_pos gtid=$gtid"

  if [[ "$n" == "$RESTARTS" ]]; then
    gap_name="e2e-source-outage-gap-${RUN_TAG}"
    gap_body="$(jq -n \
      --arg name "$gap_name" \
      --arg host "$E2E_SOURCE_HOST" \
      --arg user "$E2E_SOURCE_USER" \
      --arg pass "$E2E_SOURCE_PASS" \
      --argjson port "$E2E_MYSQL80_PORT" \
      '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:310191},start:{mode:"LATEST"},storage:{retention_days:7}}')"
    gap_resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$gap_body")"
    gap_id="$(printf '%s' "$gap_resp" | jq -r '.id // empty')"
    if [[ -z "$gap_id" || "$gap_id" == "null" ]]; then
      echo "create gap task failed: $gap_resp" >&2
      exit 1
    fi
    curl -fsS -X POST "$API/api/tasks/$gap_id/start" >/dev/null
    wait_state "$gap_id" "RUNNING"
    echo "[source-outage] gap task $gap_id running with no copied row yet"
  fi

  echo "[source-outage] cycle $n stop mysql80"
  docker compose -f "$COMPOSE_FILE" stop mysql80 >/dev/null

  sample="$(wait_backoff "$task_id" "cycle $n")"
  echo "[source-outage] cycle $n RETRY_BACKOFF: $sample"
  if [[ "$n" == "$RESTARTS" ]]; then
    gap_sample="$(wait_backoff "$gap_id" "gap task")"
    echo "[source-outage] gap task RETRY_BACKOFF: $gap_sample"
  fi

  if [[ "$n" == "1" ]]; then
    events="$(curl -fsS "$API/api/tasks/$task_id/events?limit=200")"
    if ! printf '%s' "$events" | jq -e '[.[] | select(.type=="TASK_RETRY_BACKOFF" and (.detail|tostring|startswith("SOURCE_UNREACHABLE")))] | length > 0' >/dev/null; then
      echo "scheduler events did not record SOURCE_UNREACHABLE: $events" >&2
      exit 1
    fi
  fi

  if [[ "$n" == "$RESTARTS" ]]; then
    PAUSED_PID="$(server_pid)"
    if [[ -z "$PAUSED_PID" ]]; then
      echo "binlog-server pid not found; cannot insert while the dump is paused" >&2
      exit 1
    fi
    kill -STOP "$PAUSED_PID"
    echo "[source-outage] paused binlog-server pid=$PAUSED_PID"
  fi

  echo "[source-outage] cycle $n start mysql80"
  docker compose -f "$COMPOSE_FILE" start mysql80 >/dev/null
  wait_mysql80
  mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"

  if [[ "$n" == "$RESTARTS" ]]; then
    mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('${GAP1}');"
    executed_gap="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
    gap1_gtid="$(uuid_interval_end "$server_uuid" "$executed_gap")"
    mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('${GAP2}');"
    executed_gap="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
    gap2_gtid="$(uuid_interval_end "$server_uuid" "$executed_gap")"
    mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('${GAP3}');"
    executed_gap="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
    gap3_gtid="$(uuid_interval_end "$server_uuid" "$executed_gap")"
    if [[ "$gap1_gtid" == "0" || "$gap2_gtid" == "0" || "$gap3_gtid" == "0" ]]; then
      echo "gap GTID missing: $gap1_gtid $gap2_gtid $gap3_gtid from $executed_gap" >&2
      exit 1
    fi
    if [[ "$gap1_gtid" -ge "$gap2_gtid" || "$gap2_gtid" -ge "$gap3_gtid" ]]; then
      echo "gap GTIDs are not increasing: $gap1_gtid $gap2_gtid $gap3_gtid" >&2
      exit 1
    fi
    echo "[source-outage] inserted $GAP1 $GAP2 $GAP3 while binlog-server was paused gtid=${server_uuid}:${gap1_gtid},${gap2_gtid},${gap3_gtid}"
    kill -CONT "$PAUSED_PID"
    PAUSED_PID=""
    echo "[source-outage] continued binlog-server"
  fi

  wait_running "$task_id" "cycle $n"
  echo "[source-outage] cycle $n resumed RUNNING"
  markers+=("$marker")
  gtids+=("$gtid")
done

after="outage-after-${RUN_TAG}"
mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('${after}');"
executed_after=""
last_gtid="${gtids[$((RESTARTS - 1))]}"
for _ in $(seq 1 60); do
  if ! grep -a -q -F "$after" "$task_dir"/* 2>/dev/null; then
    sleep 0.5
    continue
  fi
  cp="$(curl -fsS "$API/api/tasks/$task_id/checkpoint")"
  ckpt_file="$(printf '%s' "$cp" | jq -r '.file // empty')"
  ckpt_pos="$(printf '%s' "$cp" | jq -r '.pos // empty')"
  master="$(mysql80 "SHOW MASTER STATUS;")"
  master_file="$(printf '%s' "$master" | awk 'NR==1 { print $1 }')"
  master_pos="$(printf '%s' "$master" | awk 'NR==1 { print $2 }')"
  executed_after="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
  after_gtid="$(uuid_interval_end "$server_uuid" "$executed_after")"
  if [[ "$ckpt_file" == "$master_file" && "$ckpt_pos" == "$master_pos" && "$after_gtid" -gt "$last_gtid" ]]; then
    break
  fi
  executed_after=""
  sleep 0.5
done
if [[ -z "$executed_after" ]]; then
  echo "checkpoint did not line up with the source: checkpoint=$(curl -fsS "$API/api/tasks/$task_id/checkpoint") master=$(mysql80 "SHOW MASTER STATUS;")" >&2
  exit 1
fi
echo "[source-outage] checkpoint matches master file=$ckpt_file pos=$ckpt_pos gtid=${server_uuid}:${last_gtid}..${after_gtid}"
reject_segment_missing "$(task_json "$task_id")" "after resume"

if [[ -z "$before_pos" || -z "$ckpt_pos" ]]; then
  echo "checkpoint pos missing: before=$before_file:$before_pos after=$ckpt_file:$ckpt_pos" >&2
  exit 1
fi
if [[ "$ckpt_file" == "$before_file" && "$ckpt_pos" -lt "$before_pos" ]]; then
  echo "checkpoint moved backwards: before=$before_file:$before_pos after=$ckpt_file:$ckpt_pos" >&2
  exit 1
fi

gap_dir="$DATA_DIR/$gap_id"
found_gap=0
for _ in $(seq 1 60); do
  resp="$(task_json "$gap_id")"
  reject_segment_missing "$resp" "gap resume"
  if [[ -d "$gap_dir" ]] \
    && grep -a -q -F "$GAP1" "$gap_dir"/* 2>/dev/null \
    && grep -a -q -F "$GAP2" "$gap_dir"/* 2>/dev/null \
    && grep -a -q -F "$GAP3" "$gap_dir"/* 2>/dev/null \
    && grep -a -q -F "$after" "$gap_dir"/* 2>/dev/null; then
    found_gap=1
    break
  fi
  sleep 0.5
done
if [[ "$found_gap" != "1" ]]; then
  echo "gap task did not store $GAP1 $GAP2 $GAP3 $after under $gap_dir: $(task_json "$gap_id")" >&2
  exit 1
fi
gap_cp="$(curl -fsS "$API/api/tasks/$gap_id/checkpoint")"
gap_gtid_set="$(printf '%s' "$gap_cp" | jq -r '.gtid_set // empty')"
if [[ -n "$gap_gtid_set" ]]; then
  echo "LATEST checkpoint stored gtid_set: $gap_cp" >&2
  exit 1
fi
echo "[source-outage] gap task stored the outage rows without gtid_set"

markers+=("$after")
gtids+=("$after_gtid")

curl -fsS -X POST "$API/api/tasks/$task_id/stop" >/dev/null
wait_state "$task_id" "STOPPED"
curl -fsS -X POST "$API/api/tasks/$gap_id/stop" >/dev/null
wait_state "$gap_id" "STOPPED"

events="$(curl -fsS "$API/api/tasks/$task_id/events?limit=500")"
if printf '%s' "$events" | jq -e '[.[] | select((.type=="TASK_FAILED") or ((.detail|tostring)|contains("SEGMENT_NOT_ON_WORKER")))] | length > 0' >/dev/null; then
  echo "task events include FAILED or SEGMENT_NOT_ON_WORKER: $events" >&2
  exit 1
fi
gap_events="$(curl -fsS "$API/api/tasks/$gap_id/events?limit=500")"
if printf '%s' "$gap_events" | jq -e '[.[] | select((.type=="TASK_FAILED") or ((.detail|tostring)|contains("SEGMENT_NOT_ON_WORKER")))] | length > 0' >/dev/null; then
  echo "gap task events include FAILED or SEGMENT_NOT_ON_WORKER: $gap_events" >&2
  exit 1
fi

decode_segments() {
  local dir="$1"
  local prefix="$2"
  local segment base piece dup decoded
  shopt -s nullglob
  local segments=("$dir"/*)
  if [[ "${#segments[@]}" -eq 0 ]]; then
    echo "no backup segments in $dir" >&2
    return 1
  fi
  decoded=""
  for segment in "${segments[@]}"; do
    [[ -f "$segment" ]] || continue
    base="$(basename "$segment")"
    # The mysql:8.0 server image does not ship mysqlbinlog. Percona 8.0 does,
    # and it is already part of this suite. The bytes are the MySQL 8.0 source's.
    docker compose -f "$COMPOSE_FILE" cp "$segment" "percona80:/tmp/${prefix}-${base}"
    piece="$(docker compose -f "$COMPOSE_FILE" exec -T percona80 mysqlbinlog --verify-binlog-checksum -v "/tmp/${prefix}-${base}" | tr -d '\r')"
    dup="$(printf '%s\n' "$piece" | grep -E '^# at [0-9]+$' | sort | uniq -d || true)"
    if [[ -n "$dup" ]]; then
      echo "duplicate event positions in $prefix $base: $dup" >&2
      return 1
    fi
    decoded+="$piece"
  done
  if printf '%s\n' "$decoded" | grep 'end_log_pos 0' | grep -F 'Rotate to' >/dev/null; then
    echo "artificial rotate (end_log_pos 0) was written into a $prefix segment" >&2
    return 1
  fi
  printf '%s' "$decoded"
}

decoded="$(decode_segments "$task_dir" "source-outage")"
count_exact() {
  printf '%s' "$decoded" | grep -a -o -F "$1" | wc -l | tr -d ' '
}
for i in "${!markers[@]}"; do
  marker_n="$(count_exact "${markers[$i]}")"
  gtid_n="$(count_exact "${server_uuid}:${gtids[$i]}'")"
  if [[ "$marker_n" != "1" || "$gtid_n" != "1" ]]; then
    echo "continuity failed: marker ${markers[$i]}=$marker_n gtid ${gtids[$i]}=$gtid_n (want 1 each)" >&2
    exit 1
  fi
done
echo "[source-outage] mysqlbinlog verified; ${#markers[@]} markers and GTIDs once each"

gap_decoded="$(decode_segments "$gap_dir" "source-outage-gap")"
count_gap() {
  printf '%s' "$gap_decoded" | grep -a -o -F "$1" | wc -l | tr -d ' '
}
gap1_n="$(count_gap "$GAP1")"
gap2_n="$(count_gap "$GAP2")"
gap3_n="$(count_gap "$GAP3")"
gap_after_n="$(count_gap "$after")"
gap1_gtid_n="$(count_gap "${server_uuid}:${gap1_gtid}'")"
gap2_gtid_n="$(count_gap "${server_uuid}:${gap2_gtid}'")"
gap3_gtid_n="$(count_gap "${server_uuid}:${gap3_gtid}'")"
after_gtid_gap_n="$(count_gap "${server_uuid}:${after_gtid}'")"
if [[ "$gap1_n" != "1" || "$gap2_n" != "1" || "$gap3_n" != "1" || "$gap_after_n" != "1" || "$gap1_gtid_n" != "1" || "$gap2_gtid_n" != "1" || "$gap3_gtid_n" != "1" || "$after_gtid_gap_n" != "1" ]]; then
  echo "gap continuity failed: rows=$gap1_n,$gap2_n,$gap3_n after=$gap_after_n gtid=$gap1_gtid_n,$gap2_gtid_n,$gap3_gtid_n after_gtid=$after_gtid_gap_n (want 1 each)" >&2
  exit 1
fi
echo "[source-outage] gap task mysqlbinlog verified; outage markers and GTIDs once each"
