# internal/binlog Module

## Files
- `writer.go`: binlog 文件写入与旋转。
- `checkpoint.go`: checkpoint 数据结构。
- `durable.go`: 最高 open 分段里最后一个完整事件的源文件名和 end log_pos。封存名和没有完整事件的分段不是续传点。`end_log_pos` 为 0 的事件（源库重启后的 artificial rotate）不是续传位点；前面还有完整事件时用那个事件的 end log_pos，并把字节偏移停在它后面，后面的 artificial 字节可以丢掉。只有 log_pos 0 的分段仍然不是续传点。`PreambleOnly` 表示这段没有续传事件：只有 4 字节 magic，或只有 `end_log_pos` 为 0 的完整事件且文件停在事件边界上。撕掉的尾部、以及 `end_log_pos` 大于 0 的事件，都不是。它不是续传位点。`DurableResumeDir` 直接扫 catalog `file_path` 所在目录。`LastRotateTarget` 在最后一个完整事件是 rotate 时给出下一个文件和位点；格式描述标明 CRC32 时，校验和的 4 个字节不算进文件名。`EventSpan` 给出这段里、结束在最后一个 `end_log_pos` 大于 0 的事件上的连续事件链的起点和终点；中间有空隙时链从空隙后重算，文件头的 format description 不会把中部复制段的 `start_pos` 拉回 4。 `durableOpenSegment` 选出这个最高打开分段，`open_tail.go` 共用同一选择。`SegmentPositions` 按路径、大小和 mtime 缓存。 `HeaderEventsOnly`：文件只有头部事件（FDE、previous GTIDs、MariaDB GTID list/checkpoint、start v3、rotate，log_pos 不限）时为真（#405）。
- `segment.go`: `ClassifySegment` 是 runner、durable resume 和 tasks 磁盘扫描共用的分段名解析。`mysql-bin.000001` 是封存、epoch -1。`name.open.eN` 是打开的第 N 代。`name.sealed.eN` 是同一源文件名的后一次封存，epoch N，不是续传点。`OpenName` / `SealedName` 是这个解析结果上的谓词。被拒绝的名字（含 `.takeover-*`、`notes.txt`、`task-N.binlog`）两者都为假，保留清理不会把它们当 binlog 删掉。
- `event_time.go`: 读一个分段的事件头，给出复制流里第一个和最后一个非 0 时间戳。时间戳 0 跳过。格式描述（format description）和 previous-GTIDs 的时间是源文件创建时间，不计入覆盖。没有 magic 或没有剩余计时事件时不算覆盖。尾部撕掉的字节保留已经读完的事件时间。
- `gtid_pos.go`: `ScanSegmentForGTID` 在一个分段里找第一个 MySQL `GTID_EVENT`（UUID 不区分大小写，序号相等）。返回的偏移是该事件的起始字节，给 `mysqlbinlog --stop-position` 用，不是事件头里的 `end_log_pos`。previous-GTIDs 不算。没有 magic 时返回空结果。尾部撕掉时保留已经读完的命中。
- `cover_scan.go`: `ScanCover` 一次读完一个分段。每个事件给出起始和结束偏移、复制流时间（格式描述和 previous-GTIDs 不计），MySQL GTID 事件另给 UUID 和序号，并解出 previous-GTIDs 文本。封存段的坏 size 或 CRC32 与 `scanSegmentGTIDs` 一样返回 `ErrCorruptEventSize`。open 段的撕掉尾部保留已经读完的前缀。覆盖查询只用这一次扫描，不另建时间索引。
- `gtid_set_scan.go`: `scanSegmentGTIDs` 读同一个分段里的 previous-GTIDs 文本，以及每一条 MySQL `GTID_EVENT` 的 UUID、序号、起始字节和事件头时间。格式描述标明 CRC32 时，previous-GTIDs 的校验和尾部先剥掉再解码。尾部撕掉时保留已经读完的事件。previous-GTIDs 解不出来是错误。事件 size 超过 1GiB 返回 ErrCorruptEventSize，不分配该长度（#358）。 封存段（sealed 读法）里事件 size 小于头、事件体读不全、或格式描述标明 CRC32 而某事件的 CRC32 尾部对不上，都返回 ErrCorruptEventSize（CRC 不符时包着 `ErrEventChecksum`）；封存段不足 4 字节（含 0 字节）或断在事件头中间也返回 ErrCorruptEventSize，包着 `ErrSegmentTruncated`（#381）；没有 magic 的封存段仍不由读法判定（零填充副本是 SEGMENT_OVERLAP 的证据，原地改字节由复核器判定）；`/window`、`/replay` 与链的 I1 用的是同一个读法，所以“不可读”不会各说各的（#380 QA）。open 段不校验 CRC。
- `open_tail.go`: `ScanOpenTail` 读最高打开分段（`Ledger.Tail` 选段后调用它）：每个完整事务的 GTID（MySQL、带 tag 的 GTID、MariaDB）、previous-GTIDs，以及续传截断点。末尾的事务没写完、且它前面正好是一个完整事务结束处时给出截断偏移和位点；否则给出这个事务的 GTID，续传在它里面接着拉。XID、XA_PREPARE、压缩的 TRANSACTION_PAYLOAD、DDL 和 `COMMIT`/`ROLLBACK`/`XA COMMIT` 算提交（`TransactionQueryEffect`）。`OpenTail.Durable` 是尾部已落盘的 GTID 文本（只有完整事务；分段的 previous-GTIDs 是源端历史声明、不是本任务持久字节，不计入，#596），`Ledger.ResumeGTID` 把种子集合和它们合起来。只读，不改文件。
- `gtid_set.go`: `GTIDSet`，仓库里唯一的 GTID 区间类型：加、并、减、包含、内部空洞和规范文本。`ParseGTIDSet` 走 go-mysql 校验，`ParseGTIDSetLenient` 不走（坏串返回 false）。
- `ledger.go`: 段账本（ADR 0006）。`LedgerAt(dir)` 读一次目录，回答分段顺序、每段 GTID 区间和 previous-GTIDs（`Contents`，按文件戳 size+mtime+ctime 缓存，有上限）、已存集合 `Stored`、空洞 `Holes`、损坏 `Damage`、续传尾部 `Tail`、封存分段的集合 `SealedStored`/`SealedText`（不含打开分段，只读 MySQL）和续传集合 `ResumeGTID`（种子 + 封存分段 + 尾部的完整事务，不含尾部的 previous-GTIDs 声明，#596；检查点行落后于分段时，例如切源后旧源文件已封存，续传不再变短，#391）。不在本地目录的字节用 `ReadSegmentContents`。
- `storage_consistency.go`: `Ledger.Damage(checkpoint, start, flavor)` 只认重拉损坏的硬痕迹，并返回第一个损坏分段 `Segment`、它之前仍可恢复的分段 `Valid`、checkpoint 里有但起点集合和任何分段都没有的 MySQL GTID `Missing`，以及新任务起点 `Restart`（起点集合或第一个分段的 previous-GTIDs，加上 `Valid` 的事务；未知时为空）。Rotate 指向自己或更早的同名 binlog（身份前缀 `uuid.` 或回切段前缀 `uuid~n.` 先剥掉），或 `end_log_pos` 在该分段里倒退，才是问题。GTID 序号乱序、多个 UUID 交错、某个序号在下一个分段里、起点集合或 `gtid_purged` 里的空洞，都不是。文件之间的缺口也不拒绝启动。目录不存在不是问题。checkpoint 和 flavor 仍传进来，不参与判断。 `Ledger.SegmentDamage(name)` 只判一个分段（备份链 I1 用），结果按路径、源文件、大小和 mtime 缓存（评估器每轮扫 1500 段不重读未变的文件）。`Ledger.IsDamaged()` 只回答有没有损坏分段，不算恢复集合；runner 启动前的拒绝用它。

## Exports
- 文件写入、rotate 与 checkpoint 推进基础能力。
- `DurableResume` / `DurableResumeDir` / `DurableCursor`：runner 下次 Start 和 `GET /api/tasks/{id}/checkpoint` 共用的本地续传位点。不使用文件大小。末尾 `end_log_pos` 为 0 的 artificial 事件不把整段判成没有位点。`DurableResumeDir` 扫接管时 catalog 记下的分段目录。`PreambleOnly` 只判断这段是不是 magic 或 log_pos 0 的文件头，调用方用已保存的 checkpoint 续，不用这里的位点。
- `LastRotateTarget`：封存文件末尾的 rotate 指向的下一个文件。runner 在 checkpoint 仍停在刚封存的文件上时用它继续，避免在封存名旁边再开一段。
- `EventTimeSpan`：定点恢复用来判断一个分段的复制事件时间是否盖住窗口。格式描述和 previous-GTIDs 不参与。调用方持有 reader。
- `ScanSegmentForGTID`：在分段字节里定位一条 MySQL GTID。偏移是事件起始字节。调用方持有 reader。
- `scanSegmentGTIDs`：列出分段里的 previous-GTIDs 和全部 MySQL GTID 事件。调用方持有 reader。
- `EventSpan`：封存目录行的 `start_pos` / `end_pos`。起点是连续链里第一个事件的位置，终点是最后一个 `end_log_pos` 大于 0 的事件。`end_log_pos` 为 0 的事件和撕掉的尾部不算。`SegmentPositions` 先过 `DurableCursor`：游标不成功（缺失、只有 magic、撕掉的尾部、只有 log_pos 0）时 ok 为 false；成功时终点就是该游标，起点用 `EventSpan`。

## Dependencies
- Upstream: `internal/replication`。
- Downstream: 本地文件系统。

## Update Rule
- 文件写入语义、checkpoint 语义变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
- `seal_facts.go`: `ReadSealFacts(path)` 流式读取封存分段一次（字节经 TeeReader 进 SHA-256，内存不随段大小增长），给出首尾事件头 UTC 时间（无定时事件时 `HasSpan=false`）、整文件 SHA-256 和字节数，供封存写入目录行（ADR 0009 P1-4）。
- `sealed_truncated_test.go`: #381 回归。封存读法对 0 字节、断在 magic 里、断在事件头中间的字节返回包着 `ErrSegmentTruncated` 的 ErrCorruptEventSize，open 读法仍按撕裂尾；截在事件边界、本地大小与 `.seal-sha256` 不同的封存段由 `Ledger.Unreadable` 点名并带封存时大小。
- `seal_sums.go`: 封存校验和账本 `{任务目录}/.seal-sha256`：每次封存追加一行“sha256 大小 名字”（同名以最后一行为准）并 fsync，超过 1MiB 时去掉磁盘上已不存在的名字；`LoadSealSums` 读回，`sealSumAt`（导出 `SealSumAt`）按账本文件 stamp 缓存。没有目录行 sha256 时（standalone 无 meta，或 meta 缺 000008）它是复核器的预期校验和（#380 QA P1）。
- `stamp_cache.go` / `stamp_linux.go` / `stamp_darwin.go` / `stamp_other.go` / `walk_ops.go`: `FileStamp`（大小、mtime、Linux 和 darwin 上的 ctime）和有上限的按路径缓存。`Stat`、`Lstat`、`Open`、`OpenFile`、`ReadFile`、`ReadDir`（以及返回项的 `Info`）、`EntryInfo`、`Glob` 和 `StampOf` 都记入 `WalkOps`（`SegmentTouches` 是同一个数）。`EntryInfo` 给走查调用方用：`ReadDir` 的项本身已经计数，这里不再加一次。WalkOps 是本地的快速信号。冷启动的证明是 Linux 上对评估器一轮做 strace（`internal/tasks` 的 `TestEvaluator_SyscallWalkNearLinear`）：文件系统系统调用次数，以及 `getdents64`、`read`、`pread64`、`readv` 的返回字节。夹具含 preamble-only 的 open 文件和 open tail，pass 返回前排上的 `time.AfterFunc` 也会算进窗口。`touch -r` 保留 mtime 但改不了 ctime，所以原地改写会重新读；但同一个 ctime tick（内核粗粒度时钟）内的改写看不出来，复核器发现 sha256 结论翻转时调用 `ForgetFile` 把该路径从所有缓存里去掉；满了整表清空（被保留策略删掉的段不会永远留在缓存里）。账本内容、封存损坏、损坏检查、`SegmentPositions` 四个缓存和评估器的目录指纹都用它。
- `seal_sums_test.go`: 账本往返、坏行、压缩；真实 MySQL 8.0 / MariaDB 11.4 段翻一个 CRC 覆盖的字节后 sealed 读法报 `ErrEventChecksum`、open 读法不报；缓存有上限且 ctime 变了就失效。
- #361：事件 size 按读取器的真实剩余长度校验（文件或内存读取器），不再按 -1 放行；`Ledger.Unreadable` / `SegmentUnreadable` 把封存段里 >1GiB 或超出文件末尾的 size 报为读不了（`Contents` 对超出末尾仍按撕裂尾处理）。#381：`SegmentUnreadable` 返回原因；封存段被截短（不足 4 字节、断在事件头中间），或本地大小与 `.seal-sha256` 里封存时的大小不同（截在事件边界上，读起来像完整段），也报读不了；`StorageProblem.Reason` 带着原因进 `/window` 断点与 `/replay` 警告（不进 JSON）。没有封存大小的旧段不做大小比较。
