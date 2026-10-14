// Package replication provides module-level functionality for replication.
// input: one binlog event from the dump, the open segment, the executed GTID set, and the source session
// output: what that event does to the backup: an identity check before anything is appended, heartbeats reported as progress, the dump's synthetic rotate moving only the cursor, a first rotate opening the named file, a real or artificial rotate sealing and moving on (openSegment.rotate), a mid-file format description held for the first copied event, a stored GTID skipped with the rest of its transaction, a re-sent file header ignored, an event behind the flushed cursor returned as STREAM_REGRESSION, and every other event flushed with its checkpoint
// pos: phase 4 of MySQLRunner.run (ADR 0008): the event handler shared by the async loop and the semi-sync SynchronousEventHandler
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"fmt"
	"strings"
	"time"

	"binlog_server/internal/tasks"

	"github.com/go-mysql-org/go-mysql/replication"
)

// runLoop is one run after its start is resolved: the plan, the open
// segment and the dump.
type runLoop struct {
	r    *MySQLRunner
	ctx  context.Context
	task tasks.Task
	*runStart
	seg  *openSegment
	dump *dumpConn
	// skipStoredTxn drops the rest of a transaction whose GTID is already
	// in the flushed set. The commit of that transaction clears it.
	skipStoredTxn bool
	streamCRC     bool
}

func (l *runLoop) reportProgress(at time.Time) {
	if l.r.progressReporter != nil {
		l.r.progressReporter.ReportReplicationProgress(l.task.ID, at, l.seg.name, l.seg.pos, l.atTip)
	}
}

func (l *runLoop) handleEvent(event *replication.BinlogEvent) error {
	if event == nil || event.Header == nil {
		return nil
	}
	seg := l.seg
	// A reconnect that lands on a different server must not append this event.
	if err := l.r.noteDumpIdentity(l.ctx, l.task, l.session, l.dump.syncer, l.executed.current(), seg.name, seg.pos, seg.seal); err != nil {
		return err
	}
	l.sourceServerUUID = l.session.active
	// The format description sets the checksum flag used to decode later
	// raw GTID bodies. Noting it does not add a transaction.
	if event.Header.EventType == replication.FORMAT_DESCRIPTION_EVENT {
		if l.trackGTID {
			l.executed.note(event)
		}
		l.streamCRC = formatDescriptionHasCRC(event)
	}
	sourceEventAt := sourceEventTime(event)

	if event.Header.EventType == replication.HEARTBEAT_EVENT || event.Header.EventType == replication.HEARTBEAT_LOG_EVENT_V2 {
		l.reportProgress(sourceEventAt)
		return nil
	}

	if event.Header.EventType == replication.ROTATE_EVENT {
		rotate, ok := event.Event.(*replication.RotateEvent)
		if ok && len(rotate.NextLogName) > 0 {
			return l.handleRotate(event, rotate, sourceEventAt)
		}
	}

	// A GTID dump names the file with Rotate before any copied event.
	// Until that name exists there is nowhere to write, and the name is
	// not invented as task-{id}.binlog.
	if seg.file == nil {
		if event.Header.EventType == replication.FORMAT_DESCRIPTION_EVENT && len(event.RawData) > 0 {
			seg.pendingFormat = append([]byte(nil), event.RawData...)
		}
		return nil
	}

	// 从文件中部 dump 时，源库仍会先下发文件头的 format description（log_pos 置 0，
	// 或保留原始 end_log_pos，例如 MySQL 8 的 126）。该事件不在当前位点之后。
	// 单独落盘会把续传位点退回 126，并用文件创建时间报 DELAYED。
	// 这里只记住原文，等第一个真正复制的事件一起写入，位点停在那个事件上。
	// synthetic rotate 已在上面处理，这里不能抢在它前面把 log_pos=0 丢掉。
	if event.Header.EventType == replication.FORMAT_DESCRIPTION_EVENT && event.Header.LogPos <= seg.pos {
		if !segmentHasEvents(seg.path) && len(event.RawData) > 0 {
			seg.pendingFormat = append([]byte(nil), event.RawData...)
		}
		return nil
	}
	// GTID numbers are not compared for order or gaps. A multi-threaded
	// replica with replica_preserve_commit_order=OFF, SET GTID_NEXT, and
	// group replication blocks all write legal holes and out-of-order
	// numbers. Only a GTID already stored is dropped, with the rest of
	// its transaction.
	if l.trackGTID {
		if event.Header.EventType == replication.ANONYMOUS_GTID_EVENT {
			l.skipStoredTxn = false
		}
		if gtid, ok := l.executed.peekGTID(event); ok {
			if l.executed.contains(gtid) {
				l.skipStoredTxn = true
				return nil
			}
			l.skipStoredTxn = false
		} else if l.skipStoredTxn {
			if l.executed.endsTransaction(event) {
				l.skipStoredTxn = false
			}
			return nil
		}
	}
	// MariaDB 11.4+ row events arrive with LogPos=0. A data event gets our
	// cursor plus its size; header and control events are never filled.
	if end := filledEndPos(l.flavor, event.Header, seg.pos); end != 0 {
		event.Header.LogPos = end
		event.RawData = stampLogPosInRaw(event.RawData, end, l.streamCRC)
	}
	// A dump that starts at the top of a file the segment already holds
	// sends that file's header again. Those events are not transactions.
	if event.Header.LogPos <= seg.pos && fileHeaderEvent(event.Header.EventType) {
		return nil
	}
	if event.Header.LogPos != 0 && event.Header.LogPos < seg.pos {
		return streamRegression(fmt.Sprintf("event end pos %d is behind stored pos %d in %s", event.Header.LogPos, seg.pos, seg.name))
	}
	if event.Header.LogPos <= seg.pos {
		return nil
	}
	if l.trackGTID {
		l.executed.note(event)
	}

	next := seg.checkpointAt(seg.name, event.Header.LogPos)
	if err := seg.persistRaw(event.RawData, next); err != nil {
		return err
	}
	l.reportProgress(sourceEventAt)
	return nil
}

func (l *runLoop) handleRotate(event *replication.BinlogEvent, rotate *replication.RotateEvent, sourceEventAt time.Time) error {
	seg := l.seg
	nextFile := strings.TrimSpace(strings.TrimRight(string(rotate.NextLogName), "\x00"))
	nextPos := uint32(rotate.Position)
	// A rotate to an older file, or a real rotate that names the
	// file already open, is an earlier file being appended again.
	if seg.file != nil && sourceFileBehind(seg.name, nextFile) {
		return streamRegression(fmt.Sprintf("rotate to %s is behind open file %s", nextFile, seg.name))
	}
	if seg.file != nil && sameSourceFile(seg.name, nextFile) && event.Header.LogPos != 0 {
		return streamRegression(fmt.Sprintf("rotate to %s repeats open file %s at pos %d", nextFile, seg.name, event.Header.LogPos))
	}
	l.skipStoredTxn = false

	// Artificial rotate（end_log_pos 0）不在源 binlog 文件里。
	// 指向当前文件的是 dump 开头的 synthetic rotate，只更新位点。
	// 指向下一个文件的是源库重启后 sender 补的 rotate：不落盘，
	// 按最后一个真实事件封口并切到下一个文件。写进 open 分段会让
	// 最后一个 log_pos 变成 0，同机接管会把它当成分段不在本机。
	if event.Header.LogPos == 0 && nextFile == seg.name {
		// Position 0 is not a binlog position. Assigning it cleared
		// the cursor, and the next rotate sealed the copied events
		// with end_pos 0. A redump's synthetic rotate also names
		// position 4; moving the cursor backward would append the
		// same events again.
		if nextPos > seg.pos {
			seg.pos = nextPos
		}
		return nil
	}
	artificial := event.Header.LogPos == 0
	if artificial && nextPos == 0 {
		nextPos = 4
	}
	// No segment is open until the dump names a source file.
	// This rotate is that name. It is not written, and nothing
	// is sealed: there is no task-{id}.binlog to close.
	if seg.file == nil {
		seg.name = nextFile
		seg.pos = nextPos
		if err := seg.openAt(seg.name, seg.pos); err != nil {
			return err
		}
		seg.startPos = seg.pos
		seg.createdAt = time.Now()
		l.reportProgress(sourceEventAt)
		return nil
	}
	if err := seg.rotate(event.Header.LogPos, event.RawData, nextFile, nextPos, artificial, l.sourceServerUUID); err != nil {
		return err
	}
	l.reportProgress(sourceEventAt)
	return nil
}
