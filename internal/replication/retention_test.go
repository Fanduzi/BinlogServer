// Package replication provides module-level functionality for replication.
// input: local segment files, retention clock, catalog rows, and an object deleter
// output: proof that retention deletes an expired checksum-matched uploaded object, keeps a mismatched or unchecked uploaded file and records it UPLOAD_FAILED, keeps a segment inside retention and an open segment, leaves the local file plus OBJECT_PURGE_FAILED when a matched object's delete fails without rewriting checksum, keeps an expired UPLOAD_FAILED or LOCAL_ONLY file and its catalog row when upload and a catalog are configured, removes only the local file when bucket retention is longer, leaves a single-key config on the old full purge, and reads a catalog larger than one page through bounded pages without the unbounded list
// pos: retention purge coverage for the replication file-open path
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

// TestCleanupExpiredBinlogs_RemovesOldFiles 验证相关行为。
func TestCleanupExpiredBinlogs_RemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	oldFile := filepath.Join(dir, "mysql-bin.000001")
	newFile := filepath.Join(dir, "mysql-bin.000002")

	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	if err := os.WriteFile(newFile, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	now := time.Now()
	oldMtime := now.Add(-10 * 24 * time.Hour)
	newMtime := now.Add(-2 * time.Hour)
	if err := os.Chtimes(oldFile, oldMtime, oldMtime); err != nil {
		t.Fatalf("chtimes old file: %v", err)
	}
	if err := os.Chtimes(newFile, newMtime, newMtime); err != nil {
		t.Fatalf("chtimes new file: %v", err)
	}

	if err := cleanupExpiredBinlogs(dir, 7, now, "mysql-bin.000002"); err != nil {
		t.Fatalf("cleanupExpiredBinlogs returned error: %v", err)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("expected old file deleted, stat err=%v", err)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("expected new file kept, stat err=%v", err)
	}
}

// TestCleanupExpiredBinlogs_DefaultRetention 验证相关行为。
func TestCleanupExpiredBinlogs_DefaultRetention(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mysql-bin.000001")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	now := time.Now()
	mtime := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatalf("chtimes file: %v", err)
	}

	if err := cleanupExpiredBinlogs(dir, 0, now, ""); err != nil {
		t.Fatalf("cleanupExpiredBinlogs returned error: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("expected file deleted under default retention, stat err=%v", err)
	}
}

func TestCleanupExpiredBinlogs_KeepsOpenSegment(t *testing.T) {
	dir := t.TempDir()
	openFile := filepath.Join(dir, "mysql-bin.000001.open.e2")
	if err := os.WriteFile(openFile, []byte("open"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(openFile, old, old); err != nil {
		t.Fatal(err)
	}
	if err := cleanupExpiredBinlogs(dir, 7, time.Now(), "mysql-bin.000009.open.e3"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(openFile); err != nil {
		t.Fatalf("open segment deleted, stat err=%v", err)
	}
}

func TestOpenBinlogWriter_PurgesExpiredUploadedObject(t *testing.T) {
	dir := t.TempDir()
	taskID := "task-1"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	fresh := now.Add(-2 * time.Hour)

	expiredPath := filepath.Join(taskDir, "mysql-bin.000001")
	verifiedPath := filepath.Join(taskDir, "mysql-bin.000010")
	freshPath := filepath.Join(taskDir, "mysql-bin.000003")
	openRowPath := filepath.Join(taskDir, "mysql-bin.000004")
	localOnlyPath := filepath.Join(taskDir, "mysql-bin.000005")
	openFilePath := filepath.Join(taskDir, "mysql-bin.000006.open.e1")
	activePath := filepath.Join(taskDir, "mysql-bin.000009.open.e3")
	for path, body := range map[string]string{
		expiredPath:   "expired-uploaded",
		verifiedPath:  "verified-uploaded",
		freshPath:     "fresh-uploaded",
		openRowPath:   "open-row",
		localOnlyPath: "local-only",
		openFilePath:  "open-file",
		activePath:    "active-open",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{expiredPath, verifiedPath, openRowPath, localOnlyPath, openFilePath, activePath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(freshPath, fresh, fresh); err != nil {
		t.Fatal(err)
	}

	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
		"mysql-bin.000001": {
			TaskID: taskID, FileName: "mysql-bin.000001", FilePath: expiredPath,
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "old-object",
			Checksum: tasks.ChecksumMismatch,
		},
		"mysql-bin.000010": {
			TaskID: taskID, FileName: "mysql-bin.000010", FilePath: verifiedPath,
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "verified-object",
			Checksum: tasks.ChecksumMatch,
		},
		"mysql-bin.000003": {
			TaskID: taskID, FileName: "mysql-bin.000003", FilePath: freshPath,
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "fresh-object",
			Checksum: tasks.ChecksumMatch,
		},
		"mysql-bin.000004": {
			TaskID: taskID, FileName: "mysql-bin.000004", FilePath: openRowPath,
			State: "OPEN", UploadState: "UPLOADED", ObjectKey: "open-row-object",
		},
		"mysql-bin.000005": {
			TaskID: taskID, FileName: "mysql-bin.000005", FilePath: localOnlyPath,
			State: "SEALED", UploadState: "LOCAL_ONLY",
		},
		"mysql-bin.000006.open.e1": {
			TaskID: taskID, FileName: "mysql-bin.000006.open.e1", FilePath: openFilePath,
			State: "OPEN", UploadState: "UPLOADED", ObjectKey: "open-file-object",
		},
		"mysql-bin.000008": {
			TaskID: taskID, FileName: "mysql-bin.000008", FilePath: filepath.Join(taskDir, "mysql-bin.000008"),
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "empty-checksum-object",
			Checksum: "",
		},
	}}
	emptyChecksumPath := filepath.Join(taskDir, "mysql-bin.000008")
	if err := os.WriteFile(emptyChecksumPath, []byte("empty-checksum"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(emptyChecksumPath, old, old); err != nil {
		t.Fatal(err)
	}

	deleter := &purgeDeleter{}
	deleter.onDelete = func(key string) {
		if key != "verified-object" {
			t.Errorf("deleted unexpected object %s", key)
		}
		if _, err := os.Stat(verifiedPath); err != nil {
			t.Errorf("local verified file already gone when deleting %s: %v", key, err)
		}
		if _, ok := catalog.rows["mysql-bin.000010"]; !ok {
			t.Errorf("catalog row removed before object delete")
		}
	}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
	file, _, path, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID:                taskID,
		Epoch:             3,
		KeepLocalSegments: true,
		Storage:           tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if filepath.Base(path) != "mysql-bin.000009.open.e3" {
		t.Fatalf("active path %s", path)
	}

	if _, err := os.Stat(expiredPath); err != nil {
		t.Fatalf("mismatched uploaded file removed: %v", err)
	}
	if _, err := os.Stat(emptyChecksumPath); err != nil {
		t.Fatalf("empty-checksum uploaded file removed: %v", err)
	}
	if _, err := os.Stat(verifiedPath); !os.IsNotExist(err) {
		t.Fatalf("verified uploaded file still present: %v", err)
	}
	mismatchRow := catalog.rows["mysql-bin.000001"]
	if mismatchRow.UploadState != "UPLOAD_FAILED" || mismatchRow.Checksum != tasks.ChecksumMismatch || mismatchRow.UploadError != tasks.ChecksumMismatchError || mismatchRow.ObjectKey != "old-object" {
		t.Fatalf("mismatch row: %+v", mismatchRow)
	}
	emptyRow := catalog.rows["mysql-bin.000008"]
	if emptyRow.UploadState != "UPLOAD_FAILED" || emptyRow.Checksum != "" || !strings.HasPrefix(emptyRow.UploadError, tasks.ChecksumVerifyPrefix) || emptyRow.ObjectKey != "empty-checksum-object" {
		t.Fatalf("empty-checksum row: %+v", emptyRow)
	}
	if _, err := os.Stat(localOnlyPath); err != nil {
		t.Fatalf("expired local-only file removed: %v", err)
	}
	if catalog.rows["mysql-bin.000005"].UploadState != "LOCAL_ONLY" {
		t.Fatalf("local-only catalog row changed: %+v", catalog.rows["mysql-bin.000005"])
	}
	if n := countRetentionSkips(catalog.events, "mysql-bin.000005"); n != 1 {
		t.Fatalf("retention skip events=%d %+v", n, catalog.events)
	}
	var localOnlyEvent tasks.TaskEvent
	for _, event := range catalog.events {
		if event.Detail == "LOCAL_ONLY" {
			localOnlyEvent = event
		}
	}
	if !containsAll(localOnlyEvent.Message, "mysql-bin.000005", "upload_state=LOCAL_ONLY") {
		t.Fatalf("event=%+v", catalog.events)
	}
	if n := countRetentionSkips(catalog.events, "mysql-bin.000001"); n != 1 {
		t.Fatalf("mismatch skip events=%d %+v", n, catalog.events)
	}
	if n := countRetentionSkips(catalog.events, "mysql-bin.000008"); n != 1 {
		t.Fatalf("empty-checksum skip events=%d %+v", n, catalog.events)
	}
	if got := runner.RetentionBlockedFiles()["task-1"]; got != 3 {
		t.Fatalf("blocked gauge=%d", got)
	}
	for _, keep := range []string{freshPath, openRowPath, openFilePath, activePath} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("kept file %s missing: %v", keep, err)
		}
	}
	body, err := os.ReadFile(activePath)
	if err != nil || string(body) != "active-open" {
		t.Fatalf("active open segment changed: %q %v", body, err)
	}
	if _, ok := catalog.rows["mysql-bin.000010"]; ok {
		t.Fatal("verified uploaded catalog row still present")
	}
	if catalog.rows["mysql-bin.000003"].ObjectKey != "fresh-object" || catalog.rows["mysql-bin.000003"].Checksum != tasks.ChecksumMatch {
		t.Fatalf("fresh row changed: %+v", catalog.rows["mysql-bin.000003"])
	}
	if catalog.rows["mysql-bin.000004"].ObjectKey != "open-row-object" {
		t.Fatal("open row object was purged")
	}
	deleted := map[string]bool{}
	for _, key := range deleter.keys {
		deleted[key] = true
	}
	if !deleted["verified-object"] || deleted["old-object"] || deleted["empty-checksum-object"] {
		t.Fatalf("deleted keys = %v", deleter.keys)
	}
	for _, kept := range []string{"fresh-object", "open-row-object", "open-file-object"} {
		if deleted[kept] {
			t.Fatalf("object %s was deleted", kept)
		}
	}
	if catalog.listCalls != 3 || catalog.listLimit != retentionCatalogLimit {
		t.Fatalf("list calls=%d limit=%d", catalog.listCalls, catalog.listLimit)
	}

	file.Close()
	deleter.onDelete = nil
	healed := catalog.rows["mysql-bin.000001"]
	healed.UploadState = "UPLOADED"
	healed.Checksum = tasks.ChecksumMatch
	healed.UploadError = ""
	catalog.rows["mysql-bin.000001"] = healed
	file, _, _, err = runner.openBinlogWriter(context.Background(), tasks.Task{
		ID:                taskID,
		Epoch:             3,
		KeepLocalSegments: true,
		Storage:           tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000011", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Fatalf("matched file still present after re-verification: %v", err)
	}
	if _, ok := catalog.rows["mysql-bin.000001"]; ok {
		t.Fatal("matched catalog row still present after re-verification")
	}
	if _, err := os.Stat(emptyChecksumPath); err != nil {
		t.Fatalf("unchecked file removed before it matched: %v", err)
	}
	deleted = map[string]bool{}
	for _, key := range deleter.keys {
		deleted[key] = true
	}
	if !deleted["old-object"] {
		t.Fatalf("re-verified object was not deleted: %v", deleter.keys)
	}
	if got := runner.RetentionBlockedFiles()["task-1"]; got != 2 {
		t.Fatalf("blocked after re-verification=%d", got)
	}
}

func TestOpenBinlogWriter_InsideRetentionDoesNotListOrDelete(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(taskDir, "mysql-bin.000003")
	if err := os.WriteFile(fresh, []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
		"mysql-bin.000003": {
			TaskID: "task-1", FileName: "mysql-bin.000003", State: "SEALED",
			UploadState: "UPLOADED", ObjectKey: "fresh-object", Checksum: tasks.ChecksumMatch,
		},
	}}
	deleter := &purgeDeleter{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: "task-1", Epoch: 1, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if len(deleter.keys) != 0 || catalog.listCalls != 2 {
		t.Fatalf("keys=%v listCalls=%d", deleter.keys, catalog.listCalls)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBinlogWriter_ObjectDeleteFailureKeepsLocalFileAndChecksum(t *testing.T) {
	for _, checksum := range []string{tasks.ChecksumMatch} {
		name := checksum
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			taskDir := filepath.Join(dir, "task-1")
			if err := os.MkdirAll(taskDir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(taskDir, "mysql-bin.000001")
			if err := os.WriteFile(path, []byte("sealed"), 0o644); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-10 * 24 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
				"mysql-bin.000001": {
					TaskID: "task-1", FileName: "mysql-bin.000001", FilePath: path,
					State: "SEALED", UploadState: "UPLOADED", ObjectKey: "old-object",
					Checksum: checksum,
				},
			}}
			deleter := &purgeDeleter{fail: map[string]error{"old-object": errors.New("bucket denied")}}
			runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
			task := tasks.Task{ID: "task-1", Epoch: 1, Storage: tasks.Storage{RetentionDays: 7}}
			_, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000009", 4, "")
			if err == nil || !errors.Is(err, deleter.fail["old-object"]) {
				t.Fatalf("err=%v", err)
			}
			if !containsAll(err.Error(), objectPurgeFailed, "mysql-bin.000001") {
				t.Fatalf("err=%v", err)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("local file removed on failed object delete: %v", statErr)
			}
			row := catalog.rows["mysql-bin.000001"]
			if row.UploadState != "UPLOADED" || row.ObjectKey != "old-object" || row.Checksum != checksum {
				t.Fatalf("row changed: %+v", row)
			}
			if row.UploadError != "bucket denied" {
				t.Fatalf("upload_error=%q", row.UploadError)
			}
			if len(deleter.keys) != 0 {
				t.Fatalf("recorded a successful delete: %v", deleter.keys)
			}

			deleter.fail = nil
			file, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000009", 4, "")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("retry left local file: %v", statErr)
			}
			if _, ok := catalog.rows["mysql-bin.000001"]; ok {
				t.Fatal("retry left catalog row")
			}
			if len(deleter.keys) != 1 || deleter.keys[0] != "old-object" {
				t.Fatalf("retry keys=%v", deleter.keys)
			}
		})
	}
}

func TestOpenBinlogWriter_CatalogDropFailureKeepsLocalFile(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(taskDir, "mysql-bin.000001")
	if err := os.WriteFile(path, []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	catalog := &purgeCatalog{
		rows: map[string]tasks.BinlogFile{
			"mysql-bin.000001": {
				TaskID: "task-1", FileName: "mysql-bin.000001", State: "SEALED",
				UploadState: "UPLOADED", ObjectKey: "old-object", Checksum: tasks.ChecksumMatch,
			},
		},
		dropErr: errors.New("meta down"),
	}
	deleter := &purgeDeleter{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
	task := tasks.Task{ID: "task-1", Epoch: 1, Storage: tasks.Storage{RetentionDays: 7}}
	_, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000009", 4, "")
	if err == nil || !errors.Is(err, catalog.dropErr) || !containsAll(err.Error(), objectPurgeFailed, "remove catalog row") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal(statErr)
	}
	if _, ok := catalog.rows["mysql-bin.000001"]; !ok {
		t.Fatal("row removed when delete failed")
	}
	catalog.dropErr = nil
	file, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("retry left local file: %v", statErr)
	}
}

func TestOpenBinlogWriter_RecordFailureStillSurfacesPurgeError(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(taskDir, "mysql-bin.000001")
	if err := os.WriteFile(path, []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	catalog := &purgeCatalog{
		rows: map[string]tasks.BinlogFile{
			"mysql-bin.000001": {
				TaskID: "task-1", FileName: "mysql-bin.000001", State: "SEALED",
				UploadState: "UPLOADED", ObjectKey: "old-object", Checksum: tasks.ChecksumMatch,
			},
		},
		noteErr: errors.New("cannot write"),
	}
	deleter := &purgeDeleter{fail: map[string]error{"old-object": errors.New("bucket denied")}}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
	_, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: "task-1", Epoch: 1, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err == nil || !errors.Is(err, deleter.fail["old-object"]) || !containsAll(err.Error(), objectPurgeFailed, "also failed to record") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal(statErr)
	}
	if catalog.rows["mysql-bin.000001"].Checksum != tasks.ChecksumMatch {
		t.Fatalf("checksum rewritten: %+v", catalog.rows["mysql-bin.000001"])
	}
}

func TestOpenBinlogWriter_StandaloneDeletesConstructedObject(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	expired := filepath.Join(taskDir, "mysql-bin.000001")
	fresh := filepath.Join(taskDir, "mysql-bin.000002")
	openFile := filepath.Join(taskDir, "mysql-bin.000003.open.e1")
	for _, path := range []string{expired, fresh, openFile} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(expired, old, old); err != nil || os.Chtimes(openFile, old, old) != nil {
		t.Fatal("chtimes")
	}
	deleter := &purgeDeleter{}
	runner := NewMySQLRunner(dir, WithSealedHandler(nil, "prefix"), WithObjectDeleter(deleter))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: "task-1", Epoch: 4, ClusterKey: "cluster", KeepLocalSegments: true, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "server-uuid")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatal("expired local file remains")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(openFile); err != nil {
		t.Fatal(err)
	}
	want := "prefix/cluster/server-uuid/mysql-bin.000001"
	if len(deleter.keys) != 1 || deleter.keys[0] != want {
		t.Fatalf("keys=%v want %s", deleter.keys, want)
	}
}

func TestOpenBinlogWriter_StandaloneDeleteFailureRetries(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	expired := filepath.Join(taskDir, "mysql-bin.000001")
	if err := os.WriteFile(expired, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatal(err)
	}
	deleter := &purgeDeleter{fail: map[string]error{"prefix/cluster/server-uuid/mysql-bin.000001": errors.New("timeout")}}
	runner := NewMySQLRunner(dir, WithSealedHandler(nil, "prefix"), WithObjectDeleter(deleter))
	task := tasks.Task{ID: "task-1", Epoch: 1, ClusterKey: "cluster", Storage: tasks.Storage{RetentionDays: 7}}
	_, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000009", 4, "server-uuid")
	if err == nil || !containsAll(err.Error(), objectPurgeFailed) {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(expired); statErr != nil {
		t.Fatal(statErr)
	}
	deleter.fail = nil
	file, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000009", 4, "server-uuid")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, statErr := os.Stat(expired); !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
}

func TestRetentionCatalogLimitKeepsOldestUploadedRow(t *testing.T) {
	files := make([]tasks.BinlogFile, 201)
	for i := range files {
		name := fmt.Sprintf("mysql-bin.%06d", i+1)
		files[i] = tasks.BinlogFile{FileName: name, FilePath: name, State: "SEALED", UploadState: "UPLOADED", ObjectKey: "k-" + name}
	}
	got := tasks.WindowBinlogFilesForReplay(files, retentionCatalogLimit)
	if len(got) != len(files) || got[0].FileName != "mysql-bin.000001" {
		t.Fatalf("oldest uploaded row dropped: len=%d first=%s", len(got), got[0].FileName)
	}
}

func TestOpenBinlogWriter_CatalogListFailureDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uploaded := filepath.Join(taskDir, "mysql-bin.000001")
	localOnly := filepath.Join(taskDir, "mysql-bin.000002")
	for _, path := range []string{uploaded, localOnly} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-10 * 24 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	catalog := &purgeCatalog{listErr: errors.New("db down")}
	deleter := &purgeDeleter{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
	_, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: "task-1", Epoch: 1, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err == nil || !errors.Is(err, catalog.listErr) || !containsAll(err.Error(), objectPurgeFailed, "list binlog files") {
		t.Fatalf("err=%v", err)
	}
	for _, path := range []string{uploaded, localOnly} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("%s removed while catalog list failed: %v", path, statErr)
		}
	}
	if len(deleter.keys) != 0 {
		t.Fatalf("deleted objects without a catalog: %v", deleter.keys)
	}
}

func TestOpenBinlogWriter_CatalogWithoutDeleteRefusesLocalPurge(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	expired := filepath.Join(taskDir, "mysql-bin.000001")
	if err := os.WriteFile(expired, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatal(err)
	}
	runner := NewMySQLRunner(dir, WithFileMetaStore(&upsertOnlyMeta{}), WithObjectDeleter(&purgeDeleter{}))
	_, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: "task-1", Epoch: 1, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err == nil || !containsAll(err.Error(), objectPurgeFailed, "cannot delete retained objects") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(expired); statErr != nil {
		t.Fatal(statErr)
	}
}

func TestRetentionKeepsExpiredUnuploadedSealedFile(t *testing.T) {
	for _, uploadState := range []string{"UPLOAD_FAILED", "LOCAL_ONLY"} {
		t.Run(uploadState, func(t *testing.T) {
			dir := t.TempDir()
			taskID := "task-1"
			taskDir := filepath.Join(dir, taskID)
			if err := os.MkdirAll(taskDir, 0o755); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			old := now.Add(-10 * 24 * time.Hour)
			sealedPath := filepath.Join(taskDir, "mysql-bin.000001")
			openPath := filepath.Join(taskDir, "mysql-bin.000002")
			if err := os.WriteFile(sealedPath, []byte("only-copy"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(openPath, []byte("open"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(sealedPath, old, old); err != nil || os.Chtimes(openPath, old, old) != nil {
				t.Fatal("chtimes")
			}
			catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
				"mysql-bin.000001": {
					TaskID: taskID, FileName: "mysql-bin.000001", FilePath: sealedPath,
					State: "SEALED", UploadState: uploadState,
				},
				"mysql-bin.000002": {
					TaskID: taskID, FileName: "mysql-bin.000002", FilePath: openPath,
					State: "OPEN", UploadState: "LOCAL_ONLY",
				},
			}}
			catalog.appendErr = errors.New("event store down")
			deleter := &purgeDeleter{}
			runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
			task := tasks.Task{ID: taskID, Epoch: 1, Storage: tasks.Storage{RetentionDays: 7}}
			file, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000009", 4, "")
			if err != nil {
				t.Fatal(err)
			}
			file.Close()
			if _, err := os.Stat(sealedPath); err != nil {
				t.Fatalf("sealed file removed while event append failed: %v", err)
			}
			if _, err := os.Stat(openPath); err != nil {
				t.Fatalf("open row removed: %v", err)
			}
			if len(catalog.events) != 0 {
				t.Fatalf("failed append stored an event: %+v", catalog.events)
			}
			if len(deleter.keys) != 0 {
				t.Fatalf("deleted objects: %v", deleter.keys)
			}

			catalog.appendErr = nil
			file, _, _, err = runner.openBinlogWriter(context.Background(), task, "mysql-bin.000010", 4, "")
			if err != nil {
				t.Fatal(err)
			}
			file.Close()
			if _, err := os.Stat(sealedPath); err != nil {
				t.Fatalf("sealed file removed: %v", err)
			}
			if catalog.rows["mysql-bin.000001"].UploadState != uploadState {
				t.Fatalf("row changed: %+v", catalog.rows["mysql-bin.000001"])
			}
			if catalog.rows["mysql-bin.000002"].State != "OPEN" {
				t.Fatal("open catalog row removed")
			}
			if n := countRetentionSkips(catalog.events, "mysql-bin.000001"); n != 1 {
				t.Fatalf("events=%d %+v", n, catalog.events)
			}
			ev := catalog.events[0]
			if ev.Type != retentionSkippedNotUploaded || ev.Detail != uploadState || !containsAll(ev.Message, "mysql-bin.000001", "upload_state="+uploadState) {
				t.Fatalf("event=%+v", ev)
			}
			if got := runner.RetentionBlockedFiles()[taskID]; got != 1 {
				t.Fatalf("blocked=%d", got)
			}

			file, _, _, err = runner.openBinlogWriter(context.Background(), task, "mysql-bin.000011", 4, "")
			if err != nil {
				t.Fatal(err)
			}
			file.Close()
			if n := countRetentionSkips(catalog.events, "mysql-bin.000001"); n != 1 {
				t.Fatalf("second open flooded events=%d %+v", n, catalog.events)
			}
			if _, err := os.Stat(sealedPath); err != nil {
				t.Fatal(err)
			}

			if uploadState != "UPLOAD_FAILED" {
				return
			}
			row := catalog.rows["mysql-bin.000001"]
			row.UploadState = "UPLOADED"
			row.ObjectKey = "uploaded-object"
			row.Checksum = tasks.ChecksumMatch
			catalog.rows["mysql-bin.000001"] = row
			file, _, _, err = runner.openBinlogWriter(context.Background(), task, "mysql-bin.000012", 4, "")
			if err != nil {
				t.Fatal(err)
			}
			file.Close()
			if _, err := os.Stat(sealedPath); !os.IsNotExist(err) {
				t.Fatalf("uploaded file still present: %v", err)
			}
			if _, ok := catalog.rows["mysql-bin.000001"]; ok {
				t.Fatal("uploaded catalog row still present")
			}
			if len(deleter.keys) != 1 || deleter.keys[0] != "uploaded-object" {
				t.Fatalf("keys=%v", deleter.keys)
			}
			if n := countRetentionSkips(catalog.events, "mysql-bin.000001"); n != 1 {
				t.Fatalf("purge added a skip event: %+v", catalog.events)
			}
			if got := runner.RetentionBlockedFiles()[taskID]; got != 0 {
				t.Fatalf("blocked after purge=%d", got)
			}
		})
	}
}

func TestRetentionNoUploaderStillDeletesExpiredLocalFile(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	sealed := filepath.Join(taskDir, "mysql-bin.000001")
	openPath := filepath.Join(taskDir, "mysql-bin.000002.open.e2")
	for _, path := range []string{sealed, openPath} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
		"mysql-bin.000001": {
			TaskID: "task-1", FileName: "mysql-bin.000001", FilePath: sealed,
			State: "SEALED", UploadState: "LOCAL_ONLY",
		},
		"mysql-bin.000002.open.e2": {
			TaskID: "task-1", FileName: "mysql-bin.000002.open.e2", FilePath: openPath,
			State: "OPEN", UploadState: "LOCAL_ONLY",
		},
	}}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: "task-1", Epoch: 2, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := os.Stat(sealed); !os.IsNotExist(err) {
		t.Fatalf("no-uploader retention left the sealed file: %v", err)
	}
	if _, err := os.Stat(openPath); err != nil {
		t.Fatalf("open segment removed: %v", err)
	}
	if _, ok := catalog.rows["mysql-bin.000001"]; !ok {
		t.Fatal("no-uploader path removed the catalog row")
	}
	if len(catalog.events) != 0 {
		t.Fatalf("no-uploader path wrote a skip event: %+v", catalog.events)
	}
	if got := runner.RetentionBlockedFiles()["task-1"]; got != 0 {
		t.Fatalf("blocked=%d", got)
	}
}

func TestSplitRetentionRemovesLocalFileKeepsObject(t *testing.T) {
	dir := t.TempDir()
	taskID := "task-1"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mid := now.Add(-2 * 24 * time.Hour)
	old := now.Add(-10 * 24 * time.Hour)
	midPath := filepath.Join(taskDir, "mysql-bin.000001")
	oldPath := filepath.Join(taskDir, "mysql-bin.000002")
	if err := os.WriteFile(midPath, []byte("mid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(midPath, mid, mid); err != nil || os.Chtimes(oldPath, old, old) != nil {
		t.Fatal("chtimes")
	}
	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
		"mysql-bin.000001": {
			TaskID: taskID, FileName: "mysql-bin.000001", FilePath: midPath,
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "mid-object",
			Checksum: tasks.ChecksumMatch, SealedAt: mid,
		},
		"mysql-bin.000002": {
			TaskID: taskID, FileName: "mysql-bin.000002", FilePath: oldPath,
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "old-object",
			Checksum: tasks.ChecksumMatch, SealedAt: old,
		},
		"mysql-bin.000008": {
			TaskID: taskID, FileName: "mysql-bin.000008",
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "gone-object",
			SealedAt: old,
		},
		"mysql-bin.000007": {
			TaskID: taskID, FileName: "mysql-bin.000007",
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "young-object",
			SealedAt: mid,
		},
	}}
	deleter := &purgeDeleter{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
	task := tasks.Task{ID: taskID, Epoch: 1, Storage: tasks.Storage{
		RetentionDays: 7, LocalRetentionDays: 1, BucketRetentionDays: 7,
	}}
	file, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := os.Stat(midPath); !os.IsNotExist(err) {
		t.Fatalf("local file still present: %v", err)
	}
	if _, ok := catalog.rows["mysql-bin.000001"]; !ok {
		t.Fatal("catalog row removed inside bucket retention")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("bucket-expired local file still present: %v", err)
	}
	if _, ok := catalog.rows["mysql-bin.000002"]; ok {
		t.Fatal("bucket-expired catalog row still present")
	}
	if _, ok := catalog.rows["mysql-bin.000008"]; ok {
		t.Fatal("bucket-only row past bucket retention still present")
	}
	if _, ok := catalog.rows["mysql-bin.000007"]; !ok {
		t.Fatal("bucket-only row inside bucket retention was removed")
	}
	got := map[string]bool{}
	for _, key := range deleter.keys {
		got[key] = true
	}
	if got["mid-object"] || got["young-object"] || !got["old-object"] || !got["gone-object"] {
		t.Fatalf("deleted keys=%v", deleter.keys)
	}
}

func TestSplitRetentionKeepsUnuploadedAndSingleKeyStillPurges(t *testing.T) {
	dir := t.TempDir()
	taskID := "task-1"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	failedPath := filepath.Join(taskDir, "mysql-bin.000001")
	onlyPath := filepath.Join(taskDir, "mysql-bin.000002")
	for _, path := range []string{failedPath, onlyPath} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
		"mysql-bin.000001": {
			TaskID: taskID, FileName: "mysql-bin.000001", FilePath: failedPath,
			State: "SEALED", UploadState: "UPLOAD_FAILED",
		},
		"mysql-bin.000002": {
			TaskID: taskID, FileName: "mysql-bin.000002", FilePath: onlyPath,
			State: "SEALED", UploadState: "LOCAL_ONLY",
		},
		"mysql-bin.000003": {
			TaskID: taskID, FileName: "mysql-bin.000003",
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "no-age",
		},
	}}
	deleter := &purgeDeleter{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
	task := tasks.Task{ID: taskID, Epoch: 1, Storage: tasks.Storage{
		RetentionDays: 7, LocalRetentionDays: 1, BucketRetentionDays: 30,
	}}
	file, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := os.Stat(failedPath); err != nil {
		t.Fatalf("UPLOAD_FAILED removed: %v", err)
	}
	if _, err := os.Stat(onlyPath); err != nil {
		t.Fatalf("LOCAL_ONLY removed: %v", err)
	}
	if len(deleter.keys) != 0 {
		t.Fatalf("deleted objects: %v", deleter.keys)
	}
	if _, ok := catalog.rows["mysql-bin.000003"]; !ok {
		t.Fatal("bucket-only row with no age was purged")
	}
	if n := countRetentionSkips(catalog.events, "mysql-bin.000001"); n != 1 {
		t.Fatalf("skip events=%d %+v", n, catalog.events)
	}
	if got := runner.RetentionBlockedFiles()[taskID]; got != 2 {
		t.Fatalf("blocked=%d", got)
	}

	singleDir := t.TempDir()
	singleTaskDir := filepath.Join(singleDir, taskID)
	if err := os.MkdirAll(singleTaskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	singlePath := filepath.Join(singleTaskDir, "mysql-bin.000004")
	if err := os.WriteFile(singlePath, []byte("single"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(singlePath, old, old); err != nil {
		t.Fatal(err)
	}
	singleCatalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
		"mysql-bin.000004": {
			TaskID: taskID, FileName: "mysql-bin.000004", FilePath: singlePath,
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "single-object",
			Checksum: tasks.ChecksumMatch,
		},
		"mysql-bin.000005": {
			TaskID: taskID, FileName: "mysql-bin.000005",
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "orphan-object",
			SealedAt: old,
		},
	}}
	singleDeleter := &purgeDeleter{}
	singleRunner := NewMySQLRunner(singleDir, WithFileMetaStore(singleCatalog), WithObjectDeleter(singleDeleter))
	file, _, _, err = singleRunner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 1, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := os.Stat(singlePath); !os.IsNotExist(err) {
		t.Fatalf("single-key file still present: %v", err)
	}
	if _, ok := singleCatalog.rows["mysql-bin.000004"]; ok {
		t.Fatal("single-key catalog row still present")
	}
	if _, ok := singleCatalog.rows["mysql-bin.000005"]; !ok {
		t.Fatal("single-key retention purged a catalog-only row")
	}
	if len(singleDeleter.keys) != 1 || singleDeleter.keys[0] != "single-object" {
		t.Fatalf("single-key keys=%v", singleDeleter.keys)
	}
	if singleCatalog.listCalls == 0 {
		t.Fatal("expired local file did not read the catalog")
	}
}

func countRetentionSkips(events []tasks.TaskEvent, name string) int {
	n := 0
	for _, event := range events {
		if event.Type == retentionSkippedNotUploaded && strings.Contains(event.Message, name) {
			n++
		}
	}
	return n
}

type purgeCatalog struct {
	rows      map[string]tasks.BinlogFile
	events    []tasks.TaskEvent
	listCalls int
	listLimit int
	listErr   error
	dropErr   error
	noteErr   error
	appendErr error
}

func (c *purgeCatalog) AppendEvent(_ context.Context, event tasks.TaskEvent) error {
	if c.appendErr != nil {
		return c.appendErr
	}
	c.events = append(c.events, event)
	return nil
}

func (c *purgeCatalog) UpsertBinlogFile(_ context.Context, meta tasks.BinlogFile) error {
	if c.noteErr != nil {
		return c.noteErr
	}
	if c.rows == nil {
		c.rows = map[string]tasks.BinlogFile{}
	}
	key := meta.FileName
	if meta.Epoch != 0 {
		key = fmt.Sprintf("%s#%d", meta.FileName, meta.Epoch)
	}
	for existing, row := range c.rows {
		if row.FileName == meta.FileName && row.Epoch == meta.Epoch {
			delete(c.rows, existing)
		}
	}
	c.rows[key] = meta
	return nil
}

// pagingPurgeCatalog serves binlog_files in (file_name, epoch) pages.
// The unbounded list fails so a rotate or retention walk that still loads
// the whole catalog cannot pass.
type pagingPurgeCatalog struct {
	*purgeCatalog
	pageCalls int
	maxLimit  int
	fullLists int
}

func (c *pagingPurgeCatalog) ListBinlogFiles(context.Context, string, int) ([]tasks.BinlogFile, error) {
	c.fullLists++
	return nil, errors.New("unbounded catalog list")
}

func (c *pagingPurgeCatalog) ListBinlogFilesPage(_ context.Context, taskID, afterName string, afterEpoch int64, first bool, limit int) ([]tasks.BinlogFile, error) {
	c.pageCalls++
	if limit > c.maxLimit {
		c.maxLimit = limit
	}
	rows := make([]tasks.BinlogFile, 0, len(c.rows))
	for _, row := range c.rows {
		if taskID != "" && row.TaskID != "" && row.TaskID != taskID {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].FileName != rows[j].FileName {
			return rows[i].FileName < rows[j].FileName
		}
		return rows[i].Epoch < rows[j].Epoch
	})
	if !first {
		filtered := rows[:0]
		for _, row := range rows {
			if row.FileName > afterName || (row.FileName == afterName && row.Epoch > afterEpoch) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func catalogRow(rows map[string]tasks.BinlogFile, name string) (tasks.BinlogFile, bool) {
	for _, row := range rows {
		if row.FileName == name {
			return row, true
		}
	}
	return tasks.BinlogFile{}, false
}

func TestRetentionPagesCatalogPastFirstPage(t *testing.T) {
	dir := t.TempDir()
	taskID := "task-1"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	writeOld := func(name string) string {
		t.Helper()
		path := filepath.Join(taskDir, name)
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}
	localPath := writeOld("mysql-bin.900002")
	matchPath := writeOld("mysql-bin.900003")
	mismatchPath := writeOld("mysql-bin.900004")

	rows := map[string]tasks.BinlogFile{}
	for i := 1; i <= catalogPageSize; i++ {
		name := fmt.Sprintf("mysql-bin.%06d", i)
		rows[name] = tasks.BinlogFile{
			TaskID: taskID, FileName: name, Epoch: 1,
			State: "SEALED", UploadState: "LOCAL_ONLY",
		}
	}
	rows["mysql-bin.900001#1"] = tasks.BinlogFile{
		TaskID: taskID, FileName: "mysql-bin.900001", Epoch: 1,
		State: "SEALED", UploadState: "UPLOADED", ObjectKey: "absent-object",
		Checksum: tasks.ChecksumMatch, SealedAt: old,
	}
	rows["mysql-bin.900002#1"] = tasks.BinlogFile{
		TaskID: taskID, FileName: "mysql-bin.900002", FilePath: localPath, Epoch: 1,
		State: "SEALED", UploadState: "LOCAL_ONLY",
	}
	rows["mysql-bin.900003#1"] = tasks.BinlogFile{
		TaskID: taskID, FileName: "mysql-bin.900003", FilePath: matchPath, Epoch: 1,
		State: "SEALED", UploadState: "UPLOADED", ObjectKey: "match-object",
		Checksum: tasks.ChecksumMatch, SealedAt: old,
	}
	rows["mysql-bin.900004#1"] = tasks.BinlogFile{
		TaskID: taskID, FileName: "mysql-bin.900004", FilePath: mismatchPath, Epoch: 1,
		State: "SEALED", UploadState: "UPLOADED", ObjectKey: "mismatch-object",
		Checksum: tasks.ChecksumMismatch, SealedAt: old,
	}
	catalog := &pagingPurgeCatalog{purgeCatalog: &purgeCatalog{rows: rows}}
	deleter := &purgeDeleter{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(deleter))
	task := tasks.Task{ID: taskID, Epoch: 1, Storage: tasks.Storage{
		RetentionDays: 7, LocalRetentionDays: 1, BucketRetentionDays: 30,
	}}
	file, _, _, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.999999", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()

	if catalog.fullLists != 0 || catalog.listCalls != 0 {
		t.Fatalf("unbounded list calls=%d embedded=%d", catalog.fullLists, catalog.listCalls)
	}
	if catalog.pageCalls < 2 || catalog.maxLimit <= 0 || catalog.maxLimit > catalogPageSize {
		t.Fatalf("pageCalls=%d maxLimit=%d", catalog.pageCalls, catalog.maxLimit)
	}
	if _, ok := catalogRow(catalog.rows, "mysql-bin.900001"); ok {
		t.Fatal("absent uploaded row past bucket retention still present")
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("LOCAL_ONLY removed: %v", err)
	}
	if row, ok := catalogRow(catalog.rows, "mysql-bin.900002"); !ok || row.UploadState != "LOCAL_ONLY" {
		t.Fatalf("LOCAL_ONLY row = %+v ok=%v", row, ok)
	}
	if _, err := os.Stat(matchPath); !os.IsNotExist(err) {
		t.Fatalf("checksum-matched upload still on disk: %v", err)
	}
	if _, ok := catalogRow(catalog.rows, "mysql-bin.900003"); ok {
		t.Fatal("checksum-matched upload row still present")
	}
	if _, err := os.Stat(mismatchPath); err != nil {
		t.Fatalf("checksum mismatch removed: %v", err)
	}
	row, ok := catalogRow(catalog.rows, "mysql-bin.900004")
	if !ok || row.UploadState != "UPLOAD_FAILED" || row.Checksum != tasks.ChecksumMismatch {
		t.Fatalf("mismatch row = %+v ok=%v", row, ok)
	}
	deleted := map[string]bool{}
	for _, key := range deleter.keys {
		deleted[key] = true
	}
	if !deleted["absent-object"] || !deleted["match-object"] || deleted["mismatch-object"] {
		t.Fatalf("deleted keys=%v", deleter.keys)
	}
	fillers := 0
	for _, row := range catalog.rows {
		if strings.HasPrefix(row.FileName, "mysql-bin.0") {
			fillers++
		}
	}
	if fillers != catalogPageSize {
		t.Fatalf("filler rows=%d", fillers)
	}
}

func (c *purgeCatalog) ListBinlogFiles(_ context.Context, _ string, limit int) ([]tasks.BinlogFile, error) {
	c.listCalls++
	c.listLimit = limit
	if c.listErr != nil {
		return nil, c.listErr
	}
	out := make([]tasks.BinlogFile, 0, len(c.rows))
	for _, row := range c.rows {
		out = append(out, row)
	}
	return out, nil
}

func (c *purgeCatalog) DeleteBinlogFile(_ context.Context, _, fileName string, epoch int64) error {
	if c.dropErr != nil {
		return c.dropErr
	}
	for key, row := range c.rows {
		if row.FileName == fileName && row.Epoch == epoch {
			delete(c.rows, key)
		}
	}
	if epoch == 0 {
		delete(c.rows, fileName)
	}
	return nil
}

type purgeDeleter struct {
	keys     []string
	fail     map[string]error
	onDelete func(key string)
}

func (d *purgeDeleter) DeleteObject(_ context.Context, key string) error {
	if d.onDelete != nil {
		d.onDelete(key)
	}
	if err := d.fail[key]; err != nil {
		return err
	}
	d.keys = append(d.keys, key)
	return nil
}

type upsertOnlyMeta struct{}

func (upsertOnlyMeta) UpsertBinlogFile(context.Context, tasks.BinlogFile) error { return nil }

func containsAll(got string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(got, part) {
			return false
		}
	}
	return true
}
