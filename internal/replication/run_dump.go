// Package replication provides module-level functionality for replication.
// input: the run plan, a syncer factory, the open segment's flushed cursor, the executed GTID set, and the source session
// output: the run's dump connection: the first open (with the noted-connection KILL before it and the identity check and 1236 text after a failure), one event at a time with the 1236 GTID fallback before the first event, up to maxDumpReconnectAttempts reconnects from the flushed file/pos (executed set as the 1236 fallback), and the GTID reopen after a source switch; plus the classification of how a stream error ends the run
// pos: phases 2 and 5 of MySQLRunner.run (ADR 0008): connect/handshake, reconnect, and error/retry classification
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"binlog_server/internal/tasks"

	"github.com/go-mysql-org/go-mysql/replication"
)

// dumpConn is the run's current Binlog Dump and its reconnect budget.
type dumpConn struct {
	cfg       replication.BinlogSyncerConfig
	newSyncer func(replication.BinlogSyncerConfig) binlogSyncer
	syncer    binlogSyncer
	streamer  binlogStreamer
	// sawEvent is the first event from this dump. MySQL 1236 for a purged
	// file arrives on GetEvent after StartSync has already returned nil.
	// Falling back later would skip events already written.
	sawEvent         bool
	gtidFallbackUsed bool
	reconnects       int
	// stalled is set when the dump went silent past the read timeout; the
	// run ends at once and the dead dump is not waited for.
	stalled bool
}

// dumpStillOpen is the retryable error for a dump whose KILL is unconfirmed.
func dumpStillOpen(id uint32) error {
	return tasks.NewRetryableSourceError(tasks.CodeSourceUnreachable, fmt.Sprintf("binlog dump connection %d still open", id))
}

// streamEnded is how a stream error ends the run: a cancelled run returns
// nil, anything else returns the error unchanged.
func streamEnded(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// releaseNotedDump kills a dump an earlier run left open. The previous Close
// tried KILL with the password from that dump. A password change during
// Stop makes that KILL fail and leaves the thread up. Kill it with the
// password this run is about to use before opening another dump. An
// unconfirmed KILL does not open one.
func (l *runLoop) releaseNotedDump() error {
	r, task := l.r, l.task
	if id := r.notedDumpConn(task.ID); id > 0 {
		r.releaseDumpConn(task, id)
		if left := r.notedDumpConn(task.ID); left > 0 {
			return dumpStillOpen(left)
		}
	}
	return nil
}

// replaceSyncer closes the current dump and creates the next syncer.
func (l *runLoop) replaceSyncer() error {
	l.r.closeDump(l.task, l.dump.syncer)
	if left := l.r.notedDumpConn(l.task.ID); left > 0 {
		return dumpStillOpen(left)
	}
	l.dump.syncer = l.dump.newSyncer(l.dump.cfg)
	return nil
}

// openFirstDump opens the dump at the planned start.
func (l *runLoop) openFirstDump() error {
	dumpStart := l.start
	dumpStart.File = l.session.serverName(l.start.File)
	var err error
	l.dump.streamer, l.start, err = openDump(l.dump.syncer, l.flavor, dumpStart, l.gtidFallback)
	if err != nil {
		if l.ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			if switched := l.r.noteStreamErrorIdentity(l.ctx, l.task, l.session, l.executed.current(), l.seg.name, l.seg.pos, l.seg.seal); switched != nil {
				return switched
			}
		}
		return resumePurgedWithoutGTID(classifySourceError(err), l.start.Mode, l.gtidFallback)
	}
	l.r.publishDumpConn(l.task, l.dump.syncer)
	l.session.seenConn = dumpConnectionID(l.dump.syncer)
	return nil
}

// durableDumpStart is the flushed position. The open segment's file and
// flushed position come first, in either mode. A GTID dump would send that
// file's header and any unfinished transaction again from its GTID event,
// both behind the cursor. The executed set is the 1236 fallback.
func (l *runLoop) durableDumpStart() tasks.StartConfig {
	if l.trackGTID {
		if g := strings.TrimSpace(l.executed.current()); g != "" {
			l.gtidFallback = g
		}
	}
	s := l.seg
	if s.file != nil && s.name != "" && s.pos != 0 {
		return tasks.StartConfig{Mode: tasks.StartModeFilePos, File: l.session.serverName(s.name), Pos: s.pos}
	}
	if l.start.Mode == tasks.StartModeGTID {
		gtid := strings.TrimSpace(l.executed.current())
		if gtid == "" {
			gtid = strings.TrimSpace(l.gtidFallback)
		}
		if gtid == "" {
			gtid = strings.TrimSpace(l.start.GTIDSet)
		}
		return tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: gtid}
	}
	if s.name != "" && s.pos != 0 {
		return tasks.StartConfig{Mode: tasks.StartModeFilePos, File: l.session.serverName(s.name), Pos: s.pos}
	}
	out := l.start
	out.File = l.session.serverName(out.File)
	return out
}

func (l *runLoop) reopenDurableDump() error {
	next := l.durableDumpStart()
	log.Printf("dump reconnect %d/%d task=%s mode=%s file=%s pos=%d", l.dump.reconnects, maxDumpReconnectAttempts, l.task.ID, next.Mode, next.File, next.Pos)
	if err := l.replaceSyncer(); err != nil {
		return err
	}
	var dumpErr error
	l.dump.streamer, l.start, dumpErr = openDump(l.dump.syncer, l.flavor, next, l.gtidFallback)
	if dumpErr != nil {
		return classifySourceError(dumpErr)
	}
	l.r.publishDumpConn(l.task, l.dump.syncer)
	l.session.seenConn = dumpConnectionID(l.dump.syncer)
	l.dump.sawEvent = false
	l.dump.gtidFallbackUsed = false
	return nil
}

// fallbackToGTID reopens a file/pos dump that hit MySQL 1236 before any
// event as a GTID dump from the flushed set.
func (l *runLoop) fallbackToGTID() error {
	l.dump.gtidFallbackUsed = true
	log.Printf("file/pos resume hit MySQL 1236 before any event; opening GTID dump task=%s file=%s pos=%d", l.task.ID, l.start.File, l.start.Pos)
	if err := l.replaceSyncer(); err != nil {
		return err
	}
	gtidStart := tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: strings.TrimSpace(l.gtidFallback)}
	var dumpErr error
	l.dump.streamer, l.start, dumpErr = openDump(l.dump.syncer, l.flavor, gtidStart, "")
	if dumpErr != nil {
		return classifySourceError(dumpErr)
	}
	l.r.publishDumpConn(l.task, l.dump.syncer)
	l.session.seenConn = 0
	return nil
}

// readEvent is the next event, after any 1236 fallback or reconnect.
func (l *runLoop) readEvent() (*replication.BinlogEvent, error) {
	for {
		event, err := l.r.nextEvent(l.ctx, l.dump.streamer, l.task.ID, l.task.Source, l.seg.name, l.seg.pos, &l.atTip)
		if err != nil {
			if l.ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil, err
			}
			// A stall ends the run: the scheduler leaves RUNNING, backs off
			// and reconnects. Probing identity or reopening here would wait on
			// the same silent path (#338 re-QA-2).
			if errors.Is(err, errDumpStalled) {
				l.dump.stalled = true
				return nil, err
			}
			// 1236 from a server the VIP just moved to is not a purge of the
			// file this task was reading. Check identity before the GTID
			// fallback, which would keep writing the old file name.
			if switched := l.r.noteStreamErrorIdentity(l.ctx, l.task, l.session, l.executed.current(), l.seg.name, l.seg.pos, l.seg.seal); switched != nil {
				return nil, switched
			}
			if !l.dump.sawEvent && !l.dump.gtidFallbackUsed && l.start.Mode == tasks.StartModeFilePos && mysqlError1236(err) && strings.TrimSpace(l.gtidFallback) != "" {
				if err := l.fallbackToGTID(); err != nil {
					return nil, err
				}
				continue
			}
			if tasks.IsSourceUnreachable(err) && l.dump.reconnects < maxDumpReconnectAttempts {
				l.dump.reconnects++
				if waitErr := waitDumpReconnect(l.ctx); waitErr != nil {
					return nil, waitErr
				}
				if reopenErr := l.reopenDurableDump(); reopenErr != nil {
					return nil, reopenErr
				}
				continue
			}
			return nil, resumePurgedWithoutGTID(err, l.start.Mode, l.gtidFallback)
		}
		l.dump.reconnects = 0
		if event != nil {
			l.dump.sawEvent = true
		}
		return event, nil
	}
}

// reopenAfterSwitch opens a GTID dump from the session's resume set after
// a source switch.
func (l *runLoop) reopenAfterSwitch() error {
	gtid := strings.TrimSpace(l.session.resumeGTID)
	if gtid == "" {
		return tasks.NewPermanentError(tasks.CodeSourceSwitchover, "source switched and the executed GTID set is empty")
	}
	if err := l.replaceSyncer(); err != nil {
		return err
	}
	gtidStart := tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: gtid}
	var dumpErr error
	l.dump.streamer, l.start, dumpErr = openDump(l.dump.syncer, l.flavor, gtidStart, "")
	if dumpErr != nil {
		return classifySourceError(dumpErr)
	}
	l.r.publishDumpConn(l.task, l.dump.syncer)
	l.session.seenConn = dumpConnectionID(l.dump.syncer)
	l.atTip = false
	l.dump.sawEvent = false
	return nil
}

// pump reads until the run ends. With handle false (semi-sync) the syncer's
// own goroutine hands events to handleEvent and this only waits for an
// error or cancellation.
func (l *runLoop) pump(handle bool) error {
	for {
		event, err := l.readEvent()
		if errors.Is(err, errSwitchRestart) {
			if err := l.reopenAfterSwitch(); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return streamEnded(l.ctx, err)
		}
		if event == nil || !handle {
			continue
		}
		if err := l.handleEvent(event); err != nil {
			if errors.Is(err, errSwitchRestart) {
				if err := l.reopenAfterSwitch(); err != nil {
					return err
				}
				continue
			}
			return err
		}
	}
}
