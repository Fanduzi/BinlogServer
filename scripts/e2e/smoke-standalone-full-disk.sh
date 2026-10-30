#!/usr/bin/env bash
# Package e2e provides module-level functionality for e2e.
# input: a 30MB tmpfs as a standalone data_dir (needs passwordless sudo to mount; skipped otherwise), the suite mysql80 for the live-dump cell
# output: with the disk full, a refused create answers 503 TASK_RECORDS_UNAVAILABLE, GET /api/findings serves TASK_RECORDS_FAILED/write and FINDINGS_PERSIST_FAILED from memory, GET /api/health is 503 degraded; after space is freed both close and health is ok. A5 live-dump cell: filling the disk under a RUNNING task leaves the row non-FAILED with findings open and /api/health 503; freeing space resumes the dump and the checkpoint's file:pos covers every transaction written during the outage — no chain gap
# pos: #395 QA P2 (a full data_dir must not hide TASK_RECORDS_FAILED) plus production-readiness A5 active-dump cell; the cluster-owner cell is the fault-matrix row15
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d)"
DATA="$WORK/data"
BIN="$WORK/binlog-server"
API="http://127.0.0.1:18112"
SERVER_PID=""
log() { echo "[standalone-full-disk] $*"; }
cleanup() {
  [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  mountpoint -q "$DATA" && sudo -n umount "$DATA" || true
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'echo "[standalone-full-disk] failed at line $LINENO" >&2; tail -40 "$WORK/server.log" >&2 || true' ERR

if ! sudo -n true 2>/dev/null; then
  log "skip: needs passwordless sudo to mount a tmpfs"
  exit 0
fi
mkdir -p "$DATA"
sudo -n mount -t tmpfs -o "size=30m,uid=$(id -u),gid=$(id -g)" tmpfs "$DATA"
( cd "$ROOT_DIR" && CGO_ENABLED=0 go build -o "$BIN" ./cmd/binlog-server )
"$BIN" task-key generate --out "$WORK/task.key" >/dev/null

env -u BINLOG_SERVER_TASK_KEY BINLOG_SERVER_MODE=standalone BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18112 \
  BINLOG_SERVER_DATA_DIR="$DATA" BINLOG_SERVER_META_DSN= \
  BINLOG_SERVER_STANDALONE_TASK_KEY_FILE="$WORK/task.key" \
  "$BIN" --config "$ROOT_DIR/deploy/e2e/config.yaml" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 60); do curl -fsS "$API/healthz" >/dev/null 2>&1 && break; sleep 0.5; done

body() { # name server_id
  jq -n --arg n "$1" --argjson sid "$2" '{name:$n,cluster_key:$n,source:{host:"127.0.0.1",port:1,user:"root",password:"pw",flavor:"mysql",server_id:$sid},start:{mode:"LATEST"},storage:{retention_days:7}}'
}
findings() { curl -fsS "$API/api/findings" | jq -r '.[] | "\(.kind)/\(.subject)"' | sort | tr '\n' ' '; }
wait_for() { # description command...
  local d="$1"; shift
  for _ in $(seq 1 90); do "$@" && return 0; sleep 1; done
  echo "timed out waiting for: $d (findings: $(findings); health: $(curl -sS "$API/api/health"))" >&2
  return 1
}

code="$(curl -sS -o /dev/null -w '%{http_code}' -XPOST "$API/api/tasks" -d "$(body a 4301)")"
[[ "$code" == 201 ]] || { echo "create on an empty disk: $code" >&2; exit 1; }
[[ "$(curl -sS -o /dev/null -w '%{http_code}' "$API/api/health")" == 200 ]]

dd if=/dev/zero of="$DATA/fill" bs=1M count=64 2>/dev/null || true
log "data_dir full: $(df -h "$DATA" | tail -1 | awk '{print $5}')"
out="$(curl -sS -w '\n%{http_code}' -XPOST "$API/api/tasks" -d "$(body b 4302)")"
[[ "$(tail -1 <<<"$out")" == 503 && "$out" == *TASK_RECORDS_UNAVAILABLE* ]] || { echo "create on a full disk: $out" >&2; exit 1; }
wait_for "TASK_RECORDS_FAILED/write and FINDINGS_PERSIST_FAILED served from memory" \
  bash -c "f=\"\$(curl -fsS $API/api/findings | jq -r '.[]|\"\(.kind)/\(.subject)\"')\"; grep -qx TASK_RECORDS_FAILED/write <<<\"\$f\" && grep -qx FINDINGS_PERSIST_FAILED/ <<<\"\$f\""
health="$(curl -sS -w '\n%{http_code}' "$API/api/health")"
[[ "$(tail -1 <<<"$health")" == 503 && "$health" == *degraded* ]] || { echo "health on a full disk: $health" >&2; exit 1; }
log "full disk: create 503, findings [$(findings)], /api/health 503 degraded"

rm -f "$DATA/fill"
wait_for "findings to close after space is freed (record retry every 30s)" \
  bash -c "[[ -z \"\$(curl -fsS $API/api/findings | jq -r '.[]|select(.kind==\"FINDINGS_PERSIST_FAILED\" or .kind==\"TASK_RECORDS_FAILED\")|.kind')\" ]]"
[[ "$(curl -sS -o /dev/null -w '%{http_code}' "$API/api/health")" == 200 ]]
log "space freed: findings closed, /api/health ok"

# ---- phase 2: a live dump while the disk fills (A5) --------------------------
# A refused create proves nothing about the data plane. Fill the tmpfs under
# a RUNNING task: ENOSPC is an epoch loss — the row keeps desired RUN and
# never writes FAILED; findings and /api/health must say the backup is not
# healthy. Freeing space must resume the dump and the checkpoint must pass
# every transaction written while the disk was full — no chain gap.
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"

mysql80() {
  docker compose -f "$ROOT_DIR/deploy/e2e/docker-compose.yml" exec -T mysql80 \
    mysql -uroot -proot -Nse "$1" | tr -d '\r'
}

wait_task_state() { # id want secs
  local id="$1" want="$2" secs="${3:-60}" st i
  for i in $(seq 1 "$secs"); do
    st="$(curl -fsS "$API/api/tasks/$id" 2>/dev/null | jq -r '.state // empty')"
    [[ "$st" == "$want" ]] && return 0
    [[ "$st" == "FAILED" && "$want" != "FAILED" ]] && { echo "task $id FAILED" >&2; return 1; }
    sleep 1
  done
  echo "task $id never reached $want (state=$st)" >&2
  return 1
}

checkpoint() { curl -fsS "$API/api/tasks/$1/checkpoint" | jq -r '"\(.file):\(.pos)"'; }

# source_filepos prints the source's File:Position as name:N (the suite's
# mysql80 does not enable gtid_mode, so file/pos is the outage-boundary proof).
source_filepos() {
  mysql80 "SHOW MASTER STATUS;" | awk '{n=$1; sub(/.*\./,"",n); print n ":" $2}'
}

filepos_ge() { # "name:pos" >= "name:pos" -> 0/1; names reduce to their digit tail
  local af ap bf bp
  af="$(basename "${1%%:*}" | tr -cd '0-9' | sed 's/^0*//')"
  ap="${1##*:}"; bp="${2##*:}"
  bf="$(basename "${2%%:*}" | tr -cd '0-9' | sed 's/^0*//')"
  af="${af:-0}"; bf="${bf:-0}"; ap="${ap:-0}"; bp="${bp:-0}"
  (( af > bf || (af == bf && ap >= bp) ))
}

RUN_TAG="$(date +%s)"
DB2="a5full_${RUN_TAG}"
mysql80 "CREATE DATABASE IF NOT EXISTS ${DB2}; CREATE TABLE IF NOT EXISTS ${DB2}.t(id INT PRIMARY KEY AUTO_INCREMENT, v VARCHAR(64));"

tid="$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' \
  -d "{\"name\":\"a5-dump-full-${RUN_TAG}\",\"cluster_key\":\"a5-dump-full-${RUN_TAG}\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":$E2E_MYSQL80_PORT,\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":442001},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}" \
  | jq -r '.id // empty')"
[[ -n "$tid" && "$tid" != "null" ]] || { echo "create active task failed" >&2; exit 1; }
curl -fsS -X POST "$API/api/tasks/$tid/start" >/dev/null
wait_task_state "$tid" RUNNING 90
mysql80 "INSERT INTO ${DB2}.t(v) VALUES ('before-${RUN_TAG}');"
sleep 2
log "active dump RUNNING, checkpoint $(checkpoint "$tid")"

dd if=/dev/zero of="$DATA/fill2" bs=1M count=64 2>/dev/null || true
log "data_dir full under a live dump: $(df -h "$DATA" | tail -1 | awk '{print $5}')"

# An idle task touches no local bytes, so nothing would ever hit ENOSPC: new
# source rows plus a rotation force appends/open() onto the full disk.
mysql80 "INSERT INTO ${DB2}.t(v) VALUES ('during-${RUN_TAG}'),('during2-${RUN_TAG}'); FLUSH BINARY LOGS;"
pos_at_fill="$(source_filepos)"

# The task must leave RUNNING (epoch lost, retries) but never write FAILED,
# and the surface must admit the backup is not healthy.
wait_for "task off RUNNING without FAILED on a full disk" \
  bash -c "st=\"\$(curl -fsS $API/api/tasks/$tid | jq -r '.state')\"; [[ \"\$st\" != RUNNING && \"\$st\" != FAILED ]]"
wait_for "/api/health 503 with the dump starved" \
  bash -c "[[ \"\$(curl -sS -o /dev/null -w '%{http_code}' $API/api/health)\" == 503 ]]"
log "live dump on a full disk: findings open, /api/health 503"

rm -f "$DATA/fill2"
wait_task_state "$tid" RUNNING 120
# The checkpoint's file:pos must reach the source position captured while the
# disk was full — including the rows written during the outage.
for _ in $(seq 1 120); do
  got="$(checkpoint "$tid")"
  filepos_ge "$got" "$pos_at_fill" && break
  sleep 1
done
got="$(checkpoint "$tid")"
filepos_ge "$got" "$pos_at_fill" \
  || { echo "checkpoint $got did not pass source position $pos_at_fill" >&2; exit 1; }
wait_for "/api/health 200 after the dump recovered" \
  bash -c "[[ \"\$(curl -sS -o /dev/null -w '%{http_code}' $API/api/health)\" == 200 ]]"
log "space freed under a live dump: RUNNING, checkpoint covers $pos_at_fill, /api/health ok"

curl -fsS -X POST "$API/api/tasks/$tid/stop" >/dev/null || true
log "success"
