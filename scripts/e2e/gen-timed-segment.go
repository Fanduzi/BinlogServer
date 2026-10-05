// Package main writes one binlog segment whose event-header times are the
// RFC3339 arguments, so an e2e point-in-time window can see the segment.
// input: output path and one or more UTC timestamps
// output: a binlog file with magic and one query event per timestamp
// pos: e2e helper for the epoch-segment restore scenario
// note: if this file changes, update this header and scripts/e2e/README.md.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: gen-timed-segment <out> <rfc3339>...")
		os.Exit(2)
	}
	buf := []byte{0xfe, 'b', 'i', 'n'}
	pos := uint32(4)
	for _, raw := range os.Args[2:] {
		when, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "time %s: %v\n", raw, err)
			os.Exit(2)
		}
		const payload = 1
		size := uint32(19 + payload)
		pos += size
		var hdr [19]byte
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(when.UTC().Unix()))
		hdr[4] = 2 // QUERY_EVENT
		binary.LittleEndian.PutUint32(hdr[5:9], 1)
		binary.LittleEndian.PutUint32(hdr[9:13], size)
		binary.LittleEndian.PutUint32(hdr[13:17], pos)
		buf = append(buf, hdr[:]...)
		buf = append(buf, 0)
	}
	if err := os.WriteFile(os.Args[1], buf, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
