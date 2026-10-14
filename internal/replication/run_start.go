// Package replication provides module-level functionality for replication.
// input: a task, the source status fetcher, the checkpoint and catalog stores, the task directory, and the sealed-file handler
// output: the run plan: the source identity session, the dump start (fresh LATEST anchor, local durable resume, takeover of a catalog file_path or an UPLOADED object, or the stored checkpoint), the executed GTID seed with the open tail reconciled, the 1236 GTID fallback, whether the dump starts at tip, and the segment directory; or the error that stops the run before any dump opens (source identity, purged sealed uploads, SEGMENT_NOT_ON_WORKER, STORAGE_INCONSISTENT)
// pos: phase 1 of MySQLRunner.run (ADR 0008): resume-position resolution; no dump connection exists yet
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"path/filepath"
	"strings"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

// runStart is where a run begins. The loop keeps mutating start,
// gtidFallback and atTip as the dump reconnects.
type runStart struct {
	sourceServerUUID string
	start            tasks.StartConfig
	stored           binlog.Checkpoint
	checkpointExists bool
	// atTip: the dump starts at the master's file/pos (fresh LATEST).
	atTip bool
	// segmentDir is a takeover directory other than {dataDir}/{task}.
	segmentDir string
	carried    *tasks.BinlogFile
	flavor     string
	// gtidFallback is the executed set from the last flush. File/pos is tried
	// first. MySQL 1236 means that file is gone, and this set is how the dump
	// continues. A rewound position does not carry the set; the stored row does.
	gtidFallback string
	// executed is the GTID set of transactions fully flushed.
	executed  *executedGTID
	trackGTID bool
	session   *sourceSession
}

// resolveRunStart is phase 1: everything decided before the dump opens.
func (r *MySQLRunner) resolveRunStart(ctx context.Context, task tasks.Task) (*runStart, error) {
	// 解析源库标识与复制起点（含 checkpoint 接管修正）。
	// 常见误解：
	// onReady 只表示“复制连接+writer 已就绪”，不代表已经收到第一条业务事件。
	// FILE_POS/GTID 追旧事件时 RUNNING 仍可 DELAYED；fresh LATEST，以及 FILE_POS 起点已经不落后于 master 时，StartSync 成功即在源 tip。
	// dump 在请求位点之前下发的 format description 不是延迟。
	// source_server_uuid 作为 object key 的稳定维度，避免 cluster_key 相同但源实例切换时冲突。
	sourceServerUUID, err := r.fetcher.FetchServerUUID(ctx, task.Source)
	if err != nil {
		return nil, classifySourceError(err)
	}
	sourceServerUUID = strings.TrimSpace(sourceServerUUID)
	if sourceServerUUID == "" {
		return nil, tasks.NewPermanentError(tasks.CodeSourceIdentityUnavailable, "empty source server_uuid")
	}

	// 先解析请求的 start strategy（LATEST/FILE_POS/GTID）。
	requestedLatest := task.Start.Mode == tasks.StartModeLatest || task.Start.Mode == ""
	start, err := ResolveStart(ctx, task, r.fetcher)
	if err != nil {
		return nil, classifySourceError(err)
	}
	var stored binlog.Checkpoint
	checkpointExists := false
	if r.checkpointStore != nil {
		// 持久化 checkpoint 优先级更高，保证重启后的 resumability。
		var ok bool
		stored, ok, err = r.checkpointStore.LoadCheckpoint(ctx, task.ID)
		if err != nil {
			return nil, tasks.NewMetadataStoreError("load checkpoint", err)
		}
		checkpointExists = ok
	}
	// A crash after the rename and before the upload-state write leaves a sealed
	// file that retry cannot see. Record it as UPLOAD_FAILED first, and if the
	// checkpoint is still on that sealed rotate, continue on the next file.
	if r.sealedHandler != nil {
		// Existing files belong to the identity already recorded. A VIP that
		// has moved is not that identity; prepareSourceIdentity decides the switch.
		enrollUUID := sourceServerUUID
		if chain := loadSourceChain(r.sourceDir(task.ID)); len(chain) > 0 {
			enrollUUID = chain[len(chain)-1]
		}
		if err := r.enrollSealedUploads(ctx, task, enrollUUID); err != nil {
			return nil, err
		}
		if checkpointExists && r.checkpointStore != nil {
			if next, advanced := r.checkpointPastSealedRotate(ctx, task.ID, stored); advanced {
				if err := r.checkpointStore.UpsertCheckpoint(ctx, task.ID, next); err != nil {
					return nil, checkpointWriteError(err)
				}
				stored = next
			}
		}
	}
	// Fresh LATEST has already resolved to SHOW MASTER STATUS, so StartSync is at tip.
	// That file/pos is saved below when nothing older exists, with an empty
	// gtid_set. A later retry must not resolve LATEST again (#213). A GTID
	// checkpoint still keeps its set (#175, #199).
	// NextResumePosition is the resume choice shared with GET /api/tasks/{id}/checkpoint.
	// A local complete event wins. Adopt keeps its FILE_POS. Epoch > 1 with no
	// local event used to rewind to position 4. Takeover now follows the catalog
	// file_path: a readable segment directory continues there, an unreadable
	// open segment fails, and a checkpoint already in an UPLOADED object resumes
	// from that object. A readable open file that is only magic, or only a
	// header of events whose end log_pos is 0, continues from the saved
	// checkpoint and is renamed onto this epoch.
	atTip := requestedLatest && !checkpointExists
	// anchorLatest is a first resolution: no stored checkpoint and no local event.
	// Resume and takeover clear it so this attempt does not overwrite them.
	anchorLatest := atTip
	segmentDir := ""
	var carried *tasks.BinlogFile
	flavor := task.Source.Flavor
	if flavor == "" {
		flavor = gomysql.MySQLFlavor
	}
	gtidFallback := ""
	if checkpointExists {
		gtidFallback = strings.TrimSpace(stored.GTIDSet)
	}
	// The seed is the stored checkpoint when one exists, otherwise the GTID
	// the task was created with. An empty checkpoint write used to replace that set.
	executed := newExecutedGTID(flavor, gtidSeed(task.Start.GTIDSet, stored.GTIDSet, checkpointExists))
	// A file/pos task has no seed. Events in the middle of a file are not an
	// executed set, and writing them would make the next 1236 resume too short.
	trackGTID := executed.current() != ""
	// The segment is flushed before the checkpoint row is written, and a
	// resume inside a transaction never saw that transaction's GTID event.
	// Either way the open segment can hold complete transactions the stored
	// gtid_set does not list. Read them back from the file before anything
	// compares GTIDs, and cut a trailing unfinished transaction so the source
	// sends it again whole.
	if trackGTID && !task.KeepLocalSegments {
		if reconciled, ok := r.reconcileOpenTail(task, executed); ok {
			gtidFallback = reconciled
		}
	}
	if resume, ok := tasks.NextResumePosition(r.dataDir, task, stored, checkpointExists); ok {
		start = tasks.StartConfig{
			Mode: tasks.StartModeFilePos,
			File: resume.File,
			Pos:  resume.Pos,
		}
		if g := strings.TrimSpace(resume.GTIDSet); g != "" && !trackGTID {
			gtidFallback = g
		}
		atTip = false
		anchorLatest = false
	}
	if task.Epoch > 1 && !task.KeepLocalSegments {
		if _, _, local := binlog.DurableResume(r.dataDir, task.ID); !local {
			files, err := r.listCatalog(ctx, task.ID)
			if err != nil {
				return nil, err
			}
			decision := tasks.ResolveTakeover(r.dataDir, task, stored, checkpointExists, files)
			if decision.Missing != "" {
				return nil, segmentNotOnWorker(decision.Missing)
			}
			if decision.Covering != nil {
				carried = decision.Covering
				dir, err := r.materializeUploaded(ctx, task, *decision.Covering)
				if err != nil {
					return nil, err
				}
				file, pos, ok := binlog.DurableResumeDir(dir)
				if !ok {
					return nil, segmentNotOnWorker(decision.Covering.FileName)
				}
				start = tasks.StartConfig{Mode: tasks.StartModeFilePos, File: file, Pos: pos}
				segmentDir = dir
				atTip = false
				anchorLatest = false
			} else if decision.Apply {
				start = tasks.StartConfig{
					Mode: tasks.StartModeFilePos,
					File: decision.Checkpoint.File,
					Pos:  decision.Checkpoint.Pos,
				}
				if g := strings.TrimSpace(decision.Checkpoint.GTIDSet); g != "" {
					gtidFallback = g
				}
				dir := decision.Dir
				if dir == "" {
					dir = filepath.Join(r.dataDir, task.ID)
				}
				if err := promoteEpochZeroOpen(dir, decision.Checkpoint.File, task.Epoch, decision.Checkpoint.Pos, files); err != nil {
					return nil, err
				}
				if decision.Dir != "" {
					segmentDir = decision.Dir
				}
				atTip = false
				anchorLatest = false
			}
		}
	}
	if anchorLatest && start.File != "" && start.Pos != 0 {
		// This process already resolved LATEST and the metadata row was not
		// written, or there is no metadata store. Keep that file/pos.
		if file, pos, ok := r.latestAnchor(task.ID); ok {
			start = tasks.StartConfig{Mode: tasks.StartModeFilePos, File: file, Pos: pos}
			atTip = false
		}
		if err := r.persistLatestAnchor(ctx, task.ID, start.File, start.Pos); err != nil {
			return nil, err
		}
	}

	session, start, err := r.prepareSourceIdentity(ctx, task, sourceServerUUID, start, stored, checkpointExists, gtidFallback, requestedLatest && !checkpointExists, segmentDir)
	if err != nil {
		return nil, err
	}

	// A task already damaged by a stale GTID re-dump has a segment that
	// rotates to its own or an older file, or an event position that goes
	// backwards inside one segment. Out-of-order GTIDs are normal on a
	// replica and are not that damage. Starting again would drop more
	// transactions. Refuse before opening another dump.
	scanDir := segmentDir
	if scanDir == "" {
		scanDir = filepath.Join(r.dataDir, task.ID)
	}
	claimedGTID := ""
	if checkpointExists {
		claimedGTID = strings.TrimSpace(stored.GTIDSet)
	}
	if problem, found := binlog.LedgerAt(scanDir).Damage(claimedGTID, tasks.StartGTIDText(task), flavor); found {
		return nil, tasks.NewPermanentError(tasks.CodeStorageInconsistent, problem.Message)
	}
	return &runStart{
		sourceServerUUID: session.active,
		start:            start,
		stored:           stored,
		checkpointExists: checkpointExists,
		atTip:            atTip,
		segmentDir:       segmentDir,
		carried:          carried,
		flavor:           flavor,
		gtidFallback:     gtidFallback,
		executed:         executed,
		trackGTID:        trackGTID,
		session:          session,
	}, nil
}
