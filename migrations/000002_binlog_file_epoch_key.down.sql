DELETE newer FROM binlog_files AS newer
INNER JOIN binlog_files AS older
  ON newer.task_id = older.task_id
 AND newer.file_name = older.file_name
 AND newer.id > older.id;

ALTER TABLE binlog_files
  DROP INDEX uk_task_file_epoch,
  ADD UNIQUE KEY uk_task_file (task_id, file_name);
