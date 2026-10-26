-- sqlc catalog only. Migration 000011 renames the live column.
ALTER TABLE worker_heartbeats CHANGE COLUMN version build_version VARCHAR(64) NOT NULL;
