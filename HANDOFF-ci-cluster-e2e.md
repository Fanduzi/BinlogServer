# Handoff: run the cluster e2e jobs on every PR (#424)

Why: the #421/#424 bug (independent `data_dir` per node, SIGSTOP takeover,
new owner holds the lease and writes nothing, health stays ok) only shows
up with several nodes. The cluster e2e jobs (`e2e-cluster-roles`,
`e2e-worker-crash-recovery`, `e2e-meta-failover`) run only on
`workflow_dispatch` (meta-failover also on push to main), so a PR can
merge without them. The in-process coverage (`go test ./...`,
`internal/tasks/p26d_failure_matrix_test.go`,
`internal/tasks/p26d_fence_order_test.go`) already runs per PR in
`unit-and-e2e`; this handoff adds the real multi-process jobs.

The agent's token has no `workflow` scope, so this has to be applied by
someone whose token or UI session can edit `.github/workflows/`.

## Exact diff (`.github/workflows/e2e.yml`, against head of feat/p2-6c-unowned-health)

The new `if:` matches the per-PR jobs already in the file
(`e2e-retry-upload`, `e2e-split-retention`, ...): run on push and
pull_request, and on `workflow_dispatch` only for the job's own profile.

```diff
--- a/.github/workflows/e2e.yml
+++ b/.github/workflows/e2e.yml
@@ -103,7 +103,7 @@
         run: ./scripts/e2e/down.sh
 
   e2e-cluster-roles:
-    if: ${{ github.event_name == 'workflow_dispatch' && github.event.inputs.e2e_profile == 'cluster-roles' }}
+    if: ${{ github.event_name != 'workflow_dispatch' || github.event.inputs.e2e_profile == 'cluster-roles' }}
     runs-on: ubuntu-latest
     timeout-minutes: 40
 
@@ -164,7 +164,7 @@
         run: ./scripts/e2e/down.sh
 
   e2e-worker-crash-recovery:
-    if: ${{ github.event_name == 'workflow_dispatch' && github.event.inputs.e2e_profile == 'worker-crash-recovery' }}
+    if: ${{ github.event_name != 'workflow_dispatch' || github.event.inputs.e2e_profile == 'worker-crash-recovery' }}
     runs-on: ubuntu-latest
     timeout-minutes: 40
 
@@ -194,7 +194,7 @@
         run: ./scripts/e2e/down.sh
 
   e2e-meta-failover:
-    if: ${{ (github.event_name == 'push' && github.ref == 'refs/heads/main') || (github.event_name == 'workflow_dispatch' && github.event.inputs.e2e_profile == 'meta-failover') }}
+    if: ${{ github.event_name != 'workflow_dispatch' || github.event.inputs.e2e_profile == 'meta-failover' }}
     runs-on: ubuntu-latest
     timeout-minutes: 40
 
```

Note: `e2e-cluster-roles` and `e2e-worker-crash-recovery` are listed
in the `unit-and-e2e` job's dispatch-exclusion `if:` (line 46). That
line only matters for `workflow_dispatch`, so it does not need to change.

## Commands

```bash
git clone https://github.com/Fanduzi/BinlogServer.git && cd BinlogServer
git checkout feat/p2-6c-unowned-health      # or a follow-up branch off main after #424 merges
cat > /tmp/e2e.diff <<'DIFF'
(paste the diff above)
DIFF
git apply --check /tmp/e2e.diff && git apply /tmp/e2e.diff
git diff --stat                               # exactly 1 file, 3 insertions, 3 deletions
git commit -am "ci: run cluster e2e jobs on every PR (#424)"
git push                                      # needs a token with `workflow` scope
```

Or in the GitHub web editor: edit `.github/workflows/e2e.yml` and replace
the three `if:` lines as shown.

Optional local pre-check of the three scenarios (needs Docker):

```bash
./scripts/e2e/run-suite.sh --scenarios smoke-cluster-roles
./scripts/e2e/run-suite.sh --scenarios smoke-worker-crash-recovery
./scripts/e2e/run-suite.sh --scenarios smoke-meta-failover
```

(Use the same `E2E_DIR`/`E2E_SERVER_LOG` environment variables the job
`run:` steps set if you want identical paths.)

## Completion criteria

1. On the next `pull_request` run of workflow `ci`, the checks
   `e2e-cluster-roles`, `e2e-worker-crash-recovery` and `e2e-meta-failover`
   show as run (not "skipped") and pass.
2. A `workflow_dispatch` run with profile `quick` still skips these three
   jobs. A run with profile `cluster-roles` runs only `e2e-cluster-roles`
   among them, as before.
3. Optional: mark the three checks as required in branch protection for `main`.
4. Follow-up, not in this diff: none of the existing scenarios runs
   independent per-node `data_dir` with a SIGSTOP takeover (the QA sc1
   shape). A `smoke-takeover-independent-datadir` scenario that asserts
   `/api/health` degraded (`TASK_FAILED` / `SEGMENT_NOT_ON_WORKER`, or
   `TASK_NO_PROGRESS` within `health.no_progress_after_sec`) would cover
   it end to end. It would be added under the same per-PR `if:`.
