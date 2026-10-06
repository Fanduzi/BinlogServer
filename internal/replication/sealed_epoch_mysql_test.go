// Package replication provides module-level functionality for replication.
// input: BINLOG_TEST_META_DSN pointing at a migrated metadata database, and the same sealed-epoch dump used by the in-memory issue 189 tests
// output: two binlog_files rows for one source name at epoch 0 and epoch 1, with start_pos and end_pos taken from the events in each sealed file
// pos: real-MySQL proof that the catalog writer keeps the v0.5.49 failover positions when the segment key is (task_id, source_file, epoch)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/meta"
	"binlog_server/internal/tasks"

	_ "github.com/go-sql-driver/mysql"
)

// TestIssue189_SealedEpochsOnMySQL seals one source file as epoch 0 and epoch 1
// through MySQLTaskStore. Unit tests skip it. The migration e2e sets
// BINLOG_TEST_META_DSN to a schema 5 database and then lists the rows with
// GET /api/tasks/{id}/files.
func TestIssue189_SealedEpochsOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BINLOG_TEST_META_DSN"))
	if dsn == "" {
		t.Skip("BINLOG_TEST_META_DSN is not set")
	}
	store, err := meta.NewMySQLTaskStore(dsn)
	if err != nil {
		t.Fatalf("NewMySQLTaskStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	task := newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: issue189Source, Pos: 4,
	})
	task.Name = "issue189-mysql"
	task.State = tasks.StateStopped
	task.DesiredRun = tasks.TaskDesiredStop
	task.UpdatedAt = time.Now().UTC()
	if err := store.UpsertTask(context.Background(), task); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	base := t.TempDir()
	uploader := &recordingUploader{}
	checkpoints := &memCheckpointStore{}
	const next = "mysql-bin.000010"
	epoch0End := sealEpochFromDump(t, base, store, uploader, checkpoints, 0, tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: issue189Source, Pos: 4,
	}, next, []string{"INSERT INTO pitr VALUES ('EPOCH0')"}, 1_700_000_000, false)
	setCheckpoint(checkpoints, binlog.Checkpoint{File: issue189Source, Pos: 4})
	epoch1End := sealEpochFromDump(t, base, store, uploader, checkpoints, 1, tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: issue189Source, Pos: 4,
	}, next, []string{
		"INSERT INTO pitr VALUES ('EPOCH1-A')",
		"INSERT INTO pitr VALUES ('EPOCH1-B')",
	}, 1_700_000_100, true)

	files, err := store.ListBinlogFiles(context.Background(), task.ID, 20)
	if err != nil {
		t.Fatalf("ListBinlogFiles: %v", err)
	}
	var epoch0, epoch1 *tasks.BinlogFile
	for i := range files {
		row := &files[i]
		if row.FileName != issue189Source {
			continue
		}
		switch row.Epoch {
		case 0:
			epoch0 = row
		case 1:
			epoch1 = row
		}
	}
	if epoch0 == nil || epoch1 == nil {
		t.Fatalf("catalog rows %+v, want epoch 0 and epoch 1 of %s", files, issue189Source)
	}
	if epoch0.State != "SEALED" || epoch0.StartPos != 4 || epoch0.EndPos != epoch0End {
		t.Fatalf("epoch 0 %+v, want SEALED 4..%d", epoch0, epoch0End)
	}
	if epoch1.State != "SEALED" || epoch1.StartPos != 4 || epoch1.EndPos != epoch1End {
		t.Fatalf("epoch 1 %+v, want SEALED 4..%d", epoch1, epoch1End)
	}
	if epoch0.EndPos == epoch1.EndPos {
		t.Fatalf("epoch ends are equal (%d); epoch 1 must keep its own span", epoch0.EndPos)
	}
	if !strings.HasSuffix(epoch1.FilePath, issue189Source+".sealed.e1") {
		t.Fatalf("epoch 1 path %s", epoch1.FilePath)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var mismatched int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM binlog_files
		WHERE task_id = ? AND (source_file IS NULL OR source_file = '' OR source_file <> file_name)
	`, task.ID).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Fatalf("source_file diverged from file_name on %d rows", mismatched)
	}
	t.Logf("ISSUE189 task=%s epoch0=%d epoch1=%d", task.ID, epoch0End, epoch1End)
}
