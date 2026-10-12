// Package replication provides module-level functionality for replication.
// input: the run's writer opener, the source session (disk names, resume GTID), the executed GTID set, the checkpoint and catalog stores
// output: the open segment of a run: its file, writer, cursor and catalog row; an append that flushes before the checkpoint and the OPEN row are written; a mid-file format description held until the first copied event; a seal for a source switch; and the rotate that seals (or discards, or releases as covered by an UPLOADED epoch) the old file, records the next-file checkpoint before the put, uploads, and opens the next file
// pos: phase 3 of MySQLRunner.run (ADR 0008): segment append, rotate and seal
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"io"
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
}

// openAt opens name at pos through the opener. The handle is kept even on
// error so the run's deferred close sees it.
func (s *openSegment) openAt(name string, pos uint32) error {
	var err error
	s.file, s.writer, s.path, err = s.opener(s.task, name, pos)
	return err
}

// close is the run's deferred close of whatever file is still open.
func (s *openSegment) close() {
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			logCloseFailed(s.path, err)
		}
	}
}

func (s *openSegment) appendAndPersist(raw []byte, next binlog.Checkpoint) error {
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
	r := s.r
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
			return err
		}
	}
	return nil
}

// persistRaw appends raw, preceded by a held format description when the
// segment has no events yet.
func (s *openSegment) persistRaw(raw []byte, next binlog.Checkpoint) error {
	payload := raw
	if len(s.pendingFormat) > 0 {
		if !segmentHasEvents(s.path) {
			payload = make([]byte, 0, len(s.pendingFormat)+len(raw))
			payload = append(payload, s.pendingFormat...)
			payload = append(payload, raw...)
		}
		s.pendingFormat = nil
	}
	return s.appendAndPersist(payload, next)
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
	if !artificial {
		rotateCheckpoint := s.checkpointAt(s.name, logPos)
		if err := s.persistRaw(raw, rotateCheckpoint); err != nil {
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

	// Record the next file before opening it. A real rotate is already
	// in the sealed file. An artificial one is not, because it is
	// not part of the source binlog. If that open fails, the retry
	// must start on the next file. Resuming on the sealed name
	// opens <name>.open.e<epoch> beside it, and the next rotate
	// stops on "sealed file already exists".
	if r.checkpointStore != nil {
		if err := r.checkpointStore.UpsertCheckpoint(ctx, task.ID, s.checkpointAt(s.name, s.pos)); err != nil {
			return checkpointWriteError(err)
		}
	}
	if !discardEmpty && !covered {
		if err := r.uploadSealed(ctx, sealed); err != nil {
			return err
		}
	}

	if err := s.openAt(s.name, s.pos); err != nil {
		return err
	}
	s.startPos = s.pos
	s.createdAt = time.Now()
	return nil
}
