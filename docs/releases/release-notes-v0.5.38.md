# Binlog Server v0.5.38

Release date: 2026-10-05

A `PUT /api/tasks/{id}` on a task in `RUNNING`, `STARTING`, `LEASE_DEGRADED`, or `RETRY_BACKOFF` that changes source, start, storage, or `cluster_key` returns HTTP 400. The body is plain text `stop the task before changing source, start, storage, or cluster_key: state <STATE>`. `<STATE>` is that task's state. The saved row stays as it was, so `GET /api/tasks/{id}` still shows the source, start, storage, and `cluster_key` the dump is using. Before this, the API saved the new values and the running dump kept the copy taken at start, so GET did not match what was in effect. A PUT that only changes the name still succeeds. To change source, start, storage, or `cluster_key`: Stop, PUT, Start. The task resumes from its checkpoint with the new settings. Tracked as #179 (PR #208). Checked on MySQL 8.0.46, built from source, at `4477d19e`.

## Highlights

- The states that reject the change are `RUNNING`, `STARTING`, `LEASE_DEGRADED`, and `RETRY_BACKOFF`. `RETRY_BACKOFF` is in that list because the next attempt uses the copy this process already holds. It does not read the row a PUT just wrote.
- A change is a difference in source, start, storage, or `cluster_key`. Source includes host, port, user, password, flavor, `server_id`, and `semi_sync`. Start includes mode, file, position, and `gtid_set`. Storage includes `dir`, `retention_days`, `local_retention_days`, and `bucket_retention_days`. An omitted field keeps the stored value. An empty `source.password` keeps the stored password. Sending the same source, start, storage, and `cluster_key` again is accepted.
- A PUT that changes only `name` returns HTTP 200 in those states. The dump keeps running.
- `CREATED`, `STOPPING`, `STOPPED`, and `FAILED` still accept a PUT that changes source, start, storage, or `cluster_key`. A password written while the row is `STOPPING` is the password the next Start uses.
- HTTP 400 leaves the stored password, retention, and the other locked fields unchanged.
- Stop, wait until the row is `STOPPED`, PUT, then `POST /api/tasks/{id}/start`. Start reads the updated row and resumes from the checkpoint.
- A task in `RETRY_BACKOFF` with a wrong host or port used to take the corrected host or port from an in-place PUT. That PUT now returns HTTP 400 `stop the task before changing source, start, storage, or cluster_key: state RETRY_BACKOFF`. Stop, PUT the host or port, then Start.
- Listen address, upload endpoint, lease TTL, and encryption key are process settings. They are not fields on this PUT. Changing them still means restarting the process.

## Upgrade Notes

Rolling upgrade of the binaries from v0.5.37 is fine. This release has no schema migration. The metadata schema stays at version 2, the version introduced in v0.5.34. When `schema_migrations` is already version 2, `./migrate up` is not required.

A metadata database still on schema 1 must be migrated before this binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key.

## Known behavior

These were already present before this release. This build was checked on MySQL 8.0.46, built from source, at `4477d19e`. This release does not change them.

- Stop, Start, lease renewal, and progress updates for every task on one process share one lock with event reads and event inserts. A slow metadata write, or the Console reading events, holds the others. Opening the next binlog reads that task's whole file catalog on the dump path. Taking over a large uploaded segment reads the whole object into memory on that worker. Tracked as #177.
- After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0. `size_bytes` is the file size. The row can still be `UPLOADED` with checksum `match`. The segment stays in the files list, and download and replay still return it. This is the same behavior already noted for v0.5.34, v0.5.35, v0.5.36, and v0.5.37. Tracked as #189.
- After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. After a later Stop that reaches `STOPPED`, one Binlog Dump thread may still be connected and needs a manual `KILL` on the source. An ordinary Stop from the control plane, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. This is the same behavior already noted for v0.5.35, v0.5.36, and v0.5.37. Tracked as #193.
- On all-in-one and cluster roles, the `SOURCE_UNREACHABLE` cap of ten never fires. The claim loop, about every 2 seconds, starts an owned `RETRY_BACKOFF` task again and resets the consecutive-failure count, so the task stays in `RETRY_BACKOFF`. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:`. Tracked as #203.
- A source that goes down while the dump is already open can leave the task showing `RUNNING`. The lease keeps renewing. The dump library logs `retry sync err, wait 1s`. The scheduler does not see that error, so the task does not enter `RETRY_BACKOFF` and does not count `SOURCE_UNREACHABLE`. When the source comes back, the task continues and the position lines up. Tracked as #204.
- A GTID-mode start, and a file and position resume that opens `StartSyncGTID` after MySQL 1236, writes a manual Rotate with no CRC. `mysqlbinlog --verify-binlog-checksum` reports `Event crc check failed!` on that event. Without that flag the file reads. The copied events themselves are intact. A GTID start also leaves a stray `task-N.binlog`. The catalog records that file `SEALED` and uploads it. Tracked as #205.
- With no upload configured, age retention deletes the local sealed file and leaves the catalog row `SEALED` / `LOCAL_ONLY` with the old `file_path`. `GET /api/tasks/{id}/files` still lists that name, and the response has no `location` field. `GET /api/tasks/{id}/files/{name}` returns 404. The files list and the disk do not match. This was seen on the `4477d19e` dogfood. The retention code in this release is the same as before. Tracked as #209.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.38.zh-CN.md
