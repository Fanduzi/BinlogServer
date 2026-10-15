// Package api provides module-level functionality for api.
// input: errors from a refused task write caused by the standalone task record (locked file, failed save) and other refusals
// output: ADR 0010 §API status: record refusals answer 503 TASK_RECORDS_UNAVAILABLE with the reason; other errors keep their status
// pos: HTTP status coverage for #393 QA gap (c)
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"binlog_server/internal/taskrecords"
	"binlog_server/internal/tasks"
)

func TestTaskRecordRefusalsAre503(t *testing.T) {
	for name, err := range map[string]error{
		"locked":    fmt.Errorf("create %w: %w (wrong_key): detail", tasks.ErrTransitionNotRecorded, tasks.ErrTaskRecordsLocked),
		"not saved": fmt.Errorf("start %w: %w: no space left on device", tasks.ErrTransitionNotRecorded, tasks.ErrTaskRecordsNotSaved),
	} {
		for helper, write := range map[string]func(http.ResponseWriter, error){"writeTaskError": writeTaskError, "writeServerError": writeServerError} {
			rec := httptest.NewRecorder()
			write(rec, err)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s via %s: status %d, want 503", name, helper, rec.Code)
			}
			var body apiErrorBody
			if json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Code != "TASK_RECORDS_UNAVAILABLE" || !strings.Contains(body.Error, "task records") {
				t.Fatalf("%s via %s: body %s", name, helper, rec.Body.String())
			}
		}
	}
	rec := httptest.NewRecorder()
	writeServerError(rec, errors.New("boom"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("other error: status %d, want 500", rec.Code)
	}
}

// toggledRecordFile is the standalone record whose save starts failing
// (a full data_dir) after the task was created.
type toggledRecordFile struct {
	*taskrecords.Store
	fail bool
}

func (f *toggledRecordFile) Save(plain []byte) error {
	if f.fail {
		return errors.New("no space left on device")
	}
	return f.Store.Save(plain)
}

// #395 QA P3: a Start refused because the record could not be saved is a
// 503 TASK_RECORDS_UNAVAILABLE like every other record refusal, not a 400.
func TestStartRefusedBySaveFailureIs503(t *testing.T) {
	dir := t.TempDir()
	key, err := taskrecords.ParseKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	file := &toggledRecordFile{Store: taskrecords.Open(dir, key, nil)}
	s := tasks.NewScheduler(tasks.WithDataDir(dir), tasks.WithRunner(&fakeAPIRunner{}), tasks.WithClusterLeaseManager(tasks.NewMemoryLease()),
		tasks.WithClusterWorkerID("standalone"), tasks.WithTaskRecords(file, nil))
	if err := s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTaskFromSpec("s", "s-key", &tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "pw", Flavor: "mysql", ServerID: 9}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	file.fail = true
	rec := httptest.NewRecorder()
	NewServer(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/start", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("start with a failing save: status %d body=%s, want 503", rec.Code, rec.Body.String())
	}
	var body apiErrorBody
	if json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Code != "TASK_RECORDS_UNAVAILABLE" {
		t.Fatalf("body %s", rec.Body.String())
	}
}
