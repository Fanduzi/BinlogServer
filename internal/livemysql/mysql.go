// Package livemysql starts a throwaway MySQL for TLS tests.
// input: BINLOG_TEST_TLS=1, docker, openssl, and a directory for generated PEM files
// output: a MySQL 8 process on 127.0.0.1 with either a test CA or TLS disabled, plus the PEM files that signed it
// pos: shared fixture for the e2e-tls job (replication, app, upload)
// note: if this file changes, update this header and module README.md.
package livemysql

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Require skips when BINLOG_TEST_TLS is unset and fails when docker or openssl is missing.
func Require(t *testing.T) {
	t.Helper()
	if os.Getenv("BINLOG_TEST_TLS") == "" {
		t.Skip("BINLOG_TEST_TLS is not set")
	}
	for _, bin := range []string{"docker", "openssl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is required", bin)
		}
	}
}

// FreePort returns an unused TCP port on 127.0.0.1.
func FreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// WriteCerts writes ca.pem, server.pem, server.key, and wrong-ca.pem.
// sanExt is an OpenSSL extfile body (subjectAltName and key usage).
func WriteCerts(t *testing.T, dir, sanExt string) {
	t.Helper()
	san := filepath.Join(dir, "san.cnf")
	if err := os.WriteFile(san, []byte(sanExt), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", filepath.Join(dir, "ca.key"), "-out", filepath.Join(dir, "ca.pem"), "-days", "1", "-subj", "/CN=Binlog Test CA", "-addext", "basicConstraints=critical,CA:TRUE")
	run(t, "req", "-newkey", "rsa:2048", "-nodes", "-keyout", filepath.Join(dir, "server.key"), "-out", filepath.Join(dir, "server.csr"), "-subj", "/CN=mysql.tls.test")
	run(t, "x509", "-req", "-in", filepath.Join(dir, "server.csr"), "-CA", filepath.Join(dir, "ca.pem"), "-CAkey", filepath.Join(dir, "ca.key"), "-CAcreateserial", "-out", filepath.Join(dir, "server.pem"), "-days", "1", "-extfile", san)
	run(t, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", filepath.Join(dir, "wrong.key"), "-out", filepath.Join(dir, "wrong-ca.pem"), "-days", "1", "-subj", "/CN=Wrong CA")
}

// StartMySQL runs mysql:8.0. tlsOn mounts certDir and enables the test certificate.
// The plaintext server passes an empty tls-version so it offers no TLS.
func StartMySQL(t *testing.T, name string, port int, certDir string, tlsOn bool) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", name).Run()
	args := []string{
		"run", "-d", "--name", name,
		"-e", "MYSQL_ROOT_PASSWORD=rootpw",
		"-e", "MYSQL_ROOT_HOST=%",
		"-p", fmt.Sprintf("127.0.0.1:%d:3306", port),
	}
	serverID := "2"
	if tlsOn {
		serverID = "1"
		args = append(args, "-v", certDir+":/certs:ro", "--entrypoint", "bash")
	}
	args = append(args, "mysql:8.0")
	if tlsOn {
		args = append(args, "-c", `cp /certs/ca.pem /certs/server.pem /tmp/ && cp /certs/server.key /tmp/server.key && chmod 600 /tmp/server.key /tmp/server.pem /tmp/ca.pem && chown mysql:mysql /tmp/server.key /tmp/server.pem /tmp/ca.pem && exec docker-entrypoint.sh mysqld --ssl-ca=/tmp/ca.pem --ssl-cert=/tmp/server.pem --ssl-key=/tmp/server.key --server-id=`+serverID+` --log-bin=mysql-bin --gtid-mode=ON --enforce-gtid-consistency=ON`)
	} else {
		// mysql:8.0 (8.0.34+) removed --ssl. An empty tls-version disables TLS.
		args = append(args, "--tls-version=", "--server-id="+serverID, "--log-bin=mysql-bin", "--gtid-mode=ON", "--enforce-gtid-consistency=ON")
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
}

func run(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("openssl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("openssl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
