-- v0.5.33 left every row at epoch 0. The path is the real epoch when it ends
-- in .open.eN or .sealed.eN. Rewrite that epoch before the unique key changes
-- so a later upsert updates this row instead of inserting a second one.
-- A plain sealed name has no epoch in the path and stays 0, which is the
-- historical object key.
UPDATE binlog_files
SET epoch = CAST(
  SUBSTRING_INDEX(SUBSTRING_INDEX(REPLACE(file_path, '\\', '/'), '/', -1), '.open.e', -1)
  AS UNSIGNED)
WHERE SUBSTRING_INDEX(REPLACE(file_path, '\\', '/'), '/', -1) REGEXP '\\.open\\.e[0-9]+$';

UPDATE binlog_files
SET epoch = CAST(
  SUBSTRING_INDEX(SUBSTRING_INDEX(REPLACE(file_path, '\\', '/'), '/', -1), '.sealed.e', -1)
  AS UNSIGNED)
WHERE SUBSTRING_INDEX(REPLACE(file_path, '\\', '/'), '/', -1) REGEXP '\\.sealed\\.e[0-9]+$';

ALTER TABLE binlog_files
  DROP INDEX uk_task_file,
  ADD UNIQUE KEY uk_task_file_epoch (task_id, file_name, epoch);
