#!/usr/bin/env bash
# Package scripts provides module-level functionality for scripts.
# input: a running standalone binlog-server API and a JSON map of task id -> {name, cluster_key, source{host,port,user,password,flavor,server_id}, storage?}
# output: adopts (and with --start starts) every leftover task directory that has an open TASK_UNADOPTED finding; without --map prints the leftover ids and a map template; exits non-zero while any leftover is left unadopted
# pos: bulk adopt for a standalone upgrade from v0.5.66 (which kept no task record) and for tasks created while no task key was configured (ADR 0010 §Migration)
# note: if this file changes, update this header and scripts/README.md.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: adopt-leftovers.sh [--api URL] [--map FILE] [--start]

  --api URL   binlog-server API (default http://127.0.0.1:8080, or $BINLOG_SERVER_API)
  --map FILE  JSON object: { "<task id>": { "name": "...", "cluster_key": "...",
              "source": { "host": "...", "port": 3306, "user": "...", "password": "...",
              "flavor": "mysql|mariadb", "server_id": 1234 }, "storage": { ... } } }
              Keep it 0600: it holds source passwords.
  --start     start each task after adopting it

Leftovers are the tasks with an open TASK_UNADOPTED finding. Without --map the
script lists them and prints a map template to fill in. Auth: set
$BINLOG_SERVER_API_KEY (sent as X-API-Key) or $BINLOG_SERVER_BEARER_TOKEN.
USAGE
}

API="${BINLOG_SERVER_API:-http://127.0.0.1:8080}"
MAP=""
START=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --api) API="$2"; shift 2 ;;
    --map) MAP="$2"; shift 2 ;;
    --start) START=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

auth=()
[[ -n "${BINLOG_SERVER_API_KEY:-}" ]] && auth=(-H "X-API-Key: $BINLOG_SERVER_API_KEY")
[[ -n "${BINLOG_SERVER_BEARER_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $BINLOG_SERVER_BEARER_TOKEN")
api() { curl -sS "${auth[@]}" "$@"; }

mapfile -t leftovers < <(api -f "$API/api/findings" | jq -r '.[] | select(.kind == "TASK_UNADOPTED") | .task_id' | sort -n -u)
if [[ ${#leftovers[@]} -eq 0 ]]; then
  echo "no leftover task directories (no open TASK_UNADOPTED)"
  exit 0
fi
echo "leftover task directories: ${leftovers[*]}"

if [[ -z "$MAP" ]]; then
  echo "fill in this map (one entry per id), then run again with --map FILE:"
  printf '%s\n' "${leftovers[@]}" | jq -R . | jq -s 'map({key: ., value: {name: "", cluster_key: "", source: {host: "", port: 3306, user: "", password: "", flavor: "mysql", server_id: 0}}}) | from_entries'
  exit 1
fi
if [[ "$(stat -c %a "$MAP")" != 600 && "$(stat -c %a "$MAP")" != 400 ]]; then
  echo "warning: $MAP is mode $(stat -c %a "$MAP"); it holds passwords, chmod 600 it" >&2
fi

failed=0
for id in "${leftovers[@]}"; do
  entry="$(jq -c --arg id "$id" '.[$id] // empty' "$MAP")"
  if [[ -z "$entry" ]]; then
    echo "task $id: not in $MAP, left unadopted" >&2
    failed=1
    continue
  fi
  body="$(jq -c '{name, cluster_key, source} + (if .storage then {storage} else {} end)' <<<"$entry")"
  out="$(api -w '\n%{http_code}' -X POST "$API/api/tasks/$id/adopt" -H 'Content-Type: application/json' -d "$body")"
  code="$(tail -1 <<<"$out")"
  if [[ "$code" != 200 ]]; then
    echo "task $id: adopt failed (http $code): $(sed '$d' <<<"$out")" >&2
    failed=1
    continue
  fi
  echo "task $id: adopted"
  if [[ "$START" == 1 ]]; then
    code="$(api -o /dev/null -w '%{http_code}' -X POST "$API/api/tasks/$id/start")"
    if [[ "$code" != 204 ]]; then
      echo "task $id: start failed (http $code)" >&2
      failed=1
      continue
    fi
    echo "task $id: started"
  fi
done
exit "$failed"
