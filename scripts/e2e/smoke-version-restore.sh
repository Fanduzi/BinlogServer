#!/usr/bin/env bash
# input: compose mysql57 and mysql84, the suite API on 127.0.0.1:18080, docker, curl, jq
# output: each source's post-start binlog restored onto a fresh instance of the same major version, with CHECKSUM TABLE and row text equal to the source
# pos: D4 restore proof for MySQL 5.7 and MySQL 8.4 LTS, run from the quick profile on every PR
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
RUN_TAG="$(date +%s)"
WORK="${E2E_VERSION_RESTORE_DIR:-$ROOT_DIR/tmp/e2e/version-restore-${RUN_TAG}}"
CONTAINERS=()

need_cmd() { command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }; }
need_cmd docker
need_cmd curl
need_cmd jq

log() { echo "[version-restore] $*"; }

cleanup() {
  local rc=$?
  if [[ "$rc" -ne 0 ]]; then
    for c in "${CONTAINERS[@]}"; do
      echo "== logs $c" >&2
      docker logs "$c" 2>&1 | tail -n 40 >&2 || true
    done
  fi
  for c in "${CONTAINERS[@]}"; do
    docker rm -f -v "$c" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

mkdir -p "$WORK"
cd "$ROOT_DIR"

source_sql() {
  docker compose -f "$COMPOSE_FILE" exec -T "$1" mysql -uroot -proot -Nse "$2" | tr -d '\r'
}

# MySQL 8.4 has no SHOW MASTER STATUS. 5.7 has no SHOW BINARY LOG STATUS.
source_status() {
  local svc="$1" out
  if out=$(docker compose -f "$COMPOSE_FILE" exec -T "$svc" mysql -uroot -proot -Nse "SHOW BINARY LOG STATUS" 2>/dev/null); then
    printf '%s\n' "$out" | tr -d '\r'
    return 0
  fi
  docker compose -f "$COMPOSE_FILE" exec -T "$svc" mysql -uroot -proot -Nse "SHOW MASTER STATUS" | tr -d '\r'
}

task_state() { curl -fsS "$API/api/tasks/$1" | jq -r '.state // empty'; }

wait_state() {
  local id="$1" want="$2" i
  for i in $(seq 1 60); do
    [[ "$(task_state "$id")" == "$want" ]] && return 0
    sleep 0.5
  done
  echo "task $id not $want: $(curl -sS "$API/api/tasks/$id" || true)" >&2
  return 1
}

# file_name + end_pos on the backup must reach the source file/pos.
wait_caught_up() {
  local id="$1" file="$2" pos="$3" i
  for i in $(seq 1 60); do
    if curl -fsS "$API/api/tasks/$id/files?limit=500" | jq -e --arg f "$file" --argjson p "$pos" \
      'any(.[]; .file_name == $f and (.end_pos // 0) >= $p)' >/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  echo "task $id did not reach $file:$pos" >&2
  echo "files: $(curl -sS "$API/api/tasks/$id/files?limit=500" || true)" >&2
  echo "task: $(curl -sS "$API/api/tasks/$id" || true)" >&2
  return 1
}

start_restore() {
  local name="$1" image="$2"
  docker rm -f -v "$name" >/dev/null 2>&1 || true
  CONTAINERS+=("$name")
  docker run -d --name "$name" \
    -e MYSQL_ROOT_PASSWORD=root -e MYSQL_ROOT_HOST=% \
    "$image" >/dev/null
  local i
  for i in $(seq 1 120); do
    if docker exec "$name" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "SELECT 1" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "$name not ready" >&2
  return 1
}

# One source: stream from the current tip, write a known table across a rotate,
# replay every backed-up file onto a fresh server, compare checksum and rows.
restore_one() {
  local svc="$1" port="$2" image="$3" client_image="$4" sid="$5"
  local db="d4_${svc}" name="d4-${svc}-${RUN_TAG}" id body st file pos
  log "$svc stream from ${image} port ${port}"
  body=$(jq -n \
    --arg name "$name" \
    --arg host "$E2E_SOURCE_HOST" \
    --arg user "$E2E_SOURCE_USER" \
    --arg pass "$E2E_SOURCE_PASS" \
    --argjson port "$port" \
    --argjson sid "$sid" \
    '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:$sid},start:{mode:"LATEST"},storage:{retention_days:7}}')
  id=$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body" | jq -r '.id // empty')
  [[ -n "$id" && "$id" != "null" ]] || { echo "create task failed for $svc" >&2; return 1; }
  curl -fsS -X POST "$API/api/tasks/$id/start" >/dev/null
  wait_state "$id" "RUNNING"
  # The LATEST anchor is the checkpoint. Writes after it are what this backup holds.
  local i ck
  for i in $(seq 1 60); do
    ck=$(curl -fsS "$API/api/tasks/$id/checkpoint" 2>/dev/null || true)
    file=$(jq -r '.file // empty' <<<"$ck")
    [[ -n "$file" ]] && break
    sleep 0.5
  done
  [[ -n "$file" ]] || { echo "$svc task $id has no checkpoint: $ck" >&2; return 1; }

  source_sql "$svc" "
    CREATE DATABASE ${db};
    CREATE TABLE ${db}.t (id INT PRIMARY KEY, v VARCHAR(64) NOT NULL);
    INSERT INTO ${db}.t VALUES (1,'a'),(2,'b'),(3,'c');
    UPDATE ${db}.t SET v='bb' WHERE id=2;
    DELETE FROM ${db}.t WHERE id=3;
    FLUSH BINARY LOGS;
    INSERT INTO ${db}.t VALUES (4,'d'),(5,'e');
  "
  st=$(source_status "$svc")
  file=$(awk 'NR==1{print $1}' <<<"$st")
  pos=$(awk 'NR==1{print $2}' <<<"$st")
  wait_caught_up "$id" "$file" "$pos"
  curl -fsS -X POST "$API/api/tasks/$id/stop" >/dev/null
  wait_state "$id" "STOPPED"

  local json p i=0 stage="$WORK/$svc" err="$WORK/$svc-apply.err"
  json=$(curl -fsS "$API/api/tasks/$id/replay")
  rm -rf "$stage"
  mkdir -p "$stage"
  local -a files=()
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    [[ "$p" != /* ]] && p="$ROOT_DIR/${p#./}"
    cp "$p" "$stage/replay-$i.bin"
    files+=("/replay/replay-$i.bin")
    i=$((i + 1))
  done < <(jq -r '.paths[]' <<<"$json")
  [[ "$i" -ge 1 ]] || { echo "$svc replay has no paths: $json" >&2; return 1; }
  chmod -R a+rX "$stage"

  local restore="binlog-d4-restore-${svc}"
  log "$svc replay $i file(s) onto a fresh ${image}"
  start_restore "$restore" "$image"
  if ! docker run --rm -v "$stage:/replay:ro" --entrypoint mysqlbinlog "$client_image" \
      --verify-binlog-checksum --skip-gtids "${files[@]}" 2>"$err" \
      | docker exec -i "$restore" mysql -uroot -proot -h127.0.0.1 --protocol=tcp --binary-mode >>"$err" 2>&1; then
    echo "$svc apply failed:" >&2
    cat "$err" >&2 || true
    return 1
  fi

  local want got
  want=$(source_sql "$svc" "CHECKSUM TABLE ${db}.t; SELECT id, v FROM ${db}.t ORDER BY id;")
  got=$(docker exec "$restore" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse \
    "CHECKSUM TABLE ${db}.t; SELECT id, v FROM ${db}.t ORDER BY id;" | tr -d '\r')
  [[ -n "$want" && "$want" == "$got" ]] || { echo "$svc restore mismatch: source [$want] restore [$got]" >&2; return 1; }
  log "$svc checksum and rows match: $(head -1 <<<"$want")"
}

# The official mysql images do not ship mysqlbinlog. Percona Server of the
# same series does, and it can read that series' events.
restore_one mysql57 "$E2E_MYSQL57_PORT" mysql:5.7 "${E2E_MYSQL57_BINLOG_IMAGE:-percona/percona-server:5.7}" 330571
restore_one mysql84 "$E2E_MYSQL84_PORT" mysql:8.4 "${E2E_MYSQL84_BINLOG_IMAGE:-percona/percona-server:8.4}" 330841

log "mysql57 and mysql84 restores match"
