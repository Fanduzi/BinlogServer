-- Drop the legacy unique key. A segment is (task_id, source_file, epoch),
-- which is uk_task_source_epoch. file_name stays as a column.
-- This ALTER does not delete rows.
ALTER TABLE binlog_files
  DROP INDEX uk_task_file_epoch;
