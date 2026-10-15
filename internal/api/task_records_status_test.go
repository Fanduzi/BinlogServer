// Package api provides module-level functionality for api.
// input: errors from a refused task write caused by the standalone task record (locked file, failed save) and other refusals
// output: ADR 0010 §API status: record refusals answer 503 TASK_RECORDS_UNAVAILABLE with the reason; other errors keep their status
// pos: HTTP status coverage for #393 QA gap (c)
// note: if this file changes, update this header and module README.md.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
