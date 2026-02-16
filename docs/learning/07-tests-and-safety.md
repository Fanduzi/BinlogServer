# 第 7 节：测试与变更安全

## 目标

建立“先验证再结论”的开发习惯，让每次改动可控。

## 核心文件

- `internal/replication/upload_policy_test.go`
- `internal/api/server_test.go`
- `internal/meta/mysql_store_test.go`

## 一眼看测试分层

1. `replication` 测试：保障数据面关键语义（如上传失败最佳努力）。
2. `api` 测试：保障协议层行为（状态码、limit、生效字段、脱敏）。
3. `meta` 测试：保障持久化读写与排序语义。

## 验证基线

1. 后端全量测试：`go test ./...`
2. 前端构建验证：`npm --prefix frontend run build`
3. 关键行为测试：上传失败不打断拉流

你每次改动后至少要留下两类证据：

1. 单点行为证据（改动对应的测试）
2. 全局回归证据（全量测试/构建）

## 关键测试示例（项目已存在）

1. `TestFinalizeSealedFile_BestEffortOnUploadFailure`：  
验证上传失败时返回 `nil`，并把文件状态写为 `UPLOAD_FAILED`。
2. `TestFinalizeSealedFile_UploadSuccess`：  
验证成功时状态是 `UPLOADED` 且写入 `object_key`。
3. `server_test.go` 里有 `events/files/summary/limit` 的 API 行为测试。
4. `mysql_store_test.go` 覆盖 events/files 的读写与顺序。

## 建议流程

1. 先写/补一个最小测试。
2. 再做实现改动。
3. 运行全量验证。
4. 最后再宣称“完成”。

推荐你采用最小闭环：

1. 明确改动目标（一个句子）。
2. 新增或修改 1 个测试让它先失败。
3. 改实现到测试通过。
4. 跑 `go test ./...` + 前端 build。
5. 在变更说明里写清验证命令和结果。

## 常见误区

1. 只跑改动包测试，不跑全量。
2. 测试通过但没有覆盖负路径（如参数非法、依赖失败）。
3. 把“看起来没问题”当作验证证据。
4. 只验证后端，不验证前端构建可用性。

## 动手练习

1. 改一个小行为（例如默认 limit 值）。
2. 增加对应测试，先看失败，再改到通过。
3. 记录验证命令和结果。
4. 给上传状态新增一个枚举值，补齐 API + store + UI 影响范围测试清单（哪怕先不实现）。

推荐命令：

```bash
go test ./...
```

```bash
npm --prefix frontend run build
```

## 自测问题

1. 什么叫“新鲜验证证据”？
2. 为什么不能只跑单测而不跑全量？
3. 什么时候应该加集成测试而不是单元测试？
4. 如果回归发生在你没改的模块，你会先怀疑什么？
