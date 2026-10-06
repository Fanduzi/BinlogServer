// Package binlog provides module-level functionality for binlog.
// input: segment basenames used by the runner and the tasks disk scan
// output: proof that plain sealed names, open epochs, and later sealed epochs classify the same way, and that OpenName and SealedName reject .takeover-* and other non-binlog names
// pos: unit coverage for the shared segment-name classifier
// note: if this file changes, update this header and module README.md.
package binlog

import "testing"

func TestClassifySegment(t *testing.T) {
	cases := []struct {
		name   string
		ok     bool
		source string
		seq    uint64
		epoch  int64
		open   bool
	}{
		{name: "mysql-bin.000001", ok: true, source: "mysql-bin.000001", seq: 1, epoch: -1},
		{name: "mysql-bin.000004.open.e2", ok: true, source: "mysql-bin.000004", seq: 4, epoch: 2, open: true},
		{name: "mysql-bin.000004.sealed.e7", ok: true, source: "mysql-bin.000004", seq: 4, epoch: 7},
		{name: "notes.txt"},
		{name: ".mysql-bin.000001"},
		{name: ".takeover-9f3a"},
		{name: "task-1.binlog"},
		{name: "mysql-bin.000001.open.e"},
		{name: "mysql-bin.000001.open.e-1"},
		{name: ""},
	}
	for _, tc := range cases {
		got, ok := ClassifySegment(tc.name)
		if ok != tc.ok || got.Source != tc.source || got.Seq != tc.seq || got.Epoch != tc.epoch || got.Open != tc.open {
			t.Fatalf("%q -> %+v ok=%v", tc.name, got, ok)
		}
		if OpenName(tc.name) != (tc.ok && tc.open) {
			t.Fatalf("OpenName(%q)=%v", tc.name, OpenName(tc.name))
		}
		if SealedName(tc.name) != (tc.ok && !tc.open) {
			t.Fatalf("SealedName(%q)=%v", tc.name, SealedName(tc.name))
		}
	}
}
