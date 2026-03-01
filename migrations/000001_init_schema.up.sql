CREATE TABLE backup_tasks (
  id VARCHAR(64) PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  cluster_key VARCHAR(255) NOT NULL,
  state VARCHAR(32) NOT NULL,
  last_error TEXT NULL,
  owner_worker_id VARCHAR(128) NULL,
  epoch BIGINT NOT NULL DEFAULT 0,
  run_id VARCHAR(128) NULL,
  source_json JSON NOT NULL,
  start_json JSON NOT NULL,
  storage_json JSON NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  UNIQUE KEY uk_backup_tasks_cluster_key (cluster_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE backup_checkpoints (
  task_id VARCHAR(64) PRIMARY KEY,
  file_name VARCHAR(255) NOT NULL,
  pos BIGINT UNSIGNED NOT NULL,
  gtid_set TEXT NULL,
  updated_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE task_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL,
  event_type VARCHAR(64) NOT NULL,
  message TEXT NULL,
  detail TEXT NULL,
  event_time DATETIME(6) NOT NULL,
  event_seq BIGINT NOT NULL,
  INDEX idx_task_events_task_time (task_id, event_time)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE binlog_files (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL,
  file_name VARCHAR(255) NOT NULL,
  source_file VARCHAR(255) NULL,
  file_path TEXT NOT NULL,
  epoch BIGINT NOT NULL DEFAULT 0,
  state VARCHAR(32) NOT NULL DEFAULT 'SEALED',
  checksum VARCHAR(128) NULL,
  size_bytes BIGINT NOT NULL,
  start_pos BIGINT UNSIGNED NOT NULL,
  end_pos BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  sealed_at DATETIME(6) NOT NULL,
  object_key TEXT NULL,
  upload_state VARCHAR(32) NOT NULL DEFAULT 'LOCAL_ONLY',
  upload_error TEXT NULL,
  uploaded_at DATETIME(6) NULL,
  UNIQUE KEY uk_task_file (task_id, file_name),
  INDEX idx_task_sealed (task_id, sealed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE task_leases (
  task_id VARCHAR(64) PRIMARY KEY,
  owner_worker_id VARCHAR(128) NOT NULL,
  epoch BIGINT NOT NULL,
  lease_expire_at DATETIME(6) NOT NULL,
  renewed_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE task_runs (
  run_id VARCHAR(64) PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL,
  worker_id VARCHAR(128) NOT NULL,
  epoch BIGINT NOT NULL,
  started_at DATETIME(6) NOT NULL,
  ended_at DATETIME(6) NULL,
  end_reason VARCHAR(64) NULL,
  INDEX idx_task_runs_task_started (task_id, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE worker_heartbeats (
  worker_id VARCHAR(128) PRIMARY KEY,
  host VARCHAR(255) NOT NULL,
  version VARCHAR(64) NOT NULL,
  last_seen_at DATETIME(6) NOT NULL,
  status VARCHAR(32) NOT NULL,
  INDEX idx_worker_heartbeats_seen (last_seen_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE worker_registrations (
  worker_id VARCHAR(128) PRIMARY KEY,
  session_id VARCHAR(128) NOT NULL,
  lease_expire_at DATETIME(6) NOT NULL,
  renewed_at DATETIME(6) NOT NULL,
  INDEX idx_worker_registrations_expire (lease_expire_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
