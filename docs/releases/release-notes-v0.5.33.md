# Binlog Server v0.5.33

Release date: 2026-10-05

On Binlog Server `v0.5.33`, a sealed binlog still reaches the bucket when the process dies during the upload, or in the moment after the file is renamed and before that upload is recorded. Up to `v0.5.32`, that file stayed `LOCAL_ONLY`, or it stayed outside the `UPLOAD_FAILED` set. The background retry left it alone. A lease takeover could fail with `SEGMENT_NOT_ON_WORKER`. The upload right after seal had no deadline, so a hung object store held the dump loop, and this worker kept the lease, until the process was killed. The published v0.5.32 package still does that. No new upload state. No new config key. No schema migration.

## Highlights

- With object storage configured, the catalog row is written before the PUT. The row is `state` `SEALED`, `upload_state` `UPLOAD_FAILED`, `upload_error` `upload pending`, and the object key is set. The checkpoint for the next binlog file is written before that PUT.
- The PUT is bounded by `meta.timeout.upload_sec`. The default is 30 seconds. This is the same deadline the background retry already uses. When the deadline fires, the row stays `UPLOAD_FAILED` and replication opens the next file. The dump waits at most that long, then continues.
- On the next start, including a lease takeover, a sealed file that is on this worker's disk and never became `UPLOADED` is recorded as `UPLOAD_FAILED`, with its object key and `upload_error` `upload pending`. When the checkpoint is still on that sealed file, and the last complete event is the rotate that names the next file, the checkpoint moves to that next file at the position the rotate names. A rotate position of 0 is stored as 4. Replication continues on the next file. The worker that pulls binlog runs the background retry, which uploads the file and checks the checksum on its own. A control-plane-only process does not run that loop. `GET /api/tasks/{id}/files` shows `upload_state` `UPLOAD_FAILED` and `location` `local` while the file is waiting, then `UPLOADED`, checksum `match`, and `location` `both` while the file is still on disk. A sealed file this worker cannot read still fails takeover with `SEGMENT_NOT_ON_WORKER`.
- With object storage and a catalog, retention keeps that local file and its catalog row until the checksum is `match`. Once the file is older than the local retention window, that pass leaves it in place and writes one `RETENTION_SKIPPED_NOT_UPLOADED` event. Replication keeps running.
- A task with no object storage seals as `LOCAL_ONLY` with an empty object key. The file stays on this machine. `POST /api/tasks/{id}/files/retry-upload` returns HTTP 400, body `upload retry is not available`. Retention when a catalog exists is unchanged for that row. A lease takeover of a sealed file that is not `UPLOADED` can still fail with `SEGMENT_NOT_ON_WORKER`.
- No new upload state. No new config key. No schema migration. Migrations are still `000001_init_schema` only. The Console already shows `upload_state`, `upload_error`, and `checksum`. The existing retry button appears for `UPLOAD_FAILED`.

## Known behavior

- After `meta.timeout.upload_sec` fires, `upload_error` still reads `upload pending`. The catalog write that would name the timeout uses that same cancelled deadline, so the row keeps the text written before the PUT.
- For a file enrolled after a crash, `sealed_at` is the time this process opened the file. The open row has no separate seal time. The catalog writes `sealed_at` with the same value as `created_at`, and enrollment keeps that time.
- While the post-seal PUT is hanging, this process writes no new local binlog events. Writing resumes when the timeout fires, on the next file. This worker keeps the lease during that wait.
- While that PUT is hanging, the background retry may PUT the same object key again. The bytes are the sealed file, so the second PUT is the same bytes.
- The next run with object storage configured uploads a sealed `LOCAL_ONLY` file that is still on disk. Up to `v0.5.32`, turning object storage on later left that row unuploaded. Only a file that is still on disk is uploaded.
- A checksum is still compared once, after the upload. An object that already matched, and is then changed in the bucket, is not compared again.

## Upgrade Notes

- No schema migration is required for `v0.5.33`. Migrations are still `000001_init_schema` only.
- No new state. No new config key. `meta.timeout.upload_sec` is the existing upload deadline. The default stays 30 seconds. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- On `v0.5.32` and earlier, a sealed file left by a crash during upload stays `LOCAL_ONLY` or outside the retry set until someone acts. After this upgrade, the next start with object storage records a sealed file that is still on disk and not yet `UPLOADED` as `UPLOAD_FAILED`, and the background retry uploads it. Check `GET /api/tasks/{id}/files`. `upload_error` `upload pending` is the text stored before the PUT, including after the upload deadline has already fired. The background retry uploads that file later.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.33.zh-CN.md
