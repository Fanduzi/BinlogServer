// Package api provides module-level functionality for api.
// input: a task directory with 250 sealed segments (more than the old 200-file default)
// output: assertions that GET /replay and /replay/archive without limit return every segment from the first file, that /window agrees with that set (earliest and latest event times are those of the first and last replay files, continuous), and that an explicit limit window says how many earlier files it left out (omitted_files and warning in JSON, X-Replay-Omitted-Files and X-Replay-Warning on the archive)
// pos: regression test for #305: replay must never silently truncate
// note: if this file changes, update this header and module README.md.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

func TestReplay_MoreThan200SegmentsNotTruncated(t *testing.T) {
	const n = 250
	dir := t.TempDir()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	create(`{"name":"mysql","cluster_key":"mysql-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`, t, handler)
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		stamp := base.Add(time.Duration(i-1) * time.Minute).Format("2006-01-02 15:04:05")
		if err := os.WriteFile(filepath.Join(taskDir, fmt.Sprintf("mysql-bin.%06d", i)), apiTimedSegment(stamp), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var full tasks.ReplaySet
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &full); err != nil {
		t.Fatal(err)
	}
	if len(full.Paths) != n || filepath.Base(full.Paths[0]) != "mysql-bin.000001" || filepath.Base(full.Paths[n-1]) != fmt.Sprintf("mysql-bin.%06d", n) {
		t.Fatalf("default replay has %d paths, first %s", len(full.Paths), filepath.Base(full.Paths[0]))
	}
	if full.Warning != "" || full.OmittedFiles != 0 {
		t.Fatalf("full replay warns: %q omitted=%d", full.Warning, full.OmittedFiles)
	}

	var window tasks.RecoveryWindow
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/tasks/1/window", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("window status=%d body=%s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &window); err != nil {
		t.Fatal(err)
	}
	if !window.Continuous || window.Earliest == nil || window.Latest == nil ||
		!window.Earliest.Equal(base) || !window.Latest.Equal(base.Add((n-1)*time.Minute)) {
		t.Fatalf("window does not match the replay set: %s", resp.Body.String())
	}

	archive := getReplayArchive(t, handler, "/api/tasks/1/replay/archive")
	if len(archive.order) != n || archive.order[0] != "mysql-bin.000001" {
		t.Fatalf("default archive has %d members, first %v", len(archive.order), archive.order[:1])
	}

	var limited tasks.ReplaySet
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay?limit=200", nil))
	if err := json.Unmarshal(resp.Body.Bytes(), &limited); err != nil {
		t.Fatal(err)
	}
	if len(limited.Paths) != 200 || limited.OmittedFiles != n-200 || !strings.Contains(limited.Warning, "leaves out 50 earlier replay file(s), starting at mysql-bin.000001") {
		t.Fatalf("limit window is silent: paths=%d omitted=%d warning=%q", len(limited.Paths), limited.OmittedFiles, limited.Warning)
	}
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay/archive?limit=200", nil))
	if resp.Code != http.StatusOK || resp.Header().Get("X-Replay-Omitted-Files") != "50" || !strings.Contains(resp.Header().Get("X-Replay-Warning"), "mysql-bin.000001") {
		t.Fatalf("limit archive is silent: status=%d headers=%v", resp.Code, resp.Header())
	}
}
