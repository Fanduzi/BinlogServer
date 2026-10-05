ALTER TABLE binlog_files
  DROP INDEX uk_task_file,
  ADD UNIQUE KEY uk_task_file_epoch (task_id, file_name, epoch);
