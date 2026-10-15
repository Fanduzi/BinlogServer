# Binlog Server v0.5.68

Release date: 2026-10-16

v0.5.68 is a fix release. Task runners are now owned by `App.Run`, and shutdown is bounded and ordered (#401). No new migration and no new config key: `minRequiredSchemaVersion` stays 6 and the floor stays v0.5.51. Upgrading from v0.5.66 still follows the [v0.5.67 upgrade notes](release-notes-v0.5.67.md): configure the task key first, then adopt every leftover directory once.

## Upgrade Notes

- No migration, no config change, no API change. From v0.5.67, replace the binary and restart; standalone tasks resume from `.tasks.enc`.
- **SIGINT/SIGTERM shutdown is now bounded.** The process stops every task runner (the open segment is closed and the task's last state is written), then waits for background work, then writes the last findings. It normally exits within a few seconds. It is capped at 30 s and then exits with code **2**. After the first signal the default handlers are restored, so **a second signal exits at once**. Give the orchestrator a termination grace period of at least 30 s (for example Kubernetes `terminationGracePeriodSeconds`, whose default is 30).
- Shutdown is not an operator Stop: desired state stays RUN and the next start resumes the task (standalone from the task record, cluster through the lease claim).
- The v0.5.67 notes still apply.

## Fixes

- **#401: task runners no longer outlive `App.Run`.** Runners ran on `context.Background()`, so shutdown never cancelled them, and a resumed task could still write `data_dir` after `Run` returned. Every runner context now derives from `App.Run`'s context. Shutdown order:
  1. cancel;
  2. wait for runners (10 s);
  3. wait for background goroutines (5 s): HTTP graceful shutdown (5 s), evaluator, rehasher, source liveness probe, retry loops;
  4. retry an owed task-record save;
  5. end the findings writer last and wait for its final flush (5 s).

  Only then are the `data_dir` lock and the metadata store released. A Start after shutdown began returns `ErrShuttingDown` and writes nothing.
- This was the root cause of the failed first v0.5.67 release run: `TestApp_SecondStandaloneOnSameDataDirFailsToStart` flaked with "directory not empty". The fix is the ownership change, not a test retry; a new fs-write probe test asserts nothing writes `data_dir` after `Run` returns.

## Changes since v0.5.67

`git log --oneline --first-parent v0.5.67..038e6c9`, plus the merge of this notes PR, which v0.5.68 is tagged on:

| Commit | Change |
|---|---|
| `3c53e56` | PR #400: install docs point at v0.5.67; e2e scripts use `docker rm -v` |
| `038e6c9` | PR #401: `App.Run` owns task runners; bounded, ordered shutdown |

## QA summary

Tip QA 038e6c9 PASS (BinlogServerQA), 2026-10-16. `unit-and-e2e` and the other CI checks passed on `038e6c9`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.68 中文发布说明

> 对应版本：`v0.5.68`

发布日期：2026-10-16

v0.5.68 是一个修复版本：任务 runner 归 `App.Run` 所有，关停有界且有序（#401）。没有新迁移，没有新配置项，`minRequiredSchemaVersion` 仍是 6，地板仍是 v0.5.51。从 v0.5.66 升级仍按 [v0.5.67 发布说明](release-notes-v0.5.67.md) 的升级须知操作（先配置任务密钥，再把遗留目录各 adopt 一次）。

## 升级须知

- 没有新迁移，没有新配置项，API 不变。从 v0.5.67 升级直接换二进制重启即可；standalone 的任务按 `.tasks.enc` 自动恢复。
- **SIGINT/SIGTERM 关停现在有界：** 进程先停掉所有任务 runner（关好打开的段、落下最后一次状态），再等后台工作、再写出最后的 finding，正常在几秒内退出。最多 30 秒，超时以退出码 **2** 结束。收到第一个信号后恢复默认信号处理，**再发一次信号立即退出**。编排系统的终止宽限期（例如 Kubernetes `terminationGracePeriodSeconds`，默认 30s）建议不低于 30 秒。
- 关停不是操作员 Stop：任务的期望状态仍是 RUN，下次启动自动恢复（standalone 按任务记录，集群按租约认领）。
- v0.5.67 的升级须知仍然适用。

## 修复

- **#401：任务 runner 不再比 `App.Run` 活得久。** 以前 runner 跑在 `context.Background()` 上，进程关停时不会被取消，`Run` 返回后恢复的任务仍可能写 `data_dir`。现在每个 runner 的 context 都从 `App.Run` 的 context 派生；关停顺序为：取消 → 等 runner 退出（10s）→ 等后台 goroutine（HTTP 优雅关闭 5s、评估器、复核器、源存活探测、各重试循环，5s）→ 再试一次欠下的任务记录保存 → 最后结束 finding 写入器并等它最后一次 flush（5s）。之后才释放 `data_dir` 锁和元数据库连接。关停开始后的 Start 返回 `ErrShuttingDown`，不写任何状态。
- 这也是 v0.5.67 第一次发布流水线失败的根因：`TestApp_SecondStandaloneOnSameDataDirFailsToStart` 偶发 "directory not empty"。修复不是给测试加重试，而是修了所有权；新增落盘探针测试保证 `Run` 返回后不再写 `data_dir`。

## 自 v0.5.67 以来的变更

| 提交 | 变更 |
|---|---|
| `3c53e56` | PR #400：安装文档指向 v0.5.67；e2e 脚本用 `docker rm -v` |
| `038e6c9` | PR #401：`App.Run` 拥有任务 runner；有界、有序的关停 |

## QA 摘要

Tip QA 038e6c9 PASS（BinlogServerQA），2026-10-16。`unit-and-e2e` 与其余 CI 检查在 `038e6c9` 上通过。
