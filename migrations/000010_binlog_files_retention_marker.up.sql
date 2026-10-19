-- ADR 0009 phase 2 P2-5.4 (#341): local retention deleted the local file
-- and left the uploaded object and the catalog row. Additive and optional
-- like 000008: the floor stays 6. A process on schema 6-9 skips the column.
ALTER TABLE binlog_files
  ADD COLUMN retention_removed_at DATETIME(6) NULL;
