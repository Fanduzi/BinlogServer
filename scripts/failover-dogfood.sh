#!/usr/bin/env bash
# input: BINLOG_SERVER_BIN, optional FAILOVER_TRUNK_BIN, local mysqld, verify-binlog-server helper
# output: rerunnable dual-worker lease takeover evidence under FAILOVER_EVIDENCE and optional screenshots
# pos: live lever for one control plane plus two workers sharing one meta MySQL
# note: if this file changes, update this header and module README.md.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HELPER="${REPO_ROOT}/.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh"
export PATH="/usr/sbin:/usr/bin:${PATH:-/usr/bin}"

FAILOVER_ROOT="${FAILOVER_ROOT:-/tmp/binlog-failover-dogfood}"
RUN_ID="${FAILOVER_RUN_ID:-$(date +%Y%m%d%H%M%S)-$$}"
EVIDENCE="${FAILOVER_EVIDENCE:-/tmp/binlog-failover-evidence-${RUN_ID}}"
LEASE_TTL="${FAILOVER_LEASE_TTL:-15}"
LEASE_RENEW=5
LEASE_GRACE=30
META_PORT="${FAILOVER_META_PORT:-13316}"
SOURCE_PORT="${FAILOVER_SOURCE_PORT:-13306}"
CP_PORT="${FAILOVER_CP_PORT:-18080}"
PORT_A="${FAILOVER_PORT_A:-18081}"
PORT_B="${FAILOVER_PORT_B:-18082}"
PORT_C="${FAILOVER_PORT_C:-18083}"
META_DB="failover_meta"
META_DSN="meta:metapass@tcp(127.0.0.1:${META_PORT})/${META_DB}?parseTime=true"
SCENARIO=""
SCENARIO_EVIDENCE=""
TASK_ID=""
OLD_EPOCH=0

usage() {
  cat <<EOF
Usage: failover-dogfood.sh <run SCENARIO|live|perf|down>

  run happy|backoff|no-steal|no-stomp|failed|pause|cleanup
  live    functional lanes, including trunk happy when FAILOVER_TRUNK_BIN is set
  perf    five interleaved trunk/head kill-to-growing-file samples
  down    stop mysqld, control plane, and workers started by this script

Required:
  BINLOG_SERVER_BIN       head binlog-server (migrate binary beside it)
  FAILOVER_TRUNK_BIN      baseline binlog-server for live and perf (current main)

Optional:
  FAILOVER_LEASE_TTL      default 15
  FAILOVER_EVIDENCE       evidence root
  FAILOVER_SHOT_DIR       png directory (live writes /tmp/swarm-prod-failover-prove when unset)
  FAILOVER_CONSOLE_SHOT   png path; happy opens the console and checks task-drawer-worker
  FAILOVER_SKIP_CONSOLE   set to 1 to skip the console shot
EOF
}

say() {
  printf '%s\n' "$*" >&2
}

fail() {
  say "FAIL: $*"
  mkdir -p "${SCENARIO_EVIDENCE:-$EVIDENCE}"
  printf 'FAIL %s\n' "$*" > "${SCENARIO_EVIDENCE:-$EVIDENCE}/result.txt"
  dump_debug >&2 || true
  exit 1
}

pass() {
  say "PASS: $*"
  printf 'PASS %s\n' "$*" > "$SCENARIO_EVIDENCE/result.txt"
}

dump_debug() {
  local log
  for log in \
    "$FAILOVER_ROOT/cp/logs/stderr.log" \
    "$FAILOVER_ROOT/cp/logs/stdout.log" \
    "$FAILOVER_ROOT/worker-a/logs/stderr.log" \
    "$FAILOVER_ROOT/worker-b/logs/stderr.log" \
    "$FAILOVER_ROOT/worker-c/logs/stderr.log" \
    "$FAILOVER_ROOT/mysql-meta/mysql.err" \
    "$FAILOVER_ROOT/mysql-source/mysql.err"
  do
    if [[ -f "$log" ]]; then
      printf '\n----- %s -----\n' "$log"
      tail -n 40 "$log" || true
    fi
  done
}

need_int() {
  local name="$1" value="$2"
  [[ "$value" =~ ^[0-9]+$ ]] || fail "$name must be an integer, got $value"
}

port_listening() {
  local port="$1"
  ss -ltn 2>/dev/null | awk '{print $4}' | grep -E ":${port}$" >/dev/null 2>&1
}

stop_pid_file() {
  local file="$1" pid
  [[ -f "$file" ]] || return 0
  pid="$(tr -d '[:space:]' <"$file" || true)"
  if [[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then
    kill -9 "$pid" 2>/dev/null || true
    local _
    for _ in $(seq 1 50); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
  fi
  rm -f "$file"
}

stop_inserter() {
  stop_pid_file "$FAILOVER_ROOT/inserter.pid"
}

copy_log() {
  local src="$1" dest="$2"
  if [[ -f "$src" && -n "$dest" ]]; then
    mkdir -p "$(dirname "$dest")"
    cp "$src" "$dest" 2>/dev/null || true
  fi
}

export_verify_env() {
  export BINLOG_VERIFY_RUN_ID="$RUN_ID"
  export BINLOG_VERIFY_LISTEN="127.0.0.1:${CP_PORT}"
  export BINLOG_VERIFY_BASE="${FAILOVER_ROOT}/cp"
  export BINLOG_VERIFY_EVIDENCE="${SCENARIO_EVIDENCE}/helper"
  export BINLOG_VERIFY_META_DSN="$META_DSN"
  export BINLOG_VERIFY_MIGRATIONS="${REPO_ROOT}/migrations"
  export BINLOG_VERIFY_MIGRATE_BIN="$(dirname "${BINLOG_SERVER_BIN:-/tmp/binlog-missing}")/migrate"
  mkdir -p "$BINLOG_VERIFY_EVIDENCE"
}

stop_binlog() {
  stop_inserter
  local worker dest_root
  dest_root="${SCENARIO_EVIDENCE:-$EVIDENCE}"
  for worker in a b c; do
    copy_log "$FAILOVER_ROOT/worker-${worker}/logs/stderr.log" "$dest_root/worker-${worker}-stderr.log"
    stop_pid_file "$FAILOVER_ROOT/worker-${worker}/binlog-server.pid"
  done
  if [[ -f "$FAILOVER_ROOT/cp/binlog-server.pid" ]]; then
    copy_log "$FAILOVER_ROOT/cp/logs/stderr.log" "$dest_root/cp-stderr.log"
    export_verify_env
    "$HELPER" cleanup >/dev/null 2>&1 || true
  fi
  rm -rf "$FAILOVER_ROOT/cp" "$FAILOVER_ROOT/worker-a" "$FAILOVER_ROOT/worker-b" "$FAILOVER_ROOT/worker-c"
}

stop_mysql() {
  stop_pid_file "$FAILOVER_ROOT/mysql-meta/mysql.pid"
  stop_pid_file "$FAILOVER_ROOT/mysql-source/mysql.pid"
}

down() {
  stop_binlog || true
  stop_mysql || true
  rm -rf "$FAILOVER_ROOT"
}

mysql_exec() {
  local sock="$1"
  shift
  mysql --socket="$sock" -uroot --batch --skip-column-names "$@"
}

wait_mysql() {
  local sock="$1" err="$2" _
  for _ in $(seq 1 60); do
    if mysql --socket="$sock" -uroot -e 'SELECT 1' >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  say "mysql did not become ready: $sock"
  tail -n 40 "$err" >&2 || true
  return 1
}

start_mysql_instance() {
  local name="$1" port="$2" cnf_extra="$3"
  local base="$FAILOVER_ROOT/mysql-${name}"
  local sock="$base/mysql.sock"
  mkdir -p "$base/data"
  if [[ ! -d "$base/data/mysql" ]]; then
    mysqld --initialize-insecure --datadir="$base/data" --basedir=/usr --log-error="$base/init.err"
  fi
  cat >"$base/my.cnf" <<EOF
[mysqld]
datadir=${base}/data
socket=${sock}
pid-file=${base}/mysql.pid
port=${port}
bind-address=127.0.0.1
mysqlx=0
skip-name-resolve
log-error=${base}/mysql.err
innodb_buffer_pool_size=64M
performance_schema=OFF
${cnf_extra}
EOF
  mysqld --defaults-file="$base/my.cnf" --daemonize
  wait_mysql "$sock" "$base/mysql.err"
}

ensure_mysql() {
  local meta_sock="$FAILOVER_ROOT/mysql-meta/mysql.sock"
  local source_sock="$FAILOVER_ROOT/mysql-source/mysql.sock"
  if [[ -S "$meta_sock" ]] && mysql --socket="$meta_sock" -uroot -e 'SELECT 1' >/dev/null 2>&1 \
    && [[ -S "$source_sock" ]] && mysql --socket="$source_sock" -uroot -e 'SELECT 1' >/dev/null 2>&1; then
    return 0
  fi
  if port_listening "$META_PORT" || port_listening "$SOURCE_PORT"; then
    fail "meta port ${META_PORT} or source port ${SOURCE_PORT} is already in use"
  fi
  say "starting meta MySQL :${META_PORT} and source MySQL :${SOURCE_PORT}"
  start_mysql_instance meta "$META_PORT" $'skip-log-bin\nserver-id=102'
  start_mysql_instance source "$SOURCE_PORT" $'log-bin=mysql-bin\nbinlog-format=ROW\nserver-id=101'
  mysql_exec "$source_sock" -e "SHOW VARIABLES LIKE 'log_bin';" | grep -q 'ON' || fail "source log_bin is off"
  mysql_exec "$meta_sock" <<'SQL'
CREATE USER IF NOT EXISTS 'meta'@'%' IDENTIFIED BY 'metapass';
ALTER USER 'meta'@'%' IDENTIFIED BY 'metapass';
SQL
  mysql_exec "$source_sock" <<'SQL'
CREATE USER IF NOT EXISTS 'repl'@'%' IDENTIFIED BY 'replpass';
ALTER USER 'repl'@'%' IDENTIFIED BY 'replpass';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl'@'%';
CREATE DATABASE IF NOT EXISTS failover_src;
CREATE TABLE IF NOT EXISTS failover_src.t1 (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  v VARCHAR(1024) NOT NULL
);
FLUSH PRIVILEGES;
SQL
  mysql --protocol=TCP -h127.0.0.1 -P"$SOURCE_PORT" -urepl -preplpass -e 'SELECT 1' >/dev/null
}

reset_meta() {
  local sock="$FAILOVER_ROOT/mysql-meta/mysql.sock"
  mysql_exec "$sock" <<SQL
DROP DATABASE IF EXISTS ${META_DB};
CREATE DATABASE ${META_DB} CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
GRANT ALL PRIVILEGES ON ${META_DB}.* TO 'meta'@'%';
FLUSH PRIVILEGES;
SQL
}

write_cp_config() {
  mkdir -p "$FAILOVER_ROOT/cp"
  cat >"$FAILOVER_ROOT/cp/config.yaml" <<EOF
mode: cluster
cluster:
  role: control-plane
  lease_ttl_sec: ${LEASE_TTL}
  lease_renew_interval_sec: ${LEASE_RENEW}
  lease_grace_sec: ${LEASE_GRACE}
  failover_policy: rebuild_current_file
EOF
}

start_cp() {
  [[ -n "${BINLOG_SERVER_BIN:-}" && -x "$BINLOG_SERVER_BIN" ]] || fail "BINLOG_SERVER_BIN is not executable"
  [[ -x "$(dirname "$BINLOG_SERVER_BIN")/migrate" ]] || fail "migrate binary missing beside BINLOG_SERVER_BIN"
  export_verify_env
  write_cp_config
  say "launching control plane $( "$BINLOG_SERVER_BIN" --version | head -n 1 ) on :${CP_PORT}"
  if ! "$HELPER" launch >"$SCENARIO_EVIDENCE/launch.txt" 2>&1; then
    cat "$SCENARIO_EVIDENCE/launch.txt" >&2 || true
    fail "control plane launch failed"
  fi
  if ! "$HELPER" doctor >"$SCENARIO_EVIDENCE/doctor.txt" 2>&1; then
    cat "$SCENARIO_EVIDENCE/doctor.txt" >&2 || true
    fail "control plane doctor failed"
  fi
  grep -qx 'PASS' "$SCENARIO_EVIDENCE/doctor.txt" || fail "doctor did not print PASS"
}

worker_port() {
  case "$1" in
    a) printf '%s' "$PORT_A" ;;
    b) printf '%s' "$PORT_B" ;;
    c) printf '%s' "$PORT_C" ;;
    *) fail "unknown worker $1" ;;
  esac
}

start_worker() {
  local name="$1" id="worker-${1}" port base
  port="$(worker_port "$name")"
  base="$FAILOVER_ROOT/worker-${name}"
  rm -rf "$base"
  mkdir -p "$base/data" "$base/logs"
  cat >"$base/config.yaml" <<EOF
mode: cluster
cluster:
  role: worker
  worker_id: ${id}
  worker_health_listen_addr: 127.0.0.1:${port}
  lease_ttl_sec: ${LEASE_TTL}
  lease_renew_interval_sec: ${LEASE_RENEW}
  lease_grace_sec: ${LEASE_GRACE}
  failover_policy: rebuild_current_file
EOF
  say "starting ${id} health :${port}"
  (
    cd "$base"
    env -i \
      PATH="$PATH" \
      HOME="${HOME:-/tmp}" \
      BINLOG_SERVER_MODE=cluster \
      BINLOG_SERVER_CLUSTER_ROLE=worker \
      BINLOG_SERVER_CLUSTER_WORKER_ID="$id" \
      BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR="127.0.0.1:${port}" \
      BINLOG_SERVER_CLUSTER_LEASE_TTL_SEC="$LEASE_TTL" \
      BINLOG_SERVER_CLUSTER_LEASE_RENEW_INTERVAL_SEC="$LEASE_RENEW" \
      BINLOG_SERVER_CLUSTER_LEASE_GRACE_SEC="$LEASE_GRACE" \
      BINLOG_SERVER_CLUSTER_FAILOVER_POLICY=rebuild_current_file \
      BINLOG_SERVER_LISTEN_ADDR="127.0.0.1:${port}" \
      BINLOG_SERVER_DATA_DIR="$base/data" \
      BINLOG_SERVER_META_DSN="$META_DSN" \
      BINLOG_SERVER_LOG_FILE="$base/logs/binlog-server.log" \
      nohup "$BINLOG_SERVER_BIN" >>"$base/logs/stdout.log" 2>>"$base/logs/stderr.log" &
    echo $! >"$base/binlog-server.pid"
  )
  local pid _ code
  pid="$(tr -d '[:space:]' <"$base/binlog-server.pid")"
  for _ in $(seq 1 40); do
    code="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/healthz" 2>/dev/null || true)"
    if [[ "$code" == "200" ]]; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      fail "${id} exited before healthz"
    fi
    sleep 0.25
  done
  fail "${id} healthz did not return 200"
}

start_inserter() {
  stop_inserter
  local sock="$FAILOVER_ROOT/mysql-source/mysql.sock"
  (
    while true; do
      mysql --socket="$sock" -uroot -e "INSERT INTO failover_src.t1(v) VALUES ('tick');" >/dev/null 2>&1 || true
      sleep 0.3
    done
  ) >/dev/null 2>&1 &
  echo $! >"$FAILOVER_ROOT/inserter.pid"
}

curl_api() {
  local method="$1" path="$2" out="$3"
  shift 3
  "$HELPER" curl "$method" "$path" -o "$out" -w '%{http_code}' "$@"
}

py_field() {
  local file="$1" field="$2"
  python3 - "$file" "$field" <<'PY'
import json, sys
try:
    data = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(2)
value = data.get(sys.argv[2], "")
if value is None:
    value = ""
print(value)
PY
}

save_task() {
  local id="$1" dest="$2" code
  code="$(curl_api GET "/api/tasks/${id}" "$dest")" || return 1
  [[ "$code" == "200" ]]
}

create_task() {
  local key="$1" port="$2" password="$3" body="$SCENARIO_EVIDENCE/create.json" code sid
  sid="${FAILOVER_SERVER_ID:-410001}"
  cat >"$SCENARIO_EVIDENCE/create-request.json" <<EOF
{"name":"${key}","cluster_key":"${key}","source":{"host":"127.0.0.1","port":${port},"user":"repl","password":"${password}","flavor":"mysql","server_id":${sid}},"start":{"mode":"LATEST"},"storage":{"retention_days":7}}
EOF
  code="$(curl_api POST /api/tasks "$body" -H 'Content-Type: application/json' -d @"$SCENARIO_EVIDENCE/create-request.json")" || fail "create task curl failed"
  [[ "$code" == "201" ]] || fail "create task returned HTTP ${code}: $(cat "$body")"
  TASK_ID="$(py_field "$body" id)"
  [[ -n "$TASK_ID" ]] || fail "create task response has no id"
  say "created task ${TASK_ID}"
}

start_task() {
  local id="$1" code body="$SCENARIO_EVIDENCE/start.body"
  code="$(curl_api POST "/api/tasks/${id}/start" "$body")" || fail "start curl failed"
  [[ "$code" == "204" ]] || fail "start returned HTTP ${code}: $(cat "$body")"
}

lease_renewed_at() {
  local id="$1"
  mysql_exec "$FAILOVER_ROOT/mysql-meta/mysql.sock" -e \
    "SELECT DATE_FORMAT(renewed_at, '%Y-%m-%d %H:%i:%s.%f') FROM ${META_DB}.task_leases WHERE task_id='${id}'"
}

lease_expire_unix() {
  local id="$1"
  mysql_exec "$FAILOVER_ROOT/mysql-meta/mysql.sock" -e \
    "SELECT CAST(UNIX_TIMESTAMP(lease_expire_at) AS UNSIGNED) FROM ${META_DB}.task_leases WHERE task_id='${id}'"
}

dir_bytes() {
  local dir="$1"
  if [[ ! -d "$dir" ]]; then
    printf '0'
    return 0
  fi
  du -sb "$dir" | awk '{print $1}'
}

wait_task_owner() {
  local id="$1" owner="$2" state_mode="$3" timeout_s="$4" trace="$5"
  local started now code state got_owner epoch dest="$FAILOVER_ROOT/poll.json"
  : >"$trace"
  started="$(date +%s)"
  while true; do
    now="$(date +%s)"
    if (( now - started > timeout_s )); then
      return 1
    fi
    code="$(curl_api GET "/api/tasks/${id}" "$dest" || true)"
    if [[ "$code" == "200" ]]; then
      state="$(py_field "$dest" state || true)"
      got_owner="$(py_field "$dest" owner_worker_id || true)"
      epoch="$(py_field "$dest" epoch || true)"
      printf '%s\t%s\t%s\t%s\n' "$now" "$state" "$got_owner" "${epoch:-0}" >>"$trace"
      if [[ "$state" == "STOPPED" || "$state" == "STOPPING" ]]; then
        cp "$dest" "$SCENARIO_EVIDENCE/stopped.json" || true
        return 2
      fi
      if [[ "$got_owner" == "$owner" ]]; then
        if [[ "$state_mode" == "any" || "$state" == "$state_mode" ]]; then
          cp "$dest" "$SCENARIO_EVIDENCE/matched.json"
          return 0
        fi
      fi
    fi
    sleep 0.4
  done
}

hold_owner() {
  local id="$1" owner="$2" seconds="$3" trace="$4"
  local started now code state got_owner epoch dest="$FAILOVER_ROOT/poll.json"
  : >"$trace"
  started="$(date +%s)"
  while true; do
    now="$(date +%s)"
    code="$(curl_api GET "/api/tasks/${id}" "$dest" || true)"
    if [[ "$code" == "200" ]]; then
      state="$(py_field "$dest" state || true)"
      got_owner="$(py_field "$dest" owner_worker_id || true)"
      epoch="$(py_field "$dest" epoch || true)"
      printf '%s\t%s\t%s\t%s\n' "$now" "$state" "$got_owner" "${epoch:-0}" >>"$trace"
      if [[ "$state" == "STOPPED" || "$state" == "STOPPING" || "$got_owner" != "$owner" ]]; then
        cp "$dest" "$SCENARIO_EVIDENCE/hold-break.json" || true
        return 1
      fi
    fi
    if (( now - started >= seconds )); then
      cp "$dest" "$SCENARIO_EVIDENCE/still-a.json"
      return 0
    fi
    sleep 0.5
  done
}

wait_checkpoint() {
  local id="$1" dest="$2" code _ 
  for _ in $(seq 1 40); do
    code="$(curl_api GET "/api/tasks/${id}/checkpoint" "$dest" || true)"
    if [[ "$code" == "200" ]] && py_field "$dest" file >/dev/null 2>&1; then
      [[ -n "$(py_field "$dest" file)" ]] && return 0
    fi
    sleep 0.5
  done
  return 1
}

checkpoint_progressed() {
  python3 - "$1" "$2" <<'PY'
import json, re, sys
before = json.load(open(sys.argv[1]))
after = json.load(open(sys.argv[2]))

def seq(name):
    match = re.search(r"(\d+)$", name or "")
    return int(match.group(1)) if match else -1

bf, af = before.get("file") or "", after.get("file") or ""
bp, ap = int(before.get("pos") or 0), int(after.get("pos") or 0)
if af and (seq(af) > seq(bf) or (af == bf and ap >= bp)):
    sys.exit(0)
sys.exit(1)
PY
}

events_snapshot() {
  local id="$1" dest="$2"
  curl_api GET "/api/tasks/${id}/events?limit=200" "$dest" >/dev/null
}

assert_takeover_events() {
  local dest="$1"
  python3 - "$dest" <<'PY'
import json, sys
events = json.load(open(sys.argv[1])) or []
types = [event.get("type") for event in events]
if "TASK_LEASE_TAKEOVER" not in types:
    sys.exit("missing TASK_LEASE_TAKEOVER")
for blocked in ("TASK_STOPPED", "TASK_STOPPING"):
    if blocked in types:
        sys.exit("saw " + blocked)
PY
}

assert_no_stop_events() {
  python3 - "$1" <<'PY'
import json, sys
events = json.load(open(sys.argv[1])) or []
types = [event.get("type") for event in events]
for blocked in ("TASK_STOPPED", "TASK_STOPPING"):
    if blocked in types:
        sys.exit("saw " + blocked)
PY
}

prepare_running_on_a() {
  local key="$1" port="$2" password="$3" want="${4:-RUNNING}"
  ensure_mysql
  reset_meta
  start_cp
  start_worker a
  create_task "$key" "$port" "$password"
  start_task "$TASK_ID"
  wait_task_owner "$TASK_ID" worker-a "$want" 90 "$SCENARIO_EVIDENCE/states-a.tsv" || fail "worker A did not reach ${want} ($(tail -n 5 "$SCENARIO_EVIDENCE/states-a.tsv" 2>/dev/null | tr '\n' ' '))"
  cp "$SCENARIO_EVIDENCE/matched.json" "$SCENARIO_EVIDENCE/running-a.json"
  OLD_EPOCH="$(py_field "$SCENARIO_EVIDENCE/running-a.json" epoch)"
  [[ "${OLD_EPOCH:-0}" -gt 0 ]] || fail "worker A epoch was ${OLD_EPOCH:-empty}"
}

kill_worker() {
  local name="$1"
  stop_pid_file "$FAILOVER_ROOT/worker-${name}/binlog-server.pid"
}

pause_worker() {
  local name="$1" pid
  pid="$(tr -d '[:space:]' <"$FAILOVER_ROOT/worker-${name}/binlog-server.pid")"
  kill -STOP "$pid"
}

wait_for_renew() {
  local id="$1" first now _
  first="$(lease_renewed_at "$id" || true)"
  [[ -n "$first" ]] || fail "task ${id} has no lease row"
  for _ in $(seq 1 40); do
    sleep 0.5
    now="$(lease_renewed_at "$id" || true)"
    if [[ -n "$now" && "$now" != "$first" ]]; then
      return 0
    fi
  done
  fail "lease did not renew for task ${id}"
}

wait_b_running_and_growth() {
  local id="$1" timeout_s="$2" trace="$3"
  local started now code state owner epoch dest="$FAILOVER_ROOT/poll.json"
  local task_dir="$FAILOVER_ROOT/worker-b/data/${id}" seen=0 bytes grew=0
  : >"$trace"
  started="$(date +%s)"
  while true; do
    now="$(date +%s)"
    if (( now - started > timeout_s )); then
      return 1
    fi
    code="$(curl_api GET "/api/tasks/${id}" "$dest" || true)"
    if [[ "$code" == "200" ]]; then
      state="$(py_field "$dest" state || true)"
      owner="$(py_field "$dest" owner_worker_id || true)"
      epoch="$(py_field "$dest" epoch || true)"
      bytes="$(dir_bytes "$task_dir")"
      [[ "$bytes" =~ ^[0-9]+$ ]] || bytes=0
      printf '%s\t%s\t%s\t%s\t%s\n' "$now" "$state" "$owner" "${epoch:-0}" "$bytes" >>"$trace"
      if [[ "$state" == "STOPPED" || "$state" == "STOPPING" ]]; then
        cp "$dest" "$SCENARIO_EVIDENCE/stopped.json" || true
        return 2
      fi
      grew=0
      if (( bytes > seen && seen > 0 )); then
        grew=1
      fi
      if (( bytes > seen )); then
        seen=$bytes
      fi
      if [[ "$grew" == "1" && "$owner" == "worker-b" && "$state" == "RUNNING" ]]; then
        if [[ "${epoch:-0}" -gt "$OLD_EPOCH" ]]; then
          cp "$dest" "$SCENARIO_EVIDENCE/after-task.json"
          return 0
        fi
      fi
    fi
    sleep 0.4
  done
}

finish_checkpoint_proof() {
  local id="$1" _
  wait_checkpoint "$id" "$SCENARIO_EVIDENCE/before-checkpoint.json" || fail "checkpoint was not saved before failover"
  local before_file before_pos
  before_file="$(py_field "$SCENARIO_EVIDENCE/before-checkpoint.json" file)"
  before_pos="$(py_field "$SCENARIO_EVIDENCE/before-checkpoint.json" pos)"
  say "checkpoint before kill ${before_file}:${before_pos}"
}

assert_checkpoint_continued() {
  local id="$1" _
  for _ in $(seq 1 40); do
    curl_api GET "/api/tasks/${id}/checkpoint" "$SCENARIO_EVIDENCE/after-checkpoint.json" >/dev/null || true
    if checkpoint_progressed "$SCENARIO_EVIDENCE/before-checkpoint.json" "$SCENARIO_EVIDENCE/after-checkpoint.json"; then
      find "$FAILOVER_ROOT/worker-b/data/${id}" -type f -printf '%s %p\n' 2>/dev/null | sort >"$SCENARIO_EVIDENCE/files-b.txt" || true
      [[ -s "$SCENARIO_EVIDENCE/files-b.txt" ]] || fail "worker B data dir has no files"
      return 0
    fi
    sleep 0.5
  done
  fail "checkpoint did not continue: before=$(cat "$SCENARIO_EVIDENCE/before-checkpoint.json") after=$(cat "$SCENARIO_EVIDENCE/after-checkpoint.json" 2>/dev/null)"
}

shot_html() {
  local png="$1" title="$2"
  shift 2
  [[ -n "${FAILOVER_SHOT_DIR:-}" ]] || return 0
  mkdir -p "$(dirname "$png")"
  python3 - "$png" "$title" "$@" <<'PY'
import html, pathlib, subprocess, sys, tempfile, os
png, title, *files = sys.argv[1:]
parts = [f"<h1>{html.escape(title)}</h1>"]
for path in files:
    text = pathlib.Path(path).read_text(errors="replace") if os.path.exists(path) else "(missing)"
    parts.append(f"<h2>{html.escape(path)}</h2><pre>{html.escape(text[-8000:])}</pre>")
page = "<!doctype html><meta charset=utf-8><title>" + html.escape(title) + "</title><body style='font:14px/1.4 monospace;margin:24px'>" + "\n".join(parts) + "</body>"
tmp = tempfile.NamedTemporaryFile("w", suffix=".html", delete=False)
tmp.write(page)
tmp.close()
out = subprocess.run([
    "google-chrome", "--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
    "--window-size=1440,1100", f"--screenshot={png}", tmp.name,
], capture_output=True, text=True)
os.unlink(tmp.name)
if out.returncode != 0 or not os.path.exists(png):
    sys.stderr.write(out.stderr)
    sys.exit(1)
PY
}

shot_console() {
  local base_url="$1" task_id="$2" png="$3"
  python3 - "$base_url" "$task_id" "$png" <<'PY'
import base64, json, os, subprocess, sys, time, urllib.request
import websocket

base, task_id, png = sys.argv[1:]
port = 9333
user_dir = "/tmp/binlog-failover-chrome"
os.makedirs(os.path.dirname(png), exist_ok=True)
proc = subprocess.Popen([
    "google-chrome", "--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
    "--no-first-run", f"--user-data-dir={user_dir}", f"--remote-debugging-port={port}",
    "--window-size=1440,900", "about:blank",
], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
try:
    tabs = None
    for _ in range(40):
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/json", timeout=1) as resp:
                tabs = json.load(resp)
            break
        except Exception:
            time.sleep(0.25)
    if not tabs:
        sys.exit("chrome devtools did not open")
    page = next(tab for tab in tabs if tab.get("type") == "page")
    ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=20)
    seq = 0

    def cdp(method, params=None):
        global seq
        seq += 1
        ws.send(json.dumps({"id": seq, "method": method, "params": params or {}}))
        while True:
            msg = json.loads(ws.recv())
            if msg.get("id") == seq:
                return msg

    def evaluate(expr):
        msg = cdp("Runtime.evaluate", {"expression": expr, "awaitPromise": True, "returnByValue": True})
        result = msg.get("result", {}).get("result", {})
        if "exceptionDetails" in msg.get("result", {}):
            return "exception"
        return result.get("value")

    cdp("Page.enable")
    cdp("Page.navigate", {"url": base + "/ui/"})
    deadline = time.time() + 20
    while time.time() < deadline:
        if evaluate("document.querySelector('[data-testid=view-nav-tasks]') ? 'yes' : ''") == "yes":
            break
        time.sleep(0.3)
    else:
        sys.exit("console nav did not render")
    if evaluate("document.querySelector('[data-testid=view-nav-tasks]').click(); 'ok'") != "ok":
        sys.exit("could not open tasks")
    trigger = f"[data-testid='task-detail-trigger-{task_id}']"
    deadline = time.time() + 15
    while time.time() < deadline:
        if evaluate(f"document.querySelector(`{trigger}`) ? 'yes' : ''") == "yes":
            break
        time.sleep(0.3)
    else:
        sys.exit("task detail trigger missing")
    evaluate(f"document.querySelector(`{trigger}`).click(); 'ok'")
    deadline = time.time() + 15
    owner = ""
    while time.time() < deadline:
        owner = evaluate("document.querySelector('[data-testid=task-drawer-worker]')?.textContent?.trim() || ''") or ""
        if owner:
            break
        time.sleep(0.3)
    shot = cdp("Page.captureScreenshot", {"format": "png"})
    data = shot.get("result", {}).get("data")
    if not data:
        sys.exit("screenshot failed")
    with open(png, "wb") as fh:
        fh.write(base64.b64decode(data))
    if owner != "worker-b":
        sys.exit(f"task-drawer-worker={owner!r}")
finally:
    proc.kill()
    proc.wait(timeout=5)
PY
}

scenario_happy() {
  local key="failover-happy-${RUN_ID}"
  prepare_running_on_a "$key" "$SOURCE_PORT" replpass
  start_inserter
  mysql --socket="$FAILOVER_ROOT/mysql-source/mysql.sock" -uroot -e 'FLUSH BINARY LOGS;' >/dev/null
  finish_checkpoint_proof "$TASK_ID"
  start_worker b
  wait_for_renew "$TASK_ID"
  say "killing worker A"
  kill_worker a
  local rc=0
  wait_b_running_and_growth "$TASK_ID" "$((LEASE_TTL + 50))" "$SCENARIO_EVIDENCE/states.tsv" || rc=$?
  if [[ "$rc" == "2" ]]; then
    fail "happy path wrote STOPPED"
  fi
  if [[ "$rc" != "0" ]]; then
    fail "worker B did not reach RUNNING with growing files"
  fi
  events_snapshot "$TASK_ID" "$SCENARIO_EVIDENCE/events.json"
  assert_takeover_events "$SCENARIO_EVIDENCE/events.json" || fail "takeover events mismatch: $(cat "$SCENARIO_EVIDENCE/events.json")"
  assert_checkpoint_continued "$TASK_ID"
  if [[ -n "${FAILOVER_CONSOLE_SHOT:-}" && "${FAILOVER_SKIP_CONSOLE:-}" != "1" ]]; then
    shot_console "http://127.0.0.1:${CP_PORT}" "$TASK_ID" "$FAILOVER_CONSOLE_SHOT" || fail "console did not show worker-b"
  fi
  pass "owner=worker-b state=RUNNING epoch>$(py_field "$SCENARIO_EVIDENCE/running-a.json" epoch) no STOPPED"
}

scenario_backoff() {
  local key="failover-backoff-${RUN_ID}"
  prepare_running_on_a "$key" 1 replpass RETRY_BACKOFF
  cp "$SCENARIO_EVIDENCE/running-a.json" "$SCENARIO_EVIDENCE/backoff-a.json"
  start_worker b
  wait_task_owner "$TASK_ID" worker-a RETRY_BACKOFF 40 "$SCENARIO_EVIDENCE/states-backoff.tsv" || fail "RETRY_BACKOFF on A ended before kill"
  kill_worker a
  local rc=0
  wait_task_owner "$TASK_ID" worker-b any "$((LEASE_TTL + 40))" "$SCENARIO_EVIDENCE/states.tsv" || rc=$?
  [[ "$rc" != "2" ]] || fail "backoff takeover wrote STOPPED"
  [[ "$rc" == "0" ]] || fail "worker B did not claim RETRY_BACKOFF"
  cp "$SCENARIO_EVIDENCE/matched.json" "$SCENARIO_EVIDENCE/claimed-b.json"
  events_snapshot "$TASK_ID" "$SCENARIO_EVIDENCE/events.json"
  assert_no_stop_events "$SCENARIO_EVIDENCE/events.json" || fail "backoff events mismatch"
  [[ "$(py_field "$SCENARIO_EVIDENCE/claimed-b.json" epoch)" -gt "$OLD_EPOCH" ]] || fail "backoff claim did not increase epoch"
  pass "RETRY_BACKOFF claimed by worker-b"
}

scenario_no_steal() {
  local key="failover-nosteal-${RUN_ID}"
  prepare_running_on_a "$key" "$SOURCE_PORT" replpass
  start_worker b
  hold_owner "$TASK_ID" worker-a "$((LEASE_TTL + 8))" "$SCENARIO_EVIDENCE/states.tsv" || fail "B stole a valid lease or the task stopped"
  events_snapshot "$TASK_ID" "$SCENARIO_EVIDENCE/events.json"
  assert_no_stop_events "$SCENARIO_EVIDENCE/events.json" || fail "no-steal saw a stop event"
  python3 - "$SCENARIO_EVIDENCE/events.json" <<'PY' || fail "no-steal recorded TASK_LEASE_TAKEOVER"
import json, sys
events = json.load(open(sys.argv[1])) or []
if any(event.get("type") == "TASK_LEASE_TAKEOVER" for event in events):
    sys.exit(1)
PY
  pass "owner stayed worker-a past lease_ttl_sec"
}

scenario_no_stomp() {
  local key="failover-nostomp-${RUN_ID}"
  prepare_running_on_a "$key" "$SOURCE_PORT" replpass
  start_worker c
  hold_owner "$TASK_ID" worker-a 10 "$SCENARIO_EVIDENCE/states.tsv" || fail "worker C persisted the live task as STOPPED or stole it"
  events_snapshot "$TASK_ID" "$SCENARIO_EVIDENCE/events.json"
  assert_no_stop_events "$SCENARIO_EVIDENCE/events.json" || fail "no-stomp saw a stop event"
  pass "worker C startup left owner worker-a"
}

scenario_failed() {
  local key="failover-failed-${RUN_ID}"
  ensure_mysql
  reset_meta
  start_cp
  start_worker a
  create_task "$key" "$SOURCE_PORT" wrong-password
  start_task "$TASK_ID"
  local found=0 _ code
  for _ in $(seq 1 40); do
    code="$(curl_api GET "/api/tasks/${TASK_ID}" "$SCENARIO_EVIDENCE/failed.json" || true)"
    if [[ "$code" == "200" ]] && [[ "$(py_field "$SCENARIO_EVIDENCE/failed.json" state || true)" == "FAILED" ]]; then
      found=1
      break
    fi
    sleep 0.4
  done
  [[ "$found" == "1" ]] || fail "bad password did not reach FAILED"
  [[ -z "$(py_field "$SCENARIO_EVIDENCE/failed.json" owner_worker_id)" ]] || fail "FAILED kept owner $(py_field "$SCENARIO_EVIDENCE/failed.json" owner_worker_id)"
  kill_worker a
  start_worker b
  start_task "$TASK_ID"
  local saw=0
  for _ in $(seq 1 40); do
    curl_api GET "/api/tasks/${TASK_ID}/runs?limit=20" "$SCENARIO_EVIDENCE/runs.json" >/dev/null || true
    if python3 - "$SCENARIO_EVIDENCE/runs.json" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1])) or []
sys.exit(0 if any(row.get("worker_id") == "worker-b" for row in rows) else 1)
PY
    then
      saw=1
      break
    fi
    sleep 0.25
  done
  [[ "$saw" == "1" ]] || fail "worker B did not acquire after FAILED"
  pass "FAILED cleared the owner and worker-b acquired the next start"
}

scenario_pause() {
  local key="failover-pause-${RUN_ID}"
  prepare_running_on_a "$key" "$SOURCE_PORT" replpass
  start_inserter
  start_worker b
  wait_for_renew "$TASK_ID"
  local expire paused rc=0
  expire="$(lease_expire_unix "$TASK_ID")"
  [[ "$expire" =~ ^[0-9]+$ ]] || fail "could not read lease_expire_at"
  printf 'expire_unix=%s\n' "$expire" >"$SCENARIO_EVIDENCE/lease-expire.txt"
  paused="$(date +%s)"
  say "pausing worker A; lease expires at ${expire}"
  pause_worker a
  wait_task_owner "$TASK_ID" worker-b RUNNING "$((LEASE_TTL + LEASE_GRACE + 30))" "$SCENARIO_EVIDENCE/states.tsv" || rc=$?
  [[ "$rc" != "2" ]] || fail "pause takeover wrote STOPPED"
  [[ "$rc" == "0" ]] || fail "worker B did not claim after pause"
  python3 - "$SCENARIO_EVIDENCE/states.tsv" "$expire" <<'PY' || fail "B claimed before the lease expired"
import sys
expire = int(sys.argv[2])
first = None
for line in open(sys.argv[1]):
    parts = line.rstrip("\n").split("\t")
    if len(parts) >= 3 and parts[2] == "worker-b":
        first = int(parts[0])
        break
if first is None:
    sys.exit("no worker-b sample")
if first + 1 < expire:
    sys.exit(f"claimed before expiry sample={first} expire={expire}")
PY
  cp "$SCENARIO_EVIDENCE/matched.json" "$SCENARIO_EVIDENCE/after-task.json"
  local now goal
  goal=$((paused + LEASE_TTL + LEASE_GRACE))
  while true; do
    now="$(date +%s)"
    if (( now >= goal )); then
      break
    fi
    sleep 0.5
  done
  save_task "$TASK_ID" "$SCENARIO_EVIDENCE/after-grace.json" || fail "task read failed after grace"
  [[ "$(py_field "$SCENARIO_EVIDENCE/after-grace.json" owner_worker_id)" == "worker-b" ]] || fail "owner changed after grace"
  [[ "$(py_field "$SCENARIO_EVIDENCE/after-grace.json" state)" != "STOPPED" ]] || fail "STOPPED after grace"
  events_snapshot "$TASK_ID" "$SCENARIO_EVIDENCE/events.json"
  assert_takeover_events "$SCENARIO_EVIDENCE/events.json" || fail "pause events mismatch"
  pass "paused A; B claimed after expiry and still owned the task after grace"
}

scenario_cleanup() {
  ensure_mysql
  reset_meta
  start_cp
  start_worker a
  start_worker b
  {
    printf 'cp %s\n' "$CP_PORT"
    printf 'a %s\n' "$PORT_A"
    printf 'b %s\n' "$PORT_B"
    printf 'meta %s\n' "$META_PORT"
  } >"$SCENARIO_EVIDENCE/listeners-before.txt"
  port_listening "$CP_PORT" || fail "control plane port was not listening"
  port_listening "$PORT_A" || fail "worker A port was not listening"
  port_listening "$PORT_B" || fail "worker B port was not listening"
  port_listening "$META_PORT" || fail "meta port was not listening"
  stop_binlog
  stop_mysql
  local _ port quiet
  for _ in $(seq 1 30); do
    quiet=1
    for port in "$CP_PORT" "$PORT_A" "$PORT_B" "$META_PORT"; do
      if port_listening "$port"; then
        quiet=0
      fi
    done
    [[ "$quiet" == "1" ]] && break
    sleep 0.2
  done
  : >"$SCENARIO_EVIDENCE/listeners-after.txt"
  for port in "$CP_PORT" "$PORT_A" "$PORT_B" "$META_PORT"; do
    if port_listening "$port"; then
      ss -ltn >"$SCENARIO_EVIDENCE/listeners-after.txt" || true
      fail "port ${port} still listening"
    fi
    printf 'free %s\n' "$port" >>"$SCENARIO_EVIDENCE/listeners-after.txt"
  done
  [[ -d "$EVIDENCE" ]] || fail "evidence directory was removed"
  pass "ports free; evidence kept at ${EVIDENCE}"
}

measure_happy_seconds() {
  local key="failover-perf-${RUN_ID}-${RANDOM}"
  prepare_running_on_a "$key" "$SOURCE_PORT" replpass
  start_inserter
  start_worker b
  wait_for_renew "$TASK_ID"
  local started ended rc=0
  started="$(date +%s%N)"
  kill_worker a
  wait_b_running_and_growth "$TASK_ID" "$((LEASE_TTL + 50))" "$SCENARIO_EVIDENCE/states.tsv" || rc=$?
  ended="$(date +%s%N)"
  [[ "$rc" == "0" ]] || fail "perf sample did not reach growing files on B (rc=${rc})"
  python3 -c "print((${ended} - ${started}) / 1e9)"
}

run_scenario() {
  local fn="$1" name="${2:-$1}"
  fn="${fn//-/_}"
  SCENARIO="$name"
  SCENARIO_EVIDENCE="$EVIDENCE/$name"
  mkdir -p "$SCENARIO_EVIDENCE"
  export SCENARIO SCENARIO_EVIDENCE
  say "=== scenario ${name} bin=$("$BINLOG_SERVER_BIN" --version | head -n 1) ==="
  set +e
  (
    trap - EXIT
    set -euo pipefail
    "scenario_${fn}"
  )
  local rc=$?
  set -euo pipefail
  stop_binlog || true
  return "$rc"
}

write_lane_shot() {
  local png="$1" title="$2"
  shift 2
  FAILOVER_SHOT_DIR="$(dirname "$png")" shot_html "$png" "$title" "$@" || say "shot failed: $png"
}

run_live() {
  [[ -n "${FAILOVER_TRUNK_BIN:-}" && -x "$FAILOVER_TRUNK_BIN" ]] || fail "FAILOVER_TRUNK_BIN is required for live"
  local shot_root="${FAILOVER_SHOT_DIR:-/tmp/swarm-prod-failover-prove}"
  local head_bin="$BINLOG_SERVER_BIN" rc=0 failed=0
  mkdir -p "$shot_root" "$EVIDENCE"
  rc=0
  BINLOG_SERVER_BIN="$FAILOVER_TRUNK_BIN" run_scenario happy trunk-happy || rc=$?
  if [[ "$rc" != "0" ]]; then
    say "trunk happy missed (rc=${rc})"
    printf 'TRUNK_MISS rc=%s\n' "$rc" >"$EVIDENCE/trunk-happy/compare-note.txt"
    failed=0
  else
    printf 'TRUNK_PASS\n' >"$EVIDENCE/trunk-happy/compare-note.txt"
  fi
  export FAILOVER_CONSOLE_SHOT="${shot_root}/worker-7/console-owner.png"
  rc=0
  BINLOG_SERVER_BIN="$head_bin" run_scenario happy head-happy || rc=$?
  unset FAILOVER_CONSOLE_SHOT
  if [[ "$rc" != "0" ]]; then
    failed=1
    printf 'HEAD_FAIL rc=%s\n' "$rc" >>"$EVIDENCE/trunk-happy/compare-note.txt"
  else
    printf 'HEAD_PASS\n' >>"$EVIDENCE/trunk-happy/compare-note.txt"
  fi
  write_lane_shot "$shot_root/worker-1/failover-trunk-vs-head.png" "failover trunk vs head" \
    "$EVIDENCE/trunk-happy/compare-note.txt" "$EVIDENCE/trunk-happy/result.txt" "$EVIDENCE/head-happy/result.txt" \
    "$EVIDENCE/trunk-happy/after-task.json" "$EVIDENCE/head-happy/after-task.json"
  if [[ "$rc" == "0" ]]; then
    write_lane_shot "$shot_root/worker-2/failover-b-owns.png" "failover B owns" \
      "$EVIDENCE/head-happy/result.txt" "$EVIDENCE/head-happy/after-task.json" "$EVIDENCE/head-happy/events.json"
    write_lane_shot "$shot_root/worker-8/failover-files.png" "failover files" \
      "$EVIDENCE/head-happy/before-checkpoint.json" "$EVIDENCE/head-happy/after-checkpoint.json" "$EVIDENCE/head-happy/files-b.txt"
  fi
  local spec name png
  for spec in \
    "backoff|worker-3|failover-backoff.png|backoff-a.json|claimed-b.json|events.json" \
    "no-steal|worker-4|failover-no-steal.png|still-a.json|events.json" \
    "no-stomp|worker-5|failover-no-stomp.png|still-a.json|events.json" \
    "failed|worker-6|failover-failed-release.png|failed.json|runs.json" \
    "pause|worker-9|failover-pause.png|after-task.json|after-grace.json|lease-expire.txt" \
    "cleanup|worker-10|failover-cleanup.png|listeners-before.txt|listeners-after.txt"
  do
    IFS='|' read -r name worker png rest <<<"$spec"
    rc=0
    BINLOG_SERVER_BIN="$head_bin" run_scenario "$name" || rc=$?
    if [[ "$rc" != "0" ]]; then
      failed=1
    fi
    local args=("$EVIDENCE/$name/result.txt")
    local extra
    IFS='|' read -r -a extras <<<"$rest"
    for extra in "${extras[@]}"; do
      args+=("$EVIDENCE/$name/$extra")
    done
    write_lane_shot "$shot_root/$worker/$png" "$name" "${args[@]}"
    rc=0
  done
  if [[ "$failed" != "0" ]]; then
    say "live finished with failures; see $EVIDENCE"
    return 1
  fi
  say "live PASS evidence=$EVIDENCE shots=$shot_root"
}

median_of_file() {
  python3 - "$1" <<'PY'
import sys
xs = [float(line) for line in open(sys.argv[1]) if line.strip()]
xs.sort()
n = len(xs)
if n == 0:
    sys.exit("no samples")
if n % 2:
    print(xs[n // 2])
else:
    print((xs[n // 2 - 1] + xs[n // 2]) / 2)
PY
}

scenario_perf() {
  local seconds
  seconds="$(measure_happy_seconds)"
  printf '%s\n' "$seconds" >"$SCENARIO_EVIDENCE/seconds.txt"
  pass "seconds=${seconds}"
  printf '%s\n' "$seconds"
}

run_perf() {
  [[ -n "${FAILOVER_TRUNK_BIN:-}" && -x "$FAILOVER_TRUNK_BIN" ]] || fail "FAILOVER_TRUNK_BIN is required for perf"
  local head_bin="$BINLOG_SERVER_BIN" i sample
  mkdir -p "$EVIDENCE/perf"
  : >"$EVIDENCE/perf/trunk.txt"
  : >"$EVIDENCE/perf/head.txt"
  for i in 1 2 3 4 5; do
    say "perf sample ${i} trunk"
    BINLOG_SERVER_BIN="$FAILOVER_TRUNK_BIN"
    if ! run_scenario perf "perf-trunk-${i}"; then
      fail "trunk perf sample ${i} failed"
    fi
    sample="$(cat "$EVIDENCE/perf-trunk-${i}/seconds.txt")"
    printf '%s\n' "$sample" | tee -a "$EVIDENCE/perf/trunk.txt" >&2
    say "perf sample ${i} head (${sample}s was trunk)"
    BINLOG_SERVER_BIN="$head_bin"
    if ! run_scenario perf "perf-head-${i}"; then
      fail "head perf sample ${i} failed"
    fi
    sample="$(cat "$EVIDENCE/perf-head-${i}/seconds.txt")"
    printf '%s\n' "$sample" >>"$EVIDENCE/perf/head.txt"
    say "head sample ${i}: ${sample}s"
  done
  local trunk_median head_median
  trunk_median="$(median_of_file "$EVIDENCE/perf/trunk.txt")"
  head_median="$(median_of_file "$EVIDENCE/perf/head.txt")"
  python3 - "$trunk_median" "$head_median" "$EVIDENCE/perf/summary.txt" <<'PY'
import sys
trunk, head, path = float(sys.argv[1]), float(sys.argv[2]), sys.argv[3]
ok = head <= trunk + 5
text = f"trunk_median={trunk:.3f}\nhead_median={head:.3f}\nrule=head<=trunk+5\nresult={'PASS' if ok else 'FAIL'}\n"
open(path, "w").write(text)
sys.stdout.write(text)
sys.exit(0 if ok else 1)
PY
}

prepare_root() {
  need_int FAILOVER_LEASE_TTL "$LEASE_TTL"
  (( LEASE_TTL > LEASE_RENEW )) || fail "lease ttl ${LEASE_TTL} must be greater than renew ${LEASE_RENEW}"
  command -v mysqld >/dev/null || fail "mysqld is required"
  command -v mysql >/dev/null || fail "mysql client is required"
  command -v ss >/dev/null || fail "ss is required"
  [[ -x "$HELPER" ]] || fail "missing helper $HELPER"
  if [[ -d "$FAILOVER_ROOT" ]]; then
    down || true
  fi
  mkdir -p "$FAILOVER_ROOT" "$EVIDENCE"
  git -C "$REPO_ROOT" rev-parse HEAD >"$EVIDENCE/head-sha.txt" || true
}

main() {
  local cmd="${1:-}"
  if [[ -z "$cmd" || "$cmd" == "-h" || "$cmd" == "--help" ]]; then
    usage
    exit 2
  fi
  shift
  case "$cmd" in
    down)
      down
      say "down ${FAILOVER_ROOT}"
      ;;
    run)
      [[ -n "${1:-}" ]] || { usage; exit 2; }
      prepare_root
      trap down EXIT
      run_scenario "$1"
      ;;
    live)
      prepare_root
      trap down EXIT
      run_live
      ;;
    perf)
      prepare_root
      trap down EXIT
      run_perf
      ;;
    *)
      usage
      exit 2
      ;;
  esac
}

main "$@"
