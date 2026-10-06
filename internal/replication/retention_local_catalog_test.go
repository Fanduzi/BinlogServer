// Package replication provides module-level functionality for replication.
// input: a catalog with no uploader, an aged sealed file, a row whose file is already gone, and an UPLOADED row inside a longer bucket window
// output: proof that local-only retention drops the catalog row so the files list, download, replay, and replay archive no longer offer it, and that an UPLOADED row stays listed and downloadable from the bucket
// pos: HTTP coverage for retention issue 209 on the file-open purge
// note: if this file changes, update this header and module README.md.
package replication

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/api"
	"binlog_server/internal/tasks"
)

func TestLocalOnlyRetentionCatalogAgreesWithFilesAPI(t *testing.T) {
	dir := t.TempDir()
	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{}}
	scheduler := tasks.NewScheduler(
		tasks.WithDataDir(dir),
		tasks.WithFileStore(catalog),
		tasks.WithEventStore(catalog),
	)
	handler := api.NewServer(scheduler)
	taskID := createRetentionTask(t, handler)

	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	removedPath := filepath.Join(taskDir, "mysql-bin.000003")
	youngPath := filepath.Join(taskDir, "mysql-bin.000005")
	if err := os.WriteFile(removedPath, []byte("removed-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(youngPath, []byte("young-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(removedPath, old, old); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(taskDir, "mysql-bin.000004")
	catalog.rows = map[string]tasks.BinlogFile{
		"mysql-bin.000003": {
			TaskID: taskID, FileName: "mysql-bin.000003", FilePath: removedPath,
			State: "SEALED", UploadState: "LOCAL_ONLY",
		},
		"mysql-bin.000004": {
			TaskID: taskID, FileName: "mysql-bin.000004", FilePath: orphanPath,
			State: "SEALED", UploadState: "LOCAL_ONLY",
		},
		"mysql-bin.000005": {
			TaskID: taskID, FileName: "mysql-bin.000005", FilePath: youngPath,
			State: "SEALED", UploadState: "LOCAL_ONLY",
		},
	}

	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 1, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()

	if _, err := os.Stat(removedPath); !os.IsNotExist(err) {
		t.Fatalf("aged file still on disk: %v", err)
	}
	if _, err := os.Stat(youngPath); err != nil {
		t.Fatalf("young file: %v", err)
	}
	if _, ok := catalog.rows["mysql-bin.000003"]; ok {
		t.Fatal("aged catalog row still present")
	}
	if _, ok := catalog.rows["mysql-bin.000004"]; ok {
		t.Fatal("already-gone catalog row still present")
	}
	if _, ok := catalog.rows["mysql-bin.000005"]; !ok {
		t.Fatal("young catalog row removed")
	}
	if countRetentionRemoved(catalog.events, "mysql-bin.000003") != 1 || countRetentionRemoved(catalog.events, "mysql-bin.000004") != 1 {
		t.Fatalf("events %+v", catalog.events)
	}
	if countRetentionRemoved(catalog.events, "mysql-bin.000005") != 0 {
		t.Fatalf("events %+v", catalog.events)
	}

	listed := getRetentionFiles(t, handler, "/api/tasks/"+taskID+"/files")
	byName := map[string]tasks.BinlogFile{}
	for _, row := range listed {
		byName[row.FileName] = row
		base := filepath.Base(row.FilePath)
		if base == "mysql-bin.000003" || base == "mysql-bin.000004" || row.FileName == "mysql-bin.000003" || row.FileName == "mysql-bin.000004" {
			t.Fatalf("files list still offers a removed binlog: %+v", row)
		}
		if row.Location != "local" && row.Location != "bucket" && row.Location != "both" {
			t.Fatalf("listed file is not available: %+v", row)
		}
		assertRetentionDownload(t, handler, "/api/tasks/"+taskID+"/files/"+base)
	}
	if _, ok := byName["mysql-bin.000005"]; !ok || byName["mysql-bin.000005"].Location != "local" {
		t.Fatalf("young file %+v", byName["mysql-bin.000005"])
	}
	assertRetentionStatus(t, handler, "/api/tasks/"+taskID+"/files/mysql-bin.000003", http.StatusNotFound)

	replay := getRetentionReplay(t, handler, "/api/tasks/"+taskID+"/replay")
	for _, path := range replay.Paths {
		base := filepath.Base(path)
		if base == "mysql-bin.000003" || base == "mysql-bin.000004" {
			t.Fatalf("replay offers %s in %+v", base, replay.Paths)
		}
	}
	if !containsPathBase(replay.Paths, "mysql-bin.000005") {
		t.Fatalf("replay paths %+v", replay.Paths)
	}
	archive := getRetentionArchive(t, handler, "/api/tasks/"+taskID+"/replay/archive")
	if _, ok := archive["mysql-bin.000003"]; ok {
		t.Fatal("archive contains the removed file")
	}
	if _, ok := archive["mysql-bin.000004"]; ok {
		t.Fatal("archive contains the already-gone file")
	}
	if string(archive["mysql-bin.000005"]) != "young-bytes" {
		t.Fatalf("archive young bytes %q", archive["mysql-bin.000005"])
	}

	events := getRetentionEvents(t, handler, "/api/tasks/"+taskID+"/events")
	if countRetentionRemoved(events, "mysql-bin.000003") != 1 || countRetentionRemoved(events, "mysql-bin.000004") != 1 {
		t.Fatalf("api events %+v", events)
	}

	before := len(catalog.events)
	file, _, _, err = runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 1, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if len(catalog.events) != before {
		t.Fatalf("second pass appended events %+v", catalog.events[before:])
	}
}

func TestLocalOnlyRetentionKeepsRowOutsideTaskDir(t *testing.T) {
	dir := t.TempDir()
	taskID := "task-1"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(t.TempDir(), "mysql-bin.000008")
	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
		"mysql-bin.000008": {
			TaskID: taskID, FileName: "mysql-bin.000008", FilePath: otherPath,
			State: "SEALED", UploadState: "LOCAL_ONLY",
		},
	}}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 1, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, ok := catalog.rows["mysql-bin.000008"]; !ok {
		t.Fatal("other directory catalog row removed")
	}
	if countRetentionRemoved(catalog.events, "mysql-bin.000008") != 0 {
		t.Fatalf("events %+v", catalog.events)
	}
}

func TestSplitRetentionUploadedRowStaysDownloadable(t *testing.T) {
	dir := t.TempDir()
	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{}}
	const objectKey = "bucket/mysql-bin.000003"
	objectBody := []byte("from-bucket")
	bucket := &retentionBucket{key: objectKey, body: objectBody}
	scheduler := tasks.NewScheduler(
		tasks.WithDataDir(dir),
		tasks.WithFileStore(catalog),
		tasks.WithEventStore(catalog),
		tasks.WithFileUploader(bucket),
	)
	handler := api.NewServer(scheduler)
	taskID := createRetentionTask(t, handler)

	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mid := time.Now().Add(-2 * 24 * time.Hour)
	uploadedPath := filepath.Join(taskDir, "mysql-bin.000003")
	youngPath := filepath.Join(taskDir, "mysql-bin.000005")
	if err := os.WriteFile(uploadedPath, []byte("local-copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(youngPath, []byte("young-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(uploadedPath, mid, mid); err != nil {
		t.Fatal(err)
	}
	catalog.rows = map[string]tasks.BinlogFile{
		"mysql-bin.000003": {
			TaskID: taskID, FileName: "mysql-bin.000003", FilePath: uploadedPath,
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: objectKey,
			Checksum: tasks.ChecksumMatch, SealedAt: mid,
		},
		"mysql-bin.000005": {
			TaskID: taskID, FileName: "mysql-bin.000005", FilePath: youngPath,
			State: "SEALED", UploadState: "LOCAL_ONLY",
		},
	}

	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(bucket))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 1, Storage: tasks.Storage{
			RetentionDays: 7, LocalRetentionDays: 1, BucketRetentionDays: 7,
		},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()

	if _, err := os.Stat(uploadedPath); !os.IsNotExist(err) {
		t.Fatalf("local copy still present: %v", err)
	}
	if _, ok := catalog.rows["mysql-bin.000003"]; !ok {
		t.Fatal("uploaded catalog row removed inside bucket retention")
	}
	if len(bucket.deleted) != 0 {
		t.Fatalf("object deleted: %v", bucket.deleted)
	}
	if countRetentionRemoved(catalog.events, "mysql-bin.000003") != 0 {
		t.Fatalf("uploaded row announced removed: %+v", catalog.events)
	}

	listed := getRetentionFiles(t, handler, "/api/tasks/"+taskID+"/files")
	var uploaded tasks.BinlogFile
	seen := false
	for _, row := range listed {
		base := filepath.Base(row.FilePath)
		if row.Location != "local" && row.Location != "bucket" && row.Location != "both" {
			t.Fatalf("listed file is not available: %+v", row)
		}
		body := assertRetentionDownload(t, handler, "/api/tasks/"+taskID+"/files/"+base)
		if row.FileName == "mysql-bin.000003" {
			uploaded = row
			seen = true
			if row.Location != "bucket" || string(body) != string(objectBody) {
				t.Fatalf("uploaded download location=%s body=%q", row.Location, body)
			}
		}
	}
	if !seen || uploaded.UploadState != "UPLOADED" {
		t.Fatalf("uploaded row %+v", uploaded)
	}
	replay := getRetentionReplay(t, handler, "/api/tasks/"+taskID+"/replay")
	if !containsPathBase(replay.Paths, "mysql-bin.000003") {
		t.Fatalf("replay paths %+v", replay.Paths)
	}
	bucketLoc := false
	for i, path := range replay.Paths {
		if filepath.Base(path) == "mysql-bin.000003" {
			if i >= len(replay.Locations) || replay.Locations[i] != "bucket" {
				t.Fatalf("replay locations %+v", replay.Locations)
			}
			bucketLoc = true
		}
	}
	if !bucketLoc {
		t.Fatal("replay missing bucket location")
	}
	archive := getRetentionArchive(t, handler, "/api/tasks/"+taskID+"/replay/archive")
	if string(archive["mysql-bin.000003"]) != string(objectBody) {
		t.Fatalf("archive object bytes %q", archive["mysql-bin.000003"])
	}
	if string(archive["mysql-bin.000005"]) != "young-bytes" {
		t.Fatalf("archive young bytes %q", archive["mysql-bin.000005"])
	}
}

type retentionBucket struct {
	key     string
	body    []byte
	deleted []string
}

func (b *retentionBucket) DeleteObject(_ context.Context, key string) error {
	b.deleted = append(b.deleted, key)
	return nil
}

func (b *retentionBucket) UploadFile(context.Context, string, string, string) error {
	return nil
}

func (b *retentionBucket) OpenObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	if key != b.key {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b.body)), int64(len(b.body)), nil
}

func createRetentionTask(t *testing.T, handler http.Handler) string {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"retention-209","cluster_key":"retention-209","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"},"storage":{"retention_days":7}}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create id: %v body=%s", err, resp.Body.String())
	}
	return created.ID
}

func getRetentionFiles(t *testing.T, handler http.Handler, path string) []tasks.BinlogFile {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("files status=%d body=%s", resp.Code, resp.Body.String())
	}
	var files []tasks.BinlogFile
	if err := json.Unmarshal(resp.Body.Bytes(), &files); err != nil {
		t.Fatalf("files decode: %v body=%s", err, resp.Body.String())
	}
	return files
}

type retentionReplay struct {
	Paths     []string `json:"paths"`
	Locations []string `json:"locations"`
}

func getRetentionReplay(t *testing.T, handler http.Handler, path string) retentionReplay {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", resp.Code, resp.Body.String())
	}
	var body retentionReplay
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("replay decode: %v body=%s", err, resp.Body.String())
	}
	return body
}

func getRetentionArchive(t *testing.T, handler http.Handler, path string) map[string][]byte {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", resp.Code, resp.Body.String())
	}
	tr := tar.NewReader(bytes.NewReader(resp.Body.Bytes()))
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = body
	}
}

func getRetentionEvents(t *testing.T, handler http.Handler, path string) []tasks.TaskEvent {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", resp.Code, resp.Body.String())
	}
	var events []tasks.TaskEvent
	if err := json.Unmarshal(resp.Body.Bytes(), &events); err != nil {
		t.Fatalf("events decode: %v body=%s", err, resp.Body.String())
	}
	return events
}

func assertRetentionDownload(t *testing.T, handler http.Handler, path string) []byte {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, resp.Code, resp.Body.String())
	}
	return resp.Body.Bytes()
}

func assertRetentionStatus(t *testing.T, handler http.Handler, path string, status int) {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != status {
		t.Fatalf("%s status=%d body=%s", path, resp.Code, resp.Body.String())
	}
}

func containsPathBase(paths []string, name string) bool {
	for _, path := range paths {
		if filepath.Base(path) == name {
			return true
		}
	}
	return false
}
