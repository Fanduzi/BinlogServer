# internal/swaggerdocs Module

## Files
- `docs.go`: 生成的 swagger/openapi 文档代码。
- `swagger.json` / `swagger.yaml`: 生成的 OpenAPI 规范文件。

## Exports
- swagger 文档元数据供 API 文档页面消费。`PUT /api/tasks/{id}` 说明：任务处于 `RUNNING`、`STARTING`、`LEASE_DEGRADED`、`RETRY_BACKOFF` 时，改 source、start、storage 或 `cluster_key` 是纯文本 400 `stop the task before changing source, start, storage, or cluster_key`。包含 `GET /api/tasks/{id}/checkpoint` 的续传 file/pos（匹配时含 gtid_set；epoch 大于 1 时可读的 `file_path` 用该目录最后一个完整事件，读不到未上传尾部时不回拨到位置 4）；`POST /api/tasks/batch` 的 1..100 envelope、逐项有序结果与结构化错误；`POST /api/tasks/{id}/adopt` 把 source 接到剩余磁盘目录的同一 id；`GET /api/tasks/{id}/replay` 返回每个源序号一条路径、与之对齐的 `locations`（`local` / `bucket` / `both`）和 `source.flavor` 对应的 binlog 客户端，带 `stop_datetime` 时文档说明 UTC 定点窗口，带 `stop_gtid` 时文档说明停在该 MySQL GTID 事件起始字节之前；`bucket` 表示该路径不在本进程磁盘上，下载和 `replay/archive` 仍读对象；`GET /api/tasks/{id}/replay/archive` 以 `application/x-tar` 下载同一窗口的 basename，空窗口是空 tar；`GET /api/tasks/{id}/files/{name}` 以 `application/octet-stream` 下载清单中的一个分段，本地文件优先，本地没有且封存行已 `UPLOADED` 时从已配置的对象存储读；`BinlogFile` 的 `start_pos` / `end_pos` 可为 null（未知终点不是 0），并包含 `OPEN/SEALED` state、`location` 和封存上传后的 `checksum`（`match` / `mismatch`；只有 `match` 留在 `UPLOADED`，`mismatch` 和未完成的校验是 `UPLOAD_FAILED`）；`Storage` 含可选的 `local_retention_days` 与 `bucket_retention_days`，省略时与 `retention_days` 相同；summary/dashboard/source 契约包含独立 `starting` 计数，`running` 仅代表 RUNNING；任务列表与 dashboard 暴露 state/host/port 过滤及 `total/limit/offset` 分页字段，limit 超过 500 返回 400；`GET /api/cluster/overview` 与 `GET /api/workers` 文档包含 500。overview 含 `single_process`：无心跳的本进程拉取时 `worker_count` 为 0。
- `/api/sources/lookup` 文档说明 localhost 与显式 loopback literal 共用 host identity，其他 host 按原文字面匹配。

## Generation Note

- 使用 `go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/binlog-server/main.go -o internal/swaggerdocs --parseInternal` 生成 `docs.go`、`swagger.json` 与 `swagger.yaml`。

## Dependencies
- Upstream: `internal/api` 注释与接口定义。
- Downstream: swagger handler。

## Update Rule
- API 注释生成流程或文档结构变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
