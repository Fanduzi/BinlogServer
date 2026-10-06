-- Add-column-only. Empty string means no Binlog Dump is waiting for KILL.
-- The default lets a binary that does not list this column keep inserting.
-- minRequiredSchemaVersion stays 3: a database that has not run this migration still starts.
ALTER TABLE backup_tasks
  ADD COLUMN pending_dump_cleanup VARCHAR(512) NOT NULL DEFAULT '';
