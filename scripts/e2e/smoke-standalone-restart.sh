#!/usr/bin/env bash
# input: docker (mysql:8.0, mariadb:11.4), a locally built binlog-server, curl and jq
# output: standalone kill -9 -> restart -> automatic resume with a byte-exact restore on both flavors, plus TASK_RECORDS_FAILED for wrong key, corrupt file and missing key (MySQL 8.0)
# pos: docker coverage for ADR 0010 encrypted standalone task records ({data_dir}/.tasks.enc)
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_TAG="$(date +%s)"
LISTEN="${E2E_STANDALONE_LISTEN:-127.0.0.1:18090}"
API="http://${LISTEN}"
WORK="${E2E_STANDALONE_DIR:-$ROOT_DIR/tmp/e2e/standalone-restart-${RUN_TAG}}"
BIN="$WORK/binlog-server"
FLAVORS="${E2E_STANDALONE_FLAVORS:-mysql80 mariadb114}"
SECRET="e2e-Src-Pass-${RUN_TAG}"
SERVER_PID=""
CONTAINERS=()

need_cmd() { command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }; }
need_cmd docker; need_cmd curl; need_cmd jq; need_cmd sha256sum; need_cmd stat

log() { echo "[standalone-restart] $*"; }

cleanup() {
  local rc=$?
  if [[ "$rc" -ne 0 ]]; then
    for f in "$WORK"/*/server.log; do
      [[ -f "$f" ]] && { echo "== tail $f" >&2; tail -n 80 "$f" >&2; }
    done
  fi
  if [[ -n "$SERVER_PID" ]]; then kill -9 "$SERVER_PID" >/dev/null 2>&1 || true; fi
  for c in "${CONTAINERS[@]}"; do docker rm -f -v "$c" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

mkdir -p "$WORK"
( cd "$ROOT_DIR" && CGO_ENABLED=0 go build -o "$BIN" ./cmd/binlog-server )
"$BIN" task-key generate --out "$WORK/task.key" >/dev/null
"$BIN" task-key generate --out "$WORK/wrong.key" >/dev/null

# ---- database helpers ------------------------------------------------------
flavor_image() { [[ "$1" == mysql80 ]] && echo mysql:8.0 || echo mariadb:11.4; }
flavor_name()  { [[ "$1" == mysql80 ]] && echo mysql || echo mariadb; }
client_of()    { [[ "$1" == mysql80 ]] && echo mysql || echo mariadb; }
binlog_tool()  { [[ "$1" == mysql80 ]] && echo mysqlbinlog || echo mariadb-binlog; }
# mysql:8.0 ships no mysqlbinlog; the Percona 8.0 image does (as in smoke-gtid-pitr.sh).
tool_image()   { [[ "$1" == mysql80 ]] && echo "${E2E_MYSQLBINLOG_IMAGE:-percona/percona-server:8.0}" || echo mariadb:11.4; }

start_db() {
  local flavor="$1" name="$2" port="$3" sid="$4" binlog="$5"
  local -a args=(--server-id="$sid" --binlog-format=ROW)
  [[ "$binlog" == 1 ]] && args+=(--log-bin=mysql-bin)
  [[ "$binlog" == 0 && "$flavor" == mysql80 ]] && args+=(--skip-log-bin)
  # MySQL runs with GTIDs so the GTID task keeps its executed set across a restart (#393 QA P2-3).
  [[ "$flavor" == mysql80 ]] && args+=(--gtid-mode=ON --enforce-gtid-consistency=ON)
  docker rm -f -v "$name" >/dev/null 2>&1 || true
  CONTAINERS+=("$name")
  docker run -d --name "$name" -p "127.0.0.1:${port}:3306" \
    -e MYSQL_ROOT_PASSWORD="$SECRET" -e MYSQL_ROOT_HOST=% \
    -e MARIADB_ROOT_PASSWORD="$SECRET" -e MARIADB_ROOT_HOST=% \
    "$(flavor_image "$flavor")" "${args[@]}" >/dev/null
  local i
  for i in $(seq 1 120); do
    # TCP only reaches the final server, not the image's temporary init server.
    if docker exec "$name" "$(client_of "$flavor")" -uroot -p"$SECRET" -h127.0.0.1 --protocol=tcp -Nse "SELECT 1" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "$name not ready" >&2; docker logs "$name" >&2 || true; return 1
}

sql() { # flavor container sql
  docker exec -i "$2" "$(client_of "$1")" -uroot -p"$SECRET" -h127.0.0.1 --protocol=tcp --batch --raw -Nse "$3" 2>/dev/null | tr -d '\r'
}

write_rows() { # flavor src db from to
  local i stmt=""
  for i in $(seq "$4" "$5"); do stmt+="INSERT INTO $3.t VALUES ($i, REPEAT('r$i', 20));"; done
  sql "$1" "$2" "$stmt FLUSH BINARY LOGS;"
}

# ---- server helpers --------------------------------------------------------
start_server() { # data_dir key_file(or empty) log
  local -a env=(BINLOG_SERVER_MODE=standalone BINLOG_SERVER_LISTEN_ADDR="$LISTEN" BINLOG_SERVER_DATA_DIR="$1"
    BINLOG_SERVER_META_DSN=)
  [[ -n "$2" ]] && env+=(BINLOG_SERVER_STANDALONE_TASK_KEY_FILE="$2")
  env -u BINLOG_SERVER_TASK_KEY "${env[@]}" "$BIN" --config "$ROOT_DIR/deploy/e2e/config.yaml" >>"$3" 2>&1 &
  SERVER_PID=$!
  local i
  for i in $(seq 1 60); do
    curl -fsS "$API/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$SERVER_PID" 2>/dev/null || { echo "server exited" >&2; cat "$3" >&2; return 1; }
    sleep 0.5
  done
  echo "server not ready" >&2; cat "$3" >&2; return 1
}
kill9_server() { kill -9 "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=""; }
stop_server()  { kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=""; }

task_state() { curl -fsS "$API/api/tasks/$1" | jq -r '.state // empty'; }
wait_state() {
  local i
  for i in $(seq 1 120); do [[ "$(task_state "$1")" == "$2" ]] && return 0; sleep 0.5; done
  echo "task $1 not $2: $(curl -sS "$API/api/tasks/$1")" >&2; return 1
}
open_findings() { curl -fsS "$API/api/findings?limit=1000"; }
wait_finding() { # kind subject
  local i
  for i in $(seq 1 120); do
    open_findings | jq -e --arg k "$1" --arg s "$2" 'any(.[]; .kind==$k and .subject==$s)' >/dev/null && return 0
    sleep 0.5
  done
  echo "finding $1/$2 never opened: $(open_findings)" >&2; return 1
}
assert_no_finding_kind() {
  if open_findings | jq -e --arg k "$1" 'any(.[]; .kind==$k)' >/dev/null; then
    echo "unexpected $1: $(open_findings)" >&2; return 1
  fi
}
wait_no_finding_kind() {
  # Poll: the evaluator closes the finding on a sweep after the fix that
  # opened it, so a one-shot read can still see the finding it just cleared.
  local i
  for i in $(seq 1 120); do
    open_findings | jq -e --arg k "$1" 'any(.[]; .kind==$k)' >/dev/null || return 0
    sleep 0.5
  done
  echo "unexpected $1 still open: $(open_findings)" >&2; return 1
}

create_body() { # flavor port name [gtid_set]
  local start='{"mode":"LATEST"}'
  [[ -n "${4:-}" ]] && start="$(jq -cn --arg g "$4" '{mode:"GTID",gtid_set:$g}')"
  jq -n --arg name "$3" --arg pass "$SECRET" --arg fl "$(flavor_name "$1")" --argjson port "$2" \
    --argjson sid $((420000 + RANDOM % 10000)) --argjson start "$start" \
    '{name:$name,cluster_key:$name,source:{host:"127.0.0.1",port:$port,user:"root",password:$pass,flavor:$fl,server_id:$sid},start:$start,storage:{retention_days:7}}'
}

# After a restart the GTID task's run must start from a GTID set rebuilt
# from the stored segments (seed, sealed, open tail), which equals what the
# source had executed when the server was killed (#393 QA P2-3; before it
# the restored run had no set at all). Standalone has no checkpoint row, so
# the restored start on GET /api/tasks/{id} is what the run was given.
assert_restored_gtid_set() { # flavor src task want
  local got
  got="$(curl -fsS "$API/api/tasks/$3" | jq -r '.start.gtid_set // empty' | tr -d ' \n')"
  [[ -n "$got" ]] || { echo "GTID task $3 restored without a GTID set: $(curl -sS "$API/api/tasks/$3" | jq -c .start)" >&2; return 1; }
  if [[ "$(sql "$1" "$2" "SELECT GTID_SUBSET('$4', '$got') AND GTID_SUBSET('$got', '$4')")" != 1 ]]; then
    echo "GTID task $3 restored set [$got], want the stored set [$4]" >&2; return 1
  fi
}

# A second process on the same data_dir must refuse to start (#393 QA P2-2).
assert_second_process_refused() { # data_dir key_file
  local out="$WORK/second.log" pid i
  BINLOG_SERVER_MODE=standalone BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18099 BINLOG_SERVER_DATA_DIR="$1" \
    BINLOG_SERVER_META_DSN= BINLOG_SERVER_STANDALONE_TASK_KEY_FILE="$2" \
    "$BIN" --config "$ROOT_DIR/deploy/e2e/config.yaml" >"$out" 2>&1 &
  pid=$!
  for i in $(seq 1 30); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.5
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -9 "$pid"; echo "a second process on $1 kept running" >&2; return 1
  fi
  if wait "$pid"; then echo "a second process on $1 exited 0" >&2; return 1; fi
  grep -q '.tasks.lock' "$out" || { echo "second process did not name .tasks.lock:" >&2; cat "$out" >&2; return 1; }
  log "second process on the same data_dir refused: $(grep -o 'another binlog-server process holds[^"]*' "$out" | head -1)"
}

wait_caught_up() { # flavor src task
  local st file pos i
  st="$(sql "$1" "$2" "SHOW MASTER STATUS")"
  file="$(awk '{print $1; exit}' <<<"$st")"; pos="$(awk '{print $2; exit}' <<<"$st")"
  for i in $(seq 1 120); do
    if curl -fsS "$API/api/tasks/$3/files?limit=500" | jq -e --arg f "$file" --argjson p "$pos" \
      'any(.[]; .file_name==$f and (.end_pos // 0) >= $p)' >/dev/null; then return 0; fi
    sleep 0.5
  done
  echo "task $3 did not reach $file:$pos: $(curl -sS "$API/api/tasks/$3/files?limit=500")" >&2; return 1
}

restore_and_compare() { # flavor src restore task db
  local json p i=0 files=() stage="$WORK/$1/replay"
  json="$(curl -fsS "$API/api/tasks/$4/replay")"
  rm -rf "$stage"; mkdir -p "$stage"
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    [[ "$p" != /* ]] && p="$ROOT_DIR/${p#./}"
    cp "$p" "$stage/replay-$i.bin"; chmod 644 "$stage/replay-$i.bin"
    files+=("/replay/replay-$i.bin"); i=$((i + 1))
  done < <(jq -r '.paths[]' <<<"$json")
  [[ "$i" -ge 1 ]] || { echo "replay has no paths: $json" >&2; return 1; }
  local err="$WORK/apply-$1.err" opts=()
  # A LATEST task starts mid-stream; mariadb-binlog 11.4 strict GTID mode
  # rejects logs that do not start at the restore's empty GTID state.
  [[ "$1" == mariadb114 ]] && opts=(--skip-gtid-strict-mode)
  if ! docker run --rm -v "$stage:/replay:ro" --entrypoint "$(binlog_tool "$1")" "$(tool_image "$1")" ${opts[@]+"${opts[@]}"} "${files[@]}" 2>"$err" \
    | docker exec -i "$3" "$(client_of "$1")" -uroot -p"$SECRET" -h127.0.0.1 --protocol=tcp --binary-mode 2>>"$err"; then
    echo "apply failed on $1:" >&2; grep -v -- 'password on the command line' "$err" >&2 || true; return 1
  fi
  local want got
  want="$(sql "$1" "$2" "SELECT COUNT(*) FROM $5.t; CHECKSUM TABLE $5.t")"
  got="$(sql "$1" "$3" "SELECT COUNT(*) FROM $5.t; CHECKSUM TABLE $5.t")"
  [[ -n "$want" && "$want" == "$got" ]] || { echo "restore mismatch: source [$want] restore [$got]" >&2; return 1; }
  log "$1 restore matches source: $(head -1 <<<"$want") rows"
}

assert_secret_safe() { # data_dir log
  local f="$1/.tasks.enc" mode
  mode="$(stat -c '%a' "$f")"
  [[ "$mode" == 600 ]] || { echo "$f mode $mode, want 600" >&2; return 1; }
  if grep -aq -- "$SECRET" "$f"; then echo "$f contains the source password" >&2; return 1; fi
  if grep -aq -- "$SECRET" "$2"; then echo "server log contains the source password" >&2; return 1; fi
  if grep -aq -- "$(tr -d '\n' <"$WORK/task.key")" "$2"; then echo "server log contains the task key" >&2; return 1; fi
}

# ---- scenario --------------------------------------------------------------
run_flavor() {
  local flavor="$1" base="$2"
  local src="binlog-e2e-sa-src-${flavor}" rst="binlog-e2e-sa-rst-${flavor}"
  local data="$WORK/$flavor/data" slog="$WORK/$flavor/server.log" db="sa_${RUN_TAG}"
  mkdir -p "$data"
  log "$flavor: starting source and restore containers"
  start_db "$flavor" "$src" "$base" 4201 1
  start_db "$flavor" "$rst" $((base + 1)) 4202 0

  start_server "$data" "$WORK/task.key" "$slog"
  local resp id
  resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$(create_body "$flavor" "$base" "sa-$flavor-$RUN_TAG")")"
  id="$(jq -r '.id // empty' <<<"$resp")"
  [[ -n "$id" ]] || { echo "create failed: $resp" >&2; return 1; }
  curl -fsS -X POST "$API/api/tasks/$id/start" >/dev/null
  wait_state "$id" RUNNING
  [[ -f "$data/.tasks.enc" ]] || { echo "no $data/.tasks.enc after start" >&2; return 1; }

  local gid=""
  if [[ "$flavor" == mysql80 ]]; then
    local seed
    seed="$(sql "$flavor" "$src" "SELECT @@GLOBAL.gtid_executed" | tr -d ' \n')"
    resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$(create_body "$flavor" "$base" "sa-gtid-$RUN_TAG" "$seed")")"
    gid="$(jq -r '.id // empty' <<<"$resp")"
    [[ -n "$gid" ]] || { echo "create GTID task failed: $resp" >&2; return 1; }
    curl -fsS -X POST "$API/api/tasks/$gid/start" >/dev/null
    wait_state "$gid" RUNNING
  fi
  sql "$flavor" "$src" "CREATE DATABASE $db; CREATE TABLE $db.t (id INT PRIMARY KEY, v VARCHAR(200) NOT NULL)"
  write_rows "$flavor" "$src" "$db" 1 50
  wait_caught_up "$flavor" "$src" "$id"
  local at_kill=""
  if [[ -n "$gid" ]]; then
    wait_caught_up "$flavor" "$src" "$gid"
    at_kill="$(sql "$flavor" "$src" "SELECT @@GLOBAL.gtid_executed" | tr -d ' \n')"
  fi

  log "$flavor: kill -9 while RUNNING, then write while down"
  kill9_server
  write_rows "$flavor" "$src" "$db" 51 100
  sql "$flavor" "$src" "UPDATE $db.t SET v = CONCAT(v, '-u') WHERE id % 7 = 0"

  start_server "$data" "$WORK/task.key" "$slog"
  # No adopt and no start call: the record alone must bring the task back.
  wait_state "$id" RUNNING
  [[ "$(curl -fsS "$API/api/tasks" | jq --arg id "$id" '[.items[] | select(.id==$id)] | length')" == 1 ]] \
    || { echo "task $id not listed once after restart: $(curl -sS "$API/api/tasks")" >&2; return 1; }
  assert_no_finding_kind TASK_UNADOPTED
  assert_no_finding_kind TASK_RECORDS_FAILED
  assert_second_process_refused "$data" "$WORK/task.key"
  write_rows "$flavor" "$src" "$db" 101 150
  wait_caught_up "$flavor" "$src" "$id"
  if [[ -n "$gid" ]]; then
    wait_state "$gid" RUNNING
    assert_restored_gtid_set "$flavor" "$src" "$gid" "$at_kill"
    wait_caught_up "$flavor" "$src" "$gid"
    log "$flavor: GTID task $gid resumed with the stored GTID set $at_kill and caught up"
  fi
  restore_and_compare "$flavor" "$src" "$rst" "$id" "$db"
  assert_secret_safe "$data" "$slog"
  log "$flavor: auto-resumed after kill -9 with no operator action"

  if [[ "$flavor" == mysql80 ]]; then
    locked_cases "$flavor" "$src" "$id" "$data" "$slog" "$db"
    key_after_tasks_case "$flavor" "$src" "$db"
  fi
  stop_server
}

locked_case() { # name key_file subject data log flavor port
  local before after code
  before="$(sha256sum "$4/.tasks.enc" | awk '{print $1}')"
  start_server "$4" "$2" "$5"
  wait_finding TASK_RECORDS_FAILED "$3"
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/api/tasks" -H 'Content-Type: application/json' \
    -d "$(create_body "$6" "$7" "sa-locked-$1-$RUN_TAG")")"
  [[ "$code" -ge 400 ]] || { echo "$1: create accepted while locked (http $code)" >&2; return 1; }
  stop_server
  after="$(sha256sum "$4/.tasks.enc" | awk '{print $1}')"
  [[ "$before" == "$after" ]] || { echo "$1: record file changed while locked" >&2; return 1; }
  log "$1: TASK_RECORDS_FAILED/$3, create refused (http $code), file untouched"
}

locked_cases() { # flavor src id data log db
  local flavor="$1" src="$2" id="$3" data="$4" slog="$5" port
  port="$(docker port "$src" 3306/tcp | awk -F: '{print $NF; exit}')"
  stop_server
  locked_case wrong-key "$WORK/wrong.key" wrong_key "$data" "$slog" "$flavor" "$port"
  locked_case missing-key "" key_missing "$data" "$slog" "$flavor" "$port"
  cp -p "$data/.tasks.enc" "$WORK/tasks.enc.good"
  printf 'not a record file' >"$data/.tasks.enc"
  chmod 600 "$data/.tasks.enc"
  locked_case corrupt "$WORK/task.key" corrupt "$data" "$slog" "$flavor" "$port"
  cp -p "$WORK/tasks.enc.good" "$data/.tasks.enc"

  start_server "$data" "$WORK/task.key" "$slog"
  wait_state "$id" RUNNING
  wait_no_finding_kind TASK_RECORDS_FAILED
  write_rows "$flavor" "$src" "$6" 151 160
  wait_caught_up "$flavor" "$src" "$id"
  log "right key again: task resumed"
}

# Tip QA b4c52f4 P1: a task created while
# no key was configured is a leftover after the restart that adds the key.
# It must raise TASK_UNADOPTED and degrade /api/health (it was silent), and
# scripts/adopt-leftovers.sh must bring it back.
key_after_tasks_case() { # flavor src db
  local flavor="$1" src="$2" db="$3" port data="$WORK/$1/nokey-data" slog="$WORK/$1/nokey.log" id code
  port="$(docker port "$src" 3306/tcp | awk -F: '{print $NF; exit}')"
  stop_server
  mkdir -p "$data"
  start_server "$data" "" "$slog"
  id="$(curl -fsS -XPOST "$API/api/tasks" -d "$(create_body "$flavor" "$port" nokey)" | jq -r .id)"
  curl -fsS -o /dev/null -XPOST "$API/api/tasks/$id/start"
  wait_state "$id" RUNNING
  write_rows "$flavor" "$src" "$db" 161 165
  wait_caught_up "$flavor" "$src" "$id"
  stop_server
  start_server "$data" "$WORK/task.key" "$slog"
  wait_finding TASK_UNADOPTED ""
  code="$(curl -sS -o /dev/null -w '%{http_code}' "$API/api/health")"
  [[ "$code" == 503 ]] || { echo "health $code with a leftover, want 503" >&2; return 1; }
  log "task $id created without a key is TASK_UNADOPTED after the key restart, /api/health 503"
  jq -n --arg id "$id" --arg pass "$SECRET" --arg fl "$(flavor_name "$flavor")" --argjson port "$port" \
    '{($id): {name: "nokey", cluster_key: "nokey", source: {host: "127.0.0.1", port: $port, user: "root", password: $pass, flavor: $fl, server_id: 431999}}}' >"$WORK/adopt-map.json"
  chmod 600 "$WORK/adopt-map.json"
  "$ROOT_DIR/scripts/adopt-leftovers.sh" --api "$API" --map "$WORK/adopt-map.json" --start
  wait_state "$id" RUNNING
  wait_no_finding_kind TASK_UNADOPTED
  write_rows "$flavor" "$src" "$db" 166 170
  wait_caught_up "$flavor" "$src" "$id"
  code="$(curl -sS -o /dev/null -w '%{http_code}' "$API/api/health")"
  [[ "$code" == 200 ]] || { echo "health $code after bulk adopt, want 200: $(curl -sS "$API/api/health")" >&2; return 1; }
  log "adopt-leftovers.sh: task $id adopted and running, /api/health 200"
}

port=23390
for f in $FLAVORS; do
  run_flavor "$f" "$port"
  port=$((port + 2))
done
log "success: $FLAVORS"
