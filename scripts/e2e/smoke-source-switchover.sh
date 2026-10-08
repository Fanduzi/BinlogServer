#!/usr/bin/env bash
# input: two MySQL 8.0 servers in a GTID pair behind one TCP address, the suite API, and the Percona 8.0 mysqlbinlog client
# output: proof that a GTID task follows the new primary without mixing files, that a LATEST task stops with SOURCE_SWITCHOVER, that source_chain, source_identity, and binlog_server_source_switchovers describe both paths, that the embedded Console contains the chain view, and that pointing the address back at the old primary stops instead of mixing
# pos: docker coverage for a VIP source switch
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"
RUN_TAG="$(date +%s)"
DB="binlog_switch_${RUN_TAG}"
NET="e2e-switch-net"
PRIMARY="e2e-switch-a"
REPLICA="e2e-switch-b"
RESTORE_FULL="e2e-switch-restore-full"
RESTORE_GTID="e2e-switch-restore-gtid"
CLIENT="e2e-switch-client"
PORT_VIP=13470
PORT_A=13471
PORT_B=13472
PORT_CTRL=13469
PROXY_PID=""

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq
need_cmd python3

mysql_exec() {
  local name="$1"
  local sql="$2"
  docker exec "$name" mysql -uroot -proot --batch --raw -Nse "$sql" 2>/dev/null | tr -d '\r'
}

mysql_script() {
  local name="$1"
  docker exec -i "$name" mysql -uroot -proot --batch --raw -N <<<"$2" 2>/dev/null | tr -d '\r'
}

cleanup() {
  if [[ -n "$PROXY_PID" ]]; then
    kill "$PROXY_PID" >/dev/null 2>&1 || true
    wait "$PROXY_PID" >/dev/null 2>&1 || true
  fi
  docker rm -f "$PRIMARY" "$REPLICA" "$RESTORE_FULL" "$RESTORE_GTID" "$CLIENT" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_sql() {
  local name="$1"
  local i
  for i in $(seq 1 90); do
    if mysql_exec "$name" "SELECT 1" | grep -q 1; then
      return 0
    fi
    sleep 2
  done
  echo "$name did not accept SQL" >&2
  docker logs "$name" >&2 || true
  return 1
}

start_proxy() {
  python3 - "$PORT_VIP" "$PORT_CTRL" "127.0.0.1" "$PORT_A" <<'PY' &
import socket, sys, threading
listen_port = int(sys.argv[1])
ctrl_port = int(sys.argv[2])
backend = [sys.argv[3], int(sys.argv[4])]
lock = threading.Lock()
pairs = []

def pipe(a, b):
    try:
        while True:
            data = a.recv(65536)
            if not data:
                break
            b.sendall(data)
    except Exception:
        pass
    finally:
        for s in (a, b):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except Exception:
                pass

def handle(client):
    with lock:
        host, port = backend[0], backend[1]
    try:
        upstream = socket.create_connection((host, port), timeout=10)
    except Exception:
        client.close()
        return
    with lock:
        pairs.append((client, upstream))
    t1 = threading.Thread(target=pipe, args=(client, upstream), daemon=True)
    t2 = threading.Thread(target=pipe, args=(upstream, client), daemon=True)
    t1.start()
    t2.start()

def drop():
    with lock:
        old = list(pairs)
        pairs.clear()
    for a, b in old:
        for s in (a, b):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except Exception:
                pass
            try:
                s.close()
            except Exception:
                pass

def control():
    srv = socket.socket()
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", ctrl_port))
    srv.listen(8)
    while True:
        c, _ = srv.accept()
        data = b""
        while b"\n" not in data:
            chunk = c.recv(1024)
            if not chunk:
                break
            data += chunk
        line = data.decode(errors="replace").strip()
        if line.startswith("SET "):
            parts = line.split()
            if len(parts) == 3:
                with lock:
                    backend[0] = parts[1]
                    backend[1] = int(parts[2])
                drop()
                c.sendall(b"OK\n")
                print("backend", parts[1], parts[2], flush=True)
        c.close()

threading.Thread(target=control, daemon=True).start()
ls = socket.socket()
ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
ls.bind(("127.0.0.1", listen_port))
ls.listen(128)
print("proxy up", listen_port, flush=True)
while True:
    c, _ = ls.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
PY
  PROXY_PID=$!
  local i
  for i in $(seq 1 30); do
    if mysql_exec_host "$PORT_VIP" "SELECT 1" | grep -q 1; then
      return 0
    fi
    sleep 1
  done
  echo "proxy did not reach MySQL" >&2
  return 1
}

mysql_exec_host() {
  local port="$1"
  local sql="$2"
  docker exec "$CLIENT" mysql -urepl -preplpass -h127.0.0.1 -P"$port" --protocol=tcp --connect-timeout=3 -Nse "$sql" 2>/dev/null | tr -d '\r'
}

abs_path() {
  local p="$1"
  if [[ "$p" == /* ]]; then
    printf '%s' "$p"
  else
    printf '%s' "$ROOT_DIR/${p#./}"
  fi
}

point_vip() {
  local port="$1"
  local reply
  reply="$(python3 -c 'import socket,sys; s=socket.create_connection(("127.0.0.1", int(sys.argv[1])), 3); s.sendall(("SET 127.0.0.1 %s\n"%sys.argv[2]).encode()); print(s.recv(16).decode().strip())' "$PORT_CTRL" "$port")"
  if [[ "$reply" != "OK" ]]; then
    echo "proxy SET failed: [$reply]" >&2
    return 1
  fi
  local i uuid
  for i in $(seq 1 20); do
    uuid="$(mysql_exec_host "$PORT_VIP" "SELECT @@server_uuid")"
    if [[ -n "$uuid" ]]; then
      printf '%s' "$uuid"
      return 0
    fi
    sleep 0.5
  done
  echo "VIP did not answer after SET $port" >&2
  return 1
}

task_json() {
  curl -fsS "$API/api/tasks/$1"
}

switch_metric() {
  local id="$1"
  local outcome="$2"
  curl -fsS "$API/metrics" | awk -v id="$id" -v outcome="$outcome" '
    $1 ~ /^binlog_server_source_switchovers\{/ && index($0, "task_id=\"" id "\"") && index($0, "outcome=\"" outcome "\"") {
      print $2
      exit
    }
  '
}

assert_switch_metric() {
  local id="$1"
  local outcome="$2"
  local op="$3"
  local want="$4"
  local got
  got="$(switch_metric "$id" "$outcome")"
  if [[ -z "$got" ]]; then
    echo "missing binlog_server_source_switchovers task=$id outcome=$outcome" >&2
    curl -fsS "$API/metrics" | grep source_switchovers >&2 || true
    exit 1
  fi
  if ! awk -v got="$got" -v want="$want" -v op="$op" 'BEGIN { exit !(op == "ge" ? (got+0 >= want+0) : (got+0 == want+0)) }'; then
    echo "source_switchovers task=$id outcome=$outcome got=$got want $op $want" >&2
    exit 1
  fi
}

assert_console_chain() {
  local html src found=0
  html="$(curl -fsS "$API/ui/")"
  while IFS= read -r src; do
    [[ -z "$src" ]] && continue
    case "$src" in
      http*) ;;
      /*) ;;
      *) src="/ui/${src#./}" ;;
    esac
    if curl -fsS "$API$src" | grep -q 'task-source-chain'; then
      found=1
      break
    fi
  done < <(printf '%s\n' "$html" | grep -oE 'src="[^"]+\.js"' | sed -e 's/^src="//' -e 's/"$//')
  if [[ "$found" != 1 ]]; then
    echo "embedded console is missing task-source-chain" >&2
    exit 1
  fi
  echo "[switch] console bundle has task-source-chain"
}

wait_state() {
  local id="$1"
  local want="$2"
  local i st
  for i in $(seq 1 90); do
    st="$(task_json "$id" | jq -r '.state // empty')"
    if [[ "$st" == "$want" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task $id did not reach $want: $(task_json "$id")" >&2
  return 1
}

wait_gtid() {
  local id="$1"
  local want="$2"
  local server="$3"
  local i got subset
  want="$(printf '%s' "$want" | tr -d ' \r\n')"
  for i in $(seq 1 90); do
    got="$(curl -fsS "$API/api/tasks/$id/checkpoint" | jq -r '.gtid_set // empty' | tr -d ' \r\n')"
    if [[ -n "$got" ]]; then
      subset="$(mysql_exec "$server" "SELECT GTID_SUBSET('${want}', '${got}')" | tr -d '[:space:]')"
      if [[ "$subset" == "1" ]]; then
        return 0
      fi
    fi
    sleep 1
  done
  echo "task $id checkpoint does not include $want: $(curl -fsS "$API/api/tasks/$id/checkpoint")" >&2
  return 1
}

wait_marker() {
  local id="$1"
  local marker="$2"
  local i path
  for i in $(seq 1 60); do
    while IFS= read -r path; do
      [[ -z "$path" ]] && continue
      path="$(abs_path "$path")"
      [[ -f "$path" ]] || continue
      if grep -a -q "$marker" "$path"; then
        return 0
      fi
    done < <(curl -fsS "$API/api/tasks/$id/files?limit=50" | jq -r '.[] | .file_path // empty')
    sleep 1
  done
  echo "task $id never stored $marker" >&2
  curl -fsS "$API/api/tasks/$id/files?limit=50" >&2 || true
  return 1
}

create_task() {
  local name="$1"
  local mode="$2"
  local gtid="$3"
  local sid="$4"
  local body resp id
  body="$(jq -n \
    --arg name "$name" \
    --arg gtid "$gtid" \
    --arg mode "$mode" \
    --arg host "127.0.0.1" \
    --arg user "repl" \
    --arg pass "replpass" \
    --argjson port "$PORT_VIP" \
    --argjson sid "$sid" \
    '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:(if $mode=="GTID" then {mode:"GTID",gtid_set:$gtid} else {mode:"LATEST"} end),storage:{retention_days:7}}')"
  resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
  id="$(printf '%s' "$resp" | jq -r '.id // empty')"
  if [[ -z "$id" || "$id" == "null" ]]; then
    echo "create $name failed: $resp" >&2
    exit 1
  fi
  curl -fsS -X POST "$API/api/tasks/$id/start" >/dev/null
  printf '%s' "$id"
}

rows_of() {
  local name="$1"
  docker exec "$name" mysql -uroot -proot --batch --raw -Nse "SELECT id, v FROM ${DB}.t ORDER BY id" 2>/dev/null | tr -d '\r'
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
  local i got errf
  errf="$(mktemp)"
  for i in $(seq 1 90); do
    if docker exec "$name" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "SELECT 1" >/dev/null 2>"$errf"; then
      if docker exec "$name" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "RESET MASTER;" >/dev/null 2>"$errf" \
        || docker exec "$name" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "RESET BINARY LOGS AND GTIDS;" >/dev/null 2>"$errf"; then
        got="$(docker exec "$name" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "SELECT @@GLOBAL.gtid_executed = ''" 2>>"$errf" | tr -d '[:space:]' || true)"
        if [[ "$got" == "1" ]]; then
          rm -f "$errf"
          return 0
        fi
      fi
    fi
    sleep 1
  done
  echo "restore $name could not reset" >&2
  cat "$errf" >&2 || true
  rm -f "$errf"
  return 1
}

apply_replay() {
  local json="$1"
  local restore="$2"
  local cmd stop_pos
  local -a args=()
  cmd="$(jq -r '.command' <<<"$json")"
  stop_pos="$(printf '%s\n' "$cmd" | grep -oE -- '--stop-position=[0-9]+' || true)"
  stop_pos="${stop_pos%%$'\n'*}"
  stop_pos="${stop_pos#--stop-position=}"
  local -a extra=()
  if [[ -n "$stop_pos" ]]; then
    extra+=(--stop-position="$stop_pos")
  fi
  if printf '%s\n' "$cmd" | grep -q -- '--stop-datetime='; then
    local raw
    raw="$(printf '%s\n' "$cmd" | grep -oE -- "--stop-datetime='[^']*'" | head -1)"
    raw="${raw#--stop-datetime=}"
    raw="${raw#\'}"
    raw="${raw%\'}"
    extra+=(--stop-datetime="$raw")
  fi
  local i=0 p dest
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    p="$(abs_path "$p")"
    dest="/tmp/switch-${restore}-${i}.bin"
    docker compose -f "$COMPOSE_FILE" cp "$p" "percona80:${dest}" </dev/null
    args+=("$dest")
    i=$((i + 1))
  done < <(jq -r '.paths[]' <<<"$json")
  if [[ "$i" -lt 1 ]]; then
    echo "replay has no paths: $json" >&2
    exit 1
  fi
  local log
  log="$(mktemp)"
  if ! docker compose -f "$COMPOSE_FILE" exec -T -e TZ=UTC percona80 \
    mysqlbinlog ${extra[@]+"${extra[@]}"} "${args[@]}" \
    | docker exec -i "$restore" mysql -uroot -proot --binary-mode >"$log" 2>&1; then
    echo "apply failed" >&2
    cat "$log" >&2
    rm -f "$log"
    exit 1
  fi
  rm -f "$log"
}

assert_order() {
  local json="$1"
  local new_uuid="$2"
  local seen_new=0 seen_old=0 base
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    base="$(basename "$p")"
    base="${base%%.open.e*}"
    if [[ "$base" == "$new_uuid".* ]]; then
      seen_new=1
    else
      if [[ "$seen_new" == 1 ]]; then
        echo "old server file listed after the new server: $json" >&2
        exit 1
      fi
      seen_old=1
    fi
  done < <(jq -r '.paths[]' <<<"$json")
  if [[ "$seen_old" != 1 || "$seen_new" != 1 ]]; then
    echo "replay paths do not span both servers: $json" >&2
    exit 1
  fi
}

docker rm -f "$PRIMARY" "$REPLICA" "$CLIENT" >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null

echo "[switch] start primary and replica"
docker run -d --name "$PRIMARY" --network "$NET" \
  -p "127.0.0.1:${PORT_A}:3306" \
  -e MYSQL_ROOT_PASSWORD=root \
  mysql:8.0 \
  --server-id=201 \
  --gtid-mode=ON \
  --enforce-gtid-consistency=ON \
  --log-bin=mysql-bin \
  --binlog-format=ROW \
  --binlog-row-image=FULL \
  --binlog-transaction-compression=OFF \
  --log-replica-updates=ON \
  --default-authentication-plugin=mysql_native_password \
  --default-time-zone=+00:00 >/dev/null
docker run -d --name "$REPLICA" --network "$NET" \
  -p "127.0.0.1:${PORT_B}:3306" \
  -e MYSQL_ROOT_PASSWORD=root \
  mysql:8.0 \
  --server-id=202 \
  --gtid-mode=ON \
  --enforce-gtid-consistency=ON \
  --log-bin=mysql-bin \
  --binlog-format=ROW \
  --binlog-row-image=FULL \
  --binlog-transaction-compression=OFF \
  --log-replica-updates=ON \
  --read-only=1 \
  --default-authentication-plugin=mysql_native_password \
  --default-time-zone=+00:00 >/dev/null
wait_sql "$PRIMARY"
wait_sql "$REPLICA"
docker run -d --name "$CLIENT" --network host --entrypoint sleep mysql:8.0 infinity >/dev/null

echo "[switch] replication user and replica"
mysql_script "$PRIMARY" "
SET sql_log_bin=0;
CREATE USER 'repl'@'%' IDENTIFIED WITH mysql_native_password BY 'replpass';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl'@'%';
FLUSH PRIVILEGES;
SET sql_log_bin=1;
"
mysql_script "$REPLICA" "
SET sql_log_bin=0;
CREATE USER 'repl'@'%' IDENTIFIED WITH mysql_native_password BY 'replpass';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl'@'%';
FLUSH PRIVILEGES;
SET sql_log_bin=1;
CHANGE REPLICATION SOURCE TO SOURCE_HOST='${PRIMARY}', SOURCE_PORT=3306, SOURCE_USER='repl', SOURCE_PASSWORD='replpass', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1;
START REPLICA;
"
for i in $(seq 1 60); do
  io="$(mysql_exec "$REPLICA" "SELECT SERVICE_STATE FROM performance_schema.replication_connection_status")"
  sqls="$(mysql_exec "$REPLICA" "SELECT SERVICE_STATE FROM performance_schema.replication_applier_status")"
  if [[ "$io" == "ON" && "$sqls" == "ON" ]]; then
    break
  fi
  if [[ "$i" == 60 ]]; then
    echo "replica did not start io=$io sql=$sqls" >&2
    mysql_exec "$REPLICA" "SHOW REPLICA STATUS\G" >&2 || true
    exit 1
  fi
  sleep 1
done

OLD_UUID="$(mysql_exec "$PRIMARY" "SELECT @@server_uuid")"
NEW_UUID="$(mysql_exec "$REPLICA" "SELECT @@server_uuid")"
echo "[switch] primary $OLD_UUID replica $NEW_UUID"
start_proxy
vip_uuid="$(mysql_exec_host "$PORT_VIP" "SELECT @@server_uuid")"
if [[ "$vip_uuid" != "$OLD_UUID" ]]; then
  echo "VIP uuid $vip_uuid want $OLD_UUID" >&2
  exit 1
fi

origin="$(mysql_exec "$PRIMARY" "SELECT REPLACE(@@GLOBAL.gtid_executed, CHAR(10), '')" | tr -d ' \n')"
if [[ -z "$origin" ]]; then
  mysql_exec "$PRIMARY" "CREATE DATABASE binlog_switch_boot; DROP DATABASE binlog_switch_boot;" >/dev/null
  origin="$(mysql_exec "$PRIMARY" "SELECT REPLACE(@@GLOBAL.gtid_executed, CHAR(10), '')" | tr -d ' \n')"
fi
if [[ -z "$origin" ]]; then
  echo "primary gtid_executed is empty" >&2
  exit 1
fi
schema="$(mysql_script "$PRIMARY" "
CREATE DATABASE ${DB};
CREATE TABLE ${DB}.t (id INT PRIMARY KEY, v VARCHAR(64) NOT NULL);
INSERT INTO ${DB}.t VALUES (1, 'before-1');
INSERT INTO ${DB}.t VALUES (2, 'before-2');
SELECT @@GLOBAL.gtid_executed;
")"
before_gtid="$(mysql_exec "$PRIMARY" "SELECT REPLACE(@@GLOBAL.gtid_executed, CHAR(10), '')" | tr -d ' \n')"
echo "[switch] before gtid $before_gtid"

gtid_name="e2e-switch-gtid-${RUN_TAG}"
latest_name="e2e-switch-latest-${RUN_TAG}"
gtid_id="$(create_task "$gtid_name" "GTID" "$origin" 310301)"
echo "[switch] gtid task $gtid_id"
wait_state "$gtid_id" "RUNNING"
wait_gtid "$gtid_id" "$before_gtid" "$PRIMARY"

latest_id="$(create_task "$latest_name" "LATEST" "" 310302)"
echo "[switch] latest task $latest_id"
wait_state "$latest_id" "RUNNING"
mysql_exec "$PRIMARY" "INSERT INTO ${DB}.t VALUES (3, 'before-3');"
wait_marker "$latest_id" "before-3"
mysql_exec "$PRIMARY" "FLUSH BINARY LOGS;"
full_gtid="$(mysql_exec "$PRIMARY" "SELECT REPLACE(@@GLOBAL.gtid_executed, CHAR(10), '')" | tr -d ' \n')"
caught="$(mysql_exec "$REPLICA" "SELECT WAIT_FOR_EXECUTED_GTID_SET('${full_gtid}', 30)")"
if [[ "$caught" != "0" ]]; then
  echo "replica did not catch up: $caught" >&2
  exit 1
fi
wait_gtid "$gtid_id" "$full_gtid" "$PRIMARY"

echo "[switch] promote replica and move VIP"
mysql_script "$REPLICA" "
STOP REPLICA;
RESET REPLICA ALL;
SET GLOBAL super_read_only=0;
SET GLOBAL read_only=0;
"
vip_uuid="$(point_vip "$PORT_B")"
if [[ "$vip_uuid" != "$NEW_UUID" ]]; then
  echo "VIP after promote is $vip_uuid want $NEW_UUID" >&2
  exit 1
fi
after="$(mysql_script "$REPLICA" "
INSERT INTO ${DB}.t VALUES (4, 'after-1');
SET @b = @@GLOBAL.gtid_executed;
INSERT INTO ${DB}.t VALUES (5, 'after-2');
SELECT GTID_SUBTRACT(@@GLOBAL.gtid_executed, @b);
")"
after2="$(printf '%s\n' "$after" | tail -n 1 | tr -d ' \n')"
echo "[switch] after-2 gtid $after2"
wait_gtid "$gtid_id" "$after2" "$REPLICA"
wait_state "$latest_id" "FAILED"

latest_err="$(task_json "$latest_id" | jq -r '.last_error // empty')"
if [[ "$latest_err" != SOURCE_SWITCHOVER:* ]] || [[ "$latest_err" != *"$OLD_UUID"* ]] || [[ "$latest_err" != *"$NEW_UUID"* ]] || [[ "$latest_err" != *"no GTID set"* ]] || [[ "$latest_err" != *"Start a new task"* ]]; then
  echo "LATEST last_error [$latest_err]" >&2
  exit 1
fi
echo "[switch] LATEST stopped: $latest_err"
latest_json="$(task_json "$latest_id")"
if [[ "$(jq -r '.source_chain.outcome // empty' <<<"$latest_json")" != "stopped" ]] \
  || [[ "$(jq -r '.source_chain.current // empty' <<<"$latest_json")" != "$OLD_UUID" ]] \
  || [[ "$(jq -r '.source_chain.switches[-1].continued' <<<"$latest_json")" != "false" ]] \
  || [[ "$(jq -r '.source_chain.switches[-1].reason // empty' <<<"$latest_json")" != "no_gtid" ]] \
  || [[ "$(jq -r '.source_chain.switches[-1].old // empty' <<<"$latest_json")" != "$OLD_UUID" ]] \
  || [[ "$(jq -r '.source_chain.switches[-1].new // empty' <<<"$latest_json")" != "$NEW_UUID" ]]; then
  echo "LATEST source_chain [$latest_json]" >&2
  exit 1
fi
assert_switch_metric "$latest_id" stopped ge 1
assert_switch_metric "$latest_id" continued eq 0
echo "[switch] LATEST source_chain stopped no_gtid"

switch_msg="$(curl -fsS "$API/api/tasks/$gtid_id/events?limit=100" | jq -r '.[] | select(.type=="SOURCE_SWITCHOVER") | .message' | head -n 1)"
switch_detail="$(curl -fsS "$API/api/tasks/$gtid_id/events?limit=100" | jq -r '.[] | select(.type=="SOURCE_SWITCHOVER") | .detail' | head -n 1)"
if [[ "$switch_msg" != *"$OLD_UUID"* || "$switch_msg" != *"$NEW_UUID"* || "$switch_msg" != *"continuing from the executed GTID set"* ]]; then
  echo "switch event [$switch_msg]" >&2
  exit 1
fi
if [[ "$switch_detail" != *"old=$OLD_UUID"* || "$switch_detail" != *"new=$NEW_UUID"* ]]; then
  echo "switch detail [$switch_detail]" >&2
  exit 1
fi
echo "[switch] event $switch_msg"
gtid_json_task="$(task_json "$gtid_id")"
if [[ "$(jq -r '.source_chain.outcome // empty' <<<"$gtid_json_task")" != "continued" ]] \
  || [[ "$(jq -r '.source_chain.current // empty' <<<"$gtid_json_task")" != "$NEW_UUID" ]] \
  || [[ "$(jq -r '.source_chain.servers[0].identity // empty' <<<"$gtid_json_task")" != "$OLD_UUID" ]] \
  || [[ "$(jq -r '.source_chain.servers[0].current' <<<"$gtid_json_task")" != "false" ]] \
  || [[ "$(jq -r '.source_chain.servers[1].identity // empty' <<<"$gtid_json_task")" != "$NEW_UUID" ]] \
  || [[ "$(jq -r '.source_chain.servers[1].current' <<<"$gtid_json_task")" != "true" ]] \
  || [[ "$(jq -r '.source_chain.switches[0].old // empty' <<<"$gtid_json_task")" != "$OLD_UUID" ]] \
  || [[ "$(jq -r '.source_chain.switches[0].new // empty' <<<"$gtid_json_task")" != "$NEW_UUID" ]] \
  || [[ "$(jq -r '.source_chain.switches[0].continued' <<<"$gtid_json_task")" != "true" ]]; then
  echo "continued source_chain [$gtid_json_task]" >&2
  exit 1
fi
switch_where="$(jq -r '.source_chain.switches[0] | (.file // "") + (.gtid_set // "")' <<<"$gtid_json_task")"
if [[ -z "$switch_where" ]]; then
  echo "continued switch has no file or gtid_set [$gtid_json_task]" >&2
  exit 1
fi
assert_switch_metric "$gtid_id" continued ge 1
assert_switch_metric "$gtid_id" stopped eq 0
assert_console_chain
echo "[switch] source_chain continued current $NEW_UUID"

files_json="$(curl -fsS "$API/api/tasks/$gtid_id/files?limit=50")"
old_path=""
new_path=""
saw_before=0
saw_after=0
while IFS= read -r path; do
  [[ -z "$path" ]] && continue
  path="$(abs_path "$path")"
  [[ -f "$path" ]] || continue
  base="$(basename "$path")"
  if [[ "$base" == "$NEW_UUID".* ]]; then
    if [[ -z "$new_path" ]]; then
      new_path="$path"
    fi
    if grep -a -q 'after-1' "$path"; then
      saw_after=1
      new_path="$path"
    fi
  else
    old_path="$path"
    if grep -a -q 'after-1' "$path"; then
      echo "old segment contains after-switch rows: $path" >&2
      exit 1
    fi
    if grep -a -q 'before-1' "$path"; then
      saw_before=1
    fi
  fi
done < <(printf '%s' "$files_json" | jq -r '.[] | .file_path // empty')
if [[ -z "$old_path" || -z "$new_path" || "$saw_before" != 1 || "$saw_after" != 1 ]]; then
  echo "expected old and new server files: $files_json" >&2
  exit 1
fi
bad_ident="$(printf '%s' "$files_json" | jq -r --arg new "$NEW_UUID" --arg old "$OLD_UUID" '
  .[] | select((.file_name // "") != "") |
  if (.file_name | startswith($new + ".")) then
    select(.source_identity != $new) | .file_name
  else
    select(.source_identity != $old) | .file_name
  end
')"
if [[ -n "$bad_ident" ]]; then
  echo "source_identity mismatch: $bad_ident" >&2
  echo "$files_json" >&2
  exit 1
fi
echo "[switch] files name the server that wrote them"
latest_dir="$(abs_path "$DATA_DIR/$latest_id")"
if find "$latest_dir" -maxdepth 1 -type f -exec grep -a -l 'after-1' {} + | grep -q .; then
  echo "LATEST files contain after-switch rows" >&2
  exit 1
fi
old_sum="$(md5sum "$old_path" | awk '{print $1}')"
new_sum="$(md5sum "$new_path" | awk '{print $1}')"
if [[ "$old_sum" == "$new_sum" ]]; then
  echo "old and new segments are the same bytes" >&2
  exit 1
fi
echo "[switch] old $(basename "$old_path") new $(basename "$new_path")"

window="$(curl -fsS "$API/api/tasks/$gtid_id/window")"
if [[ "$(jq -r '.continuous' <<<"$window")" != "true" ]]; then
  echo "window is not continuous: $window" >&2
  exit 1
fi
echo "[switch] window continuous gtid $(jq -r '.gtid_set // empty' <<<"$window")"

stop_at="$(mysql_exec "$REPLICA" "SELECT DATE_FORMAT(DATE_ADD(UTC_TIMESTAMP(), INTERVAL 1 DAY), '%Y-%m-%d %H:%i:%s')")"
time_json="$(curl -fsS -G "$API/api/tasks/$gtid_id/replay" --data-urlencode "stop_datetime=${stop_at}")"
assert_order "$time_json" "$NEW_UUID"
if [[ "$(jq -r '.command' <<<"$time_json")" != *"--stop-datetime="* ]]; then
  echo "stop_datetime command missing: $time_json" >&2
  exit 1
fi
gtid_json="$(curl -fsS -G "$API/api/tasks/$gtid_id/replay" --data-urlencode "stop_gtid=${after2}")"
assert_order "$gtid_json" "$NEW_UUID"
if [[ "$(jq -r '.command' <<<"$gtid_json")" != *"--stop-position="* ]]; then
  echo "stop_gtid command missing: $gtid_json" >&2
  exit 1
fi
echo "[switch] replay spans both servers"

echo "[switch] replay full backup into a fresh MySQL"
start_restore "$RESTORE_FULL" 203
apply_replay "$time_json" "$RESTORE_FULL"
want_rows="$(rows_of "$REPLICA")"
got_rows="$(rows_of "$RESTORE_FULL")"
if [[ "$got_rows" != "$want_rows" ]]; then
  echo "full restore rows [$got_rows] primary [$want_rows]" >&2
  exit 1
fi
want_sum="$(mysql_exec "$REPLICA" "CHECKSUM TABLE ${DB}.t" | awk '{print $2}')"
got_sum="$(docker exec "$RESTORE_FULL" mysql -uroot -proot -Nse "CHECKSUM TABLE ${DB}.t" | awk '{print $2}' | tr -d '\r')"
if [[ -z "$want_sum" || "$want_sum" != "$got_sum" ]]; then
  echo "checksum primary [$want_sum] restore [$got_sum]" >&2
  exit 1
fi
echo "[switch] checksum $got_sum matches the new primary"

echo "[switch] stop_gtid excludes after-2"
start_restore "$RESTORE_GTID" 204
apply_replay "$gtid_json" "$RESTORE_GTID"
partial="$(rows_of "$RESTORE_GTID")"
if [[ "$partial" != $'1\tbefore-1\n2\tbefore-2\n3\tbefore-3\n4\tafter-1' ]]; then
  echo "stop_gtid rows [$partial]" >&2
  exit 1
fi

echo "[switch] point VIP back at the old primary"
mysql_exec "$PRIMARY" "INSERT INTO ${DB}.t VALUES (9, 'only-on-a');" >/dev/null
before_mix="$(md5sum "$new_path" | awk '{print $1}')"
vip_uuid="$(point_vip "$PORT_A")"
if [[ "$vip_uuid" != "$OLD_UUID" ]]; then
  echo "VIP back is $vip_uuid want $OLD_UUID" >&2
  exit 1
fi
wait_state "$gtid_id" "FAILED"
back_err="$(task_json "$gtid_id" | jq -r '.last_error // empty')"
if [[ "$back_err" != SOURCE_SWITCHOVER:* ]] || [[ "$back_err" != *"$OLD_UUID"* ]] || [[ "$back_err" != *"$NEW_UUID"* ]] || [[ "$back_err" != *"missing transactions"* ]] || [[ "$back_err" != *"Start a new task"* ]]; then
  echo "missing-transaction last_error [$back_err]" >&2
  exit 1
fi
back_json="$(task_json "$gtid_id")"
if [[ "$(jq -r '.source_chain.outcome // empty' <<<"$back_json")" != "stopped" ]] \
  || [[ "$(jq -r '.source_chain.current // empty' <<<"$back_json")" != "$NEW_UUID" ]] \
  || [[ "$(jq -r '.source_chain.switches[-1].continued' <<<"$back_json")" != "false" ]] \
  || [[ "$(jq -r '.source_chain.switches[-1].reason // empty' <<<"$back_json")" != "missing_transactions" ]] \
  || [[ "$(jq -r '.source_chain.switches[-1].old // empty' <<<"$back_json")" != "$NEW_UUID" ]] \
  || [[ "$(jq -r '.source_chain.switches[-1].new // empty' <<<"$back_json")" != "$OLD_UUID" ]]; then
  echo "failback source_chain [$back_json]" >&2
  exit 1
fi
chain_file="$(abs_path "$DATA_DIR/$gtid_id/.source-chain")"
if [[ "$(sed -n '1p' "$chain_file" | tr -d '[:space:]')" != "$OLD_UUID" ]] || [[ "$(sed -n '2p' "$chain_file" | tr -d '[:space:]')" != "$NEW_UUID" ]]; then
  echo ".source-chain changed after failback: $(cat "$chain_file")" >&2
  exit 1
fi
assert_switch_metric "$gtid_id" continued ge 1
assert_switch_metric "$gtid_id" stopped ge 1
echo "[switch] failback stopped missing_transactions; chain file still $OLD_UUID then $NEW_UUID"
# Sealing the new server's open segment renames it. The bytes that held
# after-1 stay the same.
after_mix=""
while IFS= read -r path; do
  [[ -z "$path" || ! -f "$path" ]] && continue
  if grep -a -q 'after-1' "$path"; then
    after_mix="$(md5sum "$path" | awk '{print $1}')"
    break
  fi
done < <(find "$(abs_path "$DATA_DIR/$gtid_id")" -maxdepth 1 -type f -print)
if [[ -z "$before_mix" || "$before_mix" != "$after_mix" ]]; then
  echo "new segment changed after the unsafe switch before=$before_mix after=$after_mix" >&2
  exit 1
fi
if find "$(abs_path "$DATA_DIR/$gtid_id")" -maxdepth 1 -type f -exec grep -a -l 'only-on-a' {} + | grep -q .; then
  echo "old primary bytes landed in the backup" >&2
  exit 1
fi
echo "[switch] unsafe switch stopped: $back_err"
echo "[switch] passed"
