#!/usr/bin/env bash
# input: all-in-one binlog-server with metadata, and a closed source port
# output: proof the task reaches FAILED with last_error SOURCE_UNREACHABLE and a lease epoch that does not climb
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
RUN_TAG="$(date +%s)"
NAME="e2e-unreachable-giveup-${RUN_TAG}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd jq

fail() {
  echo "[unreachable-giveup] $*" >&2
  exit 1
}

payload="$(jq -n --arg name "$NAME" '{name:$name,cluster_key:$name,source:{host:"127.0.0.1",port:1,user:"repl",password:"replpass",flavor:"mysql",server_id:319901},start:{mode:"LATEST"},storage:{retention_days:7}}')"
create_file="$(mktemp)"
create_code="$(curl -sS -o "$create_file" -w '%{http_code}' -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$payload")"
[[ "$create_code" == "201" ]] || fail "create failed http=$create_code body=$(cat "$create_file")"
id="$(jq -r '.id // empty' "$create_file")"
rm -f "$create_file"
[[ -n "$id" && "$id" != "null" ]] || fail "create response has no id"
start_code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/api/tasks/$id/start")"
[[ "$start_code" == "204" ]] || fail "start failed http=$start_code"

baseline=0
max_epoch=0
saw_backoff=0
state=""
err=""
epoch=0
owner=""
# Ten attempts back off 1+2+4+8+16+30*4 seconds, about three minutes.
for _ in $(seq 1 240); do
  body="$(curl -fsS "$API/api/tasks/$id")"
  state="$(printf '%s' "$body" | jq -r '.state // empty')"
  err="$(printf '%s' "$body" | jq -r '.last_error // empty')"
  epoch="$(printf '%s' "$body" | jq -r '.epoch // 0')"
  owner="$(printf '%s' "$body" | jq -r '.owner_worker_id // empty')"
  if [[ "$epoch" =~ ^[0-9]+$ && "$epoch" -gt 0 ]]; then
    if [[ "$baseline" -eq 0 ]]; then
      baseline="$epoch"
    fi
    if [[ "$epoch" -gt "$max_epoch" ]]; then
      max_epoch="$epoch"
    fi
  fi
  if [[ "$state" == "RETRY_BACKOFF" ]]; then
    saw_backoff=1
  fi
  if [[ "$state" == "FAILED" ]]; then
    [[ "$err" == SOURCE_UNREACHABLE:* ]] || fail "last_error=$err"
    [[ "$saw_backoff" == "1" ]] || fail "FAILED without staying in RETRY_BACKOFF"
    [[ "$baseline" -gt 0 && "$max_epoch" == "$baseline" ]] || fail "epoch climbed baseline=$baseline max=$max_epoch"
    [[ "$epoch" == "0" && -z "$owner" ]] || fail "lease not released epoch=$epoch owner=$owner"
    echo "[unreachable-giveup] task=$id FAILED epoch_baseline=$baseline"
    exit 0
  fi
  sleep 1
done

fail "did not reach FAILED (state=$state epoch=$epoch max=$max_epoch error=$err)"
