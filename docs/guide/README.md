# Binlog Server 学习指南

本文档面向两类读者：
- **运维人员**：学习如何部署、配置、排查故障
- **开发人员**：理解架构设计、深入代码实现

## 文档结构

```
docs/guide/
├── README.md                    # 本文档，学习路径指引
│
├── concepts/                    # 核心概念（问题驱动）
│   ├── 01-what-problem-we-solve.md    # 我们要解决什么问题
│   ├── 02-mvp-architecture.md         # 最小可行架构
│   ├── 03-why-we-need-persistence.md  # 为什么需要持久化
│   ├── 04-why-we-need-lease.md        # 为什么需要租约
│   └── 05-cluster-problems.md         # 集群模式解决的问题
│
├── admin/                       # 运维指南
│   ├── deployment.md            # 部署指南
│   ├── configuration.md         # 配置参数详解（含 CURL 示例）
│   ├── troubleshooting.md       # 故障排查
│   └── observability.md         # 可观测性
│
├── dev/                         # 开发指南
│   ├── 00-overview.md           # 概览：从 main() 到 app.Run()
│   ├── startup-flow.md          # 启动流程
│   ├── task-state-machine.md    # 任务状态机
│   ├── replication-flow.md      # 复制数据流
│   ├── metadata-layer.md        # 元数据层
│   ├── cluster-mode.md          # 集群模式
│   └── database-migrations.md   # 数据库迁移规范
│
└── reference/                   # 参考
    ├── api.md                   # API 参考
    ├── data-model.md            # 数据模型
    └── glossary.md              # 术语表
```

## 相关入口（guide 目录外）

- `docs/swagger-api-guide.md`：Swagger UI 使用与调试说明
- `docs/develop/TODO.md`：开发 TODO 与里程碑（工程计划）

## 学习路径

### 运维人员路径

```
concepts/01 → concepts/02 → admin/deployment → admin/configuration → admin/troubleshooting
```

1. 先理解系统要解决什么问题
2. 理解最小架构
3. 学习部署和配置
4. 掌握故障排查

### 开发人员路径

```
concepts/ (全部) → dev/00-overview → dev/ (其余) → admin/ (选读)
```

1. 完整理解概念层（为什么这样设计）
2. **从 dev/00-overview 开始**，建立全局认知（main → config → app.Run）
3. 深入各模块代码实现
4. 了解运维视角

## 阅读建议

### 如果你是运维

你可能不关心代码细节，但需要理解：
- 各组件是做什么的
- 配置参数如何影响行为
- 出问题时看哪些指标、查哪些日志

**从 `concepts/01-what-problem-we-solve.md` 开始**，了解系统边界。

### 如果你是开发

你需要理解：
- 为什么引入这个组件
- 它解决什么问题
- 实现上有什么细节和坑

**推荐顺序：**

1. **概念层**：完整阅读 `concepts/` 目录
2. **概览**：`dev/00-overview.md` - 理解代码是怎么跑起来的
3. **深入**：按需阅读其他 dev 文档
4. **参考**：`reference/` 目录用于查阅

**修改表结构前必读**：`dev/database-migrations.md` - 定义了迁移规范和代码同步要求
