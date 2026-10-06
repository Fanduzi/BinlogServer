-- Drop only the columns added by 000003. Do not delete backup_tasks or binlog_files rows.
ALTER TABLE backup_tasks
  DROP COLUMN desired_run,
  DROP COLUMN spec_revision,
  DROP COLUMN applied_spec_revision,
  DROP COLUMN failed_spec_revision,
  DROP COLUMN retry_attempt,
  DROP COLUMN consecutive_source_failures;
