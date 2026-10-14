// Package replication provides module-level functionality for replication.
// input: a live MariaDB 11.4 source with binlog_format=ROW and a few inserted rows
// output: proof the stored open segment contains at least one Table_map or Write_rows event
// pos: regression for issue 359 (MariaDB 11.x dumps that stored only GTID+XID)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
	_ "github.com/go-sql-driver/mysql"
)

func TestMariaDB11DumpWritesRowEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("bs-mdb11-%d", time.Now().UnixNano())
	port := "23317"
	ctx := context.Background()
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-e", "MARIADB_ROOT_PASSWORD=root",
		"-e", "MARIADB_DATABASE=d",
		"-p", port+":3306",
		"mariadb:11.4",
		"--log-bin=mysql-bin",
		"--binlog-format=ROW",
		"--server-id=11",
		"--gtid-strict-mode=1",
	)
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("docker run mariadb:11.4: %v %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	dsn := fmt.Sprintf("root:root@tcp(127.0.0.1:%s)/d", port)
	var db *sql.DB
	deadline := time.Now().Add(60 * time.Second)
	for {
		var err error
		db, err = sql.Open("mysql", dsn)
		if err == nil {
			if err = db.Ping(); err == nil {
				break
			}
			_ = db.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("mariadb not ready: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	defer db.Close()
	var file string
	var pos uint32
	if err := masterStatus(db, &file, &pos); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (id INT PRIMARY KEY, v INT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (1,10),(2,20),(3,30)`); err != nil {
		t.Fatal(err)
	}
	startPos := uint32(4)

	dir := t.TempDir()
	runner := NewMySQLRunner(dir)
	task := tasks.Task{
		ID:    "mdb11",
		Epoch: 1,
		Source: tasks.SourceConfig{
			Host: "127.0.0.1", Port: 23317, User: "root", Password: "root",
			Flavor: "mariadb", ServerID: 91011,
		},
		Start: tasks.StartConfig{Mode: tasks.StartModeFilePos, File: file, Pos: startPos},
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx, task) }()

	deadline = time.Now().Add(30 * time.Second)
	var openPath string
	for {
		matches, _ := filepath.Glob(filepath.Join(dir, "mdb11", "*.open.e1"))
		if len(matches) > 0 {
			if info, err := os.Stat(matches[0]); err == nil && info.Size() > 100 {
				openPath = matches[0]
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("no open segment with data")
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}

	counts := countEventTypes(t, openPath)
	rows := counts[goreplication.TABLE_MAP_EVENT] + counts[goreplication.WRITE_ROWS_EVENTv1] +
		counts[goreplication.WRITE_ROWS_EVENTv2] + counts[goreplication.UPDATE_ROWS_EVENTv1] +
		counts[goreplication.UPDATE_ROWS_EVENTv2]
	if rows == 0 {
		t.Fatalf("MariaDB 11.4 dump stored no row events; types=%v (issue 359)", counts)
	}
}

func countEventTypes(t *testing.T, path string) map[goreplication.EventType]int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[goreplication.EventType]int{}
	if len(raw) < 4 {
		return out
	}
	off := 4
	for off+19 <= len(raw) {
		sz := int(binary.LittleEndian.Uint32(raw[off+9 : off+13]))
		if sz < 19 || off+sz > len(raw) {
			break
		}
		out[goreplication.EventType(raw[off+4])]++
		off += sz
	}
	return out
}

func masterStatus(db *sql.DB, file *string, pos *uint32) error {
	rows, err := db.Query(`SHOW MASTER STATUS`)
	if err != nil {
		rows, err = db.Query(`SHOW BINARY LOG STATUS`)
		if err != nil {
			return err
		}
	}
	defer rows.Close()
	if !rows.Next() {
		return fmt.Errorf("no master status row")
	}
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	dest := make([]any, len(cols))
	var f sql.NullString
	var p sql.NullInt64
	for i := range dest {
		switch i {
		case 0:
			dest[i] = &f
		case 1:
			dest[i] = &p
		default:
			dest[i] = new(sql.NullString)
		}
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	*file = f.String
	*pos = uint32(p.Int64)
	return nil
}
