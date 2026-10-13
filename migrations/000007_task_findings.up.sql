-- ADR 0009 finding sink. One row per (task_id, kind, subject). A resolved row
-- keeps its history (resolved_at set); the same failure again reopens it.
-- Add-table-only: a process on schema 6 runs without it and keeps findings in
-- memory and /metrics until ./migrate up. The floor moves to 7 in v0.6.0.
CREATE TABLE task_findings (
  task_id VARCHAR(64) NOT NULL,
  kind VARCHAR(64) NOT NULL,
  subject VARCHAR(255) NOT NULL DEFAULT '',
  severity VARCHAR(16) NOT NULL,
  detail TEXT NULL,
  first_seen DATETIME(6) NOT NULL,
  last_seen DATETIME(6) NOT NULL,
  count BIGINT NOT NULL DEFAULT 1,
  resolved_at DATETIME(6) NULL,
  PRIMARY KEY (task_id, kind, subject),
  INDEX idx_task_findings_open (resolved_at, last_seen)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
