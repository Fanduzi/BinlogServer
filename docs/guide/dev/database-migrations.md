# 数据库迁移

本文档说明如何处理表结构变更，包括迁移工具使用和代码同步规范。

## 1. 迁移机制概览

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│  golang-migrate │────►│  MySQL 元数据库  │◄────│  应用启动校验    │
│  (执行变更)      │     │  (schema 存储)   │     │  (requiredSchema)│
└─────────────────┘     └─────────────────┘     └─────────────────┘
```

**两层保障：**

| 层 | 机制 | 作用 |
|----|------|------|
| 变更执行 | golang-migrate | 版本化 schema 变更 |
| 运行时校验 | requiredTableSchemas | 启动时确保 schema 满足要求 |

## 2. 迁移工具使用

### 2.1 内置迁移工具

项目内置了 Go 编写的迁移工具（`cmd/migrate`），无需额外安装：

```bash
# 直接运行
go run ./cmd/migrate [command]

# 或编译后运行
go build -o migrate ./cmd/migrate
./migrate [command]
```

### 2.2 迁移文件结构

```
migrations/
├── 000001_init_schema.up.sql    # 版本 1，向上迁移
├── 000001_init_schema.down.sql  # 版本 1，向下迁移
├── 000002_add_column.up.sql     # 版本 2，向上迁移
└── 000002_add_column.down.sql   # 版本 2，向下迁移
```

**命名规则：**

```
{version}_{description}.{direction}.sql

version:   6 位数字，递增
direction: up / down
```

### 2.3 执行迁移

**设置环境变量：**

```bash
# 必需：数据库连接串
export META_DSN='user:pass@tcp(127.0.0.1:3306)/binlog_server_meta?parseTime=true'

# 可选：运行环境（默认 dev）
export MIGRATE_ENV=dev  # 或 ENV=dev

# 可选：允许生产环境执行破坏性操作
export ALLOW_DESTRUCTIVE_MIGRATE=1
```

**常用命令：**

```bash
# 升级到最新版本
go run ./cmd/migrate up

# 查看当前版本
go run ./cmd/migrate version

# 回滚（需要 --allow-destructive 在生产环境）
go run ./cmd/migrate down --steps 1

# 跳转到指定版本
go run ./cmd/migrate goto 1

# 强制设置版本（脏状态修复）
go run ./cmd/migrate force 1
```

**命令行参数：**

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `--dsn` | 数据库连接串 | `$META_DSN` |
| `--path` | 迁移文件目录 | `./migrations` |
| `--env` | 运行环境 | `$MIGRATE_ENV` 或 `$ENV` 或 `dev` |
| `--allow-destructive` | 允许生产环境执行 down/force | `$ALLOW_DESTRUCTIVE_MIGRATE` |

### 2.4 生产环境保护

在 `prod` 或 `production` 环境下，`down` 和 `force` 命令默认被阻止：

```bash
# 设置生产环境
export MIGRATE_ENV=prod

# 以下命令会被拒绝
go run ./cmd/migrate down --steps 1
# Error: refusing destructive migrate command in production

# 必须显式允许
export ALLOW_DESTRUCTIVE_MIGRATE=1
go run ./cmd/migrate down --steps 1
```

### 2.5 安全提示

- **避免命令行传递 DSN**：会被记录到 shell 历史
- **使用环境变量**：`export META_DSN=...`
- **从文件读取**：`export META_DSN=$(cat /run/secrets/meta_dsn)`
- **CI/CD**：使用 Secret 管理工具注入

## 3. 开发规范：添加新迁移

### 3.1 创建迁移文件

使用 golang-migrate CLI 创建迁移文件模板：

```bash
# 安装 migrate CLI（仅需一次）
go install -tags 'mysql' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# 创建新迁移文件
migrate create -ext sql -dir ./migrations -seq add_new_table
```

生成：
- `./migrations/000002_add_new_table.up.sql`
- `./migrations/000002_add_new_table.down.sql`

或手动创建文件，遵循命名规则：`{6位版本号}_{描述}.{up|down}.sql`

### 3.2 编写迁移 SQL

**up.sql（向上迁移）：**

```sql
-- 000002_add_new_table.up.sql

CREATE TABLE new_table (
  id VARCHAR(64) PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  created_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

**down.sql（向下迁移）：**

```sql
-- 000002_add_new_table.down.sql

DROP TABLE IF EXISTS new_table;
```

### 3.3 同步代码（关键步骤）

**修改 `requiredTableSchemas`**（`internal/meta/mysql_store.go`）：

```go
var requiredTableSchemas = []tableSchemaSpec{
    // ... 现有表 ...

    // 新增表
    {
        Name: "new_table",
        Columns: []string{
            "id", "name", "created_at",
        },
        Indexes: []string{"PRIMARY"},
    },
}
```

**更新 `minRequiredSchemaVersion`：**

```go
const minRequiredSchemaVersion int64 = 2  // 从 1 改为 2
```

### 3.4 完整流程

```
1. 创建迁移文件     → migrate create
2. 编写 SQL         → .up.sql / .down.sql
3. 更新代码         → requiredTableSchemas + minRequiredSchemaVersion
4. 本地测试         → go run ./cmd/migrate up
5. 启动应用验证      → go run ./cmd/binlog-server
6. 提交代码         → migrations/ + internal/meta/mysql_store.go
```

## 4. requiredTableSchemas 机制

### 4.1 作用

启动时校验 schema 是否满足要求，防止：
- 迁移未执行导致运行时错误
- 迁移版本不匹配导致数据问题
- 手动修改 schema 导致不一致

### 4.2 校验逻辑

```go
func (s *MySQLTaskStore) ensureSchema(ctx context.Context) error {
    // 1. 检查 schema_migrations 版本
    if err := s.ensureSchemaVersion(ctx); err != nil {
        return err
    }

    // 2. 检查所有表、列、索引是否存在
    for _, table := range requiredTableSchemas {
        // 检查表存在
        // 检查所有列存在
        // 检查所有索引存在
    }

    // 3. 返回缺失项
    if len(missing) > 0 {
        return fmt.Errorf("metadata schema is not up to date; ...")
    }
    return nil
}
```

### 4.3 错误示例

```
metadata schema is not up to date; apply database migration before startup:
  missing column backup_tasks.new_column;
  missing table new_table
```

## 5. 最佳实践

### 5.1 迁移原则

| 原则 | 说明 |
|------|------|
| 向后兼容 | 新列允许 NULL 或有默认值 |
| 小步快跑 | 每个迁移只做一件事 |
| 可回滚 | down.sql 必须正确实现 |
| 测试先行 | 本地验证后再提交 |

### 5.2 禁止操作

- **禁止** 直接修改生产数据库 schema
- **禁止** 跳过 `requiredTableSchemas` 更新
- **禁止** 在业务高峰期执行迁移
- **禁止** 修改已发布的迁移文件

### 5.3 添加列示例

**场景：给 `backup_tasks` 添加 `priority` 列**

1. 创建迁移：

```bash
migrate create -ext sql -dir ./migrations -seq add_priority_to_backup_tasks
```

2. 编写 SQL：

```sql
-- 000002_add_priority_to_backup_tasks.up.sql
ALTER TABLE backup_tasks ADD COLUMN priority INT NOT NULL DEFAULT 0 AFTER state;

-- 000002_add_priority_to_backup_tasks.down.sql
ALTER TABLE backup_tasks DROP COLUMN priority;
```

3. 更新代码：

```go
// internal/meta/mysql_store.go

const minRequiredSchemaVersion int64 = 2

var requiredTableSchemas = []tableSchemaSpec{
    {
        Name: "backup_tasks",
        Columns: []string{
            "id", "name", "cluster_key", "state", "priority",  // 新增
            "last_error", "owner_worker_id", "epoch", "run_id",
            "source_json", "start_json", "storage_json", "updated_at",
        },
        Indexes: []string{"PRIMARY", "uk_backup_tasks_cluster_key"},
    },
    // ...
}
```

4. 验证：

```bash
go run ./cmd/migrate up
go run ./cmd/binlog-server
```

## 6. 故障排查

### 6.1 脏状态（Dirty State）

**错误：**

```
Dirty database version 1. Fix and force version.
```

**原因：** 迁移执行到一半失败

**解决：**

```bash
# 1. 手动检查数据库状态
# 2. 修复问题（删除半创建的表/列）
# 3. 强制版本
go run ./cmd/migrate force 1
# 4. 重新执行
go run ./cmd/migrate up
```

### 6.2 版本不匹配

**错误：**

```
schema_migrations is empty; run migrations to version >= 1
```

**解决：**

```bash
go run ./cmd/migrate up
```

### 6.3 缺失表/列

**错误：**

```
missing column backup_tasks.priority
```

**解决：**

```bash
# 检查迁移是否执行
go run ./cmd/migrate version

# 如果版本正确但仍报错，说明 requiredTableSchemas 与迁移不同步
# 检查代码是否最新
git pull
```

## 7. 相关文件

| 文件 | 说明 |
|------|------|
| `migrations/*.sql` | 迁移 SQL 文件 |
| `cmd/migrate/main.go` | 内置迁移工具 |
| `internal/meta/mysql_store.go` | requiredTableSchemas 定义 |
| `internal/meta/mysql_store_test.go` | Schema 校验测试 |
