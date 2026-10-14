// Package replication provides module-level functionality for replication.
// input: a binlog event's RawData and the LogPos go-mysql filled in for MariaDB 11.4+
// output: RawData whose end_log_pos matches that LogPos, with CRC32 rewritten when present
// pos: keep on-disk events consistent with the Header after FillZeroLogPos (#359)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"encoding/binary"
	"hash/crc32"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// stampLogPosInRaw copies raw with end_log_pos set to logPos when the header
// still says 0. MariaDB 11.4+ sends LogPos=0 on row events; FillZeroLogPos
// fixes the in-memory Header, and this keeps the bytes we write to disk in
// sync. When hasCRC is true the trailing IEEE CRC32 is recomputed.
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
