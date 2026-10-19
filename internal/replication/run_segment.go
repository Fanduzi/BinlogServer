// Package replication provides module-level functionality for replication.
// input: the run's writer opener, the source session (disk names, resume GTID), the executed GTID set, the checkpoint and catalog stores
// output: the open segment of a run: its file, writer, cursor and catalog row; an append that flushes every event but writes the checkpoint and the OPEN row only at a transaction boundary, batched by checkpointBatchWindow (a held commit is written by the next commit past the window, a heartbeat, a rotate, a seal, or the run exit; #391); a mid-file format description held until the first copied event; a seal for a source switch; and the rotate that seals (or discards, or releases as covered by an UPLOADED epoch) the old file, records the next-file checkpoint and opens the next file under HoldChain when a chain gate is set, then uploads outside that lock (#441); every append and checkpoint flush first passes the run's write fence (ADR 0009 P2-6b, #417)
// pos: phase 3 of MySQLRunner.run (ADR 0008): segment append, rotate and seal
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

type writerOpenerFunc func(task tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error)

// openSegment is the file a run is appending to. No file is open until the
// dump names a source file (a GTID start).
type openSegment struct {
	r        *MySQLRunner
	ctx      context.Context
	task     tasks.Task
	session  *sourceSession
	executed *executedGTID
	carried  *tasks.BinlogFile
	opener   writerOpenerFunc

	file   io.Closer
	writer *binlog.Writer
	// name is the source file name; pos the flushed cursor.
	name      string
	pos       uint32
	startPos  uint32
	createdAt time.Time
	path      string
	// pendingFormat is the format description MySQL sends before a mid-file
	// dump. Its end_log_pos is 0 or the original header position (126 on
	// MySQL 8), which is behind the dump cursor. It is written once, in
	// front of the first copied event, and does not move pos.
	// An idle dump that never copies an event leaves it unwritten so resume
	// does not rewind to that header position. A segment that already has
	// events does not gain a second copy on the next start.
	pendingFormat []byte
	// unsaved is the flushed checkpoint not yet written to the store: the
	// events after the last transaction boundary, or a commit held by the
	// batch window. It is never ahead of the flushed bytes.
	unsaved     *binlog.Checkpoint
	lastSavedAt time.Time
}

// checkpointBatchWindow bounds checkpoint writes on a busy source: a commit
// within this long of the last write is held and written by the next commit
// past the window, a heartbeat, or a rotate. Tests set it to 0.
var checkpointBatchWindow = 200 * time.Millisecond

// openAt opens name at pos through the opener. The handle is kept even on
// error so the run's deferred close sees it.
func (s *openSegment) openAt(name string, pos uint32) error {
	var err error
	s.file, s.writer, s.path, err = s.opener(s.task, name, pos)
	return err
}

// close is the run's deferred close of whatever file is still open.
func (s *openSegment) close() {
	// A commit the batch window held is written before the run lets go,
	// on a context the run's cancel does not end. It is a flushed
	// boundary, so it is never ahead of the file.
	if s.file != nil && s.unsaved != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
		runCtx := s.ctx
		s.ctx = ctx
		if err := s.flushCheckpoint(); err != nil {
			log.Printf("held checkpoint not written task=%s file=%s pos=%d err=%v", s.task.ID, s.unsaved.File, s.unsaved.Pos, err)
		}
		s.ctx = runCtx
		cancel()
	}
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			logCloseFailed(s.path, err)
		}
	}
}

// appendAndPersist appends and flushes raw. The checkpoint and the OPEN
// catalog row are written only when commit is true (a transaction boundary)
// and the batch window has passed; otherwise the flushed checkpoint is held
// in unsaved. Every event used to write both, so a 20k-event transaction did
// ~28k meta INSERTs (#391).
func (s *openSegment) appendAndPersist(raw []byte, next binlog.Checkpoint, commit bool) error {
	// A thawed owner reaches here with events it read before it froze.
	if err := s.r.fenceWrite(s.task, "append"); err != nil {
		return err
	}
	if err := s.writer.Append(raw, next); err != nil {
		return err
	}
	// 只有 writer flush 成功后才推进 checkpoint：
	// “已推进 checkpoint” 在语义上等价于“数据已安全落盘”。
	if err := s.writer.FlushAndCheckpoint(); err != nil {
		return err
	}

	checkpoint := s.writer.CurrentCheckpoint()
	s.pos = checkpoint.Pos
	if !commit {
		// Inside a transaction: keep the last saved checkpoint, which is
		// behind these bytes. The open-tail reconcile reads them back.
		return nil
	}
	s.unsaved = &checkpoint
	if checkpointBatchWindow > 0 && !s.lastSavedAt.IsZero() && time.Since(s.lastSavedAt) < checkpointBatchWindow {
		return nil
	}
	return s.flushCheckpoint()
}

// flushCheckpoint writes the held boundary checkpoint and the OPEN row.
func (s *openSegment) flushCheckpoint() error {
	if s.unsaved == nil || s.writer == nil {
		return nil
	}
	checkpoint := *s.unsaved
	r := s.r
	if err := r.fenceWrite(s.task, "checkpoint"); err != nil {
		return err
	}
	if r.checkpointStore != nil {
		if err := r.checkpointStore.UpsertCheckpoint(s.ctx, s.task.ID, checkpoint); err != nil {
			return checkpointWriteError(err)
		}
	}
	if r.fileMetaStore != nil {
		info, err := os.Stat(s.path)
		if err != nil {
			return err
		}
		if err := r.fileMetaStore.UpsertBinlogFile(s.ctx, carryUpload(tasks.BinlogFile{
			TaskID:      s.task.ID,
			FileName:    s.session.diskName(s.name),
			FilePath:    s.path,
			Epoch:       catalogEpoch(s.path, s.task.Epoch),
			State:       "OPEN",
			SizeBytes:   info.Size(),
			StartPos:    tasks.FilePos(s.startPos),
			EndPos:      tasks.FilePos(checkpoint.Pos),
			CreatedAt:   s.createdAt,
			UploadState: "LOCAL_ONLY",
		}, s.carried)); err != nil {
			err = tasks.NewMetadataStoreError("upsert binlog file", err)
			return err
		}
	}
	s.unsaved = nil
	s.lastSavedAt = time.Now()
	return nil
}

// persistRaw appends raw, preceded by a held format description when the
// segment has no events yet.
func (s *openSegment) persistRaw(raw []byte, next binlog.Checkpoint, commit bool) error {
	payload := raw
	if len(s.pendingFormat) > 0 {
		if !segmentHasEvents(s.path) {
			payload = make([]byte, 0, len(s.pendingFormat)+len(raw))
			payload = append(payload, s.pendingFormat...)
			payload = append(payload, raw...)
		}
		s.pendingFormat = nil
	}
	return s.appendAndPersist(payload, next, commit)
}

// checkpointAt is the checkpoint for file at pos (0 keeps the cursor) with
// the executed set. Reaching a file the session owns clears a pending
// switch resume GTID.
func (s *openSegment) checkpointAt(file string, pos uint32) binlog.Checkpoint {
	if pos == 0 {
		pos = s.pos
	}
	cp := binlog.Checkpoint{File: s.session.diskName(file), Pos: pos, GTIDSet: s.executed.current()}
	if s.session.resumeGTID != "" && s.session.owns(cp.File) {
		clearResumeGTID(s.session.dir)
		s.session.resumeGTID = ""
	}
	return cp
}

// seal closes and seals the open file under owner (a source switch).
func (s *openSegment) seal(owner string) error {
	s.pendingFormat = nil
	if s.file == nil {
		return nil
	}
	// The last commit held by the batch window was in the store before
	// per-event writes stopped (#391); keep it there before sealing.
	if err := s.flushCheckpoint(); err != nil {
		return err
	}
	path := s.path
	if err := s.file.Close(); err != nil {
		return err
	}
	s.file = nil
	s.writer = nil
	s.path = ""
	return s.r.sealOpenSegmentFile(s.ctx, s.task, path, owner)
}

// rotate moves from the open file to nextFile at nextPos. A real rotate is
// written into the old file first; an artificial one (end_log_pos 0) is
// not. The old file is sealed, discarded when it holds only magic, or
// released when an UPLOADED epoch already covers it. The next-file
// checkpoint is written before the upload and the open.
func (s *openSegment) rotate(logPos uint32, raw []byte, nextFile string, nextPos uint32, artificial bool, sourceServerUUID string) error {
	r, ctx, task := s.r, s.ctx, s.task
	// 真实 rotate 必须先写入旧文件，再封口旧文件并切到新文件。
	// artificial rotate 不写入：它的 CRC 能通过 mysqlbinlog，但字节
	// 不属于源文件，留下后 DurableCursor 会丢掉整段的续传位点。
	sealPos := s.pos
	if artificial {
		if err := s.flushCheckpoint(); err != nil {
			return err
		}
	}
	if !artificial {
		rotateCheckpoint := s.checkpointAt(s.name, logPos)
		// A rotate is outside any transaction; write it at once.
		lastSaved := s.lastSavedAt
		s.lastSavedAt = time.Time{}
		if err := s.persistRaw(raw, rotateCheckpoint, true); err != nil {
			s.lastSavedAt = lastSaved
			return err
		}
		sealPos = rotateCheckpoint.Pos
	}
	s.pendingFormat = nil

	if err := s.file.Close(); err != nil {
		return err
	}
	s.file = nil
	// Magic only: the file/pos name opened before a 1236 fallback,
	// or a rotate that arrived before any copied event. Sealing it
	// catalogs a segment mysqlbinlog --verify-binlog-checksum rejects.
	discardEmpty := s.path != "" && !segmentHasEvents(s.path)
	var sealed tasks.BinlogFile
	covered := false
	if discardEmpty {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		// A takeover copy of an already uploaded epoch ends at the
		// resume cursor. start_pos and end_pos are both that position,
		// and the bytes are the object already in the bucket. Sealing
		// it uploads a second copy and replay lists the source file twice.
		coveredEpoch := catalogEpoch(s.path, task.Epoch)
		var err error
		covered, err = r.coveredByUploadedEpoch(ctx, task.ID, s.session.diskName(s.name), coveredEpoch, s.path, sealPos)
		if err != nil {
			return err
		}
		if covered {
			if task.Epoch > 0 && r.leaseVerifier != nil {
				ok, err := r.leaseVerifier.Verify(ctx, task.ID, task.OwnerWorkerID, task.Epoch)
				if err != nil {
					return err
				}
				if !ok {
					return ErrLeaseEpochMismatch
				}
			}
			if err := r.fenceWrite(task, "release covered open"); err != nil {
				return err
			}
			if err := r.releaseCoveredOpen(ctx, task.ID, s.session.diskName(s.name), s.path, coveredEpoch); err != nil {
				return err
			}
		} else {
			// Record the sealed row before the put. A crash inside the put
			// leaves UPLOAD_FAILED, which the background retry already uploads.
			// The next-file checkpoint is written before the put so a hung
			// upload cannot pin the lease, and a timeout still continues here.
			sealed, err = r.sealLocalFile(
				ctx,
				task,
				sourceServerUUID,
				s.path,
				s.startPos,
				sealPos,
				s.createdAt,
				time.Now(),
			)
			if err != nil {
				return err
			}
		}
	}

	s.name = nextFile
	s.pos = nextPos

	// Record the next file before opening it, and open it before anyone
	// else can read the pair. The checkpoint stays ahead of the create so
	// a crash resumes on the next file instead of beside the sealed one.
	// The upload is after the open and outside the lock: a hung put must
	// not pin the lease, and must not leave "checkpoint on the next file,
	// file not listed" visible for the whole put (#441).
	release := func() {}
	if r.chainGate != nil {
		release = r.chainGate.HoldChain(task.ID)
	}
	defer release()
	if r.checkpointStore != nil {
		if err := r.checkpointStore.UpsertCheckpoint(ctx, task.ID, s.checkpointAt(s.name, s.pos)); err != nil {
			return checkpointWriteError(err)
		}
	}
	if err := s.openAt(s.name, s.pos); err != nil {
		return err
	}
	release()
	if !discardEmpty && !covered {
		if err := r.uploadSealed(ctx, sealed); err != nil {
			return err
		}
	}
	s.startPos = s.pos
	s.createdAt = time.Now()
	return nil
}
