// Package replication provides module-level functionality for replication.
// input: a MariaDB event header with LogPos=0, the runner's own cursor (end of the last event written), and the event's RawData
// output: the end position a data event gets (cursor + event size; 0 for header and control events, artificial events and MySQL), and RawData with that end_log_pos and CRC32 rewritten
// pos: MariaDB 11.4+ row events carry LogPos=0 (#359); positions come from our own cursor, never go-mysql FillZeroLogPos, which also filled the mid-file FDE (#362 QA)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"encoding/binary"
	"hash/crc32"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// filledEndPos is the end position for a MariaDB event the source sent with
// LogPos=0: cursor (the end of the last event this run wrote, in source
// coordinates) plus the event size. The dump asks for ANNOTATE_ROWS, so the
// events of one transaction arrive back to back. It is 0, and the event is
// left alone, for MySQL, for a known position, before any cursor exists,
// for ARTIFICIAL events, and for header and control events: format
// description, rotate, stop, heartbeats, previous-GTIDs, GTID list, binlog
// checkpoint and start-encryption. A mid-file dump sends the file's format
// description with LogPos=0 and no ARTIFICIAL flag; filling it moved the
// cursor 210 bytes past the next GTID.
func filledEndPos(flavor string, h *goreplication.EventHeader, cursor uint32) uint32 {
	if h == nil || h.LogPos != 0 || cursor == 0 || !isMariaDBFlavor(flavor) {
		return 0
	}
	if h.Flags&goreplication.LOG_EVENT_ARTIFICIAL_F != 0 || controlEvent(h.EventType) {
		return 0
	}
	return cursor + h.EventSize
}

func controlEvent(t goreplication.EventType) bool {
	switch t {
	case goreplication.FORMAT_DESCRIPTION_EVENT, goreplication.ROTATE_EVENT, goreplication.STOP_EVENT,
		goreplication.HEARTBEAT_EVENT, goreplication.HEARTBEAT_LOG_EVENT_V2, goreplication.PREVIOUS_GTIDS_EVENT,
		goreplication.MARIADB_GTID_LIST_EVENT, goreplication.MARIADB_BINLOG_CHECKPOINT_EVENT,
		goreplication.MARIADB_START_ENCRYPTION_EVENT, goreplication.UNKNOWN_EVENT:
		return true
	}
	return false
}

// stampLogPosInRaw copies raw with end_log_pos set to logPos when the header
// still says 0, so the segment we write carries the position filledEndPos
// computed. When hasCRC is true the trailing IEEE CRC32 is recomputed.
func stampLogPosInRaw(raw []byte, logPos uint32, hasCRC bool) []byte {
	if logPos == 0 || len(raw) < goreplication.EventHeaderSize {
		return raw
	}
	if binary.LittleEndian.Uint32(raw[13:17]) != 0 {
		return raw
	}
	out := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint32(out[13:17], logPos)
	if hasCRC && len(out) >= goreplication.EventHeaderSize+goreplication.BinlogChecksumLength {
		body := out[:len(out)-goreplication.BinlogChecksumLength]
		binary.LittleEndian.PutUint32(out[len(out)-goreplication.BinlogChecksumLength:], crc32.ChecksumIEEE(body))
	}
	return out
}

func formatDescriptionHasCRC(event *goreplication.BinlogEvent) bool {
	if event == nil || event.Header == nil || event.Header.EventType != goreplication.FORMAT_DESCRIPTION_EVENT {
		return false
	}
	if fde, ok := event.Event.(*goreplication.FormatDescriptionEvent); ok {
		return fde.ChecksumAlgorithm == goreplication.BINLOG_CHECKSUM_ALG_CRC32
	}
	body := event.RawData
	if len(body) > goreplication.EventHeaderSize {
		body = body[goreplication.EventHeaderSize:]
	}
	if g, ok := event.Event.(*goreplication.GenericEvent); ok && len(g.Data) > 0 {
		body = g.Data
	}
	if len(body) < 57 {
		return false
	}
	fde := &goreplication.FormatDescriptionEvent{}
	if err := fde.Decode(body); err != nil {
		return false
	}
	return fde.ChecksumAlgorithm == goreplication.BINLOG_CHECKSUM_ALG_CRC32
}
