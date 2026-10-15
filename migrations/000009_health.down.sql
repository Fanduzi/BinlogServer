DROP TABLE IF EXISTS task_mutes;
DROP TABLE IF EXISTS rehash_cursor;
DROP TABLE IF EXISTS notify_cursor;
DROP TABLE IF EXISTS finding_transitions;
DROP TABLE IF EXISTS task_health;
ALTER TABLE task_findings
  DROP COLUMN waiver_kind_version,
  DROP COLUMN waived_at,
  DROP COLUMN waiver_actor,
  DROP COLUMN waiver_reason,
  DROP COLUMN waiver_evidence,
  DROP COLUMN evaluated_at,
  DROP COLUMN owner_epoch,
  DROP COLUMN generation,
  DROP COLUMN segments,
  DROP COLUMN evidence;
