#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd docker
need_cmd awk

wait_mysql_ready() {
  local svc="$1"
  for _ in {1..120}; do
    if docker compose -f "$COMPOSE_FILE" exec -T "$svc" mysqladmin ping -h127.0.0.1 -proot >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "$svc not ready in time" >&2
  return 1
}

mysql_primary_exec() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary mysql -uroot -proot -e "$1"
}

mysql_replica_exec() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-replica mysql -uroot -proot -e "$1"
}

wait_replication_running() {
  for _ in {1..120}; do
    local slave_status io_running sql_running
    slave_status="$(docker compose -f "$COMPOSE_FILE" exec -T meta-replica mysql -uroot -proot -e "SHOW SLAVE STATUS\\G" 2>/dev/null || true)"
    io_running="$(printf '%s\n' "$slave_status" | awk -F': ' '/^[[:space:]]*Slave_IO_Running:[[:space:]]/{print $2}' | tr -d '\r')"
    sql_running="$(printf '%s\n' "$slave_status" | awk -F': ' '/^[[:space:]]*Slave_SQL_Running:[[:space:]]/{print $2}' | tr -d '\r')"
    if [[ "$io_running" == "Yes" && "$sql_running" == "Yes" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "meta-replica replication not running in time" >&2
  docker compose -f "$COMPOSE_FILE" exec -T meta-replica mysql -uroot -proot -e "SHOW SLAVE STATUS\\G" >&2 || true
  return 1
}

assert_proxysql_monitor_user_exists() {
  local count
  count="$(docker compose -f "$COMPOSE_FILE" exec -T meta-primary mysql -uroot -proot -Nse "SELECT COUNT(*) FROM mysql.user WHERE user='proxysql_monitor';" | tr -d '\r')"
  if [[ "$count" -lt 1 ]]; then
    echo "proxysql_monitor user not found on meta-primary" >&2
    return 1
  fi
}

echo "[meta-setup] start services"
docker compose -f "$COMPOSE_FILE" up -d meta-primary meta-replica meta-proxysql
wait_mysql_ready "meta-primary"
wait_mysql_ready "meta-replica"

echo "[meta-setup] ensure users and grants on primary"
mysql_primary_exec "CREATE USER IF NOT EXISTS 'repl_meta'@'%' IDENTIFIED BY 'MetaRepl!2026';"
mysql_primary_exec "GRANT REPLICATION SLAVE ON *.* TO 'repl_meta'@'%';"
mysql_primary_exec "CREATE USER IF NOT EXISTS 'proxysql_monitor'@'%' IDENTIFIED BY 'MetaMon!2026';"
mysql_primary_exec "GRANT REPLICATION CLIENT, PROCESS ON *.* TO 'proxysql_monitor'@'%';"
mysql_primary_exec "FLUSH PRIVILEGES;"

echo "[meta-setup] configure replica"
mysql_replica_exec "STOP SLAVE;"
mysql_replica_exec "RESET SLAVE ALL;"
mysql_replica_exec "CHANGE MASTER TO MASTER_HOST='meta-primary', MASTER_PORT=3306, MASTER_USER='repl_meta', MASTER_PASSWORD='MetaRepl!2026', MASTER_AUTO_POSITION=1, MASTER_CONNECT_RETRY=1;"
mysql_replica_exec "START SLAVE;"
mysql_replica_exec "SET GLOBAL read_only=1;"

echo "[meta-setup] verify replication"
wait_replication_running
assert_proxysql_monitor_user_exists

echo "[meta-setup] success"
