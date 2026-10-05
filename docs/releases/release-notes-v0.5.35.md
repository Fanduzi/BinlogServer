# Binlog Server v0.5.35

Release date: 2026-10-05

On a control-plane plus workers deployment, Stop from the API process used to write the task `STOPPED` and clear the owner while the worker kept the dump and the lease. The Console showed Stopped. The source still had a Binlog Dump thread. This release sends that Stop to the worker that holds the lease. Tracked as #174.

## Highlights

- When this process is not dumping and another worker owns the lease, Stop writes `STOPPING` and keeps the owner, the epoch, and the source config. `POST /api/tasks/{id}/stop` still returns HTTP 204. The Console shows Stopping.
- The worker claim loop runs about every 2 seconds. It cancels a live dump whose row is `STOPPING` or `STOPPED`. When that dump exits, the worker writes `STOPPED` and releases the lease.
- A store sync and `GetTask` on that worker also cancel the dump. Those reads leave the in-memory owner and epoch in place, so the lease can be released when the dump exits.
- The final `STOPPED` row keeps the latest source config stored for the task. A source password changed while the row is `STOPPING` is the password the next Start uses.
- A `STOPPING` row whose lease has expired is written `STOPPED`. That claim does not start a dump.
- All-in-one and standalone are unchanged. Stop cancels the dump in this process. The row becomes `STOPPED` when that dump exits.

## Upgrade Notes

Rolling upgrade of the binaries from v0.5.34 is fine. This release has no schema migration. The metadata schema stays at version 2, the version introduced in v0.5.34. When `schema_migrations` is already version 2, `./migrate up` is not required.

A metadata database still on schema 1 must be migrated before this binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer.

On a control-plane plus workers deployment, HTTP 204 means Stop was accepted. The row is `STOPPING`, the owner and epoch are still set, and the Console shows Stopping. The Binlog Dump thread stays on the source until the worker that holds the lease cancels it and the row becomes `STOPPED` with the owner cleared. Treat the stop as finished when the row is `STOPPED`. A Start while the row is still `STOPPING` returns HTTP 400, body `cannot start from state STOPPING`.

No new config key.

## Known behavior

- After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0. `size_bytes` is the file size. The row can still be `UPLOADED` with checksum `match`. The segment stays in the files list, and download and replay still return it. This is the same behavior already noted for v0.5.34. Tracked as #189.
- After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. After a later Stop that reaches `STOPPED`, one Binlog Dump thread may still be connected and needs a manual `KILL` on the source. An ordinary Stop from the control plane, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. Tracked as #193.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.35.zh-CN.md
