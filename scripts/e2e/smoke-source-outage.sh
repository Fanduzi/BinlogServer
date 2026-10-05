#!/usr/bin/env bash
# input: mysql80, the suite API, and the task data directory
# output: proof that a source stop during an open dump leaves RUNNING for RETRY_BACKOFF, then resumes with a continuous binlog
# pos: docker coverage for a mid-dump source outage
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"
RUN_TAG="$(date +%s)"
BEFORE="outage-before-${RUN_TAG}"
AFTER="outage-after-${RUN_TAG}"

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
  docker compose -f "$COMPOSE_FILE" start mysql80 >/dev/null 2>&1 || true
  if wait_mysql80; then
    docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -e "SET GLOBAL binlog_transaction_compression=ON;" >/dev/null 2>&1 || true
  fi
}
trap restore_mysql80 EXIT

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
mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('${BEFORE}');"
executed_before="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
before_gtid="$(uuid_interval_end "$server_uuid" "$executed_before")"
if [[ "$before_gtid" == "0" ]]; then
  echo "source GTID_EXECUTED has no interval for $server_uuid: $executed_before" >&2
  exit 1
fi
task_dir="$DATA_DIR/$task_id"
found_before=0
for _ in $(seq 1 60); do
  if [[ -d "$task_dir" ]] && grep -a -q -F "$BEFORE" "$task_dir"/* 2>/dev/null; then
    found_before=1
    break
  fi
  sleep 0.5
done
if [[ "$found_before" != "1" ]]; then
  echo "backup did not store $BEFORE under $task_dir" >&2
  exit 1
fi
before_cp="$(curl -fsS "$API/api/tasks/$task_id/checkpoint")"
before_file="$(printf '%s' "$before_cp" | jq -r '.file // empty')"
before_pos="$(printf '%s' "$before_cp" | jq -r '.pos // empty')"
echo "[source-outage] checkpoint before stop file=$before_file pos=$before_pos"

echo "[source-outage] stop mysql80"
docker compose -f "$COMPOSE_FILE" stop mysql80 >/dev/null

saw_backoff=0
sample=""
for _ in $(seq 1 80); do
  resp="$(task_json "$task_id")"
  st="$(printf '%s' "$resp" | jq -r '.state // empty')"
  err="$(printf '%s' "$resp" | jq -r '.last_error // empty')"
  if [[ "$st" == "FAILED" ]]; then
    echo "outage moved the task to FAILED: $resp" >&2
    exit 1
  fi
  if [[ "$st" == "RETRY_BACKOFF" && "$err" == SOURCE_UNREACHABLE:* ]]; then
    saw_backoff=1
    sample="$resp"
    break
  fi
  sleep 0.25
done
if [[ "$saw_backoff" != "1" ]]; then
  echo "task stayed out of RETRY_BACKOFF during the source stop: $(task_json "$task_id")" >&2
  exit 1
fi
echo "[source-outage] observed RETRY_BACKOFF: $sample"

events="$(curl -fsS "$API/api/tasks/$task_id/events?limit=200")"
if ! printf '%s' "$events" | jq -e '[.[] | select(.type=="TASK_RETRY_BACKOFF" and (.detail|tostring|startswith("SOURCE_UNREACHABLE")))] | length > 0' >/dev/null; then
  echo "scheduler events did not record SOURCE_UNREACHABLE: $events" >&2
  exit 1
fi

echo "[source-outage] start mysql80"
docker compose -f "$COMPOSE_FILE" start mysql80 >/dev/null
wait_mysql80
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"

resumed=""
for _ in $(seq 1 60); do
  resp="$(task_json "$task_id")"
  st="$(printf '%s' "$resp" | jq -r '.state // empty')"
  err="$(printf '%s' "$resp" | jq -r '.last_error // empty')"
  if [[ "$st" == "FAILED" ]]; then
    echo "resume failed: $resp" >&2
    exit 1
  fi
  if [[ "$st" == "RUNNING" && -z "$err" ]]; then
    resumed=1
    break
  fi
  sleep 1
done
if [[ "${resumed:-}" != "1" ]]; then
  echo "task did not return to RUNNING: $(task_json "$task_id")" >&2
  exit 1
fi
echo "[source-outage] resumed RUNNING"

mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('${AFTER}');"
executed_after=""
for _ in $(seq 1 60); do
  if ! grep -a -q -F "$AFTER" "$task_dir"/* 2>/dev/null; then
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
  if [[ "$ckpt_file" == "$master_file" && "$ckpt_pos" == "$master_pos" && "$after_gtid" -gt "$before_gtid" ]]; then
    break
  fi
  executed_after=""
  sleep 0.5
done
if [[ -z "$executed_after" ]]; then
  echo "checkpoint did not line up with the source: checkpoint=$(curl -fsS "$API/api/tasks/$task_id/checkpoint") master=$(mysql80 "SHOW MASTER STATUS;")" >&2
  exit 1
fi
echo "[source-outage] checkpoint matches master file=$ckpt_file pos=$ckpt_pos gtid=${server_uuid}:${before_gtid}..${after_gtid}"

if [[ -z "$before_pos" || -z "$ckpt_pos" ]]; then
  echo "checkpoint pos missing: before=$before_file:$before_pos after=$ckpt_file:$ckpt_pos" >&2
  exit 1
fi
if [[ "$ckpt_file" == "$before_file" && "$ckpt_pos" -lt "$before_pos" ]]; then
  echo "checkpoint moved backwards: before=$before_file:$before_pos after=$ckpt_file:$ckpt_pos" >&2
  exit 1
fi

curl -fsS -X POST "$API/api/tasks/$task_id/stop" >/dev/null
wait_state "$task_id" "STOPPED"

shopt -s nullglob
segments=("$task_dir"/*)
if [[ "${#segments[@]}" -eq 0 ]]; then
  echo "no backup segments in $task_dir" >&2
  exit 1
fi
decoded=""
for segment in "${segments[@]}"; do
  [[ -f "$segment" ]] || continue
  base="$(basename "$segment")"
  # The mysql:8.0 server image does not ship mysqlbinlog. Percona 8.0 does,
  # and it is already part of this suite. The bytes are the MySQL 8.0 source's.
  docker compose -f "$COMPOSE_FILE" cp "$segment" "percona80:/tmp/source-outage-${base}"
  piece="$(docker compose -f "$COMPOSE_FILE" exec -T percona80 mysqlbinlog --verify-binlog-checksum -v "/tmp/source-outage-${base}" | tr -d '\r')"
  dup="$(printf '%s\n' "$piece" | grep -E '^# at [0-9]+$' | sort | uniq -d || true)"
  if [[ -n "$dup" ]]; then
    echo "duplicate event positions in $base: $dup" >&2
    exit 1
  fi
  decoded+="$piece"
done
count_exact() {
  printf '%s' "$decoded" | grep -a -o -F "$1" | wc -l | tr -d ' '
}
before_n="$(count_exact "$BEFORE")"
after_n="$(count_exact "$AFTER")"
before_gtid_n="$(count_exact "${server_uuid}:${before_gtid}'")"
after_gtid_n="$(count_exact "${server_uuid}:${after_gtid}'")"
if [[ "$before_n" != "1" || "$after_n" != "1" || "$before_gtid_n" != "1" || "$after_gtid_n" != "1" ]]; then
  echo "continuity failed: marker before=$before_n after=$after_n gtid before=$before_gtid_n after=$after_gtid_n (want 1 each)" >&2
  exit 1
fi
echo "[source-outage] mysqlbinlog verified; markers and GTIDs once each"
