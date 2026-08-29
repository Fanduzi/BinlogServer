#!/usr/bin/env bash
# input: optional E2E database endpoint and credential environment overrides
# output: validated/exported E2E database topology and direct/failover metadata DSNs
# pos: canonical database-topology contract shared by E2E orchestration and scenarios
# note: if this file changes, update this header and module README.md.

e2e_require_value() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "$name is required" >&2
    return 1
  fi
}

e2e_require_port() {
  local name="$1"
  local value="${!name:-}"
  if [[ ! "$value" =~ ^[0-9]+$ ]] || (( 10#$value < 1 || 10#$value > 65535 )); then
    echo "$name must be an integer between 1 and 65535: $value" >&2
    return 1
  fi
}

e2e_topology_init() {
  E2E_SOURCE_HOST="${E2E_SOURCE_HOST-127.0.0.1}"
  E2E_SOURCE_USER="${E2E_SOURCE_USER-repl}"
  E2E_SOURCE_PASS="${E2E_SOURCE_PASS-replpass}"

  E2E_MYSQL57_PORT="${E2E_MYSQL57_PORT-13306}"
  E2E_MYSQL80_PORT="${E2E_MYSQL80_PORT-13307}"
  E2E_PERCONA57_PORT="${E2E_PERCONA57_PORT-13308}"
  E2E_PERCONA80_PORT="${E2E_PERCONA80_PORT-13309}"

  E2E_META_HOST="${E2E_META_HOST-127.0.0.1}"
  E2E_META_USER="${E2E_META_USER-meta}"
  E2E_META_PASS="${E2E_META_PASS-metapass}"
  E2E_META_DB="${E2E_META_DB-binlog_meta}"
  E2E_META_PRIMARY_PORT="${E2E_META_PRIMARY_PORT-13316}"
  E2E_META_REPLICA_PORT="${E2E_META_REPLICA_PORT-13317}"
  E2E_META_PROXYSQL_ADMIN_PORT="${E2E_META_PROXYSQL_ADMIN_PORT-6036}"
  E2E_META_PROXYSQL_PORT="${E2E_META_PROXYSQL_PORT-16036}"

  local required_values=(
    E2E_SOURCE_HOST E2E_SOURCE_USER E2E_SOURCE_PASS
    E2E_META_HOST E2E_META_USER E2E_META_PASS E2E_META_DB
  )
  local ports=(
    E2E_MYSQL57_PORT E2E_MYSQL80_PORT E2E_PERCONA57_PORT E2E_PERCONA80_PORT
    E2E_META_PRIMARY_PORT E2E_META_REPLICA_PORT
    E2E_META_PROXYSQL_ADMIN_PORT E2E_META_PROXYSQL_PORT
  )
  local name
  for name in "${required_values[@]}"; do
    e2e_require_value "$name" || return 1
  done
  for name in "${ports[@]}"; do
    e2e_require_port "$name" || return 1
  done

  export "${required_values[@]}" "${ports[@]}"
}

e2e_meta_dsn() {
  local topology="${1:-direct}"
  if [[ -n "${E2E_META_DSN:-}" ]]; then
    printf '%s' "$E2E_META_DSN"
    return 0
  fi

  local port
  case "$topology" in
    direct) port="$E2E_META_PRIMARY_PORT" ;;
    failover) port="$E2E_META_PROXYSQL_PORT" ;;
    *)
      echo "unsupported metadata topology: $topology" >&2
      return 1
      ;;
  esac
  printf '%s:%s@tcp(%s:%s)/%s?parseTime=true' \
    "$E2E_META_USER" "$E2E_META_PASS" "$E2E_META_HOST" "$port" "$E2E_META_DB"
}

e2e_topology_init
