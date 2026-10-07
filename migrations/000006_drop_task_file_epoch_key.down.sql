-- Restore uk_task_file_epoch. Do not delete binlog_files rows.
-- file_name stays. uk_task_source_epoch stays.
-- This fails if two rows already share (task_id, file_name, epoch).
-- Do not delete rows to make it pass. The writer keeps file_name equal to source_file.
ALTER TABLE binlog_files
  ADD UNIQUE KEY uk_task_file_epoch (task_id, file_name, epoch);
