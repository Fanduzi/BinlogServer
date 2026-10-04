# frontend/tests/e2e Module

Playwright 端到端回归模块，验证运维控制台的首屏、导航、筛选与 mock API 合同。

## Files

| File | Responsibility |
|------|---------------|
| `starting-summary.spec.ts` | 锁定 STARTING 指标可见、与 RUNNING 分离、source 映射及缺少 starting 字段时回退 0 |
| `server-pagination.spec.ts` | 锁定 dashboard server page 请求、全局 total、state page transition、当前页筛选范围、翻页不打按任务 `/lease`，以及缺少分页字段时不再本地切页 |
| `lease-risk.spec.ts` | 列表用任务主人/epoch 抄本显示租约风险，详情仍打单次 `/lease` |
| `dashboard-filters.spec.ts` | 指标卡筛选与键盘交互 |
| `dashboard-empty.spec.ts` | 空态和零指标 |
| `single-process.spec.ts` | 单机场景总览与 Worker 页都写明本进程拉取，导航人数为 0 |
| `mock-handler.spec.ts` / `dev-mock-api.spec.ts` | 共享 mock/API helper 合同，含 lookup 与 dashboard host 过滤与 Go SameSourceHost 同一 accept/reject 集 |
| `batch-create.spec.ts` | 批量创建本地 100 项上限、有序部分成功结果、密码脱敏、单次 batch 请求、安全错误文本与逐项自动启动 |
| `flavor-select.spec.ts` | 新建任务与批量创建的 Flavor 是 mysql/mariadb 下拉；选 mariadb 后创建请求带 `flavor=mariadb` |
| `adopt-leftover.spec.ts` | 遗留目录从任务详情认领：POST adopt、状态保持已停止、不回显密码、随后启动；显式 LATEST 仍不启动；目录任务编辑仍是 PUT |
| `replay-set.spec.ts` | 任务详情在文件表仍列出封存名和每个 epoch 时，回放命令只含最高 epoch 的 open 路径，并复制 `mysqlbinlog` argv；「下载回放集」请求 `GET /api/tasks/{id}/replay/archive?limit=80`，保存 `task-{id}-replay.tar`，正文是这些 basename |
| `segment-download.spec.ts` | 文件表每一行可下载该磁盘文件名，包含仍打开的 `.open.e1`；回放命令仍是每个序号一条路径 |
| 其他 `*.spec.ts` | 详情、导航、lease、集群和上传重试场景 |
| `fixtures/` | 共享场景类型与路由拦截 |

## Interfaces

- `npm run test:e2e`：运行前端 Playwright 回归。
- `registerMockRoutes(page, options)`：为浏览器测试安装共享 mock API 路由。

## Dependencies

- Upstream: `frontend/src` 应用、mock handler 和 Playwright。
- Downstream: 本地 Vite dev server；不依赖真实 API 数据库。

## Update Rule

- 新增/删除回归场景、mock contract 或测试入口变化时，更新本文件。
