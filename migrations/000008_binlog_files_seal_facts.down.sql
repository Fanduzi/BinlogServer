ALTER TABLE binlog_files
  DROP COLUMN sha256,
  DROP COLUMN last_event_at,
  DROP COLUMN first_event_at;
