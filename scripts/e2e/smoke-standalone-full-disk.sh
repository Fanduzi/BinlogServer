#!/usr/bin/env bash
# Package e2e provides module-level functionality for e2e.
# input: a 30MB tmpfs as a standalone data_dir (needs passwordless sudo to mount; skipped otherwise)
# output: with the disk full, a refused create answers 503 TASK_RECORDS_UNAVAILABLE, GET /api/findings serves TASK_RECORDS_FAILED/write and FINDINGS_PERSIST_FAILED from memory, GET /api/health is 503 degraded; after space is freed both close and health is ok
# pos: #395 QA P2 (a full data_dir must not hide TASK_RECORDS_FAILED); no MySQL needed, the tasks are never started
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
log "success"
