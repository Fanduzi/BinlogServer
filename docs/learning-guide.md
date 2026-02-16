# binlog_server 学习手册（总览）

这份文档保留学习方法和总路线。每一节的详细内容已拆分到 `docs/learning/`，便于日后回顾。

分节目录：`docs/learning/README.md`

## 1. 学习方法

1. 先跑通：先把后端和前端都启动成功，再开始读代码。
2. 走主链路：按“请求如何触发拉流并落盘”这条链路看，不按目录硬啃。
3. 每次只改一点：每一节只做 10~20 行改动，改完立刻测。
4. 结果先于感觉：以 `go test ./...` 和页面行为作为是否理解的判断标准。

## 2. 启动与验证

后端：

```bash
go run ./cmd/binlog-server
```

前端：

```bash
cd frontend
npm install
npm run dev
```

验证入口：

1. 后端健康检查：`http://127.0.0.1:8080/healthz`
2. 新前端：`http://127.0.0.1:5173`
3. 旧内置 UI（兼容）：`http://127.0.0.1:8080/ui/`

## 3. 学习路线（拆分文档）

1. `docs/learning/01-startup-and-wiring.md`
2. `docs/learning/02-task-model-and-scheduler.md`
3. `docs/learning/03-mysql-replication-runner.md`
4. `docs/learning/04-metadata-persistence.md`
5. `docs/learning/05-gin-api-layer.md`
6. `docs/learning/06-vue-elementplus-frontend.md`
7. `docs/learning/07-tests-and-safety.md`
8. `docs/learning/08-orchestrator-discovery.md`

## 4. 你要重点掌握的三条主链路

1. 控制面链路：前端 -> Gin API -> Scheduler -> Task 状态变化
2. 数据面链路：Runner -> 本地 binlog 文件 -> checkpoint 推进
3. 可观测链路：事件/文件元数据 -> MySQL Meta Store -> API/前端展示

## 5. 每次学习的固定模板

1. 10 分钟：先说清“这段代码解决什么问题”
2. 20 分钟：按关键函数走一遍调用链
3. 20 分钟：做一个最小改动并运行测试
4. 10 分钟：复盘 3 个问题

复盘问题：

1. 这段代码的输入和输出是什么？
2. 哪个失败会中断主流程，哪个不会？为什么？
3. 如果要加一个字段，最少要改哪些文件？

## 6. 当前项目里的重要结论（先记住）

1. 拉流起点支持 `LATEST/FILE_POS/GTID`。
2. checkpoint 只有在安全语义满足时才推进。
3. 上传是最佳努力模式：上传失败记 `UPLOAD_FAILED`，不打断拉流。
4. 任务与元数据可持久化到外部 MySQL（配置 `BINLOG_SERVER_META_DSN`）。
5. orchestrator 对“Binlog Dump 连接”的发现与半同步无关；是否最终纳入取决于后续实例探测是否成功。
