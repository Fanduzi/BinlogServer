#!/usr/bin/env bash
# input: two MySQL 8.0 servers in a GTID pair behind one TCP proxy, a meta MySQL, MinIO, and three binlog-server binaries (OLD, MID, NEW; default: built from FBUP_OLD_REF v0.5.57, FBUP_MID_REF 0a84917a unless FBUP_SKIP_MID=1, and this checkout; FBUP_REQUIRE_REUSE defaults to 1)
# output: a task directory written by OLD after a real A→B→A failback (A's second stint reuses mysql-bin.00000N), carried through MID and NEW; the script prints the directory, the catalog rows, NEW's /files, /replay, /window, storage_alert, Start result and STORAGE_REPAIRED event, waits for the re-upload of repaired rows, then replays /replay with a host MySQL 8.0 mysqlbinlog (FBUP_MYSQLBINLOG) into a fresh MySQL and compares CHECKSUM TABLE with A. FBUP_MODE=cluster (default) or standalone (standalone with meta: the server restarts at epoch 1, so NEW must resume above the repaired .sealed.eN, QA #404). FBUP_CASE=plain (default) keeps the directory as OLD left it; FBUP_REQUIRE_REUSE=0 does not require the reused first-stint name (plain still asserts the chain, a continuous window, and a checksum match); object-only creates the task with local_retention_days 1 and bucket_retention_days 30, moves A's first-stint mysql-bin.00000N off disk (mv) so only its MinIO object holds it (Start must fetch it back), and after the repaired restore runs D2: FLUSH BINARY LOGS on A sets a sealed boundary, every segment before it must be SEALED and UPLOADED, the product's split retention (files aged 2 days, then another rotation) removes them from disk, 30 more transactions go to the local open segment only, then /replay must mark each removed segment bucket, /replay/archive must issue at least one MinIO GetObject per removed object key (mc admin trace) and return the removed bytes, no byte-identical copy may exist under data_dir (dot dirs included), the server TMPDIR ($FBUP_WORK/tmp) or newer /tmp files after the archive read and after the restore, the bucket members alone must restore A's CHECKSUM TABLE and gtid_executed at the boundary, and the local post-boundary members on top must reach A's final checksum; object-gone also removes that object (Start must refuse). Every case asserts that NEW changes neither the catalog nor MinIO before Start, and object-gone that the refusal changes neither. FBUP_EXPECT=repaired (default; refused for object-gone) asserts the outcome; report only prints
# pos: upgrade coverage for the legacy failback name collision (QA 6c88ad5b P1), the object-only restore (D2), and an in-place upgrade from the latest release; not in any run-suite.sh profile; e2e.yml e2e-upgrade-failback runs plain, object-only, and object-gone from v0.5.57, plus plain from v0.5.82 (FBUP_SKIP_MID=1, FBUP_REQUIRE_REUSE=0), on every PR and alone for e2e_profile=upgrade-failback
 Schema 11 renames worker_heartbeats.version, so it is applied only after OLD and MID have exited and no registration lease is still in the future.
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="${FBUP_WORK:-$ROOT_DIR/tmp/e2e/fbup-$(date +%s)}"
KEEP="${FBUP_KEEP:-0}"
CASE="${FBUP_CASE:-plain}"         # plain | object-only | object-gone
MODE="${FBUP_MODE:-cluster}"       # cluster | standalone (standalone with meta)
case "$MODE" in cluster | standalone) ;; *) echo "FBUP_MODE must be cluster or standalone" >&2; exit 2 ;; esac
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
# Every server runs with TMPDIR here so the object-only scan can see its temp files.
SERVER_TMP="$WORK/tmp"
mkdir -p "$SERVER_TMP"
SERVER_PID="" PROXY_PID="" TRACE_PID=""

log() { echo "[fbup] $*"; }
die() { echo "[fbup] FAIL: $*" >&2; exit 1; }

cleanup() {
  # The object-only mc trace runs until killed; wait below would hang on it.
  [[ -n "$TRACE_PID" ]] && kill "$TRACE_PID" 2>/dev/null || true
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

OLD_REF="${FBUP_OLD_REF:-v0.5.57}"
MID_REF="${FBUP_MID_REF:-0a84917a}"
SKIP_MID="${FBUP_SKIP_MID:-0}"
REQUIRE_REUSE="${FBUP_REQUIRE_REUSE:-1}"
case "$SKIP_MID" in 0|1) ;; *) echo "FBUP_SKIP_MID must be 0 or 1" >&2; exit 2 ;; esac
case "$REQUIRE_REUSE" in 0|1) ;; *) echo "FBUP_REQUIRE_REUSE must be 0 or 1" >&2; exit 2 ;; esac
OLD_BIN="${FBUP_OLD_BIN:-}"
MID_BIN="${FBUP_MID_BIN:-}"
NEW_BIN="${FBUP_NEW_BIN:-}"
if [[ -z "$OLD_BIN" ]]; then OLD_BIN="$WORK/bin/old"; build_ref "$OLD_REF" "$OLD_BIN"; fi
if [[ "$SKIP_MID" != 1 && -z "$MID_BIN" ]]; then MID_BIN="$WORK/bin/mid"; build_ref "$MID_REF" "$MID_BIN"; fi
if [[ -z "$NEW_BIN" ]]; then NEW_BIN="$WORK/bin/new"; (cd "$ROOT_DIR" && go build -o "$NEW_BIN" ./cmd/binlog-server) || die "build new"; fi
MIGRATE="${FBUP_MIGRATE:-$OLD_BIN-migrate}"
[[ -x "$MIGRATE" ]] || MIGRATE=""

MYSQLBINLOG="${FBUP_MYSQLBINLOG:-$(command -v mysqlbinlog || true)}"
[[ -x "$MYSQLBINLOG" ]] || die "set FBUP_MYSQLBINLOG to a MySQL 8.0 mysqlbinlog"
"$MYSQLBINLOG" --version 2>/dev/null | grep -qi mariadb && die "$MYSQLBINLOG is MariaDB's; set FBUP_MYSQLBINLOG to a MySQL 8.0 mysqlbinlog"

# The MinIO image ships mc. A per-command alias: no mc config is written.
mcx() { docker exec -e MC_HOST_fbup="http://minioadmin:minioadmin@127.0.0.1:${P_S3}" "$C_S3" mc "$@"; }
# The restore MySQL's client, fed on stdin.
restore_mysql() { docker exec -i "$C_RESTORE" mysql -uroot -proot -h127.0.0.1 -P"$P_RESTORE" --protocol=tcp "$@"; }
objects() { mcx ls -r --json fbup/fbup 2>/dev/null | jq -r 'select(.key) | "\(.key) \(.size) \(.etag)"' | sort; }
rows() { m "$P_META" "SELECT file_name, epoch, state, file_path, start_pos, end_pos, size_bytes, upload_state, object_key, checksum FROM fbup.binlog_files WHERE task_id='$TASK' ORDER BY file_name, epoch"; }

m() { local p="$1"; shift; mysql -uroot -proot -h127.0.0.1 -P"$p" --protocol=tcp -Nse "$*" 2>/dev/null; }
gtid_of() { m "$1" "SELECT REPLACE(@@GLOBAL.gtid_executed, CHAR(10), '')"; }

# v0.5.57 and the MID build select worker_heartbeats.version. Schema 11
# renames that column, so those binaries only ever see migrations below 11.
LEGACY_SCHEMA=10

legacy_migration_dir() {
  local dest="$1" f base n
  mkdir -p "$dest"
  for f in "$ROOT_DIR"/migrations/*; do
    base="$(basename "$f")"
    n="${base%%_*}"
    [[ "$n" =~ ^[0-9]+$ ]] || continue
    (( 10#$n >= 11 )) && continue
    cp "$f" "$dest/"
  done
}

meta_dsn() { printf 'root:root@tcp(127.0.0.1:%s)/fbup?parseTime=true&multiStatements=true\n' "$P_META"; }

# 000011 aborts every statement while a registration lease is still in the future.
wait_registration_leases() {
  local i n
  for i in $(seq 1 40); do
    n="$(m "$P_META" "SELECT COUNT(*) FROM fbup.worker_registrations WHERE lease_expire_at > NOW(6)")" || n=""
    [[ -z "$n" || "$n" == "0" ]] && return 0
    sleep 1
  done
  die "worker registration lease still in the future ($n)"
}

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
$(if [[ "$MODE" == cluster ]]; then cat <<'CL'
mode: "cluster"
cluster:
  role: "all-in-one"
  worker_id: "worker-fbup"
  lease_ttl_sec: 8
  lease_renew_interval_sec: 3
  lease_grace_sec: 20
  failover_policy: "rebuild_current_file"
CL
else echo 'mode: "standalone"'; fi)
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
  (cd "$ROOT_DIR" && exec env -u BINLOG_SERVER_META_DSN TMPDIR="$SERVER_TMP" "$bin" -c "$WORK/config.yaml" --encryption-key 0123456789abcdef0123456789abcdef >>"$OUT/server-$tag.out" 2>&1) &
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

LEGACY_MIG="$WORK/migrations-before-11"
legacy_migration_dir "$LEGACY_MIG"
DSN="$(meta_dsn)"
if [[ -n "$MIGRATE" ]]; then
  "$MIGRATE" up --dsn "$DSN" --path "$LEGACY_MIG" >"$OUT/migrate.log" 2>&1 || die "migrate"
else
  (cd "$ROOT_DIR" && go run ./cmd/migrate up --dsn "$DSN" --path "$LEGACY_MIG") >"$OUT/migrate.log" 2>&1 || die "migrate"
fi
read -r schema_version schema_dirty <<<"$(m "$P_META" "SELECT version, dirty FROM fbup.schema_migrations")"
[[ "$schema_version" == "$LEGACY_SCHEMA" && "$schema_dirty" == "0" ]] || die "legacy schema is ${schema_version:-empty} dirty=${schema_dirty:-empty}, want $LEGACY_SCHEMA 0"
m "$P_META" "SHOW COLUMNS FROM fbup.worker_heartbeats LIKE 'version'" | grep -q '^version' || die "worker_heartbeats.version missing before OLD"
write_config

# --- object-only (D2) begin: everything from here to "object-only (D2) end"
# runs after the repaired restore. Helpers the block uses from above: m, log,
# die, wait_cp, rows, write_load, mcx, restore_mysql; variables: API, TASK,
# DATA, OUT, SERVER_TMP, P_A, P_META, MYSQLBINLOG, FIRST, FIRST_MD5.
#
# What it proves (D2): after a sealed boundary, every segment up to that
# boundary is removed from local disk by the product's own split retention
# (local_retention_days 1 < bucket_retention_days 30, so the object and the
# catalog row stay), /replay marks each one bucket, /replay/archive serves
# each one with at least one MinIO GetObject for its object key, no copy of
# its bytes appears under data_dir (dot dirs included), the server TMPDIR or
# new /tmp files, and the bucket members alone restore A's state at the
# boundary. Writes after the boundary stay in the local open segment; they are
# restored separately and labelled as local.

declare -A D2_MD5=() D2_SIZE=()
D2_MARK="" D2_N=0

d2_path() { if [[ "$1" == /* ]]; then printf '%s\n' "$1"; else printf '%s\n' "$DATA/$1"; fi; }

# d2_wait_sealed CUR GTID: the checkpoint covers GTID, every sealed row is
# UPLOADED with an object key, and the only unsealed row is A's current
# binlog CUR (the segment opened by the boundary rotation).
d2_wait_sealed() {
  local cur="$1" gtid="$2" i n open pending bad
  wait_cp "$gtid"
  for i in $(seq 1 120); do
    open="$(m "$P_META" "SELECT file_name FROM fbup.binlog_files WHERE task_id='$TASK' AND state<>'SEALED'")"
    bad=""
    while IFS= read -r n; do
      [[ -z "$n" || "$n" == "$cur" || "$n" == *".$cur" ]] || bad+=" $n"
    done <<<"$open"
    pending="$(m "$P_META" "SELECT COUNT(*) FROM fbup.binlog_files WHERE task_id='$TASK' AND state='SEALED' AND (upload_state<>'UPLOADED' OR object_key IS NULL OR object_key='')")"
    [[ -z "$bad" && "$pending" == 0 ]] && return 0
    sleep 1
  done
  rows | sed -e "s|$DATA/||g" -e 's/^/[fbup]   row /'
  die "object-only: segments before the boundary $cur are not all sealed and uploaded (unsealed:${bad:- none}, not uploaded: $pending)"
}

# d2_scan TAG: fail if a byte-identical copy of any removed segment exists
# under data_dir (recursive, dot dirs such as .cache included), the server's
# TMPDIR, or among files under /tmp newer than the removal.
d2_scan() {
  local tag="$1" f sz md hits="" checked=0
  while IFS= read -r -d '' f; do
    sz="$(stat -c %s "$f" 2>/dev/null)" || continue
    [[ -n "${D2_SIZE[$sz]:-}" ]] || continue
    checked=$((checked + 1))
    md="$(md5sum "$f" 2>/dev/null | awk '{print $1}')"
    [[ -n "$md" && -n "${D2_MD5[$md]:-}" ]] && hits+=" ${f}=${D2_MD5[$md]}"
  done < <(find "$DATA" "$SERVER_TMP" -type f -print0 2>/dev/null; find /tmp -xdev -path "$WORK" -prune -o -type f -newer "$D2_MARK" -print0 2>/dev/null)
  [[ -z "$hits" ]] || die "object-only ($tag): a removed segment's bytes are on local disk:$hits"
  log "object-only scan ($tag): no copy of the $D2_N removed segment(s) under $DATA (dot dirs included), $SERVER_TMP or new /tmp files ($checked same-size file(s) hashed)"
}

object_only_restore() {
  local gone="$OUT/object-only-gone" ext="$OUT/object-only-archive" tarball="$OUT/object-only.tar" trace="$OUT/object-only-trace.json"
  local rel name key path n=0 rc=0 i cur boundary_gtid boundary_sum final_gtid final_sum got got_gtid left attempt
  local -a removed=() keys=() members=() bucket=() tail=() files=()
  mkdir -p "$gone" "$ext" "$SERVER_TMP"

  # 1. Sealed boundary: rotate A, then wait until everything before the new
  # binlog is sealed and uploaded. A's state here is what the bucket alone
  # must restore.
  m "$P_A" "FLUSH BINARY LOGS"
  cur="$(m "$P_A" "SHOW MASTER STATUS" | awk 'NR==1{print $1}')"
  boundary_gtid="$(gtid_of "$P_A")"
  boundary_sum="$(m "$P_A" "CHECKSUM TABLE fb.t" | awk '{print $2}')"
  d2_wait_sealed "$cur" "$boundary_gtid"
  log "object-only boundary: A rotated to $cur at $boundary_gtid checksum $boundary_sum; every earlier segment is sealed and uploaded"

  # 2. The removal set: every sealed UPLOADED row at the boundary.
  while IFS=$'\t' read -r rel name key; do
    path="$(d2_path "$rel")"
    [[ -n "$rel" && -f "$path" ]] || die "object-only: sealed UPLOADED row $rel is not on disk before removal"
    cp -p "$path" "$gone/$name"
    D2_MD5["$(md5sum "$path" | awk '{print $1}')"]="$name"
    D2_SIZE["$(stat -c %s "$path")"]=1
    removed+=("$path")
    keys+=("$key")
    n=$((n + 1))
  done < <(m "$P_META" "SELECT file_path, SUBSTRING_INDEX(file_path, '/', -1), object_key FROM fbup.binlog_files WHERE task_id='$TASK' AND state='SEALED' AND upload_state='UPLOADED' AND object_key IS NOT NULL AND object_key<>'' ORDER BY id")
  [[ "$n" -gt 0 ]] || die "object-only: no sealed UPLOADED rows to remove"
  D2_N="$n"
  [[ -f "$gone/$FIRST" ]] || die "object-only: $FIRST was not among the sealed UPLOADED rows"

  # 3. Removal through the product: the task has local_retention_days 1 and
  # bucket_retention_days 30. Age the files past the local window only, then
  # rotate A; the runner's retention pass on the new file deletes the local
  # files and keeps the objects and catalog rows. No file is moved or deleted
  # by this script. A file the product keeps (for example an upload it has not
  # verified) is retried after its re-check, then the step fails.
  for attempt in 1 2 3; do
    for path in "${removed[@]}"; do [[ -e "$path" ]] && touch -d '2 days ago' "$path"; done
    m "$P_A" "FLUSH BINARY LOGS"
    left=""
    for i in $(seq 1 60); do
      left=""
      for path in "${removed[@]}"; do [[ -e "$path" ]] && left+=" ${path##*/}"; done
      [[ -z "$left" ]] && break
      sleep 1
    done
    [[ -z "$left" ]] && break
    log "object-only retention attempt $attempt kept:$left; waiting for the upload re-check"
    for i in $(seq 1 60); do
      [[ "$(m "$P_META" "SELECT COUNT(*) FROM fbup.binlog_files WHERE task_id='$TASK' AND state='SEALED' AND upload_state<>'UPLOADED'")" == 0 ]] && break
      sleep 2
    done
  done
  [[ -z "$left" ]] || die "object-only: split retention did not remove:$left (product kept them; see RETENTION events)"
  D2_MARK="$OUT/object-only.mark"
  : >"$D2_MARK"
  curl -fsS "$API/api/tasks/$TASK/events?limit=500" >"$OUT/object-only-events.json" || true
  log "object-only removal: split retention removed $n sealed segment(s) from disk: $(printf '%s\n' "${removed[@]##*/}" | paste -sd' '); RETENTION_REMOVED events: $(jq -r '[.[]? | select(.type=="RETENTION_REMOVED")] | length' "$OUT/object-only-events.json" 2>/dev/null)"
  for i in "${!removed[@]}"; do
    [[ "$(m "$P_META" "SELECT COUNT(*) FROM fbup.binlog_files WHERE task_id='$TASK' AND state='SEALED' AND upload_state='UPLOADED' AND object_key='${keys[$i]}'")" == 1 ]] ||
      die "object-only: retention dropped the catalog row for ${removed[$i]##*/}"
  done

  # 4. Writes after the boundary land in the local open segment only.
  write_load "$P_A" 30 a4
  final_gtid="$(gtid_of "$P_A")"
  final_sum="$(m "$P_A" "CHECKSUM TABLE fb.t" | awk '{print $2}')"
  wait_cp "$final_gtid"
  [[ "$final_sum" != "$boundary_sum" ]] || die "object-only: the post-boundary writes did not change fb.t"

  # 5. Count GetObject per object key while /replay and the archive are read.
  mcx admin trace --json fbup >"$trace" 2>&1 &
  TRACE_PID=$!
  for i in $(seq 1 30); do
    mcx stat "fbup/${keys[0]}" >/dev/null 2>&1 || true
    grep -q '"api"\|"funcName"' "$trace" 2>/dev/null && break
    sleep 1
  done
  grep -q '"api"\|"funcName"' "$trace" 2>/dev/null || { kill "$TRACE_PID" 2>/dev/null; die "object-only: mc admin trace never started"; }

  curl -fsS -G "$API/api/tasks/$TASK/replay" --data-urlencode "limit=200" >"$OUT/object-only-replay.json" || die "object-only: /replay failed"
  log "object-only replay: $(jq -c '{paths:[.paths[]? | split("/")[-1]], locations, warning}' "$OUT/object-only-replay.json")"
  jq -e '.warning == null or .warning == ""' "$OUT/object-only-replay.json" >/dev/null || die "object-only: replay warning"
  for rel in "${removed[@]}"; do
    jq -e --arg b "${rel##*/}" '
      [(.paths // [])[] | split("/")[-1]] as $ps | ($ps | index($b)) as $i |
      $i != null and (.locations // [])[$i] == "bucket"
    ' "$OUT/object-only-replay.json" >/dev/null || die "object-only: /replay does not mark ${rel##*/} as bucket"
  done

  curl -fsS -G "$API/api/tasks/$TASK/replay/archive" --data-urlencode "limit=200" -o "$tarball" || die "object-only: /replay/archive failed"
  mapfile -t members < <(tar -tf "$tarball")
  [[ "${#members[@]}" -gt 0 ]] || die "object-only: empty replay archive"
  tar -xf "$tarball" -C "$ext"
  for rel in "${removed[@]}"; do
    [[ ! -e "$rel" ]] || die "object-only: ${rel##*/} came back to disk while serving the archive"
    [[ -f "$ext/${rel##*/}" ]] || die "object-only: archive has no member ${rel##*/}"
    cmp -s "$ext/${rel##*/}" "$gone/${rel##*/}" || die "object-only: archive member ${rel##*/} differs from the removed file"
  done
  [[ "$(md5sum "$ext/$FIRST" | awk '{print $1}')" == "$FIRST_MD5" ]] || die "object-only: archive $FIRST differs from the first stint"
  # The removed (bucket) members must come first; whatever follows was
  # written after the boundary and is still local.
  for name in "${members[@]}"; do
    if printf '%s\n' "${removed[@]##*/}" | grep -qxF -- "$name"; then
      [[ "${#tail[@]}" == 0 ]] || die "object-only: bucket member $name comes after local member ${tail[0]}"
      bucket+=("$name")
    else
      tail+=("$name")
    fi
  done
  [[ "${#bucket[@]}" == "$n" ]] || die "object-only: archive has ${#bucket[@]} of $n removed segment(s)"
  log "object-only archive: ${#members[@]} member(s): bucket $(printf '%s\n' "${bucket[@]}" | paste -sd' ') | local after the boundary: $(printf '%s\n' "${tail[@]}" | paste -sd' ')"
  d2_scan "after the archive read"

  # 6. Restore the bucket members alone; they must reach A's boundary state.
  m "$P_RESTORE" "DROP DATABASE IF EXISTS fb" >/dev/null || true
  m "$P_RESTORE" "RESET MASTER" >/dev/null || m "$P_RESTORE" "RESET BINARY LOGS AND GTIDS" >/dev/null || true
  restore_mysql <"$OUT/seed.sql" 2>/dev/null
  for name in "${bucket[@]}"; do files+=("$ext/$name"); done
  "$MYSQLBINLOG" "${files[@]}" 2>"$OUT/object-only.mbl.err" | restore_mysql --binary-mode 2>"$OUT/object-only.apply.err" || rc=$?
  got="$(m "$P_RESTORE" "CHECKSUM TABLE fb.t" | awk '{print $2}')"
  got_gtid="$(gtid_of "$P_RESTORE")"
  log "object-only bucket-only restore rc=$rc $(grep -v Warning "$OUT/object-only.apply.err" | head -1) checksum boundary=$boundary_sum restore=$got"
  [[ "$rc" == 0 && -n "$got" && "$got" == "$boundary_sum" ]] || die "object-only bucket-only restore checksum boundary=$boundary_sum restore=$got rc=$rc"
  [[ "$(m "$P_A" "SELECT GTID_SUBSET('$boundary_gtid', '$got_gtid') AND GTID_SUBSET('$got_gtid', '$boundary_gtid')")" == 1 ]] ||
    die "object-only bucket-only restore gtid $got_gtid is not the boundary $boundary_gtid"

  # 7. The local post-boundary members on top reach A's final state.
  files=()
  for name in "${tail[@]}"; do files+=("$ext/$name"); done
  rc=0
  if [[ "${#files[@]}" -gt 0 ]]; then
    "$MYSQLBINLOG" "${files[@]}" 2>>"$OUT/object-only.mbl.err" | restore_mysql --binary-mode 2>>"$OUT/object-only.apply.err" || rc=$?
  fi
  got="$(m "$P_RESTORE" "CHECKSUM TABLE fb.t" | awk '{print $2}')"
  log "object-only bucket + local tail restore rc=$rc checksum A=$final_sum restore=$got"
  [[ "$rc" == 0 && "$got" == "$final_sum" ]] || die "object-only bucket + local tail restore checksum A=$final_sum restore=$got rc=$rc"
  for rel in "${removed[@]}"; do
    [[ ! -e "$rel" ]] || die "object-only: ${rel##*/} came back to disk during the restore"
  done
  d2_scan "after the restore"

  # 8. Every removed segment was read from MinIO.
  sleep 2
  kill "$TRACE_PID" 2>/dev/null || true
  wait "$TRACE_PID" 2>/dev/null || true
  TRACE_PID=""
  local gets total=0 missing=""
  for i in "${!keys[@]}"; do
    gets="$(jq -R -r --arg p "/fbup/${keys[$i]}" 'fromjson? | select(((.api // .funcName // "") | test("GetObject")) and .path == $p and ((.statusCode // 200) < 300)) | .path' "$trace" 2>/dev/null | wc -l)"
    total=$((total + gets))
    [[ "$gets" -ge 1 ]] || missing+=" ${removed[$i]##*/}"
  done
  [[ -z "$missing" ]] || die "object-only: no MinIO GetObject for removed segment(s):$missing"
  log "object-only GetObject: $total for the $n removed object key(s), at least one each"
  log "evidence: object-only restore from MinIO ($n sealed segment(s) removed by split retention, $FIRST among them, $total GetObject) bucket-only checksum $boundary_sum matches A at the boundary $cur; with the local tail $final_sum matches A"
}
# --- object-only (D2) end

log "=== OLD build: $OLD_BIN ($OLD_REF skip_mid=$SKIP_MID require_reuse=$REQUIRE_REUSE)"
start_server "$OLD_BIN" old
# object-only removes local files through split retention (local 1 day,
# bucket 30 days): nothing is old enough until the D2 step ages it.
storage='{"retention_days":7}'
[[ "$CASE" == object-only ]] && storage='{"retention_days":7,"local_retention_days":1,"bucket_retention_days":30}'
body="$(jq -n --arg gs "$SEED_GTID" --argjson port "$P_VIP" --argjson st "$storage" '{name:"fbup",cluster_key:"fbup",source:{host:"127.0.0.1",port:$port,user:"repl",password:"replpass",flavor:"mysql",server_id:470001},start:{mode:"GTID",gtid_set:$gs},storage:$st}')"
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

if [[ "$SKIP_MID" != 1 ]]; then
  log "=== MID build: $MID_BIN ($MID_REF)"
  start_server "$MID_BIN" mid
  sleep 10
  log "MID: $(curl -fsS "$API/api/tasks/$TASK" | jq -c '{state,last_error}')"
  snapshot "MID"
  stop_server
fi

# A's first-stint file whose name the third stint took. v0.5.57 reuses that
# name; a current release (FBUP_REQUIRE_REUSE=0) does not, and plain upgrade
# still has to resume.
FIRST=""
FIRST_KEY=""
FIRST_MD5=""
FIRST="$(cd "$DATA/$TASK" && { ls | grep -E '^mysql-bin\.[0-9]+\.(sealed|open)\.e1$' || true; } | sed -E 's/\.(sealed|open)\.e1$//' | sort -u | head -1)"
if [[ -n "$FIRST" && -f "$DATA/$TASK/$FIRST" ]]; then
  FIRST_KEY="$(mcx find fbup/fbup --name "$FIRST" 2>/dev/null | grep "/$UUID_A/$FIRST\$" | head -1 || true)"
  FIRST_MD5="$(md5sum "$DATA/$TASK/$FIRST" | awk '{print $1}')"
  log "first stint file $FIRST object ${FIRST_KEY:-none}"
else
  FIRST=""
  log "no reused first-stint name in $DATA/$TASK"
fi
if [[ "$REQUIRE_REUSE" == 1 ]]; then
  [[ -n "$FIRST_MD5" ]] || die "no reused first-stint name in $DATA/$TASK"
fi
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

log "=== schema 11 after legacy binaries stopped"
wait_registration_leases
(cd "$ROOT_DIR" && go run ./cmd/migrate up --dsn "$(meta_dsn)" --path "$ROOT_DIR/migrations") >"$OUT/migrate-11.log" 2>&1 || die "migrate schema 11"
read -r schema_version schema_dirty <<<"$(m "$P_META" "SELECT version, dirty FROM fbup.schema_migrations")"
[[ "$schema_version" -ge 11 && "$schema_dirty" == "0" ]] || die "schema before NEW is ${schema_version:-empty} dirty=${schema_dirty:-empty}"
m "$P_META" "SHOW COLUMNS FROM fbup.worker_heartbeats LIKE 'build_version'" | grep -q '^build_version' || die "worker_heartbeats.build_version missing before NEW"

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
    if [[ -n "$FIRST_MD5" ]]; then
      [[ "$(md5sum "$DATA/$TASK/$FIRST" | awk '{print $1}')" == "$FIRST_MD5" ]] || die "$FIRST on disk after repair differs from the first stint"
      log "evidence: repaired chain $(paste -sd, "$DATA/$TASK/.source-chain") $FIRST md5 $FIRST_MD5 checksum $got_sum matches A"
    else
      log "evidence: repaired chain $(paste -sd, "$DATA/$TASK/.source-chain") checksum $got_sum matches A (upgrade from $OLD_REF, no legacy name reuse)"
    fi
    if [[ "$CASE" == object-only ]]; then
      object_only_restore
    fi
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
