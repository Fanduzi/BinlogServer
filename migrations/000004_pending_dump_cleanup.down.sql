-- Drop only the column added by 000004. Do not delete backup_tasks rows.
ALTER TABLE backup_tasks
  DROP COLUMN pending_dump_cleanup;
