#!/usr/bin/env bash
# input: two MySQL 8.0 servers in a GTID pair behind one TCP proxy, a meta MySQL, MinIO, and three binlog-server binaries (OLD, MID, NEW; default: built from v0.5.57, 0a84917a, and this checkout)
# output: a task directory written by OLD after a real A→B→A failback (A's second stint reuses mysql-bin.00000N), carried through MID and NEW; the script prints the directory, the catalog rows, NEW's /files, /replay, /window, storage_alert, Start result and STORAGE_REPAIRED event, waits for the re-upload of repaired rows, then replays /replay with a host MySQL 8.0 mysqlbinlog (FBUP_MYSQLBINLOG) into a fresh MySQL and compares CHECKSUM TABLE with A. FBUP_CASE=plain (default) keeps the directory as OLD left it; object-only removes A's first-stint mysql-bin.00000N from disk so only its MinIO object holds it (Start must fetch it back); object-gone also removes that object (Start must refuse). Every case asserts that NEW changes neither the catalog nor MinIO before Start, and object-gone that the refusal changes neither. FBUP_EXPECT=repaired (default; refused for object-gone) asserts the outcome; report only prints
# pos: upgrade coverage for the legacy failback name collision (QA 6c88ad5b P1); opt-in, not in any suite profile
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="${FBUP_WORK:-$ROOT_DIR/tmp/e2e/fbup-$(date +%s)}"
KEEP="${FBUP_KEEP:-0}"
CASE="${FBUP_CASE:-plain}"         # plain | object-only | object-gone
case "$CASE" in plain | object-only | object-gone) ;; *) echo "FBUP_CASE must be plain, object-only or object-gone" >&2; exit 2 ;; esac
DEFAULT_EXPECT=repaired
[[ "$CASE" == object-gone ]] && DEFAULT_EXPECT=refused
EXPECT="${FBUP_EXPECT:-$DEFAULT_EXPECT}" # repaired | report | refused
P_META=14470 P_A=14480 P_B=14481 P_VIP=14482 P_CTRL=14483 P_RESTORE=14490 P_S3=19480 P_API=18470
API="http://127.0.0.1:${P_API}"
C_META=fbup-meta C_A=fbup-a C_B=fbup-b C_RESTORE=fbup-restore C_S3=fbup-minio
mkdir -p "$WORK/bin" "$WORK/out"
WORK="$(cd "$WORK" && pwd)"
DATA="$WORK/data"
OUT="$WORK/out"
SERVER_PID="" PROXY_PID=""

log() { echo "[fbup] $*"; }
die() { echo "[fbup] FAIL: $*" >&2; exit 1; }

cleanup() {
  [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true
  pkill -f -- "-c $WORK/config.yaml" 2>/dev/null || true
  [[ -n "$PROXY_PID" ]] && kill "$PROXY_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  docker rm -f -v "$C_META" "$C_A" "$C_B" "$C_RESTORE" "$C_S3" >/dev/null 2>&1 || true
  if [[ "$KEEP" != 1 ]]; then
    rm -rf "$DATA" "$WORK/bin" "$WORK/src-"* 2>/dev/null || true
  fi
}
trap cleanup EXIT

build_ref() {
  local ref="$1" out="$2"
  local src="$WORK/src-$ref"
  git -C "$ROOT_DIR" worktree add --detach "$src" "$ref" >/dev/null 2>&1 || die "worktree $ref"
  (cd "$src" && go build -o "$out" ./cmd/binlog-server && go build -o "$out-migrate" ./cmd/migrate) || die "build $ref"
  git -C "$ROOT_DIR" worktree remove --force "$src" >/dev/null 2>&1 || true
}

OLD_BIN="${FBUP_OLD_BIN:-}"
MID_BIN="${FBUP_MID_BIN:-}"
NEW_BIN="${FBUP_NEW_BIN:-}"
if [[ -z "$OLD_BIN" ]]; then OLD_BIN="$WORK/bin/old"; build_ref v0.5.57 "$OLD_BIN"; fi
if [[ -z "$MID_BIN" ]]; then MID_BIN="$WORK/bin/mid"; build_ref 0a84917a "$MID_BIN"; fi
if [[ -z "$NEW_BIN" ]]; then NEW_BIN="$WORK/bin/new"; (cd "$ROOT_DIR" && go build -o "$NEW_BIN" ./cmd/binlog-server) || die "build new"; fi
MIGRATE="${FBUP_MIGRATE:-$OLD_BIN-migrate}"
[[ -x "$MIGRATE" ]] || MIGRATE=""

MYSQLBINLOG="${FBUP_MYSQLBINLOG:-$(command -v mysqlbinlog || true)}"
[[ -x "$MYSQLBINLOG" ]] || die "set FBUP_MYSQLBINLOG to a MySQL 8.0 mysqlbinlog"
"$MYSQLBINLOG" --version 2>/dev/null | grep -qi mariadb && die "$MYSQLBINLOG is MariaDB's; set FBUP_MYSQLBINLOG to a MySQL 8.0 mysqlbinlog"

# The MinIO image ships mc. A per-command alias: no mc config is written.
mcx() { docker exec -e MC_HOST_fbup="http://minioadmin:minioadmin@127.0.0.1:${P_S3}" "$C_S3" mc "$@"; }
objects() { mcx ls -r --json fbup/fbup 2>/dev/null | jq -r 'select(.key) | "\(.key) \(.size) \(.etag)"' | sort; }
rows() { m "$P_META" "SELECT file_name, epoch, state, file_path, start_pos, end_pos, size_bytes, upload_state, object_key, checksum FROM fbup.binlog_files WHERE task_id='$TASK' ORDER BY file_name, epoch"; }

m() { local p="$1"; shift; mysql -uroot -proot -h127.0.0.1 -P"$p" --protocol=tcp -Nse "$*" 2>/dev/null; }
gtid_of() { m "$1" "SELECT REPLACE(@@GLOBAL.gtid_executed, CHAR(10), '')"; }

start_mysql() {
  local name="$1" port="$2" sid="$3"; shift 3
  docker rm -f -v "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" --network host -e MYSQL_ROOT_PASSWORD=root -e MYSQL_ROOT_HOST=% mysql:8.0 \
    --port="$port" --mysqlx=OFF --server-id="$sid" --gtid-mode=ON --enforce-gtid-consistency=ON \
    --log-bin=mysql-bin --binlog-format=ROW --log-replica-updates=ON --binlog-transaction-compression=OFF \
    --default-authentication-plugin=mysql_native_password --default-time-zone=+00:00 "$@" >/dev/null
  local i
  local ok=0
  for i in $(seq 1 120); do
    if m "$port" "SELECT 1" | grep -q 1; then ok=$((ok + 1)); else ok=0; fi
    [[ "$ok" -ge 3 ]] && return 0
    sleep 1
  done
  docker logs "$name" | tail -20 >&2
  die "$name did not start"
}

start_proxy() {
  python3 - "$P_VIP" "$P_CTRL" "$P_A" >"$OUT/proxy.log" 2>&1 <<'PY' &
import socket, sys, threading
listen_port, ctrl_port, backend = int(sys.argv[1]), int(sys.argv[2]), [int(sys.argv[3])]
lock = threading.Lock(); pairs = []
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d: break
            b.sendall(d)
    except Exception: pass
    finally:
        for s in (a, b):
            try: s.shutdown(socket.SHUT_RDWR)
            except Exception: pass
def handle(c):
    with lock: port = backend[0]
    try:
        u = socket.create_connection(("127.0.0.1", port), timeout=10); u.settimeout(None)
    except Exception:
        c.close(); return
    with lock: pairs.append((c, u))
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    threading.Thread(target=pipe, args=(u, c), daemon=True).start()
def control():
    srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", ctrl_port)); srv.listen(8)
    while True:
        c, _ = srv.accept(); line = c.recv(256).decode().strip()
        if line.startswith("SET "):
            with lock:
                backend[0] = int(line.split()[1]); old = list(pairs); pairs.clear()
            for a, b in old:
                for s in (a, b):
                    try: s.shutdown(socket.SHUT_RDWR); s.close()
                    except Exception: pass
            c.sendall(b"OK\n")
        c.close()
threading.Thread(target=control, daemon=True).start()
ls = socket.socket(); ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
ls.bind(("127.0.0.1", listen_port)); ls.listen(128)
while True:
    c, _ = ls.accept(); threading.Thread(target=handle, args=(c,), daemon=True).start()
PY
  PROXY_PID=$!
  sleep 1
}

point_vip() {
  python3 -c 'import socket,sys; s=socket.create_connection(("127.0.0.1", int(sys.argv[1])), 3); s.sendall(("SET %s\n" % sys.argv[2]).encode()); print(s.recv(8).decode().strip())' "$P_CTRL" "$1" | grep -q OK || die "vip $1"
  log "VIP -> $1 ($(m "$P_VIP" "SELECT @@server_uuid"))"
}

# write PORT N TAG: N transactions of INSERT + UPDATE (+ DELETE) whose replay order matters
write_load() {
  python3 - "$1" "$2" "$3" <<'PY'
import pymysql, random, sys
port, n, tag = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
random.seed(tag)
c = pymysql.connect(host='127.0.0.1', port=port, user='root', password='root', autocommit=False); cur = c.cursor()
for i in range(n):
    cur.execute("INSERT INTO fb.t(k,v) VALUES (%s,%s)", (random.randint(1, 50), '%s-%d' % (tag, i)))
    cur.execute("UPDATE fb.t SET n=n+1, v=CONCAT(LEFT(v,40),'u') WHERE k=%s ORDER BY id LIMIT 3", (random.randint(1, 50),))
    if random.random() < 0.15:
        cur.execute("DELETE FROM fb.t WHERE k=%s ORDER BY id LIMIT 1", (random.randint(1, 50),))
    c.commit()
PY
}

# promote NEW OLD: OLD stops taking writes, NEW catches up and takes over, OLD replicates from NEW
promote() {
  local new="$1" old="$2"
  m "$old" "SET GLOBAL super_read_only=1"
  local want; want="$(gtid_of "$old")"
  [[ "$(m "$new" "SELECT WAIT_FOR_EXECUTED_GTID_SET('$want', 60)")" == 0 ]] || die "$new did not catch up"
  m "$new" "STOP REPLICA; RESET REPLICA ALL; SET GLOBAL super_read_only=0; SET GLOBAL read_only=0"
  m "$old" "STOP REPLICA; RESET REPLICA ALL; CHANGE REPLICATION SOURCE TO SOURCE_HOST='127.0.0.1', SOURCE_PORT=$new, SOURCE_USER='repl', SOURCE_PASSWORD='replpass', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1; START REPLICA"
  log "promoted $new, $old now replicates from it"
}

write_config() {
  cat >"$WORK/config.yaml" <<YAML
listen_addr: "127.0.0.1:${P_API}"
data_dir: "${DATA}"
meta_dsn: "root:root@tcp(127.0.0.1:${P_META})/fbup?parseTime=true&charset=utf8mb4&multiStatements=true"
mode: "cluster"
cluster:
  role: "all-in-one"
  worker_id: "worker-fbup"
  lease_ttl_sec: 8
  lease_renew_interval_sec: 3
  lease_grace_sec: 20
  failover_policy: "rebuild_current_file"
api:
  auth:
    enabled: false
upload:
  endpoint: "127.0.0.1:${P_S3}"
  bucket: "fbup"
  access_key: "minioadmin"
  secret_key: "minioadmin"
  region: "us-east-1"
  prefix: "fbup"
  use_ssl: false
log:
  level: "info"
  encoding: "json"
  file: "${OUT}/server.log"
YAML
}

start_server() {
  local bin="$1" tag="$2"
  ! curl -fsS "$API/api/tasks" >/dev/null 2>&1 || die "port $P_API is already serving; stop the earlier server first"
  # exec: SERVER_PID must be the server itself, or stop_server leaves it
  # running and the next build's health check reaches the old process.
  (cd "$ROOT_DIR" && exec env -u BINLOG_SERVER_META_DSN "$bin" -c "$WORK/config.yaml" --encryption-key 0123456789abcdef0123456789abcdef >>"$OUT/server-$tag.out" 2>&1) &
  SERVER_PID=$!
  local i
  for i in $(seq 1 60); do
    if curl -fsS "$API/api/tasks" >/dev/null 2>&1; then log "server $tag up pid=$SERVER_PID"; return 0; fi
    sleep 1
  done
  tail -20 "$OUT/server-$tag.out" >&2
  die "server $tag did not start"
}

stop_server() {
  [[ -n "$SERVER_PID" ]] || return 0
  kill "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
  local i
  for i in $(seq 1 30); do
    curl -fsS "$API/api/tasks" >/dev/null 2>&1 || return 0
    sleep 1
  done
  die "server still answers on $P_API after stop"
}

state_of() { curl -fsS "$API/api/tasks/$TASK" | jq -r '.state'; }
wait_state() {
  local want="$1" i st
  for i in $(seq 1 90); do
    st="$(state_of)"
    [[ "$st" == "$want" ]] && return 0
    sleep 1
  done
  curl -fsS "$API/api/tasks/$TASK" >&2
  die "task did not reach $want (state $st)"
}
wait_cp() {
  local want="$1" i got
  for i in $(seq 1 120); do
    got="$(curl -fsS "$API/api/tasks/$TASK/checkpoint" 2>/dev/null | jq -r '.gtid_set // empty' | tr -d ' \n')"
    if [[ -n "$got" && "$(m "$P_A" "SELECT GTID_SUBSET('$want', '$got')")" == 1 ]]; then return 0; fi
    sleep 1
  done
  die "checkpoint never reached $want (got $got)"
}

snapshot() {
  local tag="$1"
  log "--- $tag"
  (cd "$DATA/$TASK" && ls -la --time-style=+%T | awk 'NR>1{print "[fbup]   " $6, $7}' && echo "[fbup]   .source-chain: $(paste -sd, .source-chain 2>/dev/null)")
  m "$P_META" "SELECT file_name, epoch, state, file_path, start_pos, end_pos, upload_state, object_key FROM fbup.binlog_files WHERE task_id='$TASK' ORDER BY file_name, epoch" \
    | sed -e "s|$DATA/||g" -e 's/^/[fbup]   row /'
}

log "work dir $WORK"
docker rm -f -v "$C_META" "$C_A" "$C_B" "$C_RESTORE" "$C_S3" >/dev/null 2>&1 || true
docker run -d --name "$C_S3" --network host -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin pgsty/minio:latest server /data --address "127.0.0.1:${P_S3}" --console-address "127.0.0.1:$((P_S3 + 1))" >/dev/null
start_mysql "$C_META" "$P_META" 900
start_mysql "$C_A" "$P_A" 901
start_mysql "$C_B" "$P_B" 902 --read-only=1
m "$P_META" "CREATE DATABASE fbup"
for p in "$P_A" "$P_B"; do
  m "$p" "SET sql_log_bin=0; CREATE USER 'repl'@'%' IDENTIFIED WITH mysql_native_password BY 'replpass'; GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl'@'%'; SET sql_log_bin=1"
done
m "$P_B" "CHANGE REPLICATION SOURCE TO SOURCE_HOST='127.0.0.1', SOURCE_PORT=$P_A, SOURCE_USER='repl', SOURCE_PASSWORD='replpass', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1; START REPLICA"
python3 - "$P_S3" <<'PY'
import socket, sys, time
for _ in range(60):
    try:
        socket.create_connection(("127.0.0.1", int(sys.argv[1])), 1).close(); break
    except Exception: time.sleep(1)
PY
docker exec "$C_S3" sh -c 'mkdir -p /data/fbup' >/dev/null 2>&1 || true
UUID_A="$(m "$P_A" "SELECT @@server_uuid")"
UUID_B="$(m "$P_B" "SELECT @@server_uuid")"
log "A=$UUID_A B=$UUID_B"
start_proxy
point_vip "$P_A"

m "$P_A" "CREATE DATABASE fb; CREATE TABLE fb.t (id BIGINT AUTO_INCREMENT PRIMARY KEY, k INT NOT NULL, v VARCHAR(64) NOT NULL, n INT NOT NULL DEFAULT 0, KEY (k))"
write_load "$P_A" 20 seed
docker exec "$C_A" mysqldump -uroot -proot -h127.0.0.1 -P"$P_A" --protocol=tcp --single-transaction --set-gtid-purged=ON --databases fb >"$OUT/seed.sql" 2>"$OUT/seed.err"
SEED_GTID="$(gtid_of "$P_A")"
log "seed gtid $SEED_GTID"

if [[ -n "$MIGRATE" ]]; then
  "$MIGRATE" up --dsn "root:root@tcp(127.0.0.1:${P_META})/fbup?parseTime=true&multiStatements=true" --path "$ROOT_DIR/migrations" >"$OUT/migrate.log" 2>&1 || die "migrate"
else
  (cd "$ROOT_DIR" && META_DSN="root:root@tcp(127.0.0.1:${P_META})/fbup?parseTime=true&multiStatements=true" go run ./cmd/migrate up) >"$OUT/migrate.log" 2>&1 || die "migrate"
fi
write_config

log "=== OLD build: $OLD_BIN"
start_server "$OLD_BIN" old
body="$(jq -n --arg gs "$SEED_GTID" --argjson port "$P_VIP" '{name:"fbup",cluster_key:"fbup",source:{host:"127.0.0.1",port:$port,user:"repl",password:"replpass",flavor:"mysql",server_id:470001},start:{mode:"GTID",gtid_set:$gs},storage:{retention_days:7}}')"
TASK="$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body" | jq -r '.id')"
curl -fsS -X POST "$API/api/tasks/$TASK/start" >/dev/null
wait_state RUNNING
log "task $TASK"
m "$P_A" "FLUSH BINARY LOGS"
write_load "$P_A" 200 a1
wait_cp "$(gtid_of "$P_A")"
log "stint 1 on A: $(gtid_of "$P_A")"

promote "$P_B" "$P_A"
point_vip "$P_B"
write_load "$P_B" 200 b1
wait_cp "$(gtid_of "$P_B")"
log "stint 2 on B: $(gtid_of "$P_B")"

promote "$P_A" "$P_B"
point_vip "$P_A"
sleep 3
write_load "$P_A" 200 a2
wait_cp "$(gtid_of "$P_A")"
log "stint 3 on A: $(gtid_of "$P_A")"
snapshot "OLD after failback"

log "restart OLD"
stop_server
start_server "$OLD_BIN" old2
sleep 8
log "OLD after restart: $(curl -fsS "$API/api/tasks/$TASK" | jq -c '{state,last_error}')"
snapshot "OLD after restart"
stop_server

log "=== MID build: $MID_BIN"
start_server "$MID_BIN" mid
sleep 10
log "MID: $(curl -fsS "$API/api/tasks/$TASK" | jq -c '{state,last_error}')"
snapshot "MID"
stop_server

# A's first-stint file whose name the third stint took.
FIRST="$(cd "$DATA/$TASK" && ls | grep -E '^mysql-bin\.[0-9]+\.(sealed|open)\.e1$' | sed -E 's/\.(sealed|open)\.e1$//' | sort -u | head -1)"
[[ -n "$FIRST" && -f "$DATA/$TASK/$FIRST" ]] || die "no reused first-stint name in $DATA/$TASK"
FIRST_KEY="$(mcx find fbup/fbup --name "$FIRST" 2>/dev/null | grep "/$UUID_A/$FIRST\$" | head -1 || true)"
log "first stint file $FIRST object ${FIRST_KEY:-none}"
FIRST_MD5="$(md5sum "$DATA/$TASK/$FIRST" | awk '{print $1}')"
if [[ "$CASE" != plain ]]; then
  [[ -n "$FIRST_KEY" ]] || die "OLD never uploaded $FIRST"
  mv "$DATA/$TASK/$FIRST" "$OUT/$FIRST.removed"
  log "case $CASE: removed $FIRST from disk"
  if [[ "$CASE" == object-gone ]]; then
    mcx rm "$FIRST_KEY" >/dev/null || die "rm $FIRST_KEY"
    log "case $CASE: removed object $FIRST_KEY"
  fi
fi
rows >"$OUT/rows-before-new.txt"
objects >"$OUT/objects-before-new.txt"

log "=== NEW build: $NEW_BIN"
start_server "$NEW_BIN" new
# Longer than one background upload-retry pass (15 s): an unrepaired
# legacy row must not be uploaded under its old key.
sleep 20
rows >"$OUT/rows-new-before-start.txt"
objects >"$OUT/objects-new-before-start.txt"
if diff -u "$OUT/rows-before-new.txt" "$OUT/rows-new-before-start.txt" >"$OUT/rows-before-start.diff" &&
  diff -u "$OUT/objects-before-new.txt" "$OUT/objects-new-before-start.txt" >"$OUT/objects-before-start.diff"; then
  log "NEW before Start: catalog and MinIO unchanged"
else
  diff -u "$OUT/objects-before-new.txt" "$OUT/objects-new-before-start.txt" >"$OUT/objects-before-start.diff" || true
  cat "$OUT/rows-before-start.diff" "$OUT/objects-before-start.diff" | sed 's/^/[fbup]   /'
  [[ "$EXPECT" == report ]] || die "NEW changed the catalog or MinIO before Start"
  log "NEW before Start: catalog or MinIO CHANGED (report only)"
fi
snapshot "NEW before Start"
curl -fsS "$API/api/tasks/$TASK" >"$OUT/new-task-before.json"
curl -fsS "$API/api/tasks/$TASK/files?limit=100" >"$OUT/new-files-before.json"
curl -fsS "$API/api/tasks/$TASK/replay" >"$OUT/new-replay-before.json" || true
curl -fsS "$API/api/tasks/$TASK/window" >"$OUT/new-window-before.json" || true
log "NEW task: $(jq -c '{state,last_error,storage_alert}' "$OUT/new-task-before.json")"
log "NEW files: $(jq -r '[.[].file_path | split("/")[-1]] | join(" ")' "$OUT/new-files-before.json")"
log "NEW replay: $(jq -c '{paths:[.paths[]? | split("/")[-1]], warning}' "$OUT/new-replay-before.json")"
log "NEW window: $(jq -c '{continuous,breaks}' "$OUT/new-window-before.json")"

curl -fsS -X POST "$API/api/tasks/$TASK/start" >/dev/null || true
sleep 8
curl -fsS "$API/api/tasks/$TASK" >"$OUT/new-task-after.json"
log "NEW after Start: $(jq -c '{state,last_error,storage_alert:(.storage_alert.segment // null),chain:[.source_chain.servers[]?.prefix]}' "$OUT/new-task-after.json")"
state_after="$(jq -r '.state' "$OUT/new-task-after.json")"
if [[ "$state_after" == RUNNING ]]; then
  write_load "$P_A" 50 a3
  wait_cp "$(gtid_of "$P_A")"
fi
snapshot "NEW after Start"
curl -fsS "$API/api/tasks/$TASK/events?limit=200" >"$OUT/new-events.json" || true
log "NEW events: $(jq -r '[.[]? | select(.type=="STORAGE_REPAIRED" or .type=="TASK_FAILED") | "\(.type) \(.detail // .message)"] | join(" | ")' "$OUT/new-events.json" 2>/dev/null)"
# Rows the repair marked for upload go up again on the background retry.
for i in $(seq 1 60); do
  [[ "$(m "$P_META" "SELECT COUNT(*) FROM fbup.binlog_files WHERE task_id='$TASK' AND upload_state='UPLOAD_FAILED'")" == 0 ]] && break
  sleep 2
done
log "NEW upload_failed rows: $(m "$P_META" "SELECT COUNT(*) FROM fbup.binlog_files WHERE task_id='$TASK' AND upload_state='UPLOAD_FAILED'")"
curl -fsS "$API/api/tasks/$TASK/files?limit=100" >"$OUT/new-files-after.json"
curl -fsS "$API/api/tasks/$TASK/replay" >"$OUT/new-replay-after.json" || true
curl -fsS "$API/api/tasks/$TASK/window" >"$OUT/new-window-after.json" || true
log "NEW files: $(jq -r '[.[] | "\(.file_path | split("/")[-1])#\(.source_server // 0)"] | join(" ")' "$OUT/new-files-after.json")"
log "NEW replay: $(jq -c '{paths:[.paths[]? | split("/")[-1]], warning}' "$OUT/new-replay-after.json")"
log "NEW window: $(jq -c '{continuous,breaks}' "$OUT/new-window-after.json")"

log "replay /replay into a fresh MySQL from the seed"
start_mysql "$C_RESTORE" "$P_RESTORE" 903
m "$P_RESTORE" "RESET MASTER" >/dev/null || m "$P_RESTORE" "RESET BINARY LOGS AND GTIDS" >/dev/null || true
docker exec -i "$C_RESTORE" mysql -uroot -proot -h127.0.0.1 -P"$P_RESTORE" --protocol=tcp <"$OUT/seed.sql" 2>/dev/null
mapfile -t paths < <(jq -r '.paths[]?' "$OUT/new-replay-after.json")
apply_rc=0
if [[ "${#paths[@]}" -gt 0 ]]; then
  # The mysql:8.0 image has no mysqlbinlog. Use a MySQL 8.0 mysqlbinlog on
  # this host (FBUP_MYSQLBINLOG); a MariaDB one cannot read MySQL 8 GTIDs.
  "$MYSQLBINLOG" "${paths[@]}" 2>"$OUT/apply.mbl.err" \
    | docker exec -i "$C_RESTORE" mysql -uroot -proot -h127.0.0.1 -P"$P_RESTORE" --protocol=tcp --binary-mode 2>"$OUT/apply.err" || apply_rc=$?
fi
want_sum="$(m "$P_A" "CHECKSUM TABLE fb.t" | awk '{print $2}')"
got_sum="$(m "$P_RESTORE" "CHECKSUM TABLE fb.t" | awk '{print $2}')"
log "restore apply rc=$apply_rc $(grep -v Warning "$OUT/apply.err" | head -1) checksum A=$want_sum restore=$got_sum"

case "$EXPECT" in
  report) log "report only" ;;
  repaired)
    [[ "$state_after" == RUNNING ]] || die "NEW did not repair and run: $(jq -c '{state,last_error}' "$OUT/new-task-after.json")"
    [[ "$(paste -sd, "$DATA/$TASK/.source-chain")" == "$UUID_A,$UUID_B,$UUID_A" ]] || die ".source-chain $(paste -sd, "$DATA/$TASK/.source-chain")"
    jq -e '.storage_alert == null' "$OUT/new-task-after.json" >/dev/null || die "storage_alert after repair"
    jq -e '.warning == null or .warning == ""' "$OUT/new-replay-after.json" >/dev/null || die "replay warning after repair"
    [[ "$apply_rc" == 0 && -n "$want_sum" && "$want_sum" == "$got_sum" ]] || die "restore checksum A=$want_sum restore=$got_sum rc=$apply_rc"
    jq -e '.continuous == true' "$OUT/new-window-after.json" >/dev/null || die "window not continuous after repair"
    [[ "$(md5sum "$DATA/$TASK/$FIRST" | awk '{print $1}')" == "$FIRST_MD5" ]] || die "$FIRST on disk after repair differs from the first stint"
    log "evidence: repaired chain $(paste -sd, "$DATA/$TASK/.source-chain") $FIRST md5 $FIRST_MD5 checksum $got_sum matches A"
    ;;
  refused)
    [[ "$state_after" == FAILED ]] || die "NEW did not refuse: $state_after"
    jq -e '.storage_alert != null' "$OUT/new-task-after.json" >/dev/null || die "no storage_alert"
    jq -e '.warning != null and .warning != ""' "$OUT/new-replay-after.json" >/dev/null || die "no replay warning after refusal"
    rows >"$OUT/rows-after-refusal.txt"
    objects >"$OUT/objects-after-refusal.txt"
    diff -u "$OUT/rows-before-new.txt" "$OUT/rows-after-refusal.txt" || die "the refusal changed the catalog"
    diff -u "$OUT/objects-before-new.txt" "$OUT/objects-after-refusal.txt" || die "the refusal changed MinIO"
    log "evidence: refused, catalog and MinIO unchanged; replay $(jq -c '[.paths[]? | split("/")[-1]]' "$OUT/new-replay-after.json")"
    ;;
esac
log "passed ($EXPECT)"
