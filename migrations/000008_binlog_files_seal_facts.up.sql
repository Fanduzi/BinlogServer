-- ADR 0009 phase 1: what a seal knows about the sealed bytes. Additive and
-- optional like 000007: the floor stays 6, a process on schema 6 or 7 skips
-- these columns, and older rows keep NULL.
ALTER TABLE binlog_files
  ADD COLUMN first_event_at DATETIME(6) NULL,
  ADD COLUMN last_event_at DATETIME(6) NULL,
  ADD COLUMN sha256 CHAR(64) NULL;
