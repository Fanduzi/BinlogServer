# 阶段 4 / 5：覆盖查询与还原计划

ADR 0009 第 4、5 节的实现说明。生产级清单 D3。Console 时间线和向导不在这次里：下面的 JSON 就是向导要显示的数据。

## 不建第二份索引

覆盖和还原计划只读已经存在的保留链，和 `GET /window`、`GET /replay` 同一条：

1. `ListFiles` 的全量清单（不带 HTTP `limit`）再 `SelectReplayFilesFor`。损坏截断和 `/replay` 相同。
2. 目录行上已有的位置：`object_key`、`upload_state`、`checksum`、`sha256`、`file_path`、`location`。封存时写入的 `first_event_at` / `last_event_at` 不单独做时间索引；回答一个点需要事件头时间戳和 GTID 偏移，这些已经在分段字节里。
3. 每个分段读一次，用 `binlog.ScanCover`（和封存段 CRC 判定同一套读法）。读不到（不在本进程，或封存段损坏）就是窗口里的断点，不是一条新的查找表。

`limit` 查询参数被忽略。结果里的 `segments` 是从链的第一段到停点的整段前缀，不截成 200。

## 时间与 GTID

事件头时间是整秒，和 `mysqlbinlog` 的事件头一致。`at` 收到带小数的时间时先截到秒。格式描述和 previous-GTIDs 的时间是源文件创建时间，不算进覆盖（与 `EventTimeSpan` 相同）。

`at=T`（UTC）覆盖的事件是时间戳 `<= T` 的复制事件。停点是同一分段里下一个事件的起始偏移；该事件已经是文件末尾时，停点是最后一个完整事件之后的偏移。还原命令用 `--stop-datetime=T+1s`，因为 `mysqlbinlog` 的 `--stop-datetime` 在第一个 `>=` 该时间的事件处停下，这样时间戳等于 T 的事件会被包含。

`gtid=uuid:seq` 只接受一条 MySQL GTID。停点是同一分段里下一条 GTID 事件的起始偏移（包含这条事务、不含下一条）；后面没有下一条时停在本分段末尾。不把下一条 GTID 所在的下一个文件算进来。MariaDB 的 `domain-server-seq` 返回 400。

`covered`：

- 时间早于最早复制事件：`before earliest`。晚于最晚复制事件：`after latest`。链上没有复制事件：`empty chain`。
- GTID 不在任何分段里：`gtid is not in the retained window`。
- 停点落在断点上或必须跨过断点：`falls on break:` 接上 `/window` 的原因。只点名一个分段的断点（读不到、校验和不符、对象未持久）从该段起不可恢复。点名多段的断点（缺文件、GTID 洞）从较后的那段起不可恢复，停在较早那段上仍然覆盖。
- 其余为覆盖。`segments` 是第一段到停点段。`start_from.pos` 是 4（magic 之后）。`stop_at` 是上面的停点。

窗口外的回答是 200 且 `covered: false`，不是 404。

## API

Go 里这一份回答叫 `assurance.CoverResult`。`assurance.Coverage` 已经是巡检对一个分段记下的检查记录，两边不是同一个东西。JSON 字段不变。

- `GET /api/tasks/{id}/coverage?at=` 或 `?gtid=`。二者恰一个。没有任务是 404。
- `GET /api/coverage?source=host:port&at=` 或 `&gtid=`。`source` 用最后一个冒号拆端口。同一源上每个任务各给一份覆盖（`SameSourceHost`，端口精确），不截断。没有任务时 `tasks` 为空数组。
- `POST /api/tasks/{id}/restore-plan`，无副作用。JSON 恰一个目标：`at`、`gtid` 或 `latest: true`。可选 `full_backup_gtid`（全量备份的 `@@gtid_executed`）。

还原计划：

```
target, coverage,
prerequisites[],
prechecks[]: {id, description, how, result},
steps[]: {kind: download|verify|apply, files, command},
manifest[]: {name, sha256, size, local_path, object_key},
restore_sh, readme,
bundle: {url}
```

`result` 是 `green`、`red`、`yellow`、`manual`。

- `window`：覆盖为绿，否则红。
- `checksum`：每一段 `checksum=match`，或本地文件在且 `last_verified_at` 距现在不超过 `verify_every` 的两倍（未配置巡检时按 48h）为绿。本地在但没验过为黄。`checksum=mismatch`，或既没有本地副本也没有 match，为红。
- `full_backup_gtid`：没贴为黄字 `manual`。起点集合 B（任务的 GTID 起点，否则第一段的 previous-GTIDs）不是它的子集，是洞，红。它与本计划要应用的 GTID 相交，是重叠，红。两者都没有才绿。
- `target_settings`、`client_version`：服务器不连目标库，列为操作者要执行的 SQL / `mysqlbinlog --version`，`manual`。

`apply` 步复用 `FormatPITRCommand` / `FormatGTIDCommand`。`bundle.url` 指向现有的 `GET /replay/archive`（时间目标带 `stop_datetime=T+1s`）。不往那个 tar 里加成员，避免改变已有归档字节。`manifest`、`restore_sh`、`readme` 在计划 JSON 里，操作者照此取文件、核对 SHA-256、执行命令。

## 集群

`mode: cluster` 时这三个接口都返回 501，正文 `coverage is not supported in cluster mode yet`。不根据本进程看得到的目录回答：ADR 0011 PR-4 的所有权还没落地，per-node 目录上的回答会是错的。判定是进程的集群模式，不是 `leaseManager`（单机 worker 也有一把内存租约）。

## CI

`e2e-coverage` 每个 PR 和 push main 都跑（`workflow_dispatch` 时只在选 `coverage` 时跑），场景 `scripts/e2e/smoke-coverage.sh`：

- 真实 MySQL 8.0 分段。用 `mysqlbinlog` 独立解析事件头时间和 `GTID_NEXT`，不读本服务的 `/window`。
- 窗口内、窗口外、段边界上，时间和 GTID 各 10 例。`covered`、分段名顺序、`stop_at.pos` 与该解析一致。
- 按还原计划的 `restore_sh` 对 5 个时间点回放到空实例，`CHECKSUM TABLE` 与写入当时的源库一致。插入的事件头时间取源库时钟 120 秒之后，使 `--stop-datetime` 晚于格式描述；停在文件头上时 mysqlbinlog 不会写出这些行。
- 全量备份 GTID 有洞或与将应用的事务重叠时，`full_backup_gtid` 预检为红。

单测覆盖：不截断（250 段）、断点前后、预检红/绿、集群 501、跨任务查询。
