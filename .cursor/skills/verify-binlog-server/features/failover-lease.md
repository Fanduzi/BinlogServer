# Dual-worker lease takeover

One control plane and two workers share a single meta MySQL. After worker A dies and its lease expires, worker B claims the same task through `ClaimRunnableTasks` / `ClaimExpiredTasks`. Meta stays `RUNNING`, or passes through `LEASE_DEGRADED` / `STARTING` and returns to `RUNNING`. Binlog bytes keep growing on B's data directory. That path does not write `STOPPED`.

## Sub-features

- `failover-claim` moves `owner_worker_id` to worker B after A's lease expires.
- `failover-no-stop` keeps `TASK_STOPPED` and `TASK_STOPPING` out of the task events on that path.
- `failover-files` continues the checkpoint file sequence on the same task id, and B's task directory grows.
- `failover-no-steal` leaves the owner on A while A's lease is still valid.
- `failover-no-stomp` keeps A's task out of `STOPPED` when worker C starts during a valid lease.
- `failover-backoff` lets B claim a `RETRY_BACKOFF` task after A's lease expires.
- `failover-failed-release` clears the owner on `FAILED` so a later start can be acquired by B.
- `failover-pause` lets B claim after A is paused with `SIGSTOP` and the lease expires.
- `failover-console` shows B in `task-drawer-worker`.
- `failover-cleanup` stops A, B, and meta and leaves their listen ports free.

## How to get to it (user POV)

- Run `scripts/failover-dogfood.sh`. It starts meta MySQL, a source MySQL on another port, one control plane, and workers A and B.
- Drive the control plane with `control-binlog-server.sh` `launch`, `doctor`, and `curl`.
- Open `/ui/`, choose `view-nav-tasks`, open `task-detail-trigger-<id>`, and read `task-drawer-worker`.
- `GET /api/tasks/<id>`, `GET /api/tasks/<id>/checkpoint`, `GET /api/tasks/<id>/events`, and `GET /api/tasks/<id>/runs`.

## Driving it with control-binlog-server

Preconditions:

- `BINLOG_SERVER_BIN` is a released `binlog-server`, and `migrate` sits next to it. `BINLOG_VERIFY_MIGRATIONS` defaults to this repo's `migrations/`.
- `mysqld` and `mysql` are on `PATH` (including `/usr/sbin`).
- Doctor has passed for the control plane this run started.
- The source host:port is not the meta host:port.
- `lease_ttl_sec` is 15 unless `FAILOVER_LEASE_TTL` says otherwise. `lease_renew_interval_sec` is 5 and `lease_grace_sec` is 30.
- Evidence for a scenario is `$FAILOVER_EVIDENCE/<scenario>/` (default `/tmp/binlog-failover-evidence-<run-id>/<scenario>/`).

- **Bring the cluster up and prove the happy path.** Run `BINLOG_SERVER_BIN=/absolute/path/to/binlog-server scripts/failover-dogfood.sh run happy`. Exit is 0. `running-a.json` has `owner_worker_id` `worker-a` and `state` `RUNNING`. `after-task.json` has `owner_worker_id` `worker-b`, `state` `RUNNING`, and a larger `epoch`. `events.json` contains `TASK_LEASE_TAKEOVER` and does not contain `TASK_STOPPED` or `TASK_STOPPING`. `states.tsv` has no `STOPPED` row. `after-checkpoint.json` `file` is the same binlog file or a later one, and `files-b.txt` shows bytes on B's data directory.
- **Doctor and readbacks use the helper.** The script exports `BINLOG_VERIFY_LISTEN`, `BINLOG_VERIFY_BASE`, `BINLOG_VERIFY_EVIDENCE`, and `BINLOG_VERIFY_META_DSN`, writes `config.yaml` with `mode: cluster` and `cluster.role: control-plane`, then runs `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh launch` and `doctor`. Doctor prints `PASS`. Task reads are `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl GET /api/tasks/<id>`.
- **Do not steal a live lease.** Run `scripts/failover-dogfood.sh run no-steal`. Exit is 0. After worker B has been up for longer than `lease_ttl_sec`, `still-a.json` still has `owner_worker_id` `worker-a`.
- **Do not stomp on startup.** Run `scripts/failover-dogfood.sh run no-stomp`. Exit is 0. `still-a.json` is `RUNNING` with `owner_worker_id` `worker-a`, and `events.json` has no `TASK_STOPPED`.
- **Claim during backoff.** Run `scripts/failover-dogfood.sh run backoff`. Exit is 0. `backoff-a.json` is `RETRY_BACKOFF` owned by `worker-a`. `claimed-b.json` is owned by `worker-b` and is not `STOPPED`.
- **Release on failure.** Run `scripts/failover-dogfood.sh run failed`. Exit is 0. `failed.json` is `FAILED` with an empty `owner_worker_id`. `runs.json` includes `worker_id` `worker-b` after the next start.
- **Pause A.** Run `scripts/failover-dogfood.sh run pause`. Exit is 0. `after-task.json` is `RUNNING` owned by `worker-b`. The first `worker-b` row in `states.tsv` is not earlier than the lease expiry recorded before the pause. A later row, at least `lease_ttl_sec + lease_grace_sec` after the pause, is still `worker-b` and not `STOPPED`.
- **Console owner.** With `FAILOVER_CONSOLE_SHOT` set to a png path, `run happy` opens `/ui/`, clicks `view-nav-tasks` and `task-detail-trigger-<id>`, and requires `task-drawer-worker` to read `worker-b`.
- **Cleanup.** Run `scripts/failover-dogfood.sh run cleanup`. Exit is 0. `listeners-after.txt` does not contain the control-plane, worker A, worker B, or meta ports. The evidence directory still exists.
- **Trunk and perf.** `FAILOVER_TRUNK_BIN` points at the v0.5.5 binary. `scripts/failover-dogfood.sh live` runs the scenarios above plus the same happy path on the trunk binary. `scripts/failover-dogfood.sh perf` prints five interleaved kill-to-growing-file samples and exits 0 when the head median is less than or equal to the trunk median plus 5 seconds.
- **Proof.** `result.txt` in the scenario directory starts with `PASS`. `doctor.txt` contains `PASS`.

## Gotchas

- The happy path kills A with `SIGKILL`. A graceful stop is a different entry and is allowed to write `STOPPED`.
- Worker `/healthz` returns an empty 200. `control-binlog-server.sh launch` requires the body `ok`, so only the control plane goes through `launch` / `doctor`. The script starts workers itself and still uses the helper for every control-plane HTTP call.
- This map is one control plane plus workers. It does not start a second control plane.
- `lease_grace_sec` is how long the owner keeps the task after its own renew calls fail. Another worker may claim as soon as `lease_expire_at` has passed. The pause entry waits out grace after that claim and checks the owner is still B.
- Start worker B only after A is `RUNNING` when the entry needs A to own the task. Two live workers both claim unowned `STARTING`.
- A 404 checkpoint means the dump has not flushed yet. Happy-path proof waits for a checkpoint before killing A.
- `GET /api/tasks/<id>/events` is served by the control plane from meta. Events written only inside a dead worker's memory are not the proof.
