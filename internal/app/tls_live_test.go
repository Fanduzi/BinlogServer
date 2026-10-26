// Package app proves source and meta TLS through the process that ships.
// input: BINLOG_TEST_TLS=1, a TLS MySQL, and the real App.Run HTTP API
// output: a verify-ca task's binlog dump is SSL/TLS on the server; verify-identity with server_name mysql.tls.test is SSL/TLS too; a wrong CA and a wrong hostname fail; meta.tls on App.Run is SSL/TLS for that account; migrate --config applies the same block and a wrong CA or hostname prints meta.tls
// pos: the CI e2e-tls proof that deleting the runner TLS assignment or skipping ApplyDSN goes red
// note: if this file changes, update this header and module README.md.
package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/config"
	"binlog_server/internal/livemysql"
	"binlog_server/internal/mysqltls"
	"binlog_server/internal/tasks"

	"github.com/go-sql-driver/mysql"
)

func TestLiveAppTLS(t *testing.T) {
	livemysql.Require(t)
	t.Setenv("PRODUCTION", "")
	dir := t.TempDir()
	livemysql.WriteCerts(t, dir, "subjectAltName=DNS:mysql.tls.test\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n")
	port := livemysql.FreePort(t)
	livemysql.StartMySQL(t, "binlog-g2-app", port, dir, true)
	db := adminDB(t, port)
	waitAdmin(t, db)
	grantAppUsers(t, db)
	ca := filepath.Join(dir, "ca.pem")
	wrong := filepath.Join(dir, "wrong-ca.pem")

	t.Run("dump", func(t *testing.T) {
		app, stop := startLiveApp(t, memoryAppConfig(t))
		defer stop()
		base := "http://" + app.Addr()
		good := postTask(t, base, taskBody("g2-ok", port, 420001, "verify-ca", ca, ""))
		postAction(t, base, good.ID, "start")
		got := waitState(t, base, good.ID, tasks.StateRunning)
		if got.State != tasks.StateRunning {
			t.Fatalf("state %s last_error %s", got.State, got.LastError)
		}
		waitDumpType(t, db, "SSL/TLS")
		postAction(t, base, good.ID, "stop")
		waitState(t, base, good.ID, tasks.StateStopped)
		waitNoDump(t, db)

		goodName := postTask(t, base, taskBody("g2-id", port, 420004, "verify-identity", ca, "mysql.tls.test"))
		postAction(t, base, goodName.ID, "start")
		got = waitState(t, base, goodName.ID, tasks.StateRunning)
		if got.State != tasks.StateRunning {
			t.Fatalf("verify-identity state %s last_error %s", got.State, got.LastError)
		}
		waitDumpType(t, db, "SSL/TLS")
		postAction(t, base, goodName.ID, "stop")
		waitState(t, base, goodName.ID, tasks.StateStopped)
		waitNoDump(t, db)

		badCA := postTask(t, base, taskBody("g2-ca", port, 420002, "verify-ca", wrong, ""))
		postAction(t, base, badCA.ID, "start")
		failed := waitState(t, base, badCA.ID, tasks.StateFailed)
		if !strings.Contains(failed.LastError, "x509") {
			t.Fatalf("wrong CA last_error %q", failed.LastError)
		}
		if types := dumpTypes(t, db); len(types) != 0 {
			t.Fatalf("wrong CA left a dump: %v", types)
		}

		badName := postTask(t, base, taskBody("g2-name", port, 420003, "verify-identity", ca, "wrong.example"))
		postAction(t, base, badName.ID, "start")
		failed = waitState(t, base, badName.ID, tasks.StateFailed)
		if !strings.Contains(failed.LastError, "x509") {
			t.Fatalf("wrong hostname last_error %q", failed.LastError)
		}
		if types := dumpTypes(t, db); len(types) != 0 {
			t.Fatalf("wrong hostname left a dump: %v", types)
		}
	})

	t.Run("meta", func(t *testing.T) {
		root := moduleRoot(t)
		bin := filepath.Join(t.TempDir(), "migrate")
		build := exec.Command("go", "build", "-o", bin, "./cmd/migrate")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build migrate: %v\n%s", err, out)
		}
		cfgPath := filepath.Join(t.TempDir(), "meta.yaml")
		body := fmt.Sprintf("meta_dsn: \"bmeta:bmetapw@tcp(127.0.0.1:%d)/metatls?parseTime=true\"\nmeta:\n  tls:\n    mode: verify-ca\n    ca: %q\n", port, ca)
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		up := exec.Command(bin, "up", "--config", cfgPath, "--path", filepath.Join(root, "migrations"))
		if out, err := up.CombinedOutput(); err != nil {
			t.Fatalf("migrate: %v\n%s", err, out)
		}
		badCAPath := filepath.Join(t.TempDir(), "bad-ca.yaml")
		badCABody := fmt.Sprintf("meta_dsn: \"bmeta:bmetapw@tcp(127.0.0.1:%d)/metatls?parseTime=true\"\nmeta:\n  tls:\n    mode: verify-ca\n    ca: %q\n", port, wrong)
		if err := os.WriteFile(badCAPath, []byte(badCABody), 0o644); err != nil {
			t.Fatal(err)
		}
		badUp := exec.Command(bin, "up", "--config", badCAPath, "--path", filepath.Join(root, "migrations"))
		out, err := badUp.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "meta.tls") || !strings.Contains(string(out), "x509") {
			t.Fatalf("migrate wrong CA: %v\n%s", err, out)
		}
		badNamePath := filepath.Join(t.TempDir(), "bad-name.yaml")
		badNameBody := fmt.Sprintf("meta_dsn: \"bmeta:bmetapw@tcp(127.0.0.1:%d)/metatls?parseTime=true\"\nmeta:\n  tls:\n    mode: verify-identity\n    ca: %q\n    server_name: wrong.example\n", port, ca)
		if err := os.WriteFile(badNamePath, []byte(badNameBody), 0o644); err != nil {
			t.Fatal(err)
		}
		badNameUp := exec.Command(bin, "up", "--config", badNamePath, "--path", filepath.Join(root, "migrations"))
		out, err = badNameUp.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "meta.tls") || !strings.Contains(string(out), "x509") {
			t.Fatalf("migrate wrong hostname: %v\n%s", err, out)
		}

		metaCfg := memoryAppConfig(t)
		metaCfg.MetaDSN = fmt.Sprintf("bmeta:bmetapw@tcp(127.0.0.1:%d)/metatls?parseTime=true", port)
		metaCfg.Meta.Timeout = config.MetaTimeoutConfig{ReadSec: 3, WriteSec: 5, LeaseSec: 2, UploadSec: 30}
		metaCfg.Meta.TLS = mysqltls.Config{Mode: "verify-ca", CA: ca}
		_, stop := startLiveApp(t, metaCfg)
		waitUserTLS(t, db, "bmeta")
		stop()

		bad := metaCfg
		bad.Meta.TLS.CA = wrong
		bad.DataDir = t.TempDir()
		err = New(bad).Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "meta.tls") || !strings.Contains(err.Error(), "x509") {
			t.Fatalf("wrong meta CA error = %v", err)
		}
	})
}

func memoryAppConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		ListenAddr: "127.0.0.1:0",
		DataDir:    t.TempDir(),
		Mode:       "standalone",
		Standalone: config.StandaloneConfig{TaskRecords: "off"},
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func taskBody(cluster string, port int, serverID uint32, mode, ca, serverName string) string {
	tlsBlock := fmt.Sprintf(`{"mode":%q,"ca":%q`, mode, ca)
	if serverName != "" {
		tlsBlock += fmt.Sprintf(`,"server_name":%q`, serverName)
	}
	tlsBlock += "}"
	return fmt.Sprintf(`{"name":%q,"cluster_key":%q,"source":{"host":"127.0.0.1","port":%d,"user":"repl","password":"replpw","server_id":%d,"tls":%s},"start":{"mode":"LATEST"},"storage":{"retention_days":1}}`, cluster, cluster, port, serverID, tlsBlock)
}

func postTask(t *testing.T, base, body string) tasks.Task {
	t.Helper()
	resp, err := http.Post(base+"/api/tasks", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %d: %s", resp.StatusCode, raw)
	}
	var task tasks.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	return task
}

func postAction(t *testing.T, base, id, action string) {
	t.Helper()
	resp, err := http.Post(base+"/api/tasks/"+id+"/"+action, "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%s %d: %s", action, resp.StatusCode, raw)
	}
}

func waitState(t *testing.T, base, id string, want tasks.State) tasks.Task {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last tasks.Task
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/tasks/" + id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err := json.Unmarshal(raw, &last); err != nil {
			t.Fatalf("task json: %v body %s", err, raw)
		}
		if last.State == want {
			return last
		}
		if last.State == tasks.StateFailed && want != tasks.StateFailed {
			t.Fatalf("task %s failed: %s", id, last.LastError)
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("task %s stayed %s last_error %s", id, last.State, last.LastError)
	return last
}

func dumpTypes(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return connTypes(t, db, "SELECT CONNECTION_TYPE FROM performance_schema.threads WHERE PROCESSLIST_USER='repl' AND PROCESSLIST_COMMAND LIKE 'Binlog Dump%'")
}

func waitDumpType(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last []string
	for time.Now().Before(deadline) {
		last = dumpTypes(t, db)
		for _, got := range last {
			if got == want {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("dump CONNECTION_TYPE %v, want %s", last, want)
}

func waitNoDump(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(dumpTypes(t, db)) == 0 {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("dump still open: %v", dumpTypes(t, db))
}

func waitUserTLS(t *testing.T, db *sql.DB, user string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last []string
	q := "SELECT CONNECTION_TYPE FROM performance_schema.threads WHERE PROCESSLIST_USER=?"
	for time.Now().Before(deadline) {
		rows, err := db.Query(q, user)
		if err != nil {
			t.Fatal(err)
		}
		last = scanTypes(t, rows)
		for _, got := range last {
			if got == "SSL/TLS" {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s CONNECTION_TYPE %v, want SSL/TLS", user, last)
}

func connTypes(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	return scanTypes(t, rows)
}

func scanTypes(t *testing.T, rows *sql.Rows) []string {
	t.Helper()
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		out = append(out, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func adminDB(t *testing.T, port int) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("root:rootpw@tcp(127.0.0.1:%d)/mysql?parseTime=true&tls=false", port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func waitAdmin(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		last = db.Ping()
		if last == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("mysql not ready: %v", last)
}

func grantAppUsers(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		"CREATE USER IF NOT EXISTS 'repl'@'%' IDENTIFIED BY 'replpw'",
		"GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl'@'%'",
		"CREATE DATABASE IF NOT EXISTS metatls",
		"CREATE USER IF NOT EXISTS 'bmeta'@'%' IDENTIFIED BY 'bmetapw'",
		"GRANT ALL ON metatls.* TO 'bmeta'@'%'",
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func startLiveApp(t *testing.T, cfg config.Config) (*App, func()) {
	t.Helper()
	a := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	select {
	case <-a.Ready():
	case err := <-errCh:
		t.Fatalf("run: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("app not ready")
	}
	return a, func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Log("app did not exit")
		}
	}
}

var _ = mysql.ParseDSN
