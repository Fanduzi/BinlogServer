// Package replication proves source and meta TLS against a real MySQL.
// input: BINLOG_TEST_TLS=1, docker, openssl, and a generated test CA
// output: verify-ca and verify-identity accept the test CA and reject a wrong CA or hostname, on both the replication client and the meta DSN; preferred falls back when the server has no TLS; the binlog syncer starts over TLS
// pos: the CI e2e-tls job for production-readiness G2
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/mysqltls"
	"binlog_server/internal/tasks"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
	"github.com/go-sql-driver/mysql"
)

func TestLiveMySQLTLS(t *testing.T) {
	if os.Getenv("BINLOG_TEST_TLS") == "" {
		t.Skip("BINLOG_TEST_TLS is not set")
	}
	for _, bin := range []string{"docker", "openssl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is required", bin)
		}
	}
	dir := t.TempDir()
	writeCerts(t, dir)
	tlsPort := freePort(t)
	plainPort := freePort(t)
	startMySQL(t, "binlog-g2-tls", tlsPort, dir, true)
	startMySQL(t, "binlog-g2-plain", plainPort, "", false)
	waitMySQL(t, rootSource(tlsPort, "required", "", ""))
	waitMySQL(t, rootSource(plainPort, "disabled", "", ""))
	grantRepl(t, tlsPort, true)
	grantRepl(t, plainPort, false)

	ca := filepath.Join(dir, "ca.pem")
	wrong := filepath.Join(dir, "wrong-ca.pem")
	ctx := context.Background()

	t.Run("replication", func(t *testing.T) {
		assertCipher(t, ctx, replSource(tlsPort, "required", "", ""), true)
		assertCipher(t, ctx, replSource(tlsPort, "disabled", "", ""), false)
		assertCipher(t, ctx, replSource(tlsPort, "preferred", "", ""), true)
		assertCipher(t, ctx, replSource(plainPort, "preferred", "", ""), false)
		assertCipher(t, ctx, replSource(tlsPort, "verify-ca", ca, ""), true)
		assertDialFails(t, ctx, replSource(tlsPort, "verify-ca", wrong, ""))
		assertCipher(t, ctx, replSource(tlsPort, "verify-identity", ca, "mysql.tls.test"), true)
		assertDialFails(t, ctx, replSource(tlsPort, "verify-identity", ca, "wrong.example"))
		assertDialFails(t, ctx, replSource(tlsPort, "verify-identity", wrong, "mysql.tls.test"))

		src := replSource(tlsPort, "verify-identity", ca, "mysql.tls.test")
		tlsCfg, err := sourceSyncerTLS(ctx, src)
		if err != nil || tlsCfg == nil {
			t.Fatalf("syncer tls = %v, %v", tlsCfg, err)
		}
		cfg := buildSyncerConfig(tasks.Task{ID: "g2", Source: src})
		cfg.TLSConfig = tlsCfg
		cfg.ServerID = 1001
		syncer := goreplication.NewBinlogSyncer(cfg)
		t.Cleanup(func() { syncer.Close() })
		if _, err := syncer.StartSync(masterPos(t, ctx, src)); err != nil {
			t.Fatalf("binlog dump over verify-identity: %v", err)
		}
	})

	t.Run("meta", func(t *testing.T) {
		assertMeta(t, tlsPort, mysqltls.Config{Mode: "required"}, "meta-required", true)
		assertMeta(t, tlsPort, mysqltls.Config{Mode: "disabled"}, "meta-disabled", false)
		assertMeta(t, tlsPort, mysqltls.Config{Mode: "preferred"}, "meta-preferred", true)
		assertMeta(t, plainPort, mysqltls.Config{Mode: "preferred"}, "meta-preferred-plain", false)
		assertMeta(t, tlsPort, mysqltls.Config{Mode: "verify-ca", CA: ca}, "meta-verify-ca", true)
		assertMetaFails(t, tlsPort, mysqltls.Config{Mode: "verify-ca", CA: wrong}, "meta-verify-ca-wrong")
		assertMeta(t, tlsPort, mysqltls.Config{Mode: "verify-identity", CA: ca, ServerName: "mysql.tls.test"}, "meta-verify-id", true)
		assertMetaFails(t, tlsPort, mysqltls.Config{Mode: "verify-identity", CA: ca, ServerName: "wrong.example"}, "meta-verify-id-name")
		assertMetaFails(t, tlsPort, mysqltls.Config{Mode: "verify-identity", CA: wrong, ServerName: "mysql.tls.test"}, "meta-verify-id-ca")
	})
}

func rootSource(port int, mode, ca, name string) tasks.SourceConfig {
	return tasks.SourceConfig{
		Host: "127.0.0.1", Port: uint16(port), User: "root", Password: "rootpw",
		TLS: mysqltls.Config{Mode: mode, CA: ca, ServerName: name},
	}
}

func replSource(port int, mode, ca, name string) tasks.SourceConfig {
	src := rootSource(port, mode, ca, name)
	src.User = "repl"
	src.Password = "replpw"
	return src
}

func assertCipher(t *testing.T, ctx context.Context, src tasks.SourceConfig, wantTLS bool) {
	t.Helper()
	conn, err := connectSource(ctx, src)
	if err != nil {
		t.Fatalf("%s dial: %v", src.TLS.Mode, err)
	}
	defer conn.Close()
	res, err := conn.Execute("SHOW STATUS LIKE 'Ssl_cipher'")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := res.GetString(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if wantTLS && cipher == "" {
		t.Fatalf("mode %s connected without TLS", src.TLS.Mode)
	}
	if !wantTLS && cipher != "" {
		t.Fatalf("mode %s used TLS cipher %q", src.TLS.Mode, cipher)
	}
}

func assertDialFails(t *testing.T, ctx context.Context, src tasks.SourceConfig) {
	t.Helper()
	conn, err := connectSource(ctx, src)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatalf("mode %s ca %q name %q connected", src.TLS.Mode, src.TLS.CA, src.TLS.ServerName)
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("dial error contains key material: %s", err.Error())
	}
}

func masterPos(t *testing.T, ctx context.Context, src tasks.SourceConfig) gomysql.Position {
	t.Helper()
	conn, err := connectSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	res, err := conn.Execute("SHOW MASTER STATUS")
	if err != nil {
		t.Fatal(err)
	}
	file, err := res.GetString(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	posText, err := res.GetString(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	pos, err := strconv.ParseUint(posText, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return gomysql.Position{Name: file, Pos: uint32(pos)}
}

func assertMeta(t *testing.T, port int, tlsCfg mysqltls.Config, name string, wantTLS bool) {
	t.Helper()
	db := openMeta(t, port, tlsCfg, name)
	defer db.Close()
	var variable, cipher string
	if err := db.QueryRow("SHOW STATUS LIKE 'Ssl_cipher'").Scan(&variable, &cipher); err != nil {
		t.Fatal(err)
	}
	if wantTLS && cipher == "" {
		t.Fatalf("meta %s connected without TLS", tlsCfg.Mode)
	}
	if !wantTLS && cipher != "" {
		t.Fatalf("meta %s used TLS cipher %q", tlsCfg.Mode, cipher)
	}
}

func assertMetaFails(t *testing.T, port int, tlsCfg mysqltls.Config, name string) {
	t.Helper()
	db := openMeta(t, port, tlsCfg, name)
	defer db.Close()
	err := db.Ping()
	if err == nil {
		t.Fatalf("meta %s ca %q name %q connected", tlsCfg.Mode, tlsCfg.CA, tlsCfg.ServerName)
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("meta error contains key material: %s", err.Error())
	}
}

func openMeta(t *testing.T, port int, tlsCfg mysqltls.Config, name string) *sql.DB {
	t.Helper()
	raw := fmt.Sprintf("root:rootpw@tcp(127.0.0.1:%d)/mysql?parseTime=true", port)
	dsn, err := mysqltls.ApplyDSN(raw, tlsCfg, name)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func grantRepl(t *testing.T, port int, tlsOn bool) {
	t.Helper()
	mode := "disabled"
	if tlsOn {
		mode = "required"
	}
	ctx := context.Background()
	conn, err := connectSource(ctx, rootSource(port, mode, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Execute("CREATE USER IF NOT EXISTS 'repl'@'%' IDENTIFIED BY 'replpw'"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Execute("GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl'@'%'"); err != nil {
		t.Fatal(err)
	}
}

func waitMySQL(t *testing.T, src tasks.SourceConfig) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		conn, err := connectSource(ctx, src)
		cancel()
		if err == nil {
			conn.Close()
			return
		}
		last = err
		time.Sleep(time.Second)
	}
	t.Fatalf("mysql on %d not ready: %v", src.Port, last)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func startMySQL(t *testing.T, name string, port int, certDir string, tlsOn bool) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", name).Run()
	args := []string{
		"run", "-d", "--name", name,
		"-e", "MYSQL_ROOT_PASSWORD=rootpw",
		"-e", "MYSQL_ROOT_HOST=%",
		"-p", fmt.Sprintf("127.0.0.1:%d:3306", port),
	}
	if tlsOn {
		args = append(args, "-v", certDir+":/certs:ro", "--entrypoint", "bash")
	}
	args = append(args, "mysql:8.0")
	if tlsOn {
		args = append(args, "-c", `cp /certs/ca.pem /certs/server.pem /tmp/ && cp /certs/server.key /tmp/server.key && chmod 600 /tmp/server.key /tmp/server.pem /tmp/ca.pem && chown mysql:mysql /tmp/server.key /tmp/server.pem /tmp/ca.pem && exec docker-entrypoint.sh mysqld --ssl-ca=/tmp/ca.pem --ssl-cert=/tmp/server.pem --ssl-key=/tmp/server.key --server-id=1 --log-bin=mysql-bin --gtid-mode=ON --enforce-gtid-consistency=ON`)
	} else {
		// mysql:8.0 (8.0.34+) removed --ssl. An empty tls-version disables TLS.
		args = append(args, "--tls-version=", "--server-id=2", "--log-bin=mysql-bin", "--gtid-mode=ON", "--enforce-gtid-consistency=ON")
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
}

func writeCerts(t *testing.T, dir string) {
	t.Helper()
	san := filepath.Join(dir, "san.cnf")
	if err := os.WriteFile(san, []byte("subjectAltName=DNS:mysql.tls.test\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOpenssl(t, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", filepath.Join(dir, "ca.key"), "-out", filepath.Join(dir, "ca.pem"), "-days", "1", "-subj", "/CN=Binlog Test CA", "-addext", "basicConstraints=critical,CA:TRUE")
	runOpenssl(t, "req", "-newkey", "rsa:2048", "-nodes", "-keyout", filepath.Join(dir, "server.key"), "-out", filepath.Join(dir, "server.csr"), "-subj", "/CN=mysql.tls.test")
	runOpenssl(t, "x509", "-req", "-in", filepath.Join(dir, "server.csr"), "-CA", filepath.Join(dir, "ca.pem"), "-CAkey", filepath.Join(dir, "ca.key"), "-CAcreateserial", "-out", filepath.Join(dir, "server.pem"), "-days", "1", "-extfile", san)
	runOpenssl(t, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", filepath.Join(dir, "wrong.key"), "-out", filepath.Join(dir, "wrong-ca.pem"), "-days", "1", "-subj", "/CN=Wrong CA")
}

func runOpenssl(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("openssl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("openssl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// The driver import registers the mysql dialect used by openMeta.
var _ = mysql.ParseDSN
