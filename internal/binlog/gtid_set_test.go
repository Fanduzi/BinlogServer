// Package binlog provides module-level functionality for binlog.
// input: MySQL GTID set text with several UUIDs, holes, and a tag
// output: assertions that GTIDSet add, minus, contain, gaps and print agree with GTID set arithmetic, that the lenient parser rejects garbage, and that the strict parser and Contains agree with go-mysql on random sets
// pos: unit coverage for the one GTID interval type behind the segment ledger
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

func TestGTIDSetLenient(t *testing.T) {
	const a = "51ca62d9-c2c4-11f1-a149-822b383dbcd0"
	const b = "4bf78e6c-c2c4-11f1-bd7c-822b383dbcd0"
	set, ok := ParseGTIDSetLenient(a + ":1-10:20-30,\n" + b + ":5")
	if !ok {
		t.Fatal("parse")
	}
	sub, _ := ParseGTIDSetLenient(a + ":3-4:10-21," + b + ":5")
	set = set.Minus(sub)
	if got, want := set.String(), a+":1-2:5-9:22-30"; got != want {
		t.Fatalf("minus %q want %q", got, want)
	}
	set.Add(a, 3, 4)
	set.Add(a, 10, 21)
	if got, want := set.String(), a+":1-30"; got != want {
		t.Fatalf("add %q want %q", got, want)
	}
	if _, ok := ParseGTIDSetLenient("not-a-set"); ok {
		t.Fatal("garbage parsed")
	}
	if _, ok := ParseGTIDSetLenient(""); ok {
		t.Fatal("empty parsed")
	}
	tagged, ok := ParseGTIDSetLenient(a + ":1-3:blue:1-2")
	if !ok || tagged.String() != a+":1-3,"+a+":blue:1-2" {
		t.Fatalf("tagged %q ok=%v", tagged.String(), ok)
	}
}

func TestGTIDSetStrictAndGaps(t *testing.T) {
	const a = "51ca62d9-c2c4-11f1-a149-822b383dbcd0"
	set, err := ParseGTIDSet(strings.ToUpper(a) + ":1-3:7:9-12:Blue:4")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := set.String(), a+":1-3:7:9-12,"+a+":blue:4"; got != want {
		t.Fatalf("strict %q want %q", got, want)
	}
	if got, want := set.InteriorGaps().String(), a+":4-6:8"; got != want {
		t.Fatalf("gaps %q want %q", got, want)
	}
	if _, err := ParseGTIDSet("zz"); err == nil {
		t.Fatal("garbage parsed")
	}
	if empty, err := ParseGTIDSet("  "); err != nil || !empty.IsEmpty() {
		t.Fatalf("blank %v %v", empty, err)
	}
	if !set.Has(a, 10) || set.Has(a, 8) || !set.HasEvent(GTIDEventRef{UUID: strings.ToUpper(a), Seq: 7}) {
		t.Fatal("has")
	}
}

// TestGTIDSetMatchesGoMySQL checks Contains and the union text against
// go-mysql, which the window and legacy-failback code used before the
// ledger.
func TestGTIDSetMatchesGoMySQL(t *testing.T) {
	uuids := []string{"51ca62d9-c2c4-11f1-a149-822b383dbcd0", "4bf78e6c-c2c4-11f1-bd7c-822b383dbcd0"}
	r := rand.New(rand.NewSource(7))
	gen := func() string {
		var parts []string
		for _, u := range uuids {
			if r.Intn(4) == 0 {
				continue
			}
			p := u
			for i := 0; i < 1+r.Intn(4); i++ {
				lo := 1 + r.Int63n(40)
				if r.Intn(2) == 0 {
					p += fmt.Sprintf(":%d", lo)
				} else {
					p += fmt.Sprintf(":%d-%d", lo, lo+r.Int63n(10))
				}
			}
			if r.Intn(3) == 0 {
				p += fmt.Sprintf(":tg:%d", 1+r.Int63n(5))
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, ",")
	}
	for i := 0; i < 5000; i++ {
		x, y := gen(), gen()
		gx, err := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, x)
		if x == "" {
			gx, err = &gomysql.MysqlGTIDSet{}, nil
		}
		if err != nil {
			t.Fatal(err)
		}
		gy, _ := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, y)
		if y == "" {
			gy = &gomysql.MysqlGTIDSet{}
		}
		sx, err := ParseGTIDSet(x)
		if err != nil {
			t.Fatal(err)
		}
		sy, _ := ParseGTIDSet(y)
		if got, want := sx.Contains(sy), gx.Contain(gy); got != want {
			t.Fatalf("contain %q ⊇ %q: %v want %v", x, y, got, want)
		}
		u := sx.Clone()
		u.AddAll(sy)
		_ = gx.Update(y)
		back, _ := ParseGTIDSet(gx.String())
		if u.String() != back.String() {
			t.Fatalf("union %q %q: %q want %q", x, y, u.String(), back.String())
		}
		if !strings.Contains(x, ":tg:") && !strings.Contains(y, ":tg:") && u.String() != gx.String() {
			t.Fatalf("untagged union text %q want %q", u.String(), gx.String())
		}
	}
}
