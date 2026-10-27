// Package replication provides module-level functionality for replication.
// input: VERSION() strings and a fake statement runner
// output: proof MySQL 8.4 uses SHOW BINARY LOG STATUS, older MySQL and MariaDB use SHOW MASTER STATUS, and a working statement is reused
// pos: unit tests for the single binary-log status statement choice
// note: if this file changes, update this header and module README.md.
package replication

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeStatusSQL struct {
	version string
	rows    map[string][]string
	fail    map[string]error
	calls   []string
}

func (f *fakeStatusSQL) queryRow(stmt string) ([]string, error) {
	f.calls = append(f.calls, stmt)
	if stmt == stmtVersion {
		return []string{f.version}, nil
	}
	if err := f.fail[stmt]; err != nil {
		return nil, err
	}
	if row, ok := f.rows[stmt]; ok {
		return row, nil
	}
	return nil, fmt.Errorf("unexpected statement %s", stmt)
}

func TestBinaryLogStatusStatement(t *testing.T) {
	cases := []struct {
		version string
		want    string
	}{
		{version: "5.7.44", want: stmtMasterStatus},
		{version: "5.7.44-48", want: stmtMasterStatus},
		{version: "8.0.36", want: stmtMasterStatus},
		{version: "8.0.36-28.1", want: stmtMasterStatus},
		{version: "8.4.0", want: stmtBinaryLogStatus},
		{version: "8.4.7-log", want: stmtBinaryLogStatus},
		{version: "9.0.1", want: stmtBinaryLogStatus},
		{version: "11.4.2-MariaDB", want: stmtMasterStatus},
		{version: "10.11.6-MariaDB-log", want: stmtMasterStatus},
		{version: "", want: stmtMasterStatus},
		{version: "not-a-version", want: stmtMasterStatus},
	}
	for _, tc := range cases {
		if got := binaryLogStatusStatement(tc.version); got != tc.want {
			t.Errorf("binaryLogStatusStatement(%q)=%q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestBinaryLogStatusRead_MySQL84UsesBinaryLogStatusAndRemembersIt(t *testing.T) {
	q := &fakeStatusSQL{
		version: "8.4.3",
		rows: map[string][]string{
			stmtBinaryLogStatus: {"mysql-bin.000008", "120"},
		},
	}
	cache := &binaryLogStatusCache{}
	got, err := cache.read("127.0.0.1:3306", q)
	if err != nil {
		t.Fatal(err)
	}
	if got.File != "mysql-bin.000008" || got.Pos != 120 {
		t.Fatalf("status = %+v", got)
	}
	if strings.Join(q.calls, " | ") != stmtVersion+" | "+stmtBinaryLogStatus {
		t.Fatalf("first probe sent %v", q.calls)
	}

	q.calls = nil
	if _, err := cache.read("127.0.0.1:3306", q); err != nil {
		t.Fatal(err)
	}
	if strings.Join(q.calls, " | ") != stmtBinaryLogStatus {
		t.Fatalf("cached probe sent %v, want only %s", q.calls, stmtBinaryLogStatus)
	}
}

func TestBinaryLogStatusRead_MySQL80AndMariaDBUseMasterStatus(t *testing.T) {
	for _, version := range []string{"8.0.40", "5.7.44-log", "11.4.5-MariaDB"} {
		q := &fakeStatusSQL{
			version: version,
			rows: map[string][]string{
				stmtMasterStatus: {"mysql-bin.000001", "4"},
			},
		}
		got, err := (&binaryLogStatusCache{}).read(version, q)
		if err != nil {
			t.Fatal(version, err)
		}
		if got.File != "mysql-bin.000001" || got.Pos != 4 {
			t.Fatalf("%s status = %+v", version, got)
		}
		if strings.Join(q.calls, " | ") != stmtVersion+" | "+stmtMasterStatus {
			t.Fatalf("%s probe sent %v", version, q.calls)
		}
	}
}

func TestBinaryLogStatusRead_FallsBackWhenChosenStatementFails(t *testing.T) {
	q := &fakeStatusSQL{
		version: "8.0.36",
		fail: map[string]error{
			stmtMasterStatus: errors.New("syntax error near MASTER"),
		},
		rows: map[string][]string{
			stmtBinaryLogStatus: {"binlog.000002", "88"},
		},
	}
	cache := &binaryLogStatusCache{}
	got, err := cache.read("db:3306", q)
	if err != nil {
		t.Fatal(err)
	}
	if got.File != "binlog.000002" || got.Pos != 88 {
		t.Fatalf("status = %+v", got)
	}
	q.calls = nil
	if _, err := cache.read("db:3306", q); err != nil {
		t.Fatal(err)
	}
	if strings.Join(q.calls, " | ") != stmtBinaryLogStatus {
		t.Fatalf("after fallback, probe sent %v", q.calls)
	}
}

func TestBinaryLogStatusRead_AllStatementsFail(t *testing.T) {
	q := &fakeStatusSQL{
		version: "8.4.0",
		fail: map[string]error{
			stmtBinaryLogStatus: errors.New("binary log status failed"),
			stmtMasterStatus:    errors.New("master status failed"),
			stmtBinlogStatus:    errors.New("binlog status failed"),
		},
	}
	_, err := (&binaryLogStatusCache{}).read("db:3306", q)
	if err == nil || err.Error() != "binlog status failed" {
		t.Fatalf("err = %v, want the last statement error", err)
	}
}

func TestBinaryLogStatusRead_CachedStatementFailureIsRetried(t *testing.T) {
	q := &fakeStatusSQL{
		version: "8.0.36",
		rows: map[string][]string{
			stmtMasterStatus: {"mysql-bin.000001", "4"},
		},
	}
	cache := &binaryLogStatusCache{}
	if _, err := cache.read("db:3306", q); err != nil {
		t.Fatal(err)
	}
	q.rows = map[string][]string{
		stmtBinaryLogStatus: {"mysql-bin.000009", "50"},
	}
	q.fail = map[string]error{
		stmtMasterStatus: errors.New("removed"),
	}
	got, err := cache.read("db:3306", q)
	if err != nil {
		t.Fatal(err)
	}
	if got.File != "mysql-bin.000009" || got.Pos != 50 {
		t.Fatalf("status = %+v", got)
	}
}
