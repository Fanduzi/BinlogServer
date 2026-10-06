-- Drop only the key and nullability added by 000005. Do not delete binlog_files rows.
-- start_pos and end_pos return to NOT NULL. This fails if a later writer stored NULL.
-- Do not fill those NULLs with 0. 000002's down deletes rows; do not copy that.
ALTER TABLE binlog_files
  DROP INDEX uk_task_source_epoch,
  MODIFY COLUMN source_file VARCHAR(255) NULL,
  MODIFY COLUMN start_pos BIGINT UNSIGNED NOT NULL,
  MODIFY COLUMN end_pos BIGINT UNSIGNED NOT NULL;
