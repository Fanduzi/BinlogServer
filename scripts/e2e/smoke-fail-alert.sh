#!/usr/bin/env bash
# input: the suite all-in-one server, mysql80, meta-primary, the task data directory, and the suite server log
# output: proof that a sealed-file conflict and a file/pos MySQL 1236 with no stored GTID yield once as LEASE_DEGRADED with desired RUN and no RETRY_BACKOFF, that Start after that yield returns to RUNNING, and that one metadata outage returns to RUNNING without FAILED
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"
LOG="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-suite.log}"
RUN_TAG="$(date +%s)"
META_DOWN=0

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd jq
need_cmd docker

fail() {
  echo "[fail-alert] $*" >&2
  exit 1
}

restore_meta() {
  if [[ "$META_DOWN" == "1" ]]; then
    docker compose -f "$COMPOSE_FILE" start meta-primary >/dev/null 2>&1 || true
    META_DOWN=0
  fi
}
trap restore_meta EXIT

mysql80() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -Nse "$1" | tr -d '\r'
}

meta_sql() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary \
    mysql -uroot -proot binlog_meta -Nse "$1" | tr -d '\r'
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

event_count() {
  local id="$1"
  local type="$2"
  curl -fsS "$API/api/tasks/$id/events" | jq --arg t "$type" '[.[] | select(.type == $t)] | length'
}

create_latest() {
  local name="$1"
  local server_id="$2"
  local body resp code id
  body="$(jq -n \
    --arg name "$name" \
    --arg host "$E2E_SOURCE_HOST" \
    --arg user "$E2E_SOURCE_USER" \
    --arg pass "$E2E_SOURCE_PASS" \
    --argjson port "$E2E_MYSQL80_PORT" \
    --argjson sid "$server_id" \
    '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:{mode:"LATEST"},storage:{retention_days:7}}')"
  resp="$(mktemp)"
  code="$(curl -sS -o "$resp" -w '%{http_code}' -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
  [[ "$code" == "201" ]] || fail "create $name failed http=$code body=$(cat "$resp")"
  id="$(jq -r '.id // empty' "$resp")"
  rm -f "$resp"
  [[ "$id" =~ ^[0-9]+$ ]] || fail "create $name has no numeric id: $id"
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/api/tasks/$id/start")"
  [[ "$code" == "204" ]] || fail "start $id failed http=$code"
  printf '%s' "$id"
}

task_row() {
  # Pipe-separated. A tab escape is not reliable with this server's sql_mode.
  meta_sql "SELECT CONCAT_WS('|', state, desired_run, spec_revision, failed_spec_revision, IFNULL(owner_worker_id,''), epoch, retry_attempt, consecutive_source_failures) FROM backup_tasks WHERE id=${1}"
}

assert_yielded_once() {
  local id="$1"
  local failed retries yielded
  failed="$(event_count "$id" "TASK_FAILED")"
  retries="$(event_count "$id" "TASK_RETRY_BACKOFF")"
  yielded="$(event_count "$id" "TASK_LEASE_DEGRADED")"
  [[ "$failed" == "0" ]] || fail "TASK_FAILED count=$failed want 0"
  [[ "$retries" == "0" ]] || fail "TASK_RETRY_BACKOFF count=$retries want 0"
  [[ "$yielded" -ge 1 ]] || fail "TASK_LEASE_DEGRADED count=$yielded want at least 1"
}

yield_detail() {
  local id="$1"
  curl -fsS "$API/api/tasks/$id/events?limit=500" | jq -r '[.[] | select(.type=="TASK_LEASE_DEGRADED")] | sort_by(.sequence) | .[0].detail // empty'
}

# --- sealed file: first non-allowlisted error yields, then Start arms it ---
echo "[fail-alert] sealed-file conflict"
sealed_id="$(create_latest "e2e-fail-sealed-${RUN_TAG}" 310501)"
wait_state "$sealed_id" "RUNNING"
sealed_dir="$DATA_DIR/$sealed_id"
open_path=""
for _ in $(seq 1 60); do
  open_path="$(find "$sealed_dir" -maxdepth 1 -type f -name '*.open.e*' -printf '%T@ %p\n' 2>/dev/null | sort -n | awk 'END { print $2 }' || true)"
  if [[ -n "$open_path" ]]; then
    break
  fi
  sleep 0.5
done
[[ -n "$open_path" ]] || fail "no open segment under $sealed_dir"
open_base="$(basename "$open_path")"
sealed_epoch="${open_base##*.open.e}"
sealed_source="${open_base%.open.e*}"
[[ "$sealed_epoch" =~ ^[0-9]+$ && "$sealed_epoch" != "0" ]] || fail "open segment epoch=$sealed_epoch path=$open_path"
[[ "$sealed_source" == mysql-bin.* ]] || fail "open segment source=$sealed_source"
# sealTarget switches to name.sealed.eN when the plain file exists, so both
# names have to be present or the rotate seals the plain name and continues.
printf 'sealed-conflict\n' >"$sealed_dir/$sealed_source"
printf 'sealed-conflict\n' >"$sealed_dir/${sealed_source}.sealed.e${sealed_epoch}"
mysql80 "FLUSH BINARY LOGS;"
sealed_state=""
sealed_body=""
for _ in $(seq 1 100); do
  sealed_body="$(task_json "$sealed_id")"
  sealed_state="$(printf '%s' "$sealed_body" | jq -r '.state // empty')"
  if [[ "$sealed_state" == "RETRY_BACKOFF" ]]; then
    fail "sealed file entered RETRY_BACKOFF: $sealed_body"
  fi
  if [[ "$sealed_state" == "LEASE_DEGRADED" ]] || [[ "$(event_count "$sealed_id" "TASK_LEASE_DEGRADED")" -ge 1 ]]; then
    break
  fi
  sleep 0.3
done
[[ "$(event_count "$sealed_id" "TASK_LEASE_DEGRADED")" -ge 1 ]] || fail "sealed file did not yield: $sealed_body"
sealed_err="$(yield_detail "$sealed_id")"
[[ "$sealed_err" == *SEALED_FILE_EXISTS* ]] || fail "yield detail=$sealed_err"
if [[ "$sealed_state" == "LEASE_DEGRADED" ]]; then
  sealed_owner="$(printf '%s' "$sealed_body" | jq -r '.owner_worker_id // empty')"
  sealed_desired="$(printf '%s' "$sealed_body" | jq -r '.desired_run // empty')"
  [[ -z "$sealed_owner" && "$sealed_desired" == "RUN" ]] || fail "yield owner=$sealed_owner desired=$sealed_desired body=$sealed_body"
fi
assert_yielded_once "$sealed_id"
echo "[fail-alert] sealed task=$sealed_id yielded once"

rm -f "$sealed_dir/$sealed_source" "$sealed_dir/${sealed_source}.sealed.e${sealed_epoch}"
armed_ok=0
for _ in $(seq 1 40); do
  sealed_body="$(task_json "$sealed_id")"
  sealed_state="$(printf '%s' "$sealed_body" | jq -r '.state // empty')"
  if [[ "$sealed_state" == "RUNNING" ]]; then
    armed_ok=1
    break
  fi
  if [[ "$sealed_state" == "LEASE_DEGRADED" || "$sealed_state" == "FAILED" || "$sealed_state" == "STOPPED" ]]; then
    start_code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/api/tasks/$sealed_id/start" || true)"
    if [[ "$start_code" == "204" ]]; then
      wait_state "$sealed_id" "RUNNING"
      armed_ok=1
      break
    fi
  fi
  sleep 0.5
done
[[ "$armed_ok" == "1" ]] || fail "sealed task did not return to RUNNING: $(task_json "$sealed_id")"
armed="$(task_row "$sealed_id")"
IFS='|' read -r armed_state armed_desired _ _ _ _ armed_attempt armed_consecutive <<<"$armed"
[[ "$armed_state" == "RUNNING" && "$armed_desired" == "RUN" ]] || fail "Start did not arm row=$armed"
[[ "$armed_attempt" == "0" && "$armed_consecutive" == "0" ]] || fail "counters not cleared row=$armed"
echo "[fail-alert] after the yield the sealed task is RUNNING"

# --- file/pos resume, MySQL 1236, no stored GTID ---
echo "[fail-alert] file/pos 1236 without GTID"
purge_id="$(create_latest "e2e-fail-1236-${RUN_TAG}" 310502)"
wait_state "$purge_id" "RUNNING"
ckpt_file=""
for _ in $(seq 1 60); do
  cp="$(curl -fsS "$API/api/tasks/$purge_id/checkpoint")"
  ckpt_file="$(printf '%s' "$cp" | jq -r '.file // empty')"
  ckpt_gtid="$(printf '%s' "$cp" | jq -r '.gtid_set // empty')"
  if [[ -n "$ckpt_gtid" ]]; then
    fail "LATEST checkpoint stored a GTID: $cp"
  fi
  if [[ "$ckpt_file" == mysql-bin.* ]]; then
    break
  fi
  ckpt_file=""
  sleep 0.5
done
[[ -n "$ckpt_file" ]] || fail "no file/pos checkpoint: $(curl -fsS "$API/api/tasks/$purge_id/checkpoint")"
curl -fsS -X POST "$API/api/tasks/$purge_id/stop" >/dev/null
wait_state "$purge_id" "STOPPED"
cp="$(curl -fsS "$API/api/tasks/$purge_id/checkpoint")"
ckpt_file="$(printf '%s' "$cp" | jq -r '.file // empty')"
ckpt_gtid="$(printf '%s' "$cp" | jq -r '.gtid_set // empty')"
[[ -z "$ckpt_gtid" && "$ckpt_file" == mysql-bin.* ]] || fail "stopped checkpoint is not file/pos without GTID: $cp"
mysql80 "FLUSH BINARY LOGS;"
sleep 2
newest="$(mysql80 "SHOW BINARY LOGS;" | awk 'END { print $1 }')"
[[ -n "$newest" && "$newest" != "$ckpt_file" ]] || fail "flush did not open a file after $ckpt_file (newest=$newest)"
purged=0
for _ in $(seq 1 20); do
  mysql80 "PURGE BINARY LOGS TO '${newest}';" || true
  if ! mysql80 "SHOW BINARY LOGS;" | awk '{ print $1 }' | grep -qx "$ckpt_file"; then
    purged=1
    break
  fi
  sleep 0.5
done
[[ "$purged" == "1" ]] || fail "purge left $ckpt_file (to $newest)"
echo "[fail-alert] purged $ckpt_file; newest=$newest"
start_code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/api/tasks/$purge_id/start")"
[[ "$start_code" == "204" ]] || fail "start after purge http=$start_code"
purge_state=""
purge_body=""
for _ in $(seq 1 100); do
  purge_body="$(task_json "$purge_id")"
  purge_state="$(printf '%s' "$purge_body" | jq -r '.state // empty')"
  if [[ "$purge_state" == "RETRY_BACKOFF" ]]; then
    fail "1236 entered RETRY_BACKOFF: $purge_body"
  fi
  if [[ "$purge_state" == "LEASE_DEGRADED" ]] || [[ "$(event_count "$purge_id" "TASK_LEASE_DEGRADED")" -ge 1 ]]; then
    break
  fi
  sleep 0.3
done
[[ "$(event_count "$purge_id" "TASK_LEASE_DEGRADED")" -ge 1 ]] || fail "1236 did not yield on the first error: $purge_body"
purge_err="$(yield_detail "$purge_id")"
purge_err_l="$(printf '%s' "$purge_err" | tr '[:upper:]' '[:lower:]')"
[[ "$purge_err" == *1236* && "$purge_err_l" == *purged* && "$purge_err_l" == *binlog* ]] || fail "yield detail=$purge_err"
if [[ "$purge_state" == "LEASE_DEGRADED" ]]; then
  purge_owner="$(printf '%s' "$purge_body" | jq -r '.owner_worker_id // empty')"
  purge_desired="$(printf '%s' "$purge_body" | jq -r '.desired_run // empty')"
  [[ -z "$purge_owner" && "$purge_desired" == "RUN" ]] || fail "1236 yield owner=$purge_owner desired=$purge_desired body=$purge_body"
fi
assert_yielded_once "$purge_id"
echo "[fail-alert] 1236 task=$purge_id yielded once"

# --- one metadata outage, then RUNNING again without Start ---
echo "[fail-alert] kill metadata once"
meta_id="$(create_latest "e2e-fail-meta-${RUN_TAG}" 310503)"
wait_state "$meta_id" "RUNNING"
mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('fail-alert-before-${RUN_TAG}');"
before_cp=""
for _ in $(seq 1 60); do
  before_cp="$(curl -fsS "$API/api/tasks/$meta_id/checkpoint")"
  before_file="$(printf '%s' "$before_cp" | jq -r '.file // empty')"
  before_pos="$(printf '%s' "$before_cp" | jq -r '.pos // 0')"
  if [[ "$before_file" == mysql-bin.* && "$before_pos" != "0" ]]; then
    break
  fi
  before_cp=""
  sleep 0.5
done
[[ -n "$before_cp" ]] || fail "no checkpoint before metadata outage"
[[ -f "$LOG" ]] || fail "server log not found: $LOG"
log_offset="$(wc -c <"$LOG" | tr -d ' ')"
docker compose -f "$COMPOSE_FILE" kill meta-primary >/dev/null
META_DOWN=1
mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('fail-alert-during-${RUN_TAG}');"
runner_line=""
for _ in $(seq 1 75); do
  runner_line="$(tail -c +$((log_offset + 1)) "$LOG" | grep -F "\"task_id\":\"${meta_id}\"" | grep -F 'runner error' | head -n 1 || true)"
  if [[ -n "$runner_line" ]]; then
    break
  fi
  sleep 0.2
done
docker compose -f "$COMPOSE_FILE" start meta-primary >/dev/null
[[ -n "$runner_line" ]] || fail "metadata outage produced no runner error for task $meta_id"
meta_ready=0
for _ in $(seq 1 40); do
  if docker compose -f "$COMPOSE_FILE" exec -T meta-primary mysqladmin ping -h127.0.0.1 -uroot -proot >/dev/null 2>&1; then
    meta_ready=1
    break
  fi
  sleep 0.5
done
[[ "$meta_ready" == "1" ]] || fail "meta-primary did not accept connections"
META_DOWN=0
echo "[fail-alert] runner error seen; metadata is back"

meta_state=""
for _ in $(seq 1 180); do
  if ! meta_body="$(curl -fsS "$API/api/tasks/$meta_id" 2>/dev/null)"; then
    sleep 0.5
    continue
  fi
  meta_state="$(printf '%s' "$meta_body" | jq -r '.state // empty')"
  if [[ "$meta_state" == "FAILED" ]]; then
    fail "metadata blip wrote FAILED: $meta_body"
  fi
  if [[ "$meta_state" == "RUNNING" ]]; then
    break
  fi
  sleep 0.5
done
[[ "$meta_state" == "RUNNING" ]] || fail "task did not return to RUNNING (state=$meta_state)"
if tail -c +$((log_offset + 1)) "$LOG" | grep -F "task=${meta_id} state=FAILED" >/dev/null; then
  fail "server log recorded state=FAILED for task $meta_id"
fi
meta_failed="$(event_count "$meta_id" "TASK_FAILED")"
[[ "$meta_failed" == "0" ]] || fail "TASK_FAILED count=$meta_failed during metadata blip"
sleep 1
if tail -c +$((log_offset + 1)) "$LOG" | grep -F "task=${meta_id} state=FAILED" >/dev/null; then
  fail "server log recorded state=FAILED for task $meta_id after recovery"
fi
meta_state="$(task_json "$meta_id" | jq -r '.state // empty')"
[[ "$meta_state" == "RUNNING" ]] || fail "state left RUNNING after recovery: $meta_state"

mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('fail-alert-after-${RUN_TAG}');"
advanced=0
for _ in $(seq 1 60); do
  after_cp="$(curl -fsS "$API/api/tasks/$meta_id/checkpoint")"
  after_file="$(printf '%s' "$after_cp" | jq -r '.file // empty')"
  after_pos="$(printf '%s' "$after_cp" | jq -r '.pos // 0')"
  if [[ "$after_file" != "$before_file" || "$after_pos" != "$before_pos" ]]; then
    if [[ "$after_pos" != "0" ]]; then
      advanced=1
      break
    fi
  fi
  sleep 0.5
done
[[ "$advanced" == "1" ]] || fail "checkpoint did not advance after metadata recovery before=$before_cp after=$(curl -fsS "$API/api/tasks/$meta_id/checkpoint")"
echo "[fail-alert] metadata blip returned to RUNNING without Start task=$meta_id"
echo "[fail-alert] passed"
