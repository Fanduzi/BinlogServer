#!/usr/bin/env bash
# input: mysql57 source, a scratch metadata database, and the current binlog-server binary
# output: one Binlog Dump after a password change during Stop then Start, zero dumps when a reachable Stop is STOPPED, a proxy cut that leaves pending_dump_cleanup on STOPPED and clears it with zero dumps after the path returns, a held proxy path where Stop then Start never has two Binlog Dumps and ends with pending cleared and dumps=1, a v0.5.45-shaped RUNNING row that stays running after upgrade reconcile, dropping 000006 restores uk_task_file_epoch on schema 5, schema 4 still has pending_dump_cleanup after dropping 000005, and schema 3 (column absent) is refused by this binary with schema version too old and ./migrate up
# pos: acceptance check for the desired-run control loop, issue 193, issue 249, issue 252, and the spec_revision=0 upgrade reconcile
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
WORKER_B_ID="${E2E_WORKER_B_ID:-e2e-ctl-worker-b}"
WORKER_B_HEALTH_ADDR="${E2E_WORKER_B_HEALTH_ADDR:-127.0.0.1:18082}"
WORKER_B_LOG="${E2E_WORKER_B_LOG:-/tmp/binlog-server-e2e-ctl-loop-worker-b-${RUN_TAG}.log}"
SCRATCH_DB="binlog_meta_ctlloop"
DUMP_USER="e2ectl${RUN_TAG: -6}"
UPG_USER="e2eupg${RUN_TAG: -6}"
CUT_USER="e2ecut${RUN_TAG: -6}"
FENCE_USER="e2efc${RUN_TAG: -6}"
PASS1="ctlpass1"
PASS2="ctlpass2"
PROXY_PORT=""
PROXY_PID=""

CONTROL_PID=""
WORKER_PID=""
WORKER_B_PID=""
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
need_cmd python3

kill_pid() {
  local pid="$1"
  if [[ -n "$pid" ]]; then
    kill "$pid" >/dev/null 2>&1 || true
    wait "$pid" >/dev/null 2>&1 || true
  fi
}

cleanup() {
  stop_proxy
  kill_pid "$ALL_PID"
  kill_pid "$WORKER_B_PID"
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

start_worker_b() {
  mkdir -p "$DATA_DIR/worker-b"
  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="worker" \
  BINLOG_SERVER_CLUSTER_WORKER_ID="$WORKER_B_ID" \
  BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR="$WORKER_B_HEALTH_ADDR" \
  BINLOG_SERVER_DATA_DIR="$DATA_DIR/worker-b" \
  BINLOG_SERVER_META_DSN="$SCRATCH_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$WORKER_B_LOG" 2>&1 &
  WORKER_B_PID=$!
  wait_http "http://$WORKER_B_HEALTH_ADDR/healthz" "$WORKER_B_LOG"
}

stop_pair() {
  kill_pid "$WORKER_PID"
  WORKER_PID=""
  kill_pid "$CONTROL_PID"
  CONTROL_PID=""
}

port_open() {
  (echo >/dev/tcp/127.0.0.1/"$1") >/dev/null 2>&1
}

start_proxy() {
  local target="$1"
  PROXY_PORT=$((19000 + RUN_TAG % 1000))
  while port_open "$PROXY_PORT"; do
    PROXY_PORT=$((PROXY_PORT + 1))
  done
  # One process, same cut as killing socat: the dump's TCP path disappears.
  cat >/tmp/e2e-ctl-proxy-"${RUN_TAG}".py <<'PY'
import signal, socket, sys, threading, time
listen_port, target_port = int(sys.argv[1]), int(sys.argv[2])
holding = threading.Event()
def usr1(signum, frame):
    holding.set()
def usr2(signum, frame):
    holding.clear()
signal.signal(signal.SIGUSR1, usr1)
signal.signal(signal.SIGUSR2, usr2)
signal.signal(signal.SIGPIPE, signal.SIG_IGN)
ls = socket.socket()
ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
ls.bind(("127.0.0.1", listen_port))
ls.listen(32)
def pipe(a, b):
    try:
        while not holding.is_set():
            a.settimeout(0.2)
            try:
                data = a.recv(65536)
            except socket.timeout:
                continue
            if not data:
                return False
            while holding.is_set():
                time.sleep(0.05)
            b.sendall(data)
    except Exception:
        return False
    return True
def handle(client):
    if holding.is_set():
        try:
            while holding.is_set():
                time.sleep(0.05)
        finally:
            try:
                client.close()
            except Exception:
                pass
        return
    try:
        upstream = socket.create_connection(("127.0.0.1", target_port))
    except Exception:
        client.close()
        return
    leaked = threading.Event()
    def one_way(src, dst):
        if pipe(src, dst):
            leaked.set()
    threading.Thread(target=one_way, args=(client, upstream), daemon=True).start()
    if pipe(upstream, client):
        leaked.set()
    if leaked.is_set() or holding.is_set():
        return
    for s in (client, upstream):
        try:
            s.close()
        except Exception:
            pass
while True:
    client, _ = ls.accept()
    threading.Thread(target=handle, args=(client,), daemon=True).start()
PY
  python3 /tmp/e2e-ctl-proxy-"${RUN_TAG}".py "$PROXY_PORT" "$target" \
    >/tmp/e2e-ctl-proxy-"${RUN_TAG}".log 2>&1 &
  PROXY_PID=$!
  for _ in {1..50}; do
    if port_open "$PROXY_PORT"; then
      return 0
    fi
    sleep 0.1
  done
  echo "proxy not listening on $PROXY_PORT" >&2
  cat /tmp/e2e-ctl-proxy-"${RUN_TAG}".log >&2 || true
  return 1
}

hold_proxy() {
  if [[ -z "${PROXY_PID:-}" ]]; then
    echo "proxy is not running" >&2
    return 1
  fi
  kill -USR1 "$PROXY_PID"
}

resume_proxy() {
  if [[ -z "${PROXY_PID:-}" ]]; then
    echo "proxy is not running" >&2
    return 1
  fi
  kill -USR2 "$PROXY_PID"
}

stop_proxy() {
  if [[ -n "${PROXY_PID:-}" ]]; then
    kill "$PROXY_PID" >/dev/null 2>&1 || true
    wait "$PROXY_PID" >/dev/null 2>&1 || true
    PROXY_PID=""
  fi
  if [[ -n "${PROXY_PORT:-}" ]]; then
    for _ in {1..50}; do
      if ! port_open "$PROXY_PORT"; then
        return 0
      fi
      sleep 0.1
    done
  fi
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

source_exec "CREATE USER IF NOT EXISTS '${DUMP_USER}'@'%' IDENTIFIED BY '${PASS1}'; GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '${DUMP_USER}'@'%'; CREATE USER IF NOT EXISTS '${UPG_USER}'@'%' IDENTIFIED BY '${PASS1}'; GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '${UPG_USER}'@'%'; CREATE USER IF NOT EXISTS '${CUT_USER}'@'%' IDENTIFIED BY '${PASS1}'; GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '${CUT_USER}'@'%'; FLUSH PRIVILEGES;"

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
    PENDING="$(printf '%s' "$BODY" | jq -r '.pending_dump_cleanup.connection_id // 0')"
    if [[ "$DUMPS" != "0" || -n "$OWNER" || "$PENDING" != "0" ]]; then
      echo "STOPPED with dumps=$DUMPS owner=$OWNER pending=$PENDING" >&2
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

# Cut the path to the source while a dump is open. Stop must still become
# STOPPED, name the leftover connection, and clear it after the path returns
# without a manual KILL on the source.
start_proxy "$MYSQL57_PORT"
CUT_SID=$((SID + 2))
CUT_BODY="$(jq -n \
  --arg name "e2e-cut-${RUN_TAG}" \
  --arg user "$CUT_USER" \
  --arg pass "$PASS1" \
  --argjson port "$PROXY_PORT" \
  --argjson sid "$CUT_SID" \
  '{name:$name,cluster_key:$name,source:{host:"127.0.0.1",port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:{mode:"LATEST"},storage:{retention_days:7}}')"
CUT_CREATED="$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$CUT_BODY")"
CUT_ID="$(printf '%s' "$CUT_CREATED" | jq -r '.id // empty')"
if [[ -z "$CUT_ID" || "$CUT_ID" == "null" ]]; then
  echo "cut create failed: $CUT_CREATED" >&2
  exit 1
fi
HTTP="$(curl -sS -o /tmp/e2e-ctl-cut-start.resp -w '%{http_code}' -X POST "$API/api/tasks/$CUT_ID/start")"
if [[ "$HTTP" != "204" ]]; then
  echo "cut start failed http=$HTTP body=$(cat /tmp/e2e-ctl-cut-start.resp)" >&2
  exit 1
fi
wait_state "$CUT_ID" "RUNNING"
wait_dumps "$CUT_USER" 1
stop_proxy
if port_open "$PROXY_PORT"; then
  echo "proxy still listening on $PROXY_PORT" >&2
  exit 1
fi
wait_state "$CUT_ID" "RETRY_BACKOFF"
HTTP="$(curl -sS -o /tmp/e2e-ctl-cut-stop.resp -w '%{http_code}' -X POST "$API/api/tasks/$CUT_ID/stop")"
if [[ "$HTTP" != "204" ]]; then
  echo "cut stop failed http=$HTTP body=$(cat /tmp/e2e-ctl-cut-stop.resp) state=$(task_state "$CUT_ID")" >&2
  exit 1
fi
CONN=""
WARN=""
for _ in {1..90}; do
  BODY="$(task_json "$CUT_ID")"
  STATE="$(printf '%s' "$BODY" | jq -r '.state // empty')"
  if [[ "$STATE" == "STOPPED" ]]; then
    CONN="$(printf '%s' "$BODY" | jq -r '.pending_dump_cleanup.connection_id // 0')"
    WARN="$(printf '%s' "$BODY" | jq -r '.pending_dump_cleanup.warning // empty')"
    break
  fi
  sleep 0.5
done
if [[ "$(task_state "$CUT_ID")" != "STOPPED" || "$CONN" == "0" || "$CONN" == "null" || "$CONN" == "" ]]; then
  echo "unreachable stop did not stay pending; state=$(task_state "$CUT_ID") conn=$CONN body=$(task_json "$CUT_ID")" >&2
  cat "$WORKER_LOG" >&2 || true
  exit 1
fi
if [[ "$WARN" != *"may still be open"* ]]; then
  echo "pending warning=$WARN" >&2
  exit 1
fi
echo "[control-loop] unreachable stop STOPPED pending conn=$CONN warning=$WARN"
start_proxy "$MYSQL57_PORT"
CLEARED=""
for _ in {1..90}; do
  BODY="$(task_json "$CUT_ID")"
  LEFT="$(printf '%s' "$BODY" | jq -r '.pending_dump_cleanup.connection_id // 0')"
  DUMPS="$(dump_count "$CUT_USER")"
  if [[ ("$LEFT" == "0" || "$LEFT" == "null") && "$DUMPS" == "0" ]]; then
    CLEARED=1
    break
  fi
  sleep 1
done
if [[ -z "$CLEARED" ]]; then
  echo "path restored but pending=$(task_json "$CUT_ID" | jq -c '.pending_dump_cleanup // empty') dumps=$(dump_count "$CUT_USER")" >&2
  cat "$WORKER_LOG" >&2 || true
  exit 1
fi
EVENTS="$(curl -fsS "$API/api/tasks/$CUT_ID/events")"
PENDING_N="$(printf '%s' "$EVENTS" | jq '[.[] | select(.type=="DUMP_CLEANUP_PENDING")] | length')"
CLEARED_N="$(printf '%s' "$EVENTS" | jq '[.[] | select(.type=="DUMP_CLEANUP_CLEARED")] | length')"
if [[ "$PENDING_N" != "1" || "$CLEARED_N" != "1" ]]; then
  echo "cleanup events pending=$PENDING_N cleared=$CLEARED_N" >&2
  printf '%s\n' "$EVENTS" >&2
  exit 1
fi
echo "[control-loop] path restored pending cleared dumps=0"
stop_proxy

# Case 5 (#255): hold the TCP path so the old Binlog Dump stays up, Stop, then
# Start before that thread is gone. Two workers, schema 4. At most one dump
# the whole time. After the path forwards again, pending is clear and dumps=1.
start_worker_b
source_exec "CREATE USER IF NOT EXISTS '${FENCE_USER}'@'%' IDENTIFIED BY '${PASS1}'; GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '${FENCE_USER}'@'%'; FLUSH PRIVILEGES;"
start_proxy "$MYSQL57_PORT"
FENCE_SID=$((SID + 4))
FENCE_BODY="$(jq -n \
  --arg name "e2e-fence-${RUN_TAG}" \
  --arg user "$FENCE_USER" \
  --arg pass "$PASS1" \
  --argjson port "$PROXY_PORT" \
  --argjson sid "$FENCE_SID" \
  '{name:$name,cluster_key:$name,source:{host:"127.0.0.1",port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:{mode:"LATEST"},storage:{retention_days:7}}')"
FENCE_CREATED="$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$FENCE_BODY")"
FENCE_ID="$(printf '%s' "$FENCE_CREATED" | jq -r '.id // empty')"
if [[ -z "$FENCE_ID" || "$FENCE_ID" == "null" ]]; then
  echo "fence create failed: $FENCE_CREATED" >&2
  exit 1
fi
HTTP="$(curl -sS -o /tmp/e2e-ctl-fence-start.resp -w '%{http_code}' -X POST "$API/api/tasks/$FENCE_ID/start")"
if [[ "$HTTP" != "204" ]]; then
  echo "fence start failed http=$HTTP body=$(cat /tmp/e2e-ctl-fence-start.resp)" >&2
  exit 1
fi
wait_state "$FENCE_ID" "RUNNING"
wait_dumps "$FENCE_USER" 1
hold_proxy
for _ in {1..20}; do
  if [[ "$(dump_count "$FENCE_USER")" == "1" ]]; then
    break
  fi
  sleep 0.2
done
if [[ "$(dump_count "$FENCE_USER")" != "1" ]]; then
  echo "held path dropped the dump before stop; dumps=$(dump_count "$FENCE_USER")" >&2
  exit 1
fi
HTTP="$(curl -sS -o /tmp/e2e-ctl-fence-stop.resp -w '%{http_code}' -X POST "$API/api/tasks/$FENCE_ID/stop")"
if [[ "$HTTP" != "204" ]]; then
  echo "fence stop failed http=$HTTP body=$(cat /tmp/e2e-ctl-fence-stop.resp)" >&2
  exit 1
fi
HTTP="$(curl -sS -o /tmp/e2e-ctl-fence-restart.resp -w '%{http_code}' -X POST "$API/api/tasks/$FENCE_ID/start")"
if [[ "$HTTP" != "204" ]]; then
  echo "fence restart failed http=$HTTP body=$(cat /tmp/e2e-ctl-fence-restart.resp) state=$(task_state "$FENCE_ID")" >&2
  exit 1
fi
FENCE_MAX=1
# Close of a reset connection would drop the source thread within a few seconds.
# The held path must still show that one dump after Stop and Start.
sleep 5
NOW="$(dump_count "$FENCE_USER")"
if [[ "$NOW" != "1" ]]; then
  echo "old dump was gone while the path was held; dumps=$NOW state=$(task_state "$FENCE_ID") body=$(task_json "$FENCE_ID")" >&2
  exit 1
fi
for _ in {1..60}; do
  NOW="$(dump_count "$FENCE_USER")"
  if [[ "$NOW" -gt "$FENCE_MAX" ]]; then
    FENCE_MAX="$NOW"
  fi
  if [[ "$NOW" != "1" ]]; then
    echo "during hold dumps=$NOW max_dumps=$FENCE_MAX state=$(task_state "$FENCE_ID") body=$(task_json "$FENCE_ID")" >&2
    exit 1
  fi
  sleep 0.25
done
resume_proxy
FENCE_DONE=""
for _ in {1..90}; do
  NOW="$(dump_count "$FENCE_USER")"
  if [[ "$NOW" -gt "$FENCE_MAX" ]]; then
    FENCE_MAX="$NOW"
  fi
  BODY="$(task_json "$FENCE_ID")"
  LEFT="$(printf '%s' "$BODY" | jq -r '.pending_dump_cleanup.connection_id // 0')"
  STATE="$(printf '%s' "$BODY" | jq -r '.state // empty')"
  if [[ "$STATE" == "RUNNING" && ("$LEFT" == "0" || "$LEFT" == "null") && "$NOW" == "1" ]]; then
    FENCE_DONE=1
    break
  fi
  sleep 1
done
if [[ -z "$FENCE_DONE" || "$FENCE_MAX" != "1" ]]; then
  echo "fence end max_dumps=$FENCE_MAX state=$(task_state "$FENCE_ID") pending=$(task_json "$FENCE_ID" | jq -c '.pending_dump_cleanup // empty') dumps=$(dump_count "$FENCE_USER")" >&2
  cat "$WORKER_LOG" >&2 || true
  cat "$WORKER_B_LOG" >&2 || true
  exit 1
fi
echo "[dump-fence] max_dumps=${FENCE_MAX} pending_cleared dumps=1"
stop_proxy
kill_pid "$WORKER_B_PID"
WORKER_B_PID=""

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

# Schema 6 drops uk_task_file_epoch. One down restores that index and stays on
# schema 5, which still has pending_dump_cleanup. Dropping 000005 returns to
# schema 4 and keeps that column. One more down reaches schema 3, where the
# column is gone. This binary requires schema 6, so it refuses schema 3.
kill_pid "$ALL_PID"
ALL_PID=""
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate down --steps 1
)
SCHEMA="$(meta_exec "SELECT version, dirty FROM schema_migrations")"
SCHEMA="$(printf '%s' "$SCHEMA" | tr -d '[:space:]')"
if [[ "$SCHEMA" != "50" ]]; then
  echo "schema after dropping 000006: [$SCHEMA] want 5 0" >&2
  exit 1
fi
FILE_EPOCH="$(meta_exec "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND INDEX_NAME = 'uk_task_file_epoch'")"
SOURCE_EPOCH="$(meta_exec "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND INDEX_NAME = 'uk_task_source_epoch'")"
if [[ "$FILE_EPOCH" == "0" || "$SOURCE_EPOCH" == "0" ]]; then
  echo "schema 5 indexes file_epoch=$FILE_EPOCH source_epoch=$SOURCE_EPOCH" >&2
  exit 1
fi
echo "[control-loop] schema 5 restored uk_task_file_epoch"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate down --steps 1
)
SCHEMA="$(meta_exec "SELECT version, dirty FROM schema_migrations")"
SCHEMA="$(printf '%s' "$SCHEMA" | tr -d '[:space:]')"
if [[ "$SCHEMA" != "40" ]]; then
  echo "schema after dropping 000005: [$SCHEMA] want 4 0" >&2
  exit 1
fi
PENDING_COL="$(meta_exec "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'backup_tasks' AND COLUMN_NAME = 'pending_dump_cleanup'")"
if [[ "$PENDING_COL" != "1" ]]; then
  echo "schema 4 lost pending_dump_cleanup: $PENDING_COL" >&2
  exit 1
fi
echo "[control-loop] schema 4 keeps pending_dump_cleanup"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate down --steps 1
)
SCHEMA="$(meta_exec "SELECT version, dirty FROM schema_migrations")"
SCHEMA="$(printf '%s' "$SCHEMA" | tr -d '[:space:]')"
if [[ "$SCHEMA" != "30" ]]; then
  echo "schema after dropping 000004: [$SCHEMA] want 3 0" >&2
  exit 1
fi
PENDING_COL="$(meta_exec "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'backup_tasks' AND COLUMN_NAME = 'pending_dump_cleanup'")"
if [[ "$PENDING_COL" != "0" ]]; then
  echo "schema 3 still has pending_dump_cleanup column: $PENDING_COL" >&2
  exit 1
fi
echo "[control-loop] schema 3 column absent"

mkdir -p "$DATA_DIR/schema3"
: >"$ALL_LOG"
BINLOG_SERVER_MODE="cluster" \
BINLOG_SERVER_CLUSTER_ROLE="all-in-one" \
BINLOG_SERVER_CLUSTER_WORKER_ID="e2e-ctl-all" \
BINLOG_SERVER_LISTEN_ADDR="127.0.0.1:18080" \
BINLOG_SERVER_DATA_DIR="$DATA_DIR/schema3" \
BINLOG_SERVER_META_DSN="$SCRATCH_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$ALL_LOG" 2>&1 &
ALL_PID=$!
refused=0
for _ in $(seq 1 60); do
  if ! kill -0 "$ALL_PID" >/dev/null 2>&1; then
    refused=1
    break
  fi
  if curl -fsS "$API/healthz" >/dev/null 2>&1; then
    echo "schema 3 passed healthz" >&2
    exit 1
  fi
  sleep 1
done
if [[ "$refused" != "1" ]]; then
  echo "schema 3 process still running" >&2
  cat "$ALL_LOG" >&2 || true
  exit 1
fi
set +e
wait "$ALL_PID"
rc=$?
set -e
ALL_PID=""
if [[ "$rc" != "1" ]]; then
  echo "schema 3 exit $rc, want 1" >&2
  cat "$ALL_LOG" >&2 || true
  exit 1
fi
if ! grep -q 'schema version too old' "$ALL_LOG" || ! grep -q '\./migrate up' "$ALL_LOG"; then
  echo "schema 3 log missing schema version too old or ./migrate up" >&2
  cat "$ALL_LOG" >&2 || true
  exit 1
fi
echo "[control-loop] schema 3 refused; this binary requires schema 6"
