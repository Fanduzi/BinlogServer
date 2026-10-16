package assurance

import (
	"testing"

	"binlog_server/internal/binlog"
)

// design 0009 §21.3: a segment is judged against the object listing only
// when its object key's prefix was listed completely.
func TestObjectPrefixesGateObjectKinds(t *testing.T) {
	f := live(t)
	f.in.Local = binlog.LedgerAt(f.dir)
	delete(f.in.Objects, "t/mysql-bin.000004")
	f.in.ObjectPrefixes = map[string]bool{"t/": false}
	if vs := ChainOf(f.in).Check(testNow); has(vs, I6MissingObject, "") {
		t.Fatalf("incomplete prefix judged: %s", kinds(vs))
	}
	f.in.ObjectPrefixes = map[string]bool{"t/": true}
	if vs := ChainOf(f.in).Check(testNow); !has(vs, I6MissingObject, "mysql-bin.000004") {
		t.Fatalf("complete prefix not judged: %s", kinds(vs))
	}
	f.in.ObjectPrefixes = map[string]bool{"other/": true}
	if vs := ChainOf(f.in).Check(testNow); has(vs, I6MissingObject, "") {
		t.Fatalf("unlisted prefix judged: %s", kinds(vs))
	}
	f.in.ObjectPrefixes = nil // phase 1: a non-nil Objects is a full listing
	if vs := ChainOf(f.in).Check(testNow); !has(vs, I6MissingObject, "mysql-bin.000004") {
		t.Fatalf("nil prefixes changed phase 1: %s", kinds(vs))
	}
}

// ObjectPrefixOf is the listing prefix of an object key.
func TestObjectPrefixOf(t *testing.T) {
	for key, want := range map[string]string{"p/c/uuid/mysql-bin.000001": "p/c/uuid/", "x": "", "a/b": "a/"} {
		if got := ObjectPrefixOf(key); got != want {
			t.Errorf("%q: %q want %q", key, got, want)
		}
	}
}
