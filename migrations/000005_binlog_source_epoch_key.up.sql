-- Segment identity is (task_id, source_file, epoch).
-- Backfill only a missing source name. Do not UPDATE start_pos or end_pos.
-- uk_task_file_epoch stays so a v0.5.49 process can still start after this migration.
-- The new unique key is added after the backfill, so empty source_file values
-- do not collide. This ALTER does not delete rows.
UPDATE binlog_files
SET source_file = file_name
WHERE source_file IS NULL OR source_file = '';

ALTER TABLE binlog_files
  MODIFY COLUMN source_file VARCHAR(255) NOT NULL,
  ADD UNIQUE KEY uk_task_source_epoch (task_id, source_file, epoch),
  MODIFY COLUMN start_pos BIGINT UNSIGNED NULL,
  MODIFY COLUMN end_pos BIGINT UNSIGNED NULL;
