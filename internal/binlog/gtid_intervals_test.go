// Package binlog provides module-level functionality for binlog.
// input: MySQL GTID set text with several UUIDs, holes, and a tag
// output: assertions that interval add, subtract, and print agree with GTID set arithmetic and reject garbage
// pos: unit coverage for the storage-inconsistency GTID arithmetic
// note: if this file changes, update this header and module README.md.
package binlog

import "testing"

func TestGTIDIntervals(t *testing.T) {
	const a = "51ca62d9-c2c4-11f1-a149-822b383dbcd0"
	const b = "4bf78e6c-c2c4-11f1-bd7c-822b383dbcd0"
	set, ok := parseGTIDIntervals(a + ":1-10:20-30,\n" + b + ":5")
	if !ok {
		t.Fatal("parse")
	}
	sub, _ := parseGTIDIntervals(a + ":3-4:10-21," + b + ":5")
	set.subtract(sub)
	if got, want := set.String(), a+":1-2:5-9:22-30"; got != want {
		t.Fatalf("subtract %q want %q", got, want)
	}
	set.add(a, 3, 4)
	set.add(a, 10, 21)
	if got, want := set.String(), a+":1-30"; got != want {
		t.Fatalf("add %q want %q", got, want)
	}
	if _, ok := parseGTIDIntervals("not-a-set"); ok {
		t.Fatal("garbage parsed")
	}
	if _, ok := parseGTIDIntervals(""); ok {
		t.Fatal("empty parsed")
	}
	tagged, ok := parseGTIDIntervals(a + ":1-3:blue:1-2")
	if !ok || tagged.String() != a+":1-3,"+a+":blue:1-2" {
		t.Fatalf("tagged %q ok=%v", tagged.String(), ok)
	}
}
