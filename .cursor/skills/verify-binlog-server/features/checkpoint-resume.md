# Checkpoint resume

After binlog has been flushed locally, the operator can read the saved file and position, stop the task, start it again, and see replication continue from that checkpoint instead of the original start mode.

## Sub-features

- `checkpoint-read` returns `file`, `pos`, and optional `gtid_set` from `GET /api/tasks/<id>/checkpoint`.
- `checkpoint-drawer` shows the same position in `task-drawer-checkpoint`.
- `checkpoint-resume` after stop and start keeps the saved `file` and does not jump back to the create-time start.
- `checkpoint-absent` returns 404 `checkpoint not found` before any position has been saved.

## How to get to it (user POV)

- `GET /api/tasks/<id>/checkpoint`.
- Open `/ui/`, choose `view-nav-tasks`, open `task-detail-trigger-<id>`, and read `task-drawer-checkpoint`.
- Stop with `task-action-stop` or `POST /api/tasks/<id>/stop`, then start again with `task-action-start` or `POST /api/tasks/<id>/start`.

## Driving it with control-binlog-server

Preconditions:

- Doctor has passed for this run.
- `BINLOG_VERIFY_META_DSN` is set for this launch. Without a metadata MySQL store the server has no checkpoint reader, so `GET /api/tasks/<id>/checkpoint` always returns 404 `checkpoint not found` even while replication is RUNNING.
- A MySQL or MariaDB source with `log_bin` enabled accepts the task user for replication. Its host:port must not be the metadata endpoint (create returns 400 `INVALID_REQUEST` when they match).
- The task was created with `start.mode` of `LATEST`, `FILE_POS`, or `GTID`, then started, and has reached `RUNNING` long enough for a checkpoint to exist.

- **Read checkpoint.** Fetch the saved position. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl GET /api/tasks/<id>/checkpoint -o "$BINLOG_VERIFY_EVIDENCE/checkpoint-resume/before.json" -w '%{http_code}'`. Status is `200`. `before.json` has non-empty `file` and `pos` greater than 0.
- **Drawer.** Open the task drawer. `task-drawer-checkpoint` shows the same file and position as `before.json`.
- **Resume.** Stop, then start the same task. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl POST /api/tasks/<id>/stop -w '%{http_code}'` and, once state is `STOPPED`, `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl POST /api/tasks/<id>/start -w '%{http_code}'`. Both statuses are `204`.
- **Read again.** Fetch the checkpoint after the new start. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl GET /api/tasks/<id>/checkpoint -o "$BINLOG_VERIFY_EVIDENCE/checkpoint-resume/after.json"`. `after.json` `file` matches `before.json` `file`, and `pos` is greater than or equal to the saved `pos`. It is not the create-time `FILE_POS` or an empty `LATEST` position from before the first flush.
- **Absent.** On a task that has never flushed, run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl GET /api/tasks/<id>/checkpoint -w '%{http_code}'`. Status is `404` and the body says `checkpoint not found`.
- **Proof.** Save `before.json` and `after.json` under `$BINLOG_VERIFY_EVIDENCE/checkpoint-resume/`.

## Gotchas

- A 404 checkpoint means there is no stored checkpoint and no local open segment with a complete event. Without meta, a segment that has a complete event returns that source file and end log_pos. That is the position the next Start uses. A directory of non-binlog bytes is still 404.
- Resume proof needs a source that actually accepts the dump. A closed port never writes `file` and `pos`.
- A valid checkpoint overrides the create-time mode (`LATEST`, `FILE_POS`, or `GTID`) on the next start when the task directory has no complete open-segment event. A local complete event overrides that checkpoint's file and pos. The task `start` object stays the configured identity. Comparing only `start.mode` misses the resume file:pos, and misses `gtid_set` when the stored checkpoint matches that file and pos.
- Cluster takeover (epoch greater than 1) continues in the catalog `file_path` directory when this worker can read it. An unreadable open segment fails Start with `SEGMENT_NOT_ON_WORKER` and does not rebuild from position 4. A checkpoint already inside a sealed `UPLOADED` object resumes from that object. A single-process standalone run does not take that path.
- `GET /api/tasks/<id>/checkpoint` is GET. POST returns 405.
