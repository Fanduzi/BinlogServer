// Package mysqltls tests the TLS block without a server.
// input: PEM files generated in the test, including a key whose text must not appear in errors
// output: shape errors, DSN rewrite, verify-ca versus verify-identity, key material absent from errors, and PrefixMeta still prefixes a metadb.example hostname
// pos: unit coverage for ADR 0012 config parsing and validation
// note: if this file changes, update this header and module README.md.
package mysqltls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestNormalize_DisabledIsDefault(t *testing.T) {
	got, err := Normalize("meta", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "" || !Disabled(got) {
		t.Fatalf("empty block = %#v", got)
	}
	cfg, err := ClientConfig("meta", Config{}, "db.example.com")
	if err != nil || cfg != nil {
		t.Fatalf("plaintext config = %v, %v", cfg, err)
	}
}

func TestNormalize_RejectsShape(t *testing.T) {
	cases := []struct {
		name string
		in   Config
		want string
	}{
		{"bad mode", Config{Mode: "require"}, "meta.tls.mode must be"},
		{"ca on required", Config{Mode: "required", CA: "/ca.pem"}, "meta.tls.ca is only used"},
		{"server name on verify-ca", Config{Mode: "verify-ca", CA: "/ca.pem", ServerName: "db"}, "server_name is only used"},
		{"ca required", Config{Mode: "verify-identity"}, "meta.tls.ca is required"},
		{"half key", Config{Mode: "required", Cert: "/c.pem"}, "cert and key must both"},
		{"preferred extras", Config{Mode: "PREFERRED", CA: "/ca.pem"}, "preferred does not take"},
		{"fields on disabled", Config{Mode: "disabled", CA: "/ca.pem"}, "require mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Normalize("meta", tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestApplyDSN_DisabledLeavesDSN(t *testing.T) {
	const dsn = "user:p@ss@tcp(127.0.0.1:3306)/meta?parseTime=true&tls=skip-verify"
	got, err := ApplyDSN(dsn, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != dsn {
		t.Fatalf("disabled rewrote the DSN:\n got %s\nwant %s", got, dsn)
	}
}

func TestApplyDSN_PreferredAndRejectsExistingTLS(t *testing.T) {
	got, err := ApplyDSN("user:pass@tcp(db.example:3306)/meta?parseTime=true", Config{Mode: "preferred"}, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mysql.ParseDSN(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TLSConfig != "preferred" || !parsed.ParseTime {
		t.Fatalf("dsn = %#v", parsed)
	}
	_, err = ApplyDSN("user:pass@tcp(db.example:3306)/meta?tls=skip-verify", Config{Mode: "required"}, "")
	if err == nil || !strings.Contains(err.Error(), "already sets tls=skip-verify") {
		t.Fatalf("error = %v", err)
	}
}

func TestClientConfig_VerifyCASkipsHostname(t *testing.T) {
	ca, caKey, caPEM := newCA(t)
	leaf := signLeaf(t, ca, caKey, "mysql.tls.test")
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ClientConfig("source", Config{Mode: "verify-ca", CA: caPath}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "" || !cfg.InsecureSkipVerify || cfg.VerifyPeerCertificate == nil {
		t.Fatalf("verify-ca config = ServerName %q skip %v", cfg.ServerName, cfg.InsecureSkipVerify)
	}
	if err := cfg.VerifyPeerCertificate([][]byte{leaf.Raw}, nil); err != nil {
		t.Fatalf("verify-ca rejected a CA-signed cert for a different name: %v", err)
	}
	wrong, wrongKey, _ := newCA(t)
	wrongLeaf := signLeaf(t, wrong, wrongKey, "mysql.tls.test")
	if err := cfg.VerifyPeerCertificate([][]byte{wrongLeaf.Raw}, nil); err == nil {
		t.Fatal("verify-ca accepted a cert from a different CA")
	}

	id, err := ClientConfig("source", Config{Mode: "verify-identity", CA: caPath, ServerName: "mysql.tls.test"}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if id.ServerName != "mysql.tls.test" || id.InsecureSkipVerify || id.RootCAs == nil {
		t.Fatalf("verify-identity config = %+v", id)
	}
}

func TestClientConfig_KeyMaterialStaysOutOfErrors(t *testing.T) {
	const marker = "SUPERSECRETPRIVATEKEY"
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.pem")
	keyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, []byte("not a cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("-----BEGIN PRIVATE KEY-----\n"+marker+"\n-----END PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ClientConfig("source", Config{Mode: "required", Cert: certPath, Key: keyPath}, "db")
	if err == nil {
		t.Fatal("expected a key error")
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("error contains key material: %s", err.Error())
	}
	missing := filepath.Join(dir, "missing.key")
	_, err = ClientConfig("meta", Config{Mode: "required", Cert: certPath, Key: missing}, "db")
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("missing key error = %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing key error should name the path: %v", err)
	}
}

func TestClientConfig_LoadsClientCert(t *testing.T) {
	ca, caKey, caPEM := newCA(t)
	leafPEM, keyPEM := signClient(t, ca, caKey)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.pem")
	keyPath := filepath.Join(dir, "client.key")
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(certPath, leafPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ClientConfig("source", Config{Mode: "verify-identity", CA: caPath, Cert: certPath, Key: keyPath}, "db.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates) != 1 || cfg.ServerName != "db.example.com" {
		t.Fatalf("client cert not loaded: certs %d name %q", len(cfg.Certificates), cfg.ServerName)
	}
}

func newCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func signLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{name},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestPrefixMeta(t *testing.T) {
	bare := PrefixMeta(errors.New("x509: certificate signed by unknown authority"))
	if bare == nil || bare.Error() != "meta.tls: x509: certificate signed by unknown authority" {
		t.Fatal(bare)
	}
	if again := PrefixMeta(bare); again.Error() != bare.Error() {
		t.Fatalf("stacked prefix: %v", again)
	}
	if PrefixMeta(nil) != nil {
		t.Fatal("nil")
	}
	name := PrefixMeta(errors.New("tls: failed to verify certificate: x509: certificate is valid for localhost, not metadb.example"))
	if name == nil || name.Error() != "meta.tls: tls: failed to verify certificate: x509: certificate is valid for localhost, not metadb.example" {
		t.Fatalf("server_name metadb.example was not prefixed: %v", name)
	}
}

func signClient(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
