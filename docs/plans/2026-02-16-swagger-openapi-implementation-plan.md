# Swagger OpenAPI Integration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 为 binlog_server 提供可交互的 Swagger UI，支持浏览 API、查看请求/响应 schema、在线调试参数并查看实时响应。

**Architecture:** 基于 Gin 接入 `swaggo/gin-swagger` 和 `swaggo/files`，由 `swag` 生成 OpenAPI 文档代码并在运行时挂载 `/swagger/*any`。接口注解直接写在现有 handler 上，尽量复用当前路由与请求/响应结构，避免改动业务逻辑。

**Tech Stack:** Go, Gin, swaggo/swag, swaggo/gin-swagger, swaggo/files, Go test

---

### Task 1: Swagger 路由接入（TDD）

**Files:**
- Modify: `internal/api/server.go`
- Test: `internal/api/server_test.go`

**Step 1: Write the failing test**
- 在 `internal/api/server_test.go` 新增 `TestAPI_SwaggerUI`，请求 `GET /swagger/index.html`，期望 `200` 且返回内容包含 `Swagger UI`。

**Step 2: Run test to verify it fails**
- Run: `go test ./internal/api -run TestAPI_SwaggerUI -v`
- Expected: FAIL（路由不存在或 404）

**Step 3: Write minimal implementation**
- 在 `internal/api/server.go` 注册 Swagger 路由 `GET /swagger/*any`。

**Step 4: Run test to verify it passes**
- Run: `go test ./internal/api -run TestAPI_SwaggerUI -v`
- Expected: PASS

### Task 2: OpenAPI 文档生成与关键接口注解（TDD）

**Files:**
- Modify: `cmd/binlog-server/main.go`
- Modify: `internal/api/handlers_tasks.go`
- Create/Modify: `internal/swaggerdocs/*`（生成文件）
- Test: `internal/api/server_test.go`

**Step 1: Write the failing test**
- 新增 `TestAPI_SwaggerDocContainsKeyPaths`，请求 `GET /swagger/doc.json`，校验包含：
  - `/api/dashboard`
  - `/api/sources/lookup`
  - `/api/tasks/{id}/replication`

**Step 2: Run test to verify it fails**
- Run: `go test ./internal/api -run TestAPI_SwaggerDocContainsKeyPaths -v`
- Expected: FAIL（无 doc.json 或缺路径）

**Step 3: Write minimal implementation**
- 在 `main.go` 增加 Swagger 全局注解（title/version/basePath）。
- 在关键 handler 增加 swagger 注解：`summary/dashboard/source lookup/tasks/replication`。
- 运行 `swag init` 生成文档代码到 `internal/swaggerdocs`。
- 在 `server.go` 引入生成文档包以注册 doc。

**Step 4: Run test to verify it passes**
- Run: `go test ./internal/api -run TestAPI_SwaggerDocContainsKeyPaths -v`
- Expected: PASS

### Task 3: 全量验证与文档补充

**Files:**
- Modify: `README.md`

**Step 1: Write/update doc assertions**
- 在 README 的 API/运行章节补充 Swagger 访问入口与用途。

**Step 2: Run full verification**
- Run: `go test ./...`
- Run: `cd frontend && npm run build`
- Expected: 全通过；若有 warning 记录但不阻塞交付。

**Step 3: Final consistency check**
- 核对 Swagger UI 可访问、可在线调试、关键接口路径在文档中可见。
