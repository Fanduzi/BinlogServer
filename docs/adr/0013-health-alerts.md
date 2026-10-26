# 健康指标、推荐告警和通用 webhook

Status: Accepted。落实 ADR 0009 §2.2 和决定 1 里生产验收 F2 还没落地的一半。不改 `health.evaluator` 的默认值（那是 F4）。

## 结论从哪来

`binlog_server_task_healthy` 的 1 只表示这一轮 `health.Registry.HealthOf` 的状态是 HEALTHY。0 是 UNHEALTHY、DEGRADED 或 UNKNOWN。指标不再算一遍健康，也不读 `health.evaluator` 这个配置字符串。评估器已经在的时候（`shadow` 或 `on`），快照就是它刚写下的那个结论。`off` 不装配评估器，因此没有每任务序列。

## 标签和谁输出

标签是 `task_id`（与现有指标和 §2.2 一致）加 `reason`。`reason` 是封闭枚举：`ok`、`critical`、`warning`、`unknown`、`unevaluated`、`unread`。一个任务同时只有一条序列，基数是任务数，不是 finding 数。

只由正在评估该任务的节点输出：standalone 是本进程；集群是 lease 主人、被委派的最后一任链主人，或持有 `_health`、代评无主任务的节点。其它节点省略，避免两次抓取把同一个任务加两次。抓取每一个节点。

没有样本时输出一条哨兵：`{task_id="",reason="ok"} 1`。推荐规则用 `== 0`，哨兵不会响。`absent()` 在默认 `off` 时也不该当故障：那样每一台还没开评估器的机器都会分页。进程死了用 `up == 0`。本节点负责、但还没有跑过的任务是 0，`reason="unevaluated"`，`for: 2m` 盖住默认 60s 的 sweep。

## SLO 序列

同一轮、同一批任务，不再做第二次判定：

| 序列 | 含义 |
|---|---|
| `binlog_server_task_progress_age_seconds` | desired RUN 时，`max(最后一次源字节, 本地推进)` 距今的秒数。不是 desired RUN 时为 0，停着的任务不按停滞分页。没有时钟为 0 |
| `binlog_server_task_upload_lag_seconds` | 对象存储开启时，最老一条已封存且不是 `UPLOADED`+校验和 match 的段的年龄。宽限内的落后也看得见，不只有越过宽限的 I7。没开对象存储，或全部匹配，为 0。磁盘目录缓存命中时复用上一轮的年龄 |
| `binlog_server_task_open_critical` | 这一轮转移落地后的 open 集合里，严重级别为 CRITICAL 的条数 |

`binlog_server_task_last_verified_timestamp` 已经在巡检指标里，告警直接用它，不复制一条。§2.2 的 99.9% / 30 天健康分钟和对象 RPO p99 ≤ 20min 是 SLO 定义，由 `task_healthy` 和 `upload_lag` 算，不另做 recording rule。

告警阈值用决定记录 2 的默认：停滞 45s、上传宽限 15min（900s）、巡检超过 48h（`verify_every` × 2）。`BinlogFindingCritical` 直接看 `binlog_server_finding_open{severity="CRITICAL"}`。

## Webhook

一个 JSON POST，不是 Alertmanager 的两种格式，也没有静音、`_notify` 租约或 `notify_cursor` 发件箱。那些留在阶段 2 设计 P2-9，表还在，这版不写。

正文 `{"version":1,"events":[...]}`。健康事件带 `status` 和 `previous`。finding 事件只含 CRITICAL 的 `OPEN`、`CLOSE`、`ESCALATE`，`detail` 截到 1024 字符。可选令牌放在 `X-BinlogServer-Token`。`X-BinlogServer-Delivery` 是正文 SHA-256，同一次重试相同。不跟随重定向，避免令牌被送到别的主机。

重试：传输错误、5xx、429，默认 3 次，退避从 200ms 翻倍，单次超时 5s。其它 4xx 不重试。这一轮都失败就把整批留在内存里，下一轮再送，并且记为已经通知，避免同一状态再组一份新事件。进程重启可能再通知一次。

去重：该任务第一次被看到是 HEALTHY 时只记基线，不发。第一次非 HEALTHY 要发。之后只有状态变了，或有新的 CRITICAL 转移，才发。稳态不发。`shadow` 记样本、不 POST。集群里只有评估该任务的节点发。

配置：`notify.webhook_url`（空则关）、`notify.webhook_token`、`notify.timeout_sec`、`notify.attempts`、`notify.backoff_ms`。URL 只接受带主机的 http 或 https。

## 明确不做

Alertmanager 双格式、静音、`task_muted`、`_notify` 租约、`notify_cursor` 发件箱、按厂商的 IM。评估器默认仍是 `off`。
