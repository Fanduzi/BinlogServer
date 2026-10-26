// Package main proves migrate --config prefixes a private-CA dial error.
// input: a local server that speaks a MySQL greeting and then TLS, and a --config whose CA or server_name does not match
// output: the up command's error contains meta.tls and x509, once
// pos: unit coverage for the same prefix server startup puts on a meta dial
// note: if this file changes, update this header and module README.md.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMigrateConfigPrefixesTLSError(t *testing.T) {
	caPEM, cert := migrateServerCert(t)
	addr := serveMySQLTLS(t, cert)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	wrongPEM, _ := migrateServerCert(t)
	wrongPath := filepath.Join(dir, "wrong.pem")
	if err := os.WriteFile(wrongPath, wrongPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	mig := filepath.Join(dir, "migrations")
	if err := os.Mkdir(mig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mig, "1_init.up.sql"), []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dsn := "u:p@tcp(127.0.0.1:" + port + ")/db?parseTime=true&timeout=2s"
	write := func(name, mode, ca, serverName string) string {
		t.Helper()
		body := "meta_dsn: \"" + dsn + "\"\nmeta:\n  tls:\n    mode: " + mode + "\n    ca: " + quoteYAML(ca) + "\n"
		if serverName != "" {
			body += "    server_name: " + quoteYAML(serverName) + "\n"
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		name, mode, ca, serverName string
	}{
		{name: "wrong ca", mode: "verify-ca", ca: wrongPath},
		{name: "wrong hostname", mode: "verify-identity", ca: caPath, serverName: "wrong.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := execute([]string{"up", "--config", write(tc.name+".yaml", tc.mode, tc.ca, tc.serverName), "--path", mig})
			if err == nil {
				t.Fatal("up succeeded")
			}
			msg := err.Error()
			if !strings.Contains(msg, "meta.tls:") || !strings.Contains(msg, "x509:") || strings.Count(msg, "meta.tls:") != 1 {
				t.Fatalf("error %q", msg)
			}
		})
	}
}

func quoteYAML(s string) string {
	return "\"" + s + "\""
}

func serveMySQLTLS(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				payload := migrateGreeting()
				hdr := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 0}
				if _, err := c.Write(append(hdr, payload...)); err != nil {
					return
				}
				ph := make([]byte, 4)
				if _, err := io.ReadFull(c, ph); err != nil {
					return
				}
				n := int(ph[0]) | int(ph[1])<<8 | int(ph[2])<<16
				if n > 0 && n < 1<<20 {
					if _, err := io.ReadFull(c, make([]byte, n)); err != nil {
						return
					}
				}
				_ = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}).Handshake()
			}(c)
		}
	}()
	return ln.Addr().String()
}

func migrateGreeting() []byte {
	const (
		protocol41       = uint32(1 << 9)
		clientSSL        = uint32(1 << 11)
		secureConnection = uint32(1 << 15)
		pluginAuth       = uint32(1 << 19)
	)
	flags := protocol41 | clientSSL | secureConnection | pluginAuth
	var b []byte
	b = append(b, 10)
	b = append(b, []byte("8.0.36\x00")...)
	b = append(b, 1, 0, 0, 0)
	b = append(b, make([]byte, 8)...)
	b = append(b, 0)
	b = append(b, byte(flags), byte(flags>>8))
	b = append(b, 33, 0, 0)
	b = append(b, byte(flags>>16), byte(flags>>24))
	b = append(b, 21)
	b = append(b, make([]byte, 10)...)
	b = append(b, make([]byte, 12)...)
	b = append(b, 0)
	b = append(b, []byte("mysql_native_password\x00")...)
	return b
}

func migrateServerCert(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "migrate-test-ca"},
		IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}
