-- ADR 0011 PR-4. Idempotent: each step checks information_schema and skips
-- work that is already done, so a failed up can be re-run.
-- MySQL 8 has no ADD COLUMN IF NOT EXISTS. Dynamic DDL uses PREPARE.
-- Those session variables are only for this file. They are not a protocol
-- fence (a reconnect would drop them, so the fence is minRequiredSchemaVersion
-- and worker_registrations.cluster_protocol_version).
--
-- The BEFORE UPDATE trigger and binlog_server_kill_trx are not in this file.
-- golang-migrate sends the file as one COM_QUERY. A compound BEGIN/END body
-- is installed afterwards by the migrate command from
-- 000011_cluster_ownership.guards.sql, on a single statement, after this
-- backfill has committed. The trigger is created only once the backfill
-- has run, so the backfill does not have to bypass it.

-- Make division by zero abort the statement on MySQL 8 and MariaDB.
SET SESSION sql_mode = CONCAT(@@SESSION.sql_mode, ',STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO');

-- Precheck 1: a live worker registration. No DDL runs when this returns an error.
SELECT 1/IF((SELECT COUNT(*) FROM worker_registrations WHERE lease_expire_at > NOW(6)) = 0, 1, 0);

-- Precheck 2: a transaction older than the published idle_kill cap of 5 seconds.
-- The process idle_kill can be shorter. SQL cannot see that setting. 5 seconds
-- is the longest cap this build publishes (min(5s, lease_ttl/2)).
SELECT 1/IF((SELECT COUNT(*) FROM information_schema.innodb_trx WHERE trx_started < DATE_SUB(NOW(6), INTERVAL 5 SECOND)) = 0, 1, 0);

-- Precheck 3: log_bin on requires SUPER or log_bin_trust_function_creators,
-- which CREATE TRIGGER and CREATE PROCEDURE both need.
SELECT 1/IF(
  @@global.log_bin = 0
  OR @@global.log_bin_trust_function_creators = 1
  OR (
    SELECT COUNT(*) FROM information_schema.user_privileges
    WHERE GRANTEE = CONCAT(
        '''', SUBSTRING_INDEX(CURRENT_USER(), '@', 1), '''@''',
        SUBSTRING_INDEX(CURRENT_USER(), '@', -1), ''''
      )
      AND PRIVILEGE_TYPE = 'SUPER'
  ) > 0,
  1,
  0
);

SET SESSION lock_wait_timeout = 10;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'row_generation') = 0,
    'ALTER TABLE backup_tasks ADD COLUMN row_generation BIGINT NOT NULL DEFAULT 0',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'failed_reason') = 0,
    'ALTER TABLE backup_tasks ADD COLUMN failed_reason VARCHAR(64) NOT NULL DEFAULT ''''',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'failed_at') = 0,
    'ALTER TABLE backup_tasks ADD COLUMN failed_at DATETIME(6) NULL',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'tail_unreadable_since') = 0,
    'ALTER TABLE backup_tasks ADD COLUMN tail_unreadable_since DATETIME(6) NULL',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'pending_delete') = 0,
    'ALTER TABLE backup_tasks ADD COLUMN pending_delete TINYINT NOT NULL DEFAULT 0',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

-- Rename copies the existing values. A restarted old process fails its column
-- check. A dump that is already running is not stopped by this rename.
SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'worker_heartbeats' AND column_name = 'version') = 1
    AND (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'worker_heartbeats' AND column_name = 'build_version') = 0,
    'ALTER TABLE worker_heartbeats CHANGE COLUMN version build_version VARCHAR(64) NOT NULL',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'worker_registrations' AND column_name = 'cluster_protocol_version') = 0,
    'ALTER TABLE worker_registrations ADD COLUMN cluster_protocol_version INT NOT NULL DEFAULT 0',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.tables
      WHERE table_schema = DATABASE() AND table_name = 'task_chain') = 0,
    'CREATE TABLE task_chain (task_id VARCHAR(64) NOT NULL PRIMARY KEY, owner_node VARCHAR(128) NOT NULL, verified_at DATETIME(6) NULL, verified_epoch BIGINT NOT NULL DEFAULT 0) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.tables
      WHERE table_schema = DATABASE() AND table_name = 'task_node_refusal') = 0,
    'CREATE TABLE task_node_refusal (task_id VARCHAR(64) NOT NULL, node_id VARCHAR(128) NOT NULL, refuse_until DATETIME(6) NULL, reason VARCHAR(64) NOT NULL, attempts INT NOT NULL DEFAULT 0, PRIMARY KEY (task_id, node_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.tables
      WHERE table_schema = DATABASE() AND table_name = 'meta_canary') = 0,
    'CREATE TABLE meta_canary (node_id VARCHAR(128) NOT NULL PRIMARY KEY) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.tables
      WHERE table_schema = DATABASE() AND table_name = 'cluster_storage') = 0,
    'CREATE TABLE cluster_storage (id TINYINT NOT NULL PRIMARY KEY, storage_mode VARCHAR(16) NOT NULL, registered_at DATETIME(6) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

-- Backfill before the trigger exists. Operator Stop is STOPPING or STOPPED,
-- not FAILED, so a finished Stop is not rewritten. FAILED with desired RUN
-- is left alone, with an empty failed_reason.
UPDATE backup_tasks
SET desired_run = 'RUN',
    failed_reason = 'LEGACY_UNKNOWN',
    row_generation = row_generation + 1
WHERE state = 'FAILED'
  AND desired_run = 'STOP';

-- Heartbeat candidates only. verified_at stays NULL until a real ChainVerified.
INSERT IGNORE INTO task_chain (task_id, owner_node, verified_at, verified_epoch)
SELECT task_id, chain_owner, NULL, owner_epoch
FROM task_health
WHERE chain_owner <> '';
