// Package replication proves source and meta TLS against a real MySQL.
// input: BINLOG_TEST_TLS=1, docker, openssl, and a generated test CA
// output: verify-ca and verify-identity accept the test CA and reject a wrong CA or hostname on connectSource and on the meta DSN; preferred uses TLS when the server offers it and plaintext when the greeting has no TLS; a preferred auth failure does not open a second plaintext connection
// pos: the CI e2e-tls job for connectSource modes (dump and meta pool TLS are TestLiveAppTLS)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/livemysql"
	"binlog_server/internal/mysqltls"
	"binlog_server/internal/tasks"

	sqlclient "github.com/go-mysql-org/go-mysql/client"
	"github.com/go-sql-driver/mysql"
)

func TestLiveMySQLTLS(t *testing.T) {
	livemysql.Require(t)
	dir := t.TempDir()
	livemysql.WriteCerts(t, dir, "subjectAltName=DNS:mysql.tls.test\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n")
	tlsPort := livemysql.FreePort(t)
	plainPort := livemysql.FreePort(t)
	livemysql.StartMySQL(t, "binlog-g2-tls", tlsPort, dir, true)
	livemysql.StartMySQL(t, "binlog-g2-plain", plainPort, "", false)
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

		root, err := connectSource(ctx, rootSource(tlsPort, "required", "", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		beforeConn := globalStatus(t, root, "Connections")
		beforeSSL := globalStatus(t, root, "Ssl_accepts")
		bad := replSource(tlsPort, "preferred", "", "")
		bad.Password = "not-the-password"
		conn, err := connectSource(ctx, bad)
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "access denied") {
			t.Fatalf("preferred wrong password = %v", err)
		}
		deltaConn := globalStatus(t, root, "Connections") - beforeConn
		deltaSSL := globalStatus(t, root, "Ssl_accepts") - beforeSSL
		if deltaConn < 1 || deltaConn != deltaSSL {
			t.Fatalf("preferred auth failure Connections +%d Ssl_accepts +%d (a second plaintext dial is a fallback on any error)", deltaConn, deltaSSL)
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

func globalStatus(t *testing.T, conn *sqlclient.Conn, name string) int {
	t.Helper()
	res, err := conn.Execute("SHOW GLOBAL STATUS LIKE '" + name + "'")
	if err != nil {
		t.Fatal(err)
	}
	text, err := res.GetString(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func assertCipher(t *testing.T, ctx context.Context, src tasks.SourceConfig, wantTLS bool) {
	t.Helper()
	conn, err := connectSource(ctx, src)
	if err != nil {
		t.Fatalf("%s dial: %v", src.TLS.Mode, err)
	}
	defer func() { _ = conn.Close() }()
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
		_ = conn.Close()
	}
	if err == nil {
		t.Fatalf("mode %s ca %q name %q connected", src.TLS.Mode, src.TLS.CA, src.TLS.ServerName)
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("dial error contains key material: %s", err.Error())
	}
}

func assertMeta(t *testing.T, port int, tlsCfg mysqltls.Config, name string, wantTLS bool) {
	t.Helper()
	db := openMeta(t, port, tlsCfg, name)
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = conn.Close() }()
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
			_ = conn.Close()
			return
		}
		last = err
		time.Sleep(time.Second)
	}
	t.Fatalf("mysql on %d not ready: %v", src.Port, last)
}

// The driver import registers the mysql dialect used by openMeta.
var _ = mysql.ParseDSN
