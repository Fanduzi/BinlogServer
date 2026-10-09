// Package binlog provides module-level functionality for binlog.
// input: a task segment directory and the bytes of its highest open segment
// output: the GTIDs of every complete transaction in that segment, its previous-GTIDs text, and the resume cut: the end of the last complete transaction when the segment ends inside one that can be cut cleanly, otherwise the GTID of that unfinished transaction
// pos: reconcile a checkpoint written after the segment flush with what the open segment really holds, so a resume never treats its own tail as a gap
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"
	"strconv"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// OpenTail is what the highest open segment of a task directory holds.
//
// The segment is flushed before the checkpoint row is written, so a stop,
// crash, or dump drop between the two leaves complete transactions in the
// file that the checkpoint gtid_set does not list. A resume in the middle of
// a transaction also loses the GTID that transaction started with. OpenTail
// is the on-disk truth the next start continues from.
type OpenTail struct {
	// Path is the open segment that was read.
	Path string
	// File is the source binlog name of that segment.
	File string
	// Pos is the end log_pos the next start continues from. With Truncate it
	// is the end of the last complete transaction; otherwise it is the last
	// complete event, the same position DurableCursor returns.
	Pos uint32
	// GTIDs lists every complete transaction in the segment in file order.
	// A MySQL GTID is uuid:n. A MariaDB GTID is domain-server-seq.
	GTIDs []string
	// Previous is the previous-GTIDs (MySQL) or GTID list (MariaDB) text the
	// segment starts with. Those transactions committed before this file.
	Previous []string
	// Truncate is set when the segment ends inside a transaction whose first
	// event starts exactly where the last complete transaction ended. Cutting
	// the file at CutOffset leaves only whole transactions, and the source
	// sends the whole unfinished transaction again from Pos.
	Truncate  bool
	CutOffset int64
	// PartialGTID is the GTID of a trailing unfinished transaction. Without
	// Truncate (for example the first transaction after a mid-file format
	// description) the resume continues inside it, so its commit must still
	// record this GTID. PartialInTxn reports that BEGIN was already seen.
	PartialGTID  string
	PartialInTxn bool
}

// ReconcileOpenTail reads the open segment DurableResumeDir would resume
// from. ok is false when the directory has no open segment with a complete
// event. The file is not changed; the caller applies Truncate.
func ReconcileOpenTail(taskDir string) (OpenTail, bool) {
	seg, ok := durableOpenSegment(taskDir)
	if !ok {
		return OpenTail{}, false
	}
	tail, ok := ScanOpenTail(seg.path)
	if !ok {
		return OpenTail{}, false
	}
	tail.File = seg.source
	return tail, true
}

// ScanOpenTail reads one segment. ok is false when the file is unreadable or
// has no event whose end log_pos is greater than 0.
func ScanOpenTail(path string) (OpenTail, bool) {
	f, err := os.Open(path)
	if err != nil {
		return OpenTail{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return OpenTail{}, false
	}
	size := info.Size()
	r := bufio.NewReaderSize(f, 1<<20)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != string(durableMagic) {
		return OpenTail{}, false
	}
	out := OpenTail{Path: path}
	var (
		offset       = int64(4)
		hdr          = make([]byte, goreplication.EventHeaderSize)
		crc32        bool
		pending      string
		pendingOff   int64
		pendingStart uint32
		inTxn        bool
		lastPos      uint32
		cutOff       int64
		cutPos       uint32
		found        bool
	)
	commit := func() {
		if pending != "" {
			out.GTIDs = append(out.GTIDs, pending)
		}
		pending = ""
		inTxn = false
	}
	for offset+int64(goreplication.EventHeaderSize) <= size {
		if _, err := io.ReadFull(r, hdr); err != nil {
			break
		}
		eventSize := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		logPos := binary.LittleEndian.Uint32(hdr[13:17])
		if eventSize < int64(goreplication.EventHeaderSize) || offset+eventSize > size {
			break
		}
		eventType := goreplication.EventType(hdr[4])
		bodyLen := int(eventSize) - goreplication.EventHeaderSize
		var body []byte
		if tailNeedsBody(eventType) {
			body = make([]byte, bodyLen)
			if _, err := io.ReadFull(r, body); err != nil {
				break
			}
		} else if _, err := r.Discard(bodyLen); err != nil {
			break
		}
		start := offset
		offset += eventSize
		payload := body
		if crc32 && eventType != goreplication.FORMAT_DESCRIPTION_EVENT && len(payload) >= goreplication.BinlogChecksumLength {
			payload = payload[:len(payload)-goreplication.BinlogChecksumLength]
		}
		switch eventType {
		case goreplication.FORMAT_DESCRIPTION_EVENT:
			crc32 = fdChecksum(body)
		case goreplication.PREVIOUS_GTIDS_EVENT:
			if text, ok := decodePreviousGTIDs(body, crc32); ok && strings.TrimSpace(text) != "" {
				out.Previous = append(out.Previous, text)
			}
		case goreplication.MARIADB_GTID_LIST_EVENT:
			if text, ok := mariadbGTIDList(payload); ok && text != "" {
				out.Previous = append(out.Previous, text)
			}
		case goreplication.ANONYMOUS_GTID_EVENT:
			pending = ""
			inTxn = false
		case goreplication.GTID_EVENT, goreplication.GTID_TAGGED_LOG_EVENT:
			pending = ""
			inTxn = false
			if gtid, ok := mysqlGTIDText(eventType, payload); ok {
				pending = gtid
				pendingOff = start
				pendingStart = 0
				if logPos >= uint32(eventSize) {
					pendingStart = logPos - uint32(eventSize)
				}
			}
		case goreplication.MARIADB_GTID_EVENT:
			pending = ""
			inTxn = false
			if gtid, standalone, ok := mariadbGTIDText(hdr, payload); ok {
				if standalone {
					out.GTIDs = append(out.GTIDs, gtid)
				} else {
					// A MariaDB GTID event opens the transaction itself;
					// there is no separate BEGIN query.
					pending = gtid
					inTxn = true
					pendingOff = start
					pendingStart = 0
					if logPos >= uint32(eventSize) {
						pendingStart = logPos - uint32(eventSize)
					}
				}
			}
		case goreplication.XID_EVENT, goreplication.XA_PREPARE_LOG_EVENT, goreplication.TRANSACTION_PAYLOAD_EVENT:
			// A transaction payload carries the whole compressed transaction,
			// including its commit.
			commit()
		case goreplication.QUERY_EVENT:
			if pending == "" {
				break
			}
			switch TransactionQueryEffect(queryText(payload), inTxn) {
			case QueryBegins:
				inTxn = true
			case QueryCommits:
				commit()
			}
		}
		if logPos == 0 {
			continue
		}
		lastPos = logPos
		found = true
		if pending == "" {
			cutOff = offset
			cutPos = logPos
		}
	}
	if !found {
		return OpenTail{}, false
	}
	out.Pos = lastPos
	if pending != "" {
		out.PartialGTID = pending
		out.PartialInTxn = inTxn
		if cutPos != 0 && cutOff == pendingOff && cutPos == pendingStart {
			out.Truncate = true
			out.CutOffset = cutOff
			out.Pos = cutPos
		}
	}
	return out, true
}

// QueryEffect is what one query event does to the open GTID transaction.
type QueryEffect int

const (
	// QueryInside leaves the transaction open.
	QueryInside QueryEffect = iota
	// QueryBegins opens a multi-statement transaction.
	QueryBegins
	// QueryCommits ends the transaction: COMMIT, a full ROLLBACK, XA COMMIT,
	// XA ROLLBACK, or a statement outside BEGIN (DDL and other autocommit
	// statements carry their own GTID).
	QueryCommits
)

// TransactionQueryEffect classifies one query text for an open GTID
// transaction. inTxn reports that BEGIN was already seen. The replication
// runner and the open-tail scan share this rule.
func TransactionQueryEffect(query string, inTxn bool) QueryEffect {
	upper := strings.ToUpper(strings.TrimSpace(query))
	head := upper
	if i := strings.IndexAny(upper, " \t\r\n"); i >= 0 {
		head = upper[:i]
	}
	if head == "" {
		return QueryInside
	}
	switch head {
	case "BEGIN":
		return QueryBegins
	case "START":
		if strings.HasPrefix(upper, "START TRANSACTION") {
			return QueryBegins
		}
		return QueryInside
	case "COMMIT":
		return QueryCommits
	case "ROLLBACK":
		if strings.HasPrefix(upper, "ROLLBACK TO") {
			return QueryInside
		}
		return QueryCommits
	case "XA":
		if strings.HasPrefix(upper, "XA COMMIT") || strings.HasPrefix(upper, "XA ROLLBACK") {
			return QueryCommits
		}
		return QueryInside
	}
	if inTxn {
		return QueryInside
	}
	return QueryCommits
}

func tailNeedsBody(t goreplication.EventType) bool {
	switch t {
	case goreplication.FORMAT_DESCRIPTION_EVENT, goreplication.PREVIOUS_GTIDS_EVENT,
		goreplication.GTID_EVENT, goreplication.GTID_TAGGED_LOG_EVENT,
		goreplication.MARIADB_GTID_EVENT, goreplication.MARIADB_GTID_LIST_EVENT,
		goreplication.QUERY_EVENT:
		return true
	default:
		return false
	}
}

// queryText is the statement of a query event body without its checksum.
func queryText(body []byte) string {
	if len(body) < 13 {
		return ""
	}
	schemaLen := int(body[8])
	statusVars := int(binary.LittleEndian.Uint16(body[11:13]))
	start := 13 + statusVars + schemaLen + 1
	if start > len(body) {
		return ""
	}
	q := body[start:]
	if len(q) > 64 {
		q = q[:64]
	}
	return string(q)
}

func mysqlGTIDText(t goreplication.EventType, body []byte) (string, bool) {
	if t == goreplication.GTID_EVENT {
		need := 1 + goreplication.SidLength + 8
		if len(body) < need {
			return "", false
		}
		seq := int64(binary.LittleEndian.Uint64(body[1+goreplication.SidLength : need]))
		uuid := formatSID(body[1 : 1+goreplication.SidLength])
		if seq <= 0 || uuid == "" {
			return "", false
		}
		return uuid + ":" + strconv.FormatInt(seq, 10), true
	}
	ev := &goreplication.GtidTaggedLogEvent{}
	if err := ev.Decode(body); err != nil {
		return "", false
	}
	set, err := ev.GTIDNext()
	if err != nil || set == nil {
		return "", false
	}
	text := strings.TrimSpace(set.String())
	return text, text != ""
}

func mariadbGTIDText(hdr, body []byte) (string, bool, bool) {
	if len(body) < 13 {
		return "", false, false
	}
	if body[12]&goreplication.BINLOG_MARIADB_FL_GROUP_COMMIT_ID != 0 && len(body) < 21 {
		return "", false, false
	}
	ev := &goreplication.MariadbGTIDEvent{}
	ev.GTID.ServerID = binary.LittleEndian.Uint32(hdr[5:9])
	if err := ev.Decode(body); err != nil {
		return "", false, false
	}
	return ev.GTID.String(), ev.IsStandalone(), true
}

func mariadbGTIDList(body []byte) (string, bool) {
	if len(body) < 4 {
		return "", false
	}
	count := binary.LittleEndian.Uint32(body) & ((1 << 28) - 1)
	if uint64(len(body)-4) < uint64(count)*16 {
		return "", false
	}
	ev := &goreplication.MariadbGTIDListEvent{}
	if err := ev.Decode(body); err != nil {
		return "", false
	}
	parts := make([]string, 0, len(ev.GTIDs))
	for _, g := range ev.GTIDs {
		parts = append(parts, g.String())
	}
	return strings.Join(parts, ","), true
}

// UnionGTIDText adds the open tail's transactions and previous-GTIDs text to
// seed and returns the combined set. A text that does not parse for flavor
// is skipped. An empty seed and an empty tail return "".
func UnionGTIDText(flavor, seed string, tail OpenTail) string {
	if strings.TrimSpace(flavor) == "" {
		flavor = gomysql.MySQLFlavor
	}
	var set gomysql.GTIDSet
	add := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if set == nil {
			parsed, err := gomysql.ParseGTIDSet(flavor, text)
			if err != nil {
				return
			}
			set = parsed
			return
		}
		_ = set.Update(text)
	}
	add(seed)
	for _, p := range tail.Previous {
		add(p)
	}
	for _, g := range tail.GTIDs {
		add(g)
	}
	if set == nil || set.IsEmpty() {
		return ""
	}
	return set.String()
}
