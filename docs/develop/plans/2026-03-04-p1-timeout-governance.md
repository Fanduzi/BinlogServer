# P1 Internal Timeout Governance

## Scope

- 目标：治理 `internal/tasks` 与 `internal/meta` 的无界内部依赖调用，补齐超时边界。
- 非目标：不改 HTTP 入站超时语义，不改状态机流程、状态码和核心错误语义。

## Configuration Design

新增配置层：`meta.timeout.*`（单位：秒）。

- `meta.timeout.read_sec`：内部读调用（store/query/list/checkpoint）超时。
- `meta.timeout.write_sec`：内部写调用（upsert/delete/event append/schema check）超时。
- `meta.timeout.lease_sec`：lease acquire/renew/release 超时。
- `meta.timeout.upload_sec`：上传与上传补偿相关调用超时。

默认值：

- `read_sec: 3`
- `write_sec: 5`
- `lease_sec: 2`
- `upload_sec: 30`

说明：

- `http.control_plane.*` / `http.worker_health.*` 仍仅控制入站连接层。
- `meta.timeout.*` 仅用于进程内部依赖调用边界（store/lease/uploader）。

## Call-Site Inventory (Before)

### internal/tasks

| Category | File | Call |
|---|---|---|
| write | `scheduler.go` | `store.UpsertTask(context.Background(), ...)` |
| write | `scheduler.go` | `eventStore.AppendEvent(context.Background(), ...)` |
| read | `scheduler.go` | `store.ListTasks(context.Background())` |
| read | `scheduler_task_ops.go` | `store.ListTasks(context.Background())` |
| write | `scheduler_task_ops.go` | `store.DeleteTask(context.Background(), ...)` |
| lease | `scheduler_task_ops.go` | `leaseManager.Release(context.Background(), ...)` |
| read | `scheduler_observability.go` | `store.ListTasks(context.Background())` |
| read | `scheduler_observability.go` | `eventStore.ListEvents(context.Background(), ...)` |
| read | `scheduler_observability.go` | `fileStore.ListBinlogFiles(context.Background(), ...)` |
| read | `scheduler_observability.go` | `taskRunReader.ListTaskRuns(context.Background(), ...)` |
| read | `scheduler_observability.go` | `workerHeartbeatReader.ListWorkerHeartbeats(context.Background(), ...)` |
| upload | `scheduler_retry_upload.go` | `uploader.UploadFile(context.Background(), ...)` |
| write | `scheduler_retry_upload.go` | `fileStore.UpsertBinlogFile(context.Background(), ...)` |
| read | `scheduler_retry_upload.go` | `ListFailedUploadBinlogFiles/ListBinlogFiles(...context.Background...)` |
| read | `scheduler_retry_upload.go` | `CountUploadFailures(...context.Background...)` |
| read | `scheduler_retry_upload.go` | `ListUploadFailureReasons(...context.Background...)` |
| lease | `scheduler_lifecycle.go` | `leaseManager.Acquire(context.Background(), ...)` |
| read | `scheduler_lifecycle.go` | `store.ListTasks(context.Background())` |
| lease | `scheduler_lifecycle.go` | `leaseManager.Release(context.Background(), ...)` |
| lease | `scheduler_cluster_lease.go` | `leaseManager.Renew(context.Background(), ...)` |

### internal/meta

| Category | File | Call |
|---|---|---|
| write/schema | `mysql_store.go` | `ensureSchema(context.Background())` |

## Call-Site Inventory (After)

- 全部调用点改为带界限上下文：
  - read：`s.withReadTimeout(...)`
  - write：`s.withWriteTimeout(...)`
  - lease：`s.withLeaseTimeout(...)`
  - upload：`s.withUploadTimeout(...)`
- `internal/meta/mysql_store.go` 启动校验改为：
  - `context.WithTimeout(context.Background(), schemaTimeout)`
  - 构造函数支持 `NewMySQLTaskStoreWithSchemaTimeout(...)`

## Key Tests Added

- `internal/tasks/timeout_test.go`
  - `TestScheduler_GetTaskUsesReadTimeout`
  - `TestScheduler_StartTaskUsesLeaseTimeout`
  - `TestScheduler_RetryFailedUploadsUsesUploadTimeout`
- `internal/config/config_test.go`
  - 默认值覆盖 `meta.timeout.*` 正数校验
  - YAML 覆盖与 legacy YAML（缺失 `meta` 段）回退默认值校验

## Compatibility Notes

- `config.example.yaml` 已新增 `meta.timeout.*` 示例。
- 历史配置（无 `meta` 段）可启动：使用默认值，不崩溃。
