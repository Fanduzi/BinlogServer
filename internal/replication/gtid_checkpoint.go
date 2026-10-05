// Package replication provides module-level functionality for replication.
// input: a flavor-specific GTID seed and binlog events from the dump
// output: the executed GTID set written on a flushed checkpoint, and MySQL 1236 detection for file/pos resume
// pos: GTID memory for checkpoint writes so a purged source file can resume by GTID
// note: if this file changes, update this header and module README.md.
package replication

import (
	"errors"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

// mysqlErrMasterFatalReadingBinlog is MySQL 1236, ER_MASTER_FATAL_ERROR_READING_BINLOG.
// A file/pos dump returns it when the requested binlog is gone.
const mysqlErrMasterFatalReadingBinlog uint16 = 1236

// executedGTID is the set of transactions fully flushed.
// A GTID event is pending until its commit (XID, COMMIT, or an autocommit statement)
// so a crash mid-transaction does not record a GTID the local file does not contain.
type executedGTID struct {
	flavor  string
	set     gomysql.GTIDSet
	pending gomysql.GTIDSet
	inTxn   bool
}

func newExecutedGTID(flavor, seed string) *executedGTID {
	if flavor == "" {
		flavor = gomysql.MySQLFlavor
	}
	t := &executedGTID{flavor: flavor}
	t.union(seed)
	return t
}

func gtidSeed(taskGTID, storedGTID string, ok bool) string {
	if ok {
		if g := strings.TrimSpace(storedGTID); g != "" {
			return g
		}
	}
	return strings.TrimSpace(taskGTID)
}

func (t *executedGTID) current() string {
	if t == nil || t.set == nil || t.set.IsEmpty() {
		return ""
	}
	return t.set.String()
}

func (t *executedGTID) note(ev *replication.BinlogEvent) {
	if t == nil || ev == nil || ev.Header == nil {
		return
	}
	switch ev.Header.EventType {
	case replication.ANONYMOUS_GTID_EVENT:
		t.pending = nil
		t.inTxn = false
	case replication.GTID_EVENT, replication.GTID_TAGGED_LOG_EVENT, replication.MARIADB_GTID_EVENT:
		t.noteGTID(ev)
	case replication.PREVIOUS_GTIDS_EVENT:
		if e, ok := ev.Event.(*replication.PreviousGTIDsEvent); ok {
			t.union(e.GTIDSets)
		}
	case replication.MARIADB_GTID_LIST_EVENT:
		if e, ok := ev.Event.(*replication.MariadbGTIDListEvent); ok {
			for _, g := range e.GTIDs {
				t.union(g.String())
			}
		}
	case replication.XID_EVENT:
		t.commit()
	case replication.QUERY_EVENT:
		t.noteQuery(ev)
	}
}

func (t *executedGTID) noteGTID(ev *replication.BinlogEvent) {
	next, ok := ev.Event.(gomysql.BinlogGTIDEvent)
	if !ok {
		t.pending = nil
		t.inTxn = false
		return
	}
	gtid, err := next.GTIDNext()
	if err != nil {
		t.pending = nil
		t.inTxn = false
		return
	}
	if maria, ok := ev.Event.(*replication.MariadbGTIDEvent); ok && maria.IsStandalone() {
		t.union(gtid.String())
		t.pending = nil
		t.inTxn = false
		return
	}
	t.pending = gtid
	t.inTxn = false
}

func (t *executedGTID) noteQuery(ev *replication.BinlogEvent) {
	qe, ok := ev.Event.(*replication.QueryEvent)
	if !ok || t.pending == nil {
		return
	}
	upper := strings.ToUpper(strings.TrimSpace(string(qe.Query)))
	head := queryWord(upper)
	if head == "" {
		return
	}
	switch head {
	case "BEGIN":
		t.inTxn = true
		return
	case "START":
		if strings.HasPrefix(upper, "START TRANSACTION") {
			t.inTxn = true
		}
		return
	case "COMMIT":
		t.commit()
		return
	case "ROLLBACK":
		if strings.HasPrefix(upper, "ROLLBACK TO") {
			return
		}
		t.commit()
		return
	case "XA":
		if strings.HasPrefix(upper, "XA COMMIT") || strings.HasPrefix(upper, "XA ROLLBACK") {
			t.commit()
		}
		return
	}
	if t.inTxn {
		return
	}
	t.commit()
}

func (t *executedGTID) commit() {
	if t.pending == nil {
		t.inTxn = false
		return
	}
	t.union(t.pending.String())
	t.pending = nil
	t.inTxn = false
}

func (t *executedGTID) union(gtid string) {
	gtid = strings.TrimSpace(gtid)
	if gtid == "" {
		return
	}
	if t.set == nil {
		set, err := gomysql.ParseGTIDSet(t.flavor, gtid)
		if err != nil {
			return
		}
		t.set = set
		return
	}
	_ = t.set.Update(gtid)
}

func queryWord(upper string) string {
	if i := strings.IndexAny(upper, " \t\r\n"); i >= 0 {
		return upper[:i]
	}
	return upper
}

func mysqlError1236(err error) bool {
	if err == nil {
		return false
	}
	var my *gomysql.MyError
	if errors.As(err, &my) && my.Code == mysqlErrMasterFatalReadingBinlog {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "ERROR 1236") || strings.Contains(msg, "Error 1236")
}
