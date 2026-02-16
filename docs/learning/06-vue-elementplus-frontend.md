# 第 6 节：前端（Vue3 + Element Plus）

## 目标

看懂前端如何组织状态、请求后端 API 并呈现任务与文件状态。

## 核心文件

- `frontend/src/App.vue`
- `frontend/src/api.js`
- `frontend/vite.config.js`

## 一眼看前端架构

```text
App.vue (页面状态 + 交互)
  -> api.js (HTTP 封装)
      -> /api/* (Gin 后端)
```

当前是单页面管理台，状态全部在 `App.vue` 内部维护，`api.js` 负责统一请求入口。

## 逐文件讲解

### 1) `frontend/src/api.js`

1. 基于 `axios.create` 创建统一客户端，`baseURL=/`，超时 10s。
2. 所有接口函数都在这里导出，组件只调用函数，不直接拼 URL。
3. `getCheckpoint` 对 `404` 做了特殊处理：返回 `null`，避免页面报错。
4. `listEvents/listFiles` 内置默认 `limit`，减少组件重复参数代码。

### 2) `frontend/src/App.vue`

可以按“状态区 + 行为区”来读：

状态区：

1. `summary`：顶部统计卡片数据。
2. `tasks`：任务列表。
3. `form/formMode/formVisible`：创建和编辑弹窗状态。
4. `detailTask/checkpoint/events/files/detailVisible`：详情抽屉状态。

行为区：

1. `refreshAll()`：并发拉取 summary + tasks。
2. `openCreate/openEdit/submitForm`：任务表单流程。
3. `onStart/onStop/onDelete`：任务操作按钮。
4. `showDetail()`：并发拉取 task + checkpoint + events + files。
5. `parseErr()`：统一把接口异常转成用户可读消息。

### 3) `frontend/vite.config.js`

开发期代理配置：

1. `/api` -> `http://127.0.0.1:8080`
2. `/healthz` -> `http://127.0.0.1:8080`

这样前端开发服务器和后端端口不同也不会有跨域问题。

## 关键点

1. 前后端分离：前端只关心 HTTP 协议，不关心 Go 内部结构。
2. UI 中重点看上传状态：`LOCAL_ONLY/UPLOADED/UPLOAD_FAILED`。
3. 统一 API 封装在 `api.js`，避免在组件里散落请求细节。
4. 编辑任务时密码默认留空，表示“保持原密码不变”。
5. 详情页是并发请求，避免串行导致打开慢。

## 你要重点理解的交互细节

1. `buildPayload()` 会按起点模式裁剪字段：  
`FILE_POS` 才提交 `file/pos`，`GTID` 才提交 `gtid_set`。
2. 删除弹窗使用 `ElMessageBox.confirm`，取消不报错。
3. 状态标签 `stateTagType` 只是展示映射，不改变后端状态语义。

## 动手练习

1. 在任务详情增加“上传失败数”展示。
2. 增加一个按 `upload_state` 的前端过滤。
3. 执行 `npm --prefix frontend run build` 验证构建。
4. 在 `refreshAll` 增加自动轮询（例如每 5 秒），并考虑抽屉打开时是否需要暂停轮询。

## 自测问题

1. 哪些状态适合前端本地缓存，哪些必须实时拉取？
2. 为什么 API 封装单独抽文件更易维护？
3. `getCheckpoint` 对 404 返回 null 的设计，对页面有什么好处？
4. 如果后端字段重命名，最小修改点应该优先在哪里？
