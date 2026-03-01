# Input Validation Task 模板（可直接发给苦工）

用途：补齐“用户输入参数合法性校验”专项任务，确保前后端和文档契约一致。

## 1. 任务目标

1. 全量梳理本次改动涉及的用户输入参数。
2. 前端增加提交前预校验，后端增加权威校验。
3. 更新 Swagger/README/相关文档，保证校验规则可见。
4. 补齐测试与 e2e 非法输入回归。

## 2. 范围（按需修改）

1. `frontend/src/App.vue`（或相关页面）
2. `frontend/src/api.js`
3. `internal/api/*.go`
4. `internal/tasks/*.go` / `internal/meta/*.go`（如涉及存储约束）
5. `internal/swaggerdocs/*`
6. `README.md` / `docs/*`

## 3. 参数清单（必须先填）

| 参数名 | 来源（UI/API/Config） | 必填 | 类型 | 约束（范围/长度/枚举/格式） | 前端校验点 | 后端校验点 |
| --- | --- | --- | --- | --- | --- | --- |
| `<example>` | `API body` | `yes` | `string` | `^[a-z0-9_-]{3,64}$` | `form rules` | `handler validate` |

## 4. 实现要求（硬约束）

1. 前端：非法输入必须在提交前拦截并提示，不允许静默失败。
2. 后端：禁止信任前端；所有入口重复校验，非法请求返回 `400`。
3. 错误语义：错误码/错误消息必须稳定可读，前端可直接展示。
4. 不允许只改一层：若缺少前端或后端任一侧校验，任务视为未完成。

## 5. 验收命令（必须执行）

```bash
go test ./internal/api ./internal/tasks ./internal/meta -count=1
cd frontend && npm run build
./scripts/e2e/run-suite.sh --scenarios <affected-scenarios>
```

## 6. 提交要求

1. 仅 1 个主题 commit（如范围过大可拆，但每个 commit 必须可独立解释）。
2. commit message 示例：`feat(validation): enforce frontend and backend input checks for <feature>`

## 7. 完成后回报格式（固定）

1. commit hash
2. 修改文件列表
3. 参数清单（最终版）
4. 与实现要求逐条对照结果
5. 三条命令结果摘要
