// Package api provides module-level functionality for api.
// input: HTTP coverage and restore-plan requests against on-disk segments, including a cluster scheduler and two tasks on one source
// output: assertions for inside, outside, and boundary answers, an ignored limit, 400 and 405 and 501, and a red full-backup precheck
// pos: HTTP coverage for ADR 0009 phases 4 and 5
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

const coverageUUID = "3e11fa47-71ca-11e1-9e33-c80aa9429562"

func TestTaskAPI_CoverageAndRestorePlan(t *testing.T) {
	dir := t.TempDir()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	create(`{"name":"mysql","cluster_key":"mysql-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"},"start":{"mode":"GTID","gtid_set":"`+coverageUUID+`:1-5"}}`, t, handler)
	create(`{"name":"other","cluster_key":"other-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`, t, handler)
	create(`{"name":"maria","cluster_key":"maria-key","source":{"host":"10.0.0.8","port":3306,"user":"repl","password":"secret","flavor":"mariadb"}}`, t, handler)

	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSeg := func(name string, stamps ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(taskDir, name), apiTimedSegment(stamps...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSeg("mysql-bin.000001", "2024-01-01 00:00:00", "2024-01-01 00:10:00")
	writeSeg("mysql-bin.000002", "2024-01-01 00:20:00", "2024-01-01 00:30:00")
	writeSeg("mysql-bin.000003", "2024-01-01 00:40:00", "2024-01-01 00:50:00")

	inside := coverageJSON(t, handler, http.MethodGet, "/api/tasks/1/coverage?at=2024-01-01%2000:10:00&limit=1", nil)
	if inside["covered"] != true {
		t.Fatalf("inside %#v", inside)
	}
	segs := inside["segments"].([]any)
	if len(segs) != 1 || segs[0].(map[string]any)["name"] != "mysql-bin.000001" {
		t.Fatalf("limit must not truncate the prefix: %#v", segs)
	}
	stop := inside["stop_at"].(map[string]any)
	if stop["segment"] != "mysql-bin.000001" || stop["pos"] != float64(4+2*20) {
		t.Fatalf("inside stop %#v", stop)
	}

	boundary := coverageJSON(t, handler, http.MethodGet, "/api/tasks/1/coverage?at=2024-01-01T00:20:00Z", nil)
	if boundary["covered"] != true || boundary["stop_at"].(map[string]any)["pos"] != float64(24) {
		t.Fatalf("boundary %#v", boundary)
	}
	if len(boundary["segments"].([]any)) != 2 {
		t.Fatalf("boundary prefix %#v", boundary["segments"])
	}

	before := coverageJSON(t, handler, http.MethodGet, "/api/tasks/1/coverage?at=2023-12-31%2023:59:59", nil)
	if before["covered"] != false || before["reason"] != "before earliest" || len(before["segments"].([]any)) != 0 {
		t.Fatalf("before %#v", before)
	}
	after := coverageJSON(t, handler, http.MethodGet, "/api/tasks/1/coverage?at=2024-01-01%2001:00:00", nil)
	if after["covered"] != false || after["reason"] != "after latest" {
		t.Fatalf("after %#v", after)
	}

	gtidDir := filepath.Join(dir, "2")
	if err := os.MkdirAll(gtidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gtidDir, "mysql-bin.000001"), coverageGTIDSegment(t, "2024-01-02 00:00:00", "2024-01-02 00:00:10"), 0o644); err != nil {
		t.Fatal(err)
	}
	hit := coverageJSON(t, handler, http.MethodGet, "/api/tasks/2/coverage?gtid="+coverageUUID+":1", nil)
	if hit["covered"] != true || hit["stop_at"].(map[string]any)["segment"] != "mysql-bin.000001" {
		t.Fatalf("gtid hit %#v", hit)
	}
	// The next GTID starts after the first event, so the stop is that offset, not EOF.
	if hit["stop_at"].(map[string]any)["pos"] == hit["segments"].([]any)[0].(map[string]any)["size"] {
		t.Fatalf("gtid stop included the later transaction: %#v", hit["stop_at"])
	}
	miss := coverageJSON(t, handler, http.MethodGet, "/api/tasks/2/coverage?gtid="+coverageUUID+":99", nil)
	if miss["covered"] != false || miss["reason"] != "gtid is not in the retained window" {
		t.Fatalf("gtid miss %#v", miss)
	}

	both := coverageStatus(t, handler, http.MethodGet, "/api/tasks/1/coverage?at=2024-01-01%2000:00:00&gtid="+coverageUUID+":1", nil)
	if both.Code != http.StatusBadRequest || !strings.Contains(both.Body.String(), "at and gtid cannot both be set") {
		t.Fatalf("both %d %s", both.Code, both.Body.String())
	}
	none := coverageStatus(t, handler, http.MethodGet, "/api/tasks/1/coverage", nil)
	if none.Code != http.StatusBadRequest || !strings.Contains(none.Body.String(), "at or gtid is required") {
		t.Fatalf("none %d %s", none.Code, none.Body.String())
	}
	badAt := coverageStatus(t, handler, http.MethodGet, "/api/tasks/1/coverage?at=yesterday", nil)
	if badAt.Code != http.StatusBadRequest || !strings.Contains(badAt.Body.String(), "invalid at") {
		t.Fatalf("at %d %s", badAt.Code, badAt.Body.String())
	}
	badGTID := coverageStatus(t, handler, http.MethodGet, "/api/tasks/1/coverage?gtid=1-1-1", nil)
	if badGTID.Code != http.StatusBadRequest || !strings.Contains(badGTID.Body.String(), "invalid gtid") {
		t.Fatalf("gtid %d %s", badGTID.Code, badGTID.Body.String())
	}
	flavor := coverageStatus(t, handler, http.MethodGet, "/api/tasks/3/coverage?gtid="+coverageUUID+":1", nil)
	if flavor.Code != http.StatusBadRequest || !strings.Contains(flavor.Body.String(), "gtid coverage is not supported for this flavor") {
		t.Fatalf("flavor %d %s", flavor.Code, flavor.Body.String())
	}
	missingTask := coverageStatus(t, handler, http.MethodGet, "/api/tasks/9/coverage?at=2024-01-01%2000:00:00", nil)
	if missingTask.Code != http.StatusNotFound {
		t.Fatalf("missing %d", missingTask.Code)
	}
	postCov := coverageStatus(t, handler, http.MethodPost, "/api/tasks/1/coverage", strings.NewReader(`{}`))
	if postCov.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post coverage %d", postCov.Code)
	}
	getPlan := coverageStatus(t, handler, http.MethodGet, "/api/tasks/1/restore-plan", nil)
	if getPlan.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get plan %d", getPlan.Code)
	}

	plan := coverageJSON(t, handler, http.MethodPost, "/api/tasks/1/restore-plan", strings.NewReader(`{"at":"2024-01-01 00:10:00"}`))
	if !strings.Contains(plan["restore_sh"].(string), "--stop-datetime='2024-01-01 00:10:01'") {
		t.Fatalf("restore.sh %s", plan["restore_sh"])
	}
	if !strings.Contains(plan["bundle"].(map[string]any)["url"].(string), "stop_datetime=2024-01-01+00%3A10%3A01") && !strings.Contains(plan["bundle"].(map[string]any)["url"].(string), "stop_datetime=2024-01-01%2000%3A10%3A01") {
		t.Fatalf("bundle %#v", plan["bundle"])
	}
	if resultOf(t, plan, "window") != "green" || resultOf(t, plan, "full_backup_gtid") != "manual" || resultOf(t, plan, "target_settings") != "manual" {
		t.Fatalf("prechecks %#v", plan["prechecks"])
	}

	hole := coverageJSON(t, handler, http.MethodPost, "/api/tasks/1/restore-plan", strings.NewReader(`{"at":"2024-01-01 00:10:00","full_backup_gtid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:1-9"}`))
	if resultOf(t, hole, "full_backup_gtid") != "red" || !strings.Contains(describe(t, hole, "full_backup_gtid"), "hole") {
		t.Fatalf("hole %#v", hole["prechecks"])
	}
	overlap := coverageJSON(t, handler, http.MethodPost, "/api/tasks/2/restore-plan", strings.NewReader(`{"gtid":"`+coverageUUID+`:2","full_backup_gtid":"`+coverageUUID+`:1-2"}`))
	if overlap["coverage"].(map[string]any)["covered"] != true || resultOf(t, overlap, "full_backup_gtid") != "red" || !strings.Contains(describe(t, overlap, "full_backup_gtid"), "overlap") {
		t.Fatalf("overlap %#v", overlap["prechecks"])
	}
	latest := coverageJSON(t, handler, http.MethodPost, "/api/tasks/1/restore-plan", strings.NewReader(`{"latest":true}`))
	if latest["target"].(map[string]any)["latest"] != true || latest["coverage"].(map[string]any)["covered"] != true {
		t.Fatalf("latest %#v", latest["target"])
	}
	badPlan := coverageStatus(t, handler, http.MethodPost, "/api/tasks/1/restore-plan", strings.NewReader(`{"at":"2024-01-01 00:10:00","latest":true}`))
	if badPlan.Code != http.StatusBadRequest || !strings.Contains(badPlan.Body.String(), "set exactly one of at, gtid, or latest") {
		t.Fatalf("plan %d %s", badPlan.Code, badPlan.Body.String())
	}

	cross := coverageJSON(t, handler, http.MethodGet, "/api/coverage?source=127.0.0.1:3306&at=2024-01-01%2000:00:00", nil)
	tasksOut := cross["tasks"].([]any)
	if cross["source"] != "127.0.0.1:3306" || len(tasksOut) != 2 {
		t.Fatalf("cross %#v", cross)
	}
	emptySrc := coverageJSON(t, handler, http.MethodGet, "/api/coverage?source=127.0.0.1:3999&at=2024-01-01%2000:00:00", nil)
	if len(emptySrc["tasks"].([]any)) != 0 {
		t.Fatalf("empty source %#v", emptySrc)
	}
	badSrc := coverageStatus(t, handler, http.MethodGet, "/api/coverage?source=127.0.0.1&at=2024-01-01%2000:00:00", nil)
	if badSrc.Code != http.StatusBadRequest || !strings.Contains(badSrc.Body.String(), "invalid source") {
		t.Fatalf("source %d %s", badSrc.Code, badSrc.Body.String())
	}
	postSrc := coverageStatus(t, handler, http.MethodPost, "/api/coverage?source=127.0.0.1:3306&at=2024-01-01%2000:00:00", nil)
	if postSrc.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post source %d", postSrc.Code)
	}

	cluster := NewServer(tasks.NewScheduler(tasks.WithDataDir(dir), tasks.WithClusterMode()))
	refused := coverageStatus(t, cluster, http.MethodGet, "/api/tasks/1/coverage?at=2024-01-01%2000:00:00", nil)
	if refused.Code != http.StatusNotImplemented || strings.TrimSpace(refused.Body.String()) != "coverage is not supported in cluster mode yet" {
		t.Fatalf("cluster task %d %s", refused.Code, refused.Body.String())
	}
	refusedSrc := coverageStatus(t, cluster, http.MethodGet, "/api/coverage?source=127.0.0.1:3306&at=2024-01-01%2000:00:00", nil)
	if refusedSrc.Code != http.StatusNotImplemented {
		t.Fatalf("cluster source %d %s", refusedSrc.Code, refusedSrc.Body.String())
	}
	refusedPlan := coverageStatus(t, cluster, http.MethodPost, "/api/tasks/1/restore-plan", strings.NewReader(`{"latest":true}`))
	if refusedPlan.Code != http.StatusNotImplemented || strings.TrimSpace(refusedPlan.Body.String()) != "coverage is not supported in cluster mode yet" {
		t.Fatalf("cluster plan %d %s", refusedPlan.Code, refusedPlan.Body.String())
	}
}

func resultOf(t *testing.T, plan map[string]any, id string) string {
	t.Helper()
	for _, raw := range plan["prechecks"].([]any) {
		item := raw.(map[string]any)
		if item["id"] == id {
			return item["result"].(string)
		}
	}
	t.Fatalf("no precheck %s in %#v", id, plan["prechecks"])
	return ""
}

func describe(t *testing.T, plan map[string]any, id string) string {
	t.Helper()
	for _, raw := range plan["prechecks"].([]any) {
		item := raw.(map[string]any)
		if item["id"] == id {
			return item["description"].(string)
		}
	}
	t.Fatalf("no precheck %s", id)
	return ""
}

func coverageJSON(t *testing.T, handler http.Handler, method, path string, body *strings.Reader) map[string]any {
	t.Helper()
	resp := coverageStatus(t, handler, method, path, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("%s %s -> %d %s", method, path, resp.Code, resp.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func coverageStatus(t *testing.T, handler http.Handler, method, path string, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		rdr = body
	} else {
		rdr = strings.NewReader("")
	}
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, rdr)
	handler.ServeHTTP(resp, req)
	return resp
}

func coverageGTIDSegment(t *testing.T, stamps ...string) []byte {
	t.Helper()
	sid, err := hex.DecodeString("3e11fa4771ca11e19e33c80aa9429562")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	for i, stamp := range stamps {
		when, err := tasks.ParsePITRDatetime(stamp)
		if err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(i+1))
		size := uint32(goreplication.EventHeaderSize + len(payload))
		var hdr [19]byte
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(when.Unix()))
		hdr[4] = byte(goreplication.GTID_EVENT)
		binary.LittleEndian.PutUint32(hdr[5:9], 1)
		binary.LittleEndian.PutUint32(hdr[9:13], size)
		binary.LittleEndian.PutUint32(hdr[13:17], 4)
		buf.Write(hdr[:])
		buf.Write(payload)
	}
	return buf.Bytes()
}
