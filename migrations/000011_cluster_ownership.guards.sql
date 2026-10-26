-- Installed by the migrate command after 000011_cluster_ownership.up.sql.
-- Each block is one statement. The installer splits on a line that is exactly `-- next`.
-- DEFINER is the account that runs the migration. Do not drop that account:
-- backup_tasks updates and CALL binlog_server_kill_trx fail closed without it.
-- GRANT EXECUTE ON PROCEDURE binlog_server_kill_trx TO the binlog-server
-- account only. The DEFINER also needs PROCESS, and CONNECTION_ADMIN or SUPER.
-- MySQL 8 reads performance_schema.data_locks. MariaDB has no data_locks;
-- innodb_locks shows only locks that are part of a wait, so the body no-ops
-- unless a waiter exists.

CREATE DEFINER = CURRENT_USER TRIGGER backup_tasks_bu_generation
BEFORE UPDATE ON backup_tasks
FOR EACH ROW
BEGIN
  IF NEW.row_generation <> OLD.row_generation + 1 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT = 'backup_tasks update must bump row_generation';
  END IF;
END;

-- next
CREATE DEFINER = CURRENT_USER PROCEDURE binlog_server_kill_trx(IN p_trx_id VARCHAR(64))
SQL SECURITY DEFINER
proc: BEGIN
  DECLARE v_thread BIGINT DEFAULT NULL;
  DECLARE v_locked INT DEFAULT 0;
  DECLARE v_rds INT DEFAULT 0;

  DECLARE CONTINUE HANDLER FOR NOT FOUND SET v_thread = NULL;
  DECLARE CONTINUE HANDLER FOR SQLEXCEPTION SET v_locked = v_locked;

  SELECT trx_mysql_thread_id INTO v_thread
  FROM information_schema.innodb_trx
  WHERE trx_id = p_trx_id
  LIMIT 1;

  IF v_thread IS NULL THEN
    LEAVE proc;
  END IF;

  SELECT COUNT(*) INTO v_locked
  FROM performance_schema.data_locks
  WHERE ENGINE_TRANSACTION_ID = CAST(p_trx_id AS UNSIGNED)
    AND LOCK_STATUS = 'GRANTED'
    AND OBJECT_SCHEMA = DATABASE()
    AND OBJECT_NAME IN ('task_leases', 'meta_canary');

  IF v_locked = 0 THEN
    SELECT COUNT(*) INTO v_locked
    FROM information_schema.innodb_locks
    WHERE lock_trx_id = p_trx_id
      AND (lock_table LIKE '%task_leases%' OR lock_table LIKE '%meta_canary%');
  END IF;

  IF v_locked = 0 THEN
    LEAVE proc;
  END IF;

  SELECT COUNT(*) INTO v_rds
  FROM information_schema.routines
  WHERE routine_schema = 'mysql' AND routine_name = 'rds_kill';

  IF v_rds > 0 THEN
    CALL mysql.rds_kill(v_thread);
  ELSE
    SET @bs_kill_sql = CONCAT('KILL ', v_thread);
    PREPARE bs_kill_stmt FROM @bs_kill_sql;
    EXECUTE bs_kill_stmt;
    DEALLOCATE PREPARE bs_kill_stmt;
  END IF;
END;
