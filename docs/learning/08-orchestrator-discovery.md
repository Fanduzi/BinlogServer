# 第 8 节：orchestrator 是否会把 binlog 拉流客户端当从库

## 先给结论

1. 会不会被发现为“从库”，与是否开启半同步无关。  
半同步只影响 ACK 机制，不影响发现入口。
2. 只要主库上出现 `Binlog Dump`/`Binlog Dump GTID` 连接，orchestrator 就可能把该连接当作“候选 replica”去探测。
3. 被发现不等于被纳入拓扑。  
如果 orchestrator 无法把该候选对象当作 MySQL 实例连通并读取实例信息，最终不会稳定纳入拓扑。
4. 你现场看到 canal/dm 没被当从库，根因是：它们不是 orchestrator 可连通、可读取的 MySQL 实例（至少在 orchestrator 探测路径上不是）。

## 发现路径（按源码）

orchestrator 会通过两类入口找 replica：

1. `SHOW SLAVE HOSTS` / `SHOW REPLICAS`（可配置）
2. `processlist` 中命令为 `Binlog Dump` / `Binlog Dump GTID` 的连接

对应源码（Percona orchestrator）：

- `go/inst/instance_dao.go:802`（“Get replicas...”）
- `go/inst/instance_dao.go:856`（fallback 到 `processlist`）
- `go/inst/query_string_provider.go:298`（`information_schema.processlist`）
- `go/inst/query_string_provider.go:310`（`performance_schema.processlist`）

## 为什么“发现了”但“没纳入”

关键逻辑是：

1. orchestrator 发现候选实例后，会调用 `ReadTopologyInstanceBufferable` 去连接并读取。
2. 如果 `OpenDiscovery(...)` 或 `Ping()` 失败，会走失败分支。
3. 失败分支最终返回 `nil` 实例。
4. 调用方拿到 `instance == nil` 后直接 `return`，不会继续拓扑扩展。

对应源码：

- `go/inst/instance_dao.go:393`（`OpenDiscovery`）
- `go/inst/instance_dao.go:404`（`Ping`）
- `go/inst/instance_dao.go:1144`（失败返回 `nil`）
- `go/logic/orchestrator.go:289`（`instance == nil`）
- `go/logic/orchestrator.go:307`（直接返回）

## 对本项目的含义

当前项目使用 `go-mysql` 的 `BinlogSyncer` 拉流。该路径会走 replica register（`COM_REGISTER_SLAVE`）语义，因此在主库视角可被识别为 replication client。

这意味着：

1. 不开半同步也可能被 orchestrator 发现为候选 replica。
2. 若你的 binlog server 宿主机上恰好有可被 orchestrator 探测的 MySQL 实例（host/port 可达且可读），就有被纳入拓扑的风险。

## 风险控制建议（生产）

1. 给 binlog backup 使用独立 replication 用户。
2. 在 orchestrator 显式配置：
   - `DiscoveryIgnoreReplicationUsernameFilters`
   - `DiscoveryIgnoreReplicaHostnameFilters`
3. 约束 binlog backup 的 `server_id` 范围，便于运维审计和规则匹配。
4. 在变更前后用 `orchestrator-client -c topology -i <source>` 对比拓扑，确认没有误纳入节点。

## 一页检查清单

1. 主库上执行 `SHOW PROCESSLIST`，确认是否存在 `Binlog Dump` 连接。
2. orchestrator 日志里搜索 `discoverInstance` 失败记录。
3. orchestrator UI/API 检查是否出现不应纳入的候选实例。
4. 验证 ignore filters 生效后，候选连接不再入队。

