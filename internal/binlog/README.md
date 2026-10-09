# internal/binlog Module

## Files
- `writer.go`: binlog 文件写入与旋转。
- `checkpoint.go`: checkpoint 数据结构。
- `durable.go`: 最高 open 分段里最后一个完整事件的源文件名和 end log_pos。封存名和没有完整事件的分段不是续传点。`end_log_pos` 为 0 的事件（源库重启后的 artificial rotate）不是续传位点；前面还有完整事件时用那个事件的 end log_pos，并把字节偏移停在它后面，后面的 artificial 字节可以丢掉。只有 log_pos 0 的分段仍然不是续传点。`PreambleOnly` 表示这段没有续传事件：只有 4 字节 magic，或只有 `end_log_pos` 为 0 的完整事件且文件停在事件边界上。撕掉的尾部、以及 `end_log_pos` 大于 0 的事件，都不是。它不是续传位点。`DurableResumeDir` 直接扫 catalog `file_path` 所在目录。`LastRotateTarget` 在最后一个完整事件是 rotate 时给出下一个文件和位点；格式描述标明 CRC32 时，校验和的 4 个字节不算进文件名。`EventSpan` 给出这段里、结束在最后一个 `end_log_pos` 大于 0 的事件上的连续事件链的起点和终点；中间有空隙时链从空隙后重算，文件头的 format description 不会把中部复制段的 `start_pos` 拉回 4。 `durableOpenSegment` 选出这个最高打开分段，`open_tail.go` 共用同一选择。
- `segment.go`: `ClassifySegment` 是 runner、durable resume 和 tasks 磁盘扫描共用的分段名解析。`mysql-bin.000001` 是封存、epoch -1。`name.open.eN` 是打开的第 N 代。`name.sealed.eN` 是同一源文件名的后一次封存，epoch N，不是续传点。`OpenName` / `SealedName` 是这个解析结果上的谓词。被拒绝的名字（含 `.takeover-*`、`notes.txt`、`task-N.binlog`）两者都为假，保留清理不会把它们当 binlog 删掉。
- `event_time.go`: 读一个分段的事件头，给出复制流里第一个和最后一个非 0 时间戳。时间戳 0 跳过。格式描述（format description）和 previous-GTIDs 的时间是源文件创建时间，不计入覆盖。没有 magic 或没有剩余计时事件时不算覆盖。尾部撕掉的字节保留已经读完的事件时间。
- `gtid_pos.go`: `ScanSegmentForGTID` 在一个分段里找第一个 MySQL `GTID_EVENT`（UUID 不区分大小写，序号相等）。返回的偏移是该事件的起始字节，给 `mysqlbinlog --stop-position` 用，不是事件头里的 `end_log_pos`。previous-GTIDs 不算。没有 magic 时返回空结果。尾部撕掉时保留已经读完的命中。
- `gtid_set_scan.go`: `ScanSegmentGTIDs` 读同一个分段里的 previous-GTIDs 文本，以及每一条 MySQL `GTID_EVENT` 的 UUID、序号、起始字节和事件头时间。格式描述标明 CRC32 时，previous-GTIDs 的校验和尾部先剥掉再解码。尾部撕掉时保留已经读完的事件。previous-GTIDs 解不出来是错误。
- `open_tail.go`: `ReconcileOpenTail` / `ScanOpenTail` 读最高打开分段：每个完整事务的 GTID（MySQL、带 tag 的 GTID、MariaDB）、previous-GTIDs，以及续传截断点。末尾的事务没写完、且它前面正好是一个完整事务结束处时给出截断偏移和位点；否则给出这个事务的 GTID，续传在它里面接着拉。XID、XA_PREPARE、压缩的 TRANSACTION_PAYLOAD、DDL 和 `COMMIT`/`ROLLBACK`/`XA COMMIT` 算提交（`TransactionQueryEffect`）。`UnionGTIDText` 把种子集合和这些 GTID 合起来。只读，不改文件。
- `gtid_intervals.go`: MySQL GTID 集合的区间加、减和输出，给 `storage_consistency.go` 算缺失的 GTID 和新任务起点。
- `storage_consistency.go`: `DetectStorageProblem(dir, checkpoint, start, flavor)` 只认重拉损坏的硬痕迹，并返回第一个损坏分段 `Segment`、它之前仍可恢复的分段 `Valid`、checkpoint 里有但起点集合和任何分段都没有的 MySQL GTID `Missing`，以及新任务起点 `Restart`（起点集合或第一个分段的 previous-GTIDs，加上 `Valid` 的事务；未知时为空）。Rotate 指向自己或更早的同名 binlog（身份前缀 `uuid.` 或回切段前缀 `uuid~n.` 先剥掉），或 `end_log_pos` 在该分段里倒退，才是问题。GTID 序号乱序、多个 UUID 交错、某个序号在下一个分段里、起点集合或 `gtid_purged` 里的空洞，都不是。文件之间的缺口也不拒绝启动。目录不存在不是问题。checkpoint 和 flavor 仍传进来，不参与判断。

## Exports
- 文件写入、rotate 与 checkpoint 推进基础能力。
- `DurableResume` / `DurableResumeDir` / `DurableCursor`：runner 下次 Start 和 `GET /api/tasks/{id}/checkpoint` 共用的本地续传位点。不使用文件大小。末尾 `end_log_pos` 为 0 的 artificial 事件不把整段判成没有位点。`DurableResumeDir` 扫接管时 catalog 记下的分段目录。`PreambleOnly` 只判断这段是不是 magic 或 log_pos 0 的文件头，调用方用已保存的 checkpoint 续，不用这里的位点。
- `LastRotateTarget`：封存文件末尾的 rotate 指向的下一个文件。runner 在 checkpoint 仍停在刚封存的文件上时用它继续，避免在封存名旁边再开一段。
- `EventTimeSpan`：定点恢复用来判断一个分段的复制事件时间是否盖住窗口。格式描述和 previous-GTIDs 不参与。调用方持有 reader。
- `ScanSegmentForGTID`：在分段字节里定位一条 MySQL GTID。偏移是事件起始字节。调用方持有 reader。
- `ScanSegmentGTIDs`：列出分段里的 previous-GTIDs 和全部 MySQL GTID 事件。调用方持有 reader。
- `EventSpan`：封存目录行的 `start_pos` / `end_pos`。起点是连续链里第一个事件的位置，终点是最后一个 `end_log_pos` 大于 0 的事件。`end_log_pos` 为 0 的事件和撕掉的尾部不算。`SegmentPositions` 先过 `DurableCursor`：游标不成功（缺失、只有 magic、撕掉的尾部、只有 log_pos 0）时 ok 为 false；成功时终点就是该游标，起点用 `EventSpan`。

## Dependencies
- Upstream: `internal/replication`。
- Downstream: 本地文件系统。

## Update Rule
- 文件写入语义、checkpoint 语义变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
