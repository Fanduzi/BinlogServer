// Package replication provides module-level functionality for replication.
// input: a flavor-specific GTID seed and binlog events from the dump, including raw-mode events whose body is only in RawData or GenericEvent
// output: the executed GTID set written on a flushed checkpoint, a read-only peek of an event GTID, containment and forward-gap checks against that set, and MySQL 1236 detection for file/pos resume
// pos: GTID memory for checkpoint writes so a purged source file can resume by GTID
// note: if this file changes, update this header and module README.md.
package replication

import (
	"encoding/binary"
	"errors"
	"strconv"
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
	// crc32 is set from the format description. RawData still carries the
	// checksum; GenericEvent.Data from go-mysql already has it removed.
	crc32 bool
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

// peekGTID returns the GTID carried by ev without changing the executed set.
// A raw-mode event is decoded from its body. Events that are not GTID events
// return false.
func (t *executedGTID) peekGTID(ev *replication.BinlogEvent) (string, bool) {
	if t == nil || ev == nil || ev.Header == nil {
		return "", false
	}
	decoded := ev
	if next, ok := t.decodeRaw(ev); ok && next != nil {
		decoded = next
	}
	switch decoded.Header.EventType {
	case replication.GTID_EVENT, replication.GTID_TAGGED_LOG_EVENT, replication.MARIADB_GTID_EVENT:
	default:
		return "", false
	}
	gtidEvent, ok := decoded.Event.(gomysql.BinlogGTIDEvent)
	if !ok || gtidEvent == nil {
		return "", false
	}
	gtid, err := gtidEvent.GTIDNext()
	if err != nil || gtid == nil {
		return "", false
	}
	text := strings.TrimSpace(gtid.String())
	if text == "" {
		return "", false
	}
	return text, true
}

// contains reports whether gtid is already inside the flushed set.
func (t *executedGTID) contains(gtid string) bool {
	if t == nil || t.set == nil {
		return false
	}
	gtid = strings.TrimSpace(gtid)
	if gtid == "" {
		return false
	}
	one, err := gomysql.ParseGTIDSet(t.flavor, gtid)
	if err != nil {
		return false
	}
	return t.set.Contain(one)
}

// forwardGap reports a MySQL GTID whose sequence skips past the flushed set
// for a UUID that set already contains. The next sequence is not a gap.
// A UUID the set does not contain is not a gap. MariaDB sets are not compared
// this way.
func (t *executedGTID) forwardGap(gtid string) bool {
	if t == nil || t.set == nil || isMariaDBFlavor(t.flavor) {
		return false
	}
	uuid, gno, ok := splitMysqlGTID(gtid)
	if !ok {
		return false
	}
	max, known := mysqlMaxGNO(t.set, uuid)
	if !known {
		return false
	}
	return gno > max+1
}

// endsTransaction reports a commit or rollback that finishes the current transaction.
func (t *executedGTID) endsTransaction(ev *replication.BinlogEvent) bool {
	if t == nil || ev == nil || ev.Header == nil {
		return false
	}
	if ev.Header.EventType == replication.XID_EVENT {
		return true
	}
	if ev.Header.EventType != replication.QUERY_EVENT {
		return false
	}
	decoded := ev
	if next, ok := t.decodeRaw(ev); ok && next != nil {
		decoded = next
	}
	qe, ok := decoded.Event.(*replication.QueryEvent)
	if !ok {
		return false
	}
	upper := strings.ToUpper(strings.TrimSpace(string(qe.Query)))
	switch queryWord(upper) {
	case "COMMIT":
		return true
	case "ROLLBACK":
		return !strings.HasPrefix(upper, "ROLLBACK TO")
	case "XA":
		return strings.HasPrefix(upper, "XA COMMIT") || strings.HasPrefix(upper, "XA ROLLBACK")
	default:
		return false
	}
}

func splitMysqlGTID(gtid string) (string, int64, bool) {
	gtid = strings.TrimSpace(gtid)
	colon := strings.LastIndex(gtid, ":")
	if colon <= 0 || colon == len(gtid)-1 {
		return "", 0, false
	}
	gno, err := strconv.ParseInt(gtid[colon+1:], 10, 64)
	if err != nil || gno <= 0 {
		return "", 0, false
	}
	return gtid[:colon], gno, true
}

// mysqlMaxGNO is the highest sequence stored for uuid.
// The interval list is read from GTIDSet.String because the MySQL set's
// intervals are not exported. A UUID that is absent returns false.
func mysqlMaxGNO(set gomysql.GTIDSet, uuid string) (int64, bool) {
	if set == nil {
		return 0, false
	}
	var max int64
	found := false
	for _, part := range strings.Split(set.String(), ",") {
		part = strings.TrimSpace(part)
		colon := strings.Index(part, ":")
		if colon <= 0 || !strings.EqualFold(part[:colon], uuid) {
			continue
		}
		for _, iv := range strings.Split(part[colon+1:], ":") {
			iv = strings.TrimSpace(iv)
			if iv == "" {
				continue
			}
			end := iv
			if dash := strings.Index(iv, "-"); dash >= 0 {
				end = iv[dash+1:]
			}
			n, err := strconv.ParseInt(end, 10, 64)
			if err != nil || n <= 0 {
				continue
			}
			if !found || n > max {
				max = n
				found = true
			}
		}
	}
	return max, found
}

func (t *executedGTID) note(ev *replication.BinlogEvent) {
	if t == nil || ev == nil || ev.Header == nil {
		return
	}
	if ev.Header.EventType == replication.FORMAT_DESCRIPTION_EVENT {
		if fde, ok := ev.Event.(*replication.FormatDescriptionEvent); ok {
			t.crc32 = fde.ChecksumAlgorithm == replication.BINLOG_CHECKSUM_ALG_CRC32
		}
		return
	}
	// RawMode parses only the format description and rotate. GTID and query
	// bodies stay in GenericEvent.Data, or in RawData when Event is unset.
	if decoded, ok := t.decodeRaw(ev); ok {
		ev = decoded
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

func (t *executedGTID) decodeRaw(ev *replication.BinlogEvent) (*replication.BinlogEvent, bool) {
	if gtidEventTyped(ev.Event) {
		return nil, false
	}
	switch ev.Header.EventType {
	case replication.GTID_EVENT, replication.GTID_TAGGED_LOG_EVENT, replication.QUERY_EVENT,
		replication.PREVIOUS_GTIDS_EVENT, replication.MARIADB_GTID_EVENT, replication.MARIADB_GTID_LIST_EVENT:
	default:
		return nil, false
	}
	body := t.eventBody(ev)
	cloned := *ev
	switch ev.Header.EventType {
	case replication.GTID_EVENT:
		if len(body) < 1+replication.SidLength+8 {
			return ev, true
		}
		ge := &replication.GTIDEvent{}
		if err := ge.Decode(body); err != nil {
			return ev, true
		}
		cloned.Event = ge
	case replication.GTID_TAGGED_LOG_EVENT:
		ge := &replication.GtidTaggedLogEvent{}
		if err := ge.Decode(body); err != nil {
			return ev, true
		}
		cloned.Event = ge
	case replication.QUERY_EVENT:
		qe := decodeQueryBody(body)
		if qe == nil {
			return ev, true
		}
		cloned.Event = qe
	case replication.PREVIOUS_GTIDS_EVENT:
		pe := &replication.PreviousGTIDsEvent{}
		if err := pe.Decode(body); err != nil {
			return ev, true
		}
		cloned.Event = pe
	case replication.MARIADB_GTID_EVENT:
		if len(body) < 13 {
			return ev, true
		}
		if body[12]&replication.BINLOG_MARIADB_FL_GROUP_COMMIT_ID != 0 && len(body) < 21 {
			return ev, true
		}
		me := &replication.MariadbGTIDEvent{}
		me.GTID.ServerID = ev.Header.ServerID
		if err := me.Decode(body); err != nil {
			return ev, true
		}
		cloned.Event = me
	case replication.MARIADB_GTID_LIST_EVENT:
		if !mariadbGTIDListBodyOK(body) {
			return ev, true
		}
		le := &replication.MariadbGTIDListEvent{}
		if err := le.Decode(body); err != nil {
			return ev, true
		}
		cloned.Event = le
	default:
		return ev, true
	}
	return &cloned, true
}

func (t *executedGTID) eventBody(ev *replication.BinlogEvent) []byte {
	if g, ok := ev.Event.(*replication.GenericEvent); ok && len(g.Data) > 0 {
		return g.Data
	}
	raw := ev.RawData
	if len(raw) <= replication.EventHeaderSize {
		return nil
	}
	body := raw[replication.EventHeaderSize:]
	if t.crc32 && len(body) >= replication.BinlogChecksumLength {
		body = body[:len(body)-replication.BinlogChecksumLength]
	}
	return body
}

func gtidEventTyped(event replication.Event) bool {
	switch event.(type) {
	case *replication.GTIDEvent,
		*replication.GtidTaggedLogEvent,
		*replication.QueryEvent,
		*replication.PreviousGTIDsEvent,
		*replication.MariadbGTIDEvent,
		*replication.MariadbGTIDListEvent:
		return true
	default:
		return false
	}
}

func decodeQueryBody(body []byte) *replication.QueryEvent {
	if len(body) < 13 {
		return nil
	}
	statusVars := int(binary.LittleEndian.Uint16(body[11:13]))
	schemaLen := int(body[8])
	need := 13 + statusVars + schemaLen + 1
	if len(body) < need {
		return nil
	}
	qe := &replication.QueryEvent{}
	if err := qe.Decode(body); err != nil {
		return nil
	}
	return qe
}

func mariadbGTIDListBodyOK(body []byte) bool {
	if len(body) < 4 {
		return false
	}
	count := binary.LittleEndian.Uint32(body) & ((1 << 28) - 1)
	return uint64(len(body)-4) >= uint64(count)*16
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
