// Package replication provides module-level functionality for replication.
// input: each copied binlog event of a run, in order, with a decoder for raw query bodies
// output: whether the segment stands at a transaction boundary after that event (XID, XA PREPARE, a compressed transaction payload, COMMIT/ROLLBACK/XA COMMIT, an autocommit statement, or any event outside a transaction), how long the open transaction has run, and a once-per-interval log line when a transaction runs past longTxnWarnAfter with no commit
// pos: the commit rule for checkpoint writes (#391): the runner writes the checkpoint and the OPEN catalog row only at a boundary, so a large transaction costs one meta write, not one per row event
// note: if this file changes, update this header and module README.md.
package replication

import (
	"log"
	"time"

	"binlog_server/internal/binlog"

	"github.com/go-mysql-org/go-mysql/replication"
)

// longTxnWarnAfter is how long a transaction may stream with no commit before
// the run logs that its checkpoint is not advancing. Tests shorten it.
var longTxnWarnAfter = 2 * time.Minute

// txnBoundary follows transaction framing across copied events.
type txnBoundary struct {
	inTxn    bool
	begun    bool
	openedAt time.Time
	warnedAt time.Time
	events   int
}

// observe classifies ev and reports whether the stream is at a transaction
// boundary after it. decode turns a raw query event into a typed one.
func (b *txnBoundary) observe(ev *replication.BinlogEvent, decode func(*replication.BinlogEvent) (*replication.BinlogEvent, bool), now time.Time) bool {
	switch ev.Header.EventType {
	case replication.GTID_EVENT, replication.GTID_TAGGED_LOG_EVENT, replication.ANONYMOUS_GTID_EVENT, replication.MARIADB_GTID_EVENT:
		b.open(now)
		return false
	case replication.XID_EVENT, replication.XA_PREPARE_LOG_EVENT, replication.TRANSACTION_PAYLOAD_EVENT:
		b.close()
		return true
	case replication.QUERY_EVENT:
		q, ok := ev.Event.(*replication.QueryEvent)
		if !ok && decode != nil {
			if d, dok := decode(ev); dok {
				q, ok = d.Event.(*replication.QueryEvent)
			}
		}
		if !ok {
			if b.inTxn {
				b.events++
				return false
			}
			return true
		}
		switch binlog.TransactionQueryEffect(string(q.Query), b.begun) {
		case binlog.QueryBegins:
			if !b.inTxn {
				b.open(now)
			}
			b.begun = true
			return false
		case binlog.QueryCommits:
			b.close()
			return true
		}
		b.events++
		return false
	}
	if b.inTxn {
		b.events++
		return false
	}
	return true
}

func (b *txnBoundary) open(now time.Time) {
	b.inTxn, b.begun, b.openedAt, b.warnedAt, b.events = true, false, now, time.Time{}, 0
}

func (b *txnBoundary) close() {
	b.inTxn, b.begun, b.events = false, false, 0
}

// warnLong logs, at most once per longTxnWarnAfter, that the open
// transaction has streamed that long with no commit: bytes arrive, so the
// liveness clock stays quiet, but the checkpoint does not move (#391).
func (b *txnBoundary) warnLong(taskID, file string, pos uint32, now time.Time) bool {
	if !b.inTxn || longTxnWarnAfter <= 0 || now.Sub(b.openedAt) < longTxnWarnAfter {
		return false
	}
	if !b.warnedAt.IsZero() && now.Sub(b.warnedAt) < longTxnWarnAfter {
		return false
	}
	b.warnedAt = now
	log.Printf("long transaction task=%s open_for=%s events=%d file=%s pos=%d: no commit yet, checkpoint not advancing; a drop restarts this transaction", taskID, now.Sub(b.openedAt).Round(time.Second), b.events, file, pos)
	return true
}
