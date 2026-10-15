-- ADR 0009 phase 2 (design docs/design/0009-phase2-health-evaluator.md §9).
-- Additive only: v0.5.x keeps the schema floor at 6, a process without these
-- tables or columns falls back to the v0.5.66 finding upsert and probes
-- again on the next pass. v0.6.0 raises the floor to 9 (proposed D1).
ALTER TABLE task_findings
  ADD COLUMN evidence TEXT NULL,
  ADD COLUMN segments JSON NULL,
  ADD COLUMN generation BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN owner_epoch BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN evaluated_at DATETIME(6) NULL,
  ADD COLUMN waiver_evidence TEXT NULL,
  ADD COLUMN waiver_reason VARCHAR(512) NULL,
  ADD COLUMN waiver_actor VARCHAR(128) NULL,
  ADD COLUMN waived_at DATETIME(6) NULL,
  ADD COLUMN waiver_kind_version INT NULL;

-- One row per task: the only generation counter (allocated inside the
-- health write transaction) and the fence last written.
CREATE TABLE task_health (
  task_id VARCHAR(64) NOT NULL PRIMARY KEY,
  status VARCHAR(16) NOT NULL,
  reasons JSON NULL,
  evaluated_at DATETIME(6) NOT NULL,
  generation BIGINT NOT NULL DEFAULT 0,
  owner VARCHAR(128) NOT NULL DEFAULT '',
  owner_epoch BIGINT NOT NULL DEFAULT 0,
  fence_mode VARCHAR(16) NOT NULL DEFAULT '',
  fence_instance VARCHAR(64) NOT NULL DEFAULT '',
  fence_epoch BIGINT NOT NULL DEFAULT 0,
  fence_node VARCHAR(128) NOT NULL DEFAULT '',
  chain_owner VARCHAR(128) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- The transition log and notification outbox. A replay is recognised by
-- content (transition_digest), never by generation.
CREATE TABLE finding_transitions (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL,
  kind VARCHAR(64) NOT NULL,
  subject VARCHAR(255) NOT NULL DEFAULT '',
  op VARCHAR(16) NOT NULL,
  reason VARCHAR(32) NOT NULL DEFAULT 'EVAL',
  generation BIGINT NOT NULL,
  kind_version INT NOT NULL DEFAULT 1,
  fact_at DATETIME(6) NOT NULL,
  severity VARCHAR(16) NOT NULL,
  detail TEXT NULL,
  at DATETIME(6) NOT NULL,
  transition_digest CHAR(64) NOT NULL,
  UNIQUE KEY uk_ft_digest (task_id, transition_digest),
  INDEX idx_ft_key (task_id, kind, subject),
  INDEX idx_ft_task_gen (task_id, generation),
  INDEX idx_ft_at (at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE notify_cursor (
  id TINYINT NOT NULL PRIMARY KEY,
  last_id BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE rehash_cursor (
  id TINYINT NOT NULL PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL,
  segment VARCHAR(255) NOT NULL,
  updated_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE task_mutes (
  task_id VARCHAR(64) NOT NULL,
  kind VARCHAR(64) NOT NULL,
  subject VARCHAR(255) NOT NULL DEFAULT '',
  reason VARCHAR(512) NOT NULL,
  actor VARCHAR(128) NOT NULL,
  started_at DATETIME(6) NOT NULL,
  until DATETIME(6) NOT NULL,
  ended_at DATETIME(6) NULL,
  end_reason VARCHAR(16) NULL,
  PRIMARY KEY (task_id, kind, subject, started_at),
  INDEX idx_mutes_active (ended_at, until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
