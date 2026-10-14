// Package replication provides module-level functionality for replication.
// input: live MariaDB 10.11 and 11.4 sources with binlog_format=ROW, started at pos 4, LATEST, FILE_POS mid-file, and Stop/Start
// output: proof each run keeps running, stores row events, and writes end_log_pos values that chain
// pos: regression for issue 359 (MariaDB 11.x dumps that stored only GTID+XID) and the #362 QA STREAM_REGRESSION on mid-file starts
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

// TestMariaDBDumpMatrix runs MariaDB 10.11 and 11.4 sources through every
// way a dump starts: file start (pos 4), LATEST, FILE_POS in the middle of a
// file, and Stop then Start (which resumes mid-file). Each must keep running,
// store row events, and write a segment whose end_log_pos values chain
// (end - size == previous end). Issues 359 and the #362 QA regression.
func TestMariaDBDumpMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	for i, image := range []string{"mariadb:10.11", "mariadb:11.4"} {
		image, port := image, 23317+i
		t.Run(image, func(t *testing.T) {
			db := startMariaDB(t, image, port)
			scenarios := []string{"pos4", "latest", "midfile", "stopstart"}
			for _, sc := range scenarios {
				sc := sc
				t.Run(sc, func(t *testing.T) { runMariaDBScenario(t, db, port, sc) })
			}
		})
	}
}

func startMariaDB(t *testing.T, image string, port int) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("bs-mdb-%d", time.Now().UnixNano())
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "MARIADB_ROOT_PASSWORD=root", "-e", "MARIADB_DATABASE=d",
		"-p", fmt.Sprintf("%d:3306", port), image,
		"--log-bin=mysql-bin", "--binlog-format=ROW", "--server-id=11", "--gtid-strict-mode=1")
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("docker run %s: %v %s", image, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	dsn := fmt.Sprintf("root:root@tcp(127.0.0.1:%d)/d", port)
	deadline := time.Now().Add(90 * time.Second)
	for {
		db, err := sql.Open("mysql", dsn)
		if err == nil {
			if err = db.Ping(); err == nil {
				t.Cleanup(func() { _ = db.Close() })
				if _, err := db.Exec(`CREATE TABLE t (id INT PRIMARY KEY, v INT)`); err != nil {
					t.Fatal(err)
				}
				return db
			}
			_ = db.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not ready: %v", image, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

var mdbRowID int64

func writeMariaDBRows(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		mdbRowID++
		if _, err := db.Exec(`INSERT INTO t VALUES (?, 1)`, mdbRowID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE t SET v = v + 1 WHERE id = ?`, mdbRowID); err != nil {
			t.Fatal(err)
		}
	}
}

type mdbRun struct {
	cancel context.CancelFunc
	done   chan error
}

func startMariaDBRun(runner *MySQLRunner, task tasks.Task) *mdbRun {
	ctx, cancel := context.WithCancel(context.Background())
	r := &mdbRun{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- runner.Run(ctx, task) }()
	return r
}

// stop cancels the run; a run that already ended with an error fails.
func (r *mdbRun) stop(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.done:
		r.cancel()
		t.Fatalf("dump ended before stop: %v", err)
	default:
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		t.Fatal("run did not stop")
	}
}

func runMariaDBScenario(t *testing.T, db *sql.DB, port int, scenario string) {
	writeMariaDBRows(t, db, 3)
	var file string
	var pos uint32
	if err := masterStatus(db, &file, &pos); err != nil {
		t.Fatal(err)
	}
	start := tasks.StartConfig{Mode: tasks.StartModeFilePos, File: file, Pos: 4}
	switch scenario {
	case "latest":
		start = tasks.StartConfig{Mode: tasks.StartModeLatest}
	case "midfile":
		start = tasks.StartConfig{Mode: tasks.StartModeFilePos, File: file, Pos: pos}
	}
	dir := t.TempDir()
	runner := NewMySQLRunner(dir)
	task := tasks.Task{
		ID: "mdb-" + scenario, Epoch: 1,
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: uint16(port), User: "root", Password: "root", Flavor: "mariadb", ServerID: uint32(91000 + port)},
		Start:  start,
	}
	run := startMariaDBRun(runner, task)
	time.Sleep(2 * time.Second) // LATEST resolves its start before these rows
	writeMariaDBRows(t, db, 5)
	segPath := waitRowEvents(t, dir, task.ID, run, 1)
	run.stop(t)
	if scenario == "stopstart" {
		before := rowEventCount(t, segPath)
		writeMariaDBRows(t, db, 5)
		run = startMariaDBRun(runner, task)
		waitRowEvents(t, dir, task.ID, run, before+1)
		time.Sleep(time.Second)
		writeMariaDBRows(t, db, 2)
		waitRowEvents(t, dir, task.ID, run, before+2)
		run.stop(t)
	}
	checkEndPosChain(t, segPath)
}

func rowEventCount(t *testing.T, path string) int {
	c := countEventTypes(t, path)
	return c[goreplication.TABLE_MAP_EVENT] + c[goreplication.WRITE_ROWS_EVENTv1] + c[goreplication.WRITE_ROWS_EVENTv2] +
		c[goreplication.UPDATE_ROWS_EVENTv1] + c[goreplication.UPDATE_ROWS_EVENTv2]
}

// waitRowEvents waits until the open segment holds at least want row events.
// The dump ending first (STREAM_REGRESSION) fails at once.
func waitRowEvents(t *testing.T, dir, id string, run *mdbRun, want int) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-run.done:
			t.Fatalf("dump ended: %v", err)
		default:
		}
		matches, _ := filepath.Glob(filepath.Join(dir, id, "*.open.e*"))
		for _, m := range matches {
			if rowEventCount(t, m) >= want {
				return m
			}
		}
		if time.Now().After(deadline) {
			for _, m := range matches {
				t.Logf("%s types=%v", m, countEventTypes(t, m))
			}
			run.cancel()
			t.Fatalf("no segment with %d row events (issue 359)", want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// checkEndPosChain: after the format description, every stored event's
// end_log_pos is non-zero and equals the previous end plus its size.
func checkEndPosChain(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var prev uint32
	for off := 4; off+19 <= len(raw); {
		sz := binary.LittleEndian.Uint32(raw[off+9 : off+13])
		end := binary.LittleEndian.Uint32(raw[off+13 : off+17])
		kind := goreplication.EventType(raw[off+4])
		if int(sz) < 19 || off+int(sz) > len(raw) {
			break
		}
		if kind != goreplication.FORMAT_DESCRIPTION_EVENT {
			if end == 0 {
				t.Fatalf("%s at %d stored with end_log_pos 0", kind, off)
			}
			if prev != 0 && end-sz != prev {
				t.Fatalf("%s at %d: end %d - size %d != previous end %d", kind, off, end, sz, prev)
			}
			prev = end
		}
		off += int(sz)
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
