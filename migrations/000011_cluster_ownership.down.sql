-- ADR 0011 PR-4 down. Stop every new node first. A live registration at
-- protocol 11 refuses this file before any drop. Re-run is safe: each drop
-- checks information_schema, and LEGACY_UNKNOWN is not written back to STOP.

SET SESSION sql_mode = CONCAT(@@SESSION.sql_mode, ',STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO');

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'worker_registrations' AND column_name = 'cluster_protocol_version') = 0,
    'SELECT 1',
    'SELECT 1/IF((SELECT COUNT(*) FROM worker_registrations WHERE cluster_protocol_version >= 11 AND lease_expire_at > NOW(6)) = 0, 1, 0)'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'pending_delete') = 0,
    'SELECT 1',
    'SELECT 1/IF((SELECT COUNT(*) FROM backup_tasks t INNER JOIN task_leases l ON l.task_id = t.id WHERE t.pending_delete <> 0 AND l.owner_worker_id <> '''' AND l.lease_expire_at > NOW(6)) = 0, 1, 0)'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET SESSION lock_wait_timeout = 10;

DROP PROCEDURE IF EXISTS binlog_server_kill_trx;
DROP TRIGGER IF EXISTS backup_tasks_bu_generation;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'worker_heartbeats' AND column_name = 'build_version') = 1
    AND (SELECT COUNT(*) FROM information_schema.columns
      WHERE table_schema = DATABASE() AND table_name = 'worker_heartbeats' AND column_name = 'version') = 0,
    'ALTER TABLE worker_heartbeats CHANGE COLUMN build_version version VARCHAR(64) NOT NULL',
    'SELECT 1'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

-- Expired or owner-less pending_delete rows go away before the column does,
-- including rows already STOPPED. A live lease was refused above.
SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'task_node_refusal') = 0
    OR (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'pending_delete') = 0,
    'SELECT 1',
    'DELETE r FROM task_node_refusal r INNER JOIN backup_tasks t ON t.id = r.task_id LEFT JOIN task_leases l ON l.task_id = t.id WHERE t.pending_delete <> 0 AND (l.task_id IS NULL OR l.owner_worker_id = '''' OR l.lease_expire_at <= NOW(6))'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'task_chain') = 0
    OR (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'pending_delete') = 0,
    'SELECT 1',
    'DELETE c FROM task_chain c INNER JOIN backup_tasks t ON t.id = c.task_id LEFT JOIN task_leases l ON l.task_id = t.id WHERE t.pending_delete <> 0 AND (l.task_id IS NULL OR l.owner_worker_id = '''' OR l.lease_expire_at <= NOW(6))'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'task_findings') = 0
    OR (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'pending_delete') = 0,
    'SELECT 1',
    'DELETE f FROM task_findings f INNER JOIN backup_tasks t ON t.id = f.task_id LEFT JOIN task_leases l ON l.task_id = t.id WHERE t.pending_delete <> 0 AND (l.task_id IS NULL OR l.owner_worker_id = '''' OR l.lease_expire_at <= NOW(6))'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'pending_delete') = 0,
    'SELECT 1',
    'DELETE l FROM task_leases l INNER JOIN backup_tasks t ON t.id = l.task_id WHERE t.pending_delete <> 0 AND (l.owner_worker_id = '''' OR l.lease_expire_at <= NOW(6))'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'pending_delete') = 0,
    'SELECT 1',
    'DELETE t FROM backup_tasks t LEFT JOIN task_leases l ON l.task_id = t.id WHERE t.pending_delete <> 0 AND (l.task_id IS NULL OR l.owner_worker_id = '''' OR l.lease_expire_at <= NOW(6))'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'row_generation') = 0,
    'SELECT 1',
    'ALTER TABLE backup_tasks DROP COLUMN row_generation'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'failed_reason') = 0,
    'SELECT 1',
    'ALTER TABLE backup_tasks DROP COLUMN failed_reason'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'failed_at') = 0,
    'SELECT 1',
    'ALTER TABLE backup_tasks DROP COLUMN failed_at'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'tail_unreadable_since') = 0,
    'SELECT 1',
    'ALTER TABLE backup_tasks DROP COLUMN tail_unreadable_since'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'backup_tasks' AND column_name = 'pending_delete') = 0,
    'SELECT 1',
    'ALTER TABLE backup_tasks DROP COLUMN pending_delete'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

SET @bs_ddl = (
  SELECT IF(
    (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'worker_registrations' AND column_name = 'cluster_protocol_version') = 0,
    'SELECT 1',
    'ALTER TABLE worker_registrations DROP COLUMN cluster_protocol_version'
  )
);
PREPARE bs_ddl FROM @bs_ddl;
EXECUTE bs_ddl;
DEALLOCATE PREPARE bs_ddl;

DROP TABLE IF EXISTS task_chain;
DROP TABLE IF EXISTS task_node_refusal;
DROP TABLE IF EXISTS meta_canary;
DROP TABLE IF EXISTS cluster_storage;
