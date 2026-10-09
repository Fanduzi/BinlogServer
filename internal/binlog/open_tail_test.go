// Package binlog provides module-level functionality for binlog.
// input: synthetic open segments with complete, out-of-order, multi-UUID, DDL, XA, compressed, MariaDB, and unfinished transactions
// output: assertions that the open-tail scan lists every complete transaction, cuts an unfinished one only where a complete one ended, and otherwise reports the GTID the resume continues inside
// pos: unit coverage for checkpoint reconciliation on resume
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

const tailUUID = "3e11fa47-71ca-11e1-9e33-c80aa9429562"
const tailUUID2 = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// tailSegment builds a segment whose event positions chain from start.
type tailSegment struct {
	t       *testing.T
	buf     []byte
	pos     uint32
	crc     bool
	offsets []int64
	ends    []uint32
}

func newTailSegment(t *testing.T, crc bool) *tailSegment {
	s := &tailSegment{t: t, buf: append([]byte(nil), durableMagic...), pos: 4, crc: crc}
	body := formatDescriptionWithCRC()
	if !crc {
		body[57] = byte(goreplication.BINLOG_CHECKSUM_ALG_OFF)
	}
	s.add(goreplication.FORMAT_DESCRIPTION_EVENT, body, 0)
	return s
}

// add appends one event. serverID is used by MariaDB GTID events.
func (s *tailSegment) add(kind goreplication.EventType, body []byte, serverID uint32) {
	payload := append([]byte(nil), body...)
	if s.crc && kind != goreplication.FORMAT_DESCRIPTION_EVENT {
		payload = append(payload, 0xde, 0xad, 0xbe, 0xef)
	}
	size := uint32(goreplication.EventHeaderSize + len(payload))
	hdr := make([]byte, goreplication.EventHeaderSize)
	hdr[4] = byte(kind)
	binary.LittleEndian.PutUint32(hdr[5:9], serverID)
	binary.LittleEndian.PutUint32(hdr[9:13], size)
	s.offsets = append(s.offsets, int64(len(s.buf)))
	s.pos += size
	binary.LittleEndian.PutUint32(hdr[13:17], s.pos)
	s.ends = append(s.ends, s.pos)
	s.buf = append(s.buf, hdr...)
	s.buf = append(s.buf, payload...)
}

// jump moves the next event's start position, as a mid-file dump does.
func (s *tailSegment) jump(pos uint32) { s.pos = pos }

func (s *tailSegment) gtid(uuid string, seq int64) {
	sid, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", ""))
	if err != nil {
		s.t.Fatal(err)
	}
	body := make([]byte, 1+goreplication.SidLength+8+8+8)
	copy(body[1:], sid)
	binary.LittleEndian.PutUint64(body[1+goreplication.SidLength:], uint64(seq))
	s.add(goreplication.GTID_EVENT, body, 1)
}

func (s *tailSegment) query(sql string) {
	body := make([]byte, 14+len(sql))
	copy(body[14:], sql)
	s.add(goreplication.QUERY_EVENT, body, 1)
}

func (s *tailSegment) rows() { s.add(goreplication.WRITE_ROWS_EVENTv2, make([]byte, 40), 1) }
func (s *tailSegment) xid()  { s.add(goreplication.XID_EVENT, make([]byte, 8), 1) }

func (s *tailSegment) txn(uuid string, seq int64) {
	s.gtid(uuid, seq)
	s.query("BEGIN")
	s.rows()
	s.xid()
}

func (s *tailSegment) mariaGTID(domain, server uint32, seq uint64, standalone bool) {
	body := make([]byte, 19)
	binary.LittleEndian.PutUint64(body[0:8], seq)
	binary.LittleEndian.PutUint32(body[8:12], domain)
	if standalone {
		body[12] = goreplication.BINLOG_MARIADB_FL_STANDALONE
	}
	s.add(goreplication.MARIADB_GTID_EVENT, body, server)
}

func (s *tailSegment) write(dir, name string) string {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, s.buf, 0o644); err != nil {
		s.t.Fatal(err)
	}
	return path
}

func TestScanOpenTail_CompleteTransactionsPastCheckpoint(t *testing.T) {
	for _, crc := range []bool{false, true} {
		seg := newTailSegment(t, crc)
		seg.txn(tailUUID, 10)
		seg.txn(tailUUID, 11)
		dir := t.TempDir()
		seg.write(dir, "mysql-bin.000003.open.e1")
		tail, ok := ReconcileOpenTail(dir)
		if !ok {
			t.Fatal("no tail")
		}
		if want := []string{tailUUID + ":10", tailUUID + ":11"}; !reflect.DeepEqual(tail.GTIDs, want) {
			t.Fatalf("crc=%v gtids %v want %v", crc, tail.GTIDs, want)
		}
		if tail.Truncate || tail.PartialGTID != "" || tail.File != "mysql-bin.000003" || tail.Pos != seg.pos {
			t.Fatalf("crc=%v tail %+v want pos %d", crc, tail, seg.pos)
		}
		// The checkpoint was written before :11 reached the row.
		if got := UnionGTIDText("mysql", tailUUID+":1-10", tail); got != tailUUID+":1-11" {
			t.Fatalf("union %s", got)
		}
	}
}

func TestScanOpenTail_CutsUnfinishedTransactionAtCommit(t *testing.T) {
	seg := newTailSegment(t, true)
	seg.txn(tailUUID, 10)
	commitEnd := seg.pos
	cut := int64(len(seg.buf))
	seg.gtid(tailUUID, 11)
	seg.query("BEGIN")
	seg.rows()
	seg.rows()
	dir := t.TempDir()
	path := seg.write(dir, "mysql-bin.000003.open.e1")
	tail, ok := ReconcileOpenTail(dir)
	if !ok || !tail.Truncate || tail.CutOffset != cut || tail.Pos != commitEnd {
		t.Fatalf("tail %+v want cut %d pos %d", tail, cut, commitEnd)
	}
	if tail.PartialGTID != tailUUID+":11" {
		t.Fatalf("partial %q", tail.PartialGTID)
	}
	if want := []string{tailUUID + ":10"}; !reflect.DeepEqual(tail.GTIDs, want) {
		t.Fatalf("gtids %v", tail.GTIDs)
	}
	if err := os.Truncate(path, tail.CutOffset); err != nil {
		t.Fatal(err)
	}
	if _, pos, ok := DurableResumeDir(dir); !ok || pos != commitEnd {
		t.Fatalf("resume after cut %d ok=%v want %d", pos, ok, commitEnd)
	}
	again, ok := ReconcileOpenTail(dir)
	if !ok || again.Truncate || again.PartialGTID != "" || again.Pos != commitEnd {
		t.Fatalf("second scan %+v", again)
	}
}

func TestScanOpenTail_UnfinishedFirstTransactionAfterMidFileHeader(t *testing.T) {
	// A mid-file start writes the format description (end pos 126 here)
	// in front of the first copied event at 5000. Cutting there would
	// resume at 126, so the scan reports the GTID to continue inside.
	seg := newTailSegment(t, false)
	seg.jump(5000)
	seg.gtid(tailUUID, 40)
	seg.query("BEGIN")
	seg.rows()
	dir := t.TempDir()
	seg.write(dir, "mysql-bin.000007.open.e2")
	tail, ok := ReconcileOpenTail(dir)
	if !ok || tail.Truncate || tail.PartialGTID != tailUUID+":40" || !tail.PartialInTxn || tail.Pos != seg.pos {
		t.Fatalf("tail %+v", tail)
	}
	if len(tail.GTIDs) != 0 {
		t.Fatalf("gtids %v", tail.GTIDs)
	}
}

func TestScanOpenTail_GapsOrderAndUUIDsAreAllKept(t *testing.T) {
	seg := newTailSegment(t, true)
	seg.txn(tailUUID, 12)
	seg.txn(tailUUID, 100)
	seg.txn(tailUUID, 11)
	seg.txn(tailUUID2, 5)
	seg.txn(tailUUID, 13)
	dir := t.TempDir()
	seg.write(dir, "mysql-bin.000003.open.e1")
	tail, ok := ReconcileOpenTail(dir)
	if !ok || tail.Truncate || tail.PartialGTID != "" {
		t.Fatalf("tail %+v", tail)
	}
	got := UnionGTIDText("mysql", tailUUID+":1-10", tail)
	want := tailUUID + ":1-13:100," + tailUUID2 + ":5"
	if !sameGTIDText(t, got, want) {
		t.Fatalf("union %s want %s", got, want)
	}
}

func TestScanOpenTail_DDLXAAndCompressedPayloadCommit(t *testing.T) {
	seg := newTailSegment(t, true)
	seg.gtid(tailUUID, 30)
	seg.query("CREATE TABLE t (id INT)")
	seg.gtid(tailUUID, 31)
	seg.query("XA START 'x'")
	seg.rows()
	seg.query("XA END 'x'")
	seg.add(goreplication.XA_PREPARE_LOG_EVENT, make([]byte, 20), 1)
	seg.gtid(tailUUID, 32)
	seg.query("XA COMMIT 'x'")
	seg.gtid(tailUUID, 33)
	seg.add(goreplication.TRANSACTION_PAYLOAD_EVENT, make([]byte, 64), 1)
	seg.gtid(tailUUID, 34)
	seg.query("BEGIN")
	seg.query("SAVEPOINT a")
	seg.query("ROLLBACK TO SAVEPOINT a")
	seg.query("ROLLBACK")
	dir := t.TempDir()
	seg.write(dir, "mysql-bin.000003.open.e1")
	tail, ok := ReconcileOpenTail(dir)
	if !ok || tail.Truncate || tail.PartialGTID != "" {
		t.Fatalf("tail %+v", tail)
	}
	want := []string{tailUUID + ":30", tailUUID + ":31", tailUUID + ":32", tailUUID + ":33", tailUUID + ":34"}
	if !reflect.DeepEqual(tail.GTIDs, want) {
		t.Fatalf("gtids %v want %v", tail.GTIDs, want)
	}
}

func TestScanOpenTail_MariaDB(t *testing.T) {
	seg := newTailSegment(t, true)
	seg.mariaGTID(0, 1, 7, false)
	seg.query("INSERT INTO t VALUES (1)")
	seg.xid()
	seg.mariaGTID(0, 1, 8, true)
	seg.query("CREATE TABLE u (id INT)")
	commitEnd := seg.pos
	cut := int64(len(seg.buf))
	seg.mariaGTID(0, 1, 9, false)
	seg.query("INSERT INTO t VALUES (2)")
	dir := t.TempDir()
	seg.write(dir, "mariadb-bin.000002.open.e1")
	tail, ok := ReconcileOpenTail(dir)
	if !ok {
		t.Fatal("no tail")
	}
	if want := []string{"0-1-7", "0-1-8"}; !reflect.DeepEqual(tail.GTIDs, want) {
		t.Fatalf("gtids %v", tail.GTIDs)
	}
	if !tail.Truncate || tail.CutOffset != cut || tail.Pos != commitEnd || tail.PartialGTID != "0-1-9" {
		t.Fatalf("tail %+v want cut %d pos %d", tail, cut, commitEnd)
	}
	if got := UnionGTIDText("mariadb", "0-1-6", tail); got != "0-1-8" {
		t.Fatalf("union %s", got)
	}
}

func TestReconcileOpenTail_NoOpenSegment(t *testing.T) {
	dir := t.TempDir()
	seg := newTailSegment(t, false)
	seg.txn(tailUUID, 1)
	seg.write(dir, "mysql-bin.000001")
	if _, ok := ReconcileOpenTail(dir); ok {
		t.Fatal("a sealed segment is not a resume tail")
	}
	if _, ok := ReconcileOpenTail(filepath.Join(dir, "missing")); ok {
		t.Fatal("missing directory")
	}
}

func TestTransactionQueryEffect(t *testing.T) {
	cases := []struct {
		q     string
		inTxn bool
		want  QueryEffect
	}{
		{"BEGIN", false, QueryBegins},
		{"start transaction", false, QueryBegins},
		{"COMMIT", true, QueryCommits},
		{"ROLLBACK", true, QueryCommits},
		{"ROLLBACK TO SAVEPOINT a", true, QueryInside},
		{"XA START 'x'", false, QueryInside},
		{"XA END 'x'", false, QueryInside},
		{"XA COMMIT 'x'", false, QueryCommits},
		{"XA ROLLBACK 'x'", false, QueryCommits},
		{"CREATE TABLE t (id INT)", false, QueryCommits},
		{"INSERT INTO t VALUES (1)", true, QueryInside},
		{"", false, QueryInside},
	}
	for _, tc := range cases {
		if got := TransactionQueryEffect(tc.q, tc.inTxn); got != tc.want {
			t.Fatalf("%q inTxn=%v: %v want %v", tc.q, tc.inTxn, got, tc.want)
		}
	}
}

func sameGTIDText(t *testing.T, a, b string) bool {
	t.Helper()
	x := UnionGTIDText("mysql", a, OpenTail{})
	y := UnionGTIDText("mysql", b, OpenTail{})
	return x == y && x != ""
}
