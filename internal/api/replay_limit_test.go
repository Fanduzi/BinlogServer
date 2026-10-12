// Package api provides module-level functionality for api.
// input: HTTP GET /api/tasks/{id}/replay and /replay/archive with a bad limit, and with limit next to stop_datetime
// output: assertions that 0, negative, empty and non-numeric limits are HTTP 400 on both routes, and that a limit sent with a point-in-time parameter is reported as ignored (warning in JSON, X-Replay-Warning on the archive) without changing the set
// pos: HTTP coverage for #307
// note: if this file changes, update this header and module README.md.
package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"binlog_server/internal/tasks"
)

func TestTaskAPI_ReplayLimitValidation(t *testing.T) {
	dir := t.TempDir()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	create(`{"name":"mysql","cluster_key":"mysql-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`, t, handler)
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range [][]string{
		{"mysql-bin.000001", "2024-01-01 00:00:00", "2024-01-01 00:30:00"},
		{"mysql-bin.000002", "2024-01-01 01:00:00", "2024-01-01 01:30:00"},
	} {
		if err := os.WriteFile(filepath.Join(taskDir, f[0]), apiTimedSegment(f[1:]...), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, route := range []string{"/api/tasks/1/replay", "/api/tasks/1/replay/archive"} {
		for _, bad := range []string{"0", "-1", "abc", "", "1.5"} {
			for _, extra := range []string{"", "&stop_datetime=2024-01-01+01:20:00"} {
				resp := httptest.NewRecorder()
				handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, route+"?limit="+url.QueryEscape(bad)+extra, nil))
				if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "invalid limit") {
					t.Fatalf("%s limit=%q%s: status=%d body=%s", route, bad, extra, resp.Code, resp.Body.String())
				}
			}
		}
	}

	plain := getPITR(t, handler, "/api/tasks/1/replay", url.Values{"stop_datetime": {"2024-01-01 01:20:00"}})
	limited := getPITR(t, handler, "/api/tasks/1/replay", url.Values{"stop_datetime": {"2024-01-01 01:20:00"}, "limit": {"1"}})
	if strings.Join(plain.Paths, "\n") != strings.Join(limited.Paths, "\n") || plain.Command != limited.Command {
		t.Fatalf("limit changed the point-in-time set: %v vs %v", plain.Paths, limited.Paths)
	}
	if plain.Warning != "" || !strings.Contains(limited.Warning, "limit is ignored with stop_datetime, stop_gtid or start_gtid_set") {
		t.Fatalf("warnings plain=%q limited=%q", plain.Warning, limited.Warning)
	}

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay/archive?limit=1&stop_datetime=2024-01-01+01:20:00", nil))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Header().Get("X-Replay-Warning"), "limit is ignored") {
		t.Fatalf("archive status=%d warning=%q", resp.Code, resp.Header().Get("X-Replay-Warning"))
	}
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay/archive?stop_datetime=2024-01-01+01:20:00", nil))
	if resp.Code != http.StatusOK || resp.Header().Get("X-Replay-Warning") != "" {
		t.Fatalf("archive without limit status=%d warning=%q", resp.Code, resp.Header().Get("X-Replay-Warning"))
	}
}
