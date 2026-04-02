# Contributing to BinlogServer

感谢你考虑为 BinlogServer 做贡献！

## 如何贡献

### 报告 Bug

如果你发现了 bug，请通过 [GitHub Issues](../../issues) 提交，包含：

- 问题的详细描述
- 复现步骤
- 期望行为 vs 实际行为
- 相关日志（如果有）
- 环境信息（Go 版本、操作系统等）

### 提交功能请求

欢迎提交新功能建议！请在 Issue 中描述：

- 功能的使用场景
- 期望的行为
- 可能的实现思路（可选）

### 提交代码

1. **Fork 仓库**

2. **创建分支**
   ```bash
   git checkout -b feature/your-feature-name
   ```

3. **编写代码**
   - 确保代码通过 `go fmt` 格式化
   - 确保通过 `go vet` 检查
   - 为新功能添加测试

4. **运行测试**
   ```bash
   # 单元测试
   go test ./...

   # E2E 测试（需要 Docker）
   make e2e-quick
   ```

5. **提交代码**

   建议使用清晰的提交消息：
   ```
   feat: add support for xxx
   fix: resolve issue with xxx
   docs: update documentation for xxx
   refactor: simplify xxx logic
   test: add tests for xxx
   ```

6. **创建 Pull Request**
   - 描述 PR 的目的和改动
   - 关联相关的 Issue（如有）
   - 确保 CI（E2E 测试）通过

## 开发环境

### 前置要求

- Go 1.26.1+
- Docker（用于 E2E 测试）
- Node.js 18+（用于前端开发）

### 本地构建

```bash
# 构建后端（默认关闭 CGO，避免 Linux 产物绑定构建机 glibc）
make build

# 构建 Linux 发布二进制
make build-linux

# 构建前端
make ui-build

# 运行服务
./binlog-server --config config.yaml
```

### 运行测试

```bash
# 单元测试
go test ./...

# E2E 测试
make e2e-quick    # 快速：smoke + compression
make e2e-full     # 完整：所有场景
```

## 代码规范

- 遵循 [Effective Go](https://go.dev/doc/effective_go) 指南
- 使用 `gofmt` 格式化代码
- 添加必要的注释，特别是导出的函数和类型
- 错误处理要完整，不要忽略错误

## 目录结构

```
cmd/                    # 命令行入口
internal/
├── api/                # HTTP API 和路由
├── app/                # 应用生命周期
├── binlog/             # Binlog 文件写入
├── config/             # 配置加载
├── meta/               # 元数据存储
├── replication/        # MySQL 复制协议
├── tasks/              # 任务调度和状态机
├── upload/             # 云存储上传
└── ui/                 # 嵌入式前端
frontend/               # Vue3 前端源码
migrations/             # 数据库迁移
scripts/                # 脚本工具
```

## 获取帮助

如果你有任何问题，可以：

- 在 [GitHub Issues](../../issues) 中提问
- 查阅 [文档](./docs/guide/)

再次感谢你的贡献！
