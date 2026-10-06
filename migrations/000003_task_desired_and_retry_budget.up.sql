-- ADR 0005 step 2. Defaults let an older binary insert through upsertTaskSQL,
-- which does not list these columns. Existing state values are not rewritten.
-- RUNNING, STARTING, RETRY_BACKOFF, LEASE_DEGRADED, REBUILDING_FILE -> RUN.
-- CREATED, STOPPING, STOPPED, FAILED, and any other state -> STOP.
-- Revisions and counters are 0. FAILED stays failed_spec_revision 0.
ALTER TABLE backup_tasks
  ADD COLUMN desired_run VARCHAR(8) NOT NULL DEFAULT 'STOP',
  ADD COLUMN spec_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN applied_spec_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN failed_spec_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN retry_attempt BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN consecutive_source_failures BIGINT NOT NULL DEFAULT 0;

UPDATE backup_tasks
SET
  desired_run = CASE
    WHEN state IN ('RUNNING', 'STARTING', 'RETRY_BACKOFF', 'LEASE_DEGRADED', 'REBUILDING_FILE') THEN 'RUN'
    ELSE 'STOP'
  END,
  spec_revision = 0,
  applied_spec_revision = 0,
  failed_spec_revision = 0,
  retry_attempt = 0,
  consecutive_source_failures = 0;
