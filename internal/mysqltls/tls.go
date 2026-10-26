// Package mysqltls builds one TLS mode for the meta DSN and the source.
// input: a tls block (mode, ca, cert, key, server_name) and the host being dialed
// output: a tls.Config for go-mysql, or a meta DSN whose tls= parameter selects the same mode; errors that name the path and never the PEM; GreetingOffersTLS reads CLIENT_SSL from a server greeting; IsVerifyFailure marks an x509 check failure; PrefixMeta names a bare driver error as the meta pool when the text does not already contain meta.tls:
// pos: the only place source and meta TLS modes are interpreted (ADR 0012)
// note: if this file changes, update this header and module README.md.
package mysqltls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/go-sql-driver/mysql"
)

const (
	// ModeDisabled dials plaintext.
	ModeDisabled = "disabled"
	// ModePreferred uses TLS when the server offers it, otherwise plaintext.
	// The certificate is not checked.
	ModePreferred = "preferred"
	// ModeRequired requires TLS and does not check the certificate.
	ModeRequired = "required"
	// ModeVerifyCA requires TLS and a chain to ca. The hostname is not checked.
	ModeVerifyCA = "verify-ca"
	// ModeVerifyIdentity requires TLS, a chain to ca, and a matching hostname.
	ModeVerifyIdentity = "verify-identity"

	// MetaTLSName is the go-sql-driver tls= name registered for the meta pool.
	MetaTLSName = "binlog-meta"
)

// Config is the per-connection tls block (ADR 0012).
type Config struct {
	Mode       string `json:"mode,omitempty"`
	CA         string `json:"ca,omitempty"`
	Cert       string `json:"cert,omitempty"`
	Key        string `json:"key,omitempty"`
	ServerName string `json:"server_name,omitempty"`
}

// Disabled reports whether the block dials plaintext.
func Disabled(c Config) bool {
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	return mode == "" || mode == ModeDisabled
}

// Preferred reports whether the block may fall back to plaintext.
func Preferred(c Config) bool {
	return strings.ToLower(strings.TrimSpace(c.Mode)) == ModePreferred
}

// clientSSL is the MySQL capability bit CLIENT_SSL (protocol handshake).
const clientSSL = uint32(1 << 11)

// GreetingOffersTLS reports whether a server greeting payload (no packet
// header) has CLIENT_SSL set. preferred continues plaintext on that same
// socket only when this is false. An auth error or a certificate error is
// not this case.
func GreetingOffersTLS(payload []byte) bool {
	if len(payload) < 1 || payload[0] != 10 {
		return false
	}
	i := 1
	for i < len(payload) && payload[i] != 0 {
		i++
	}
	i++            // NUL after the server version
	i += 4 + 8 + 1 // connection id, scramble, filler
	if i+2 > len(payload) {
		return false
	}
	flags := uint32(payload[i]) | uint32(payload[i+1])<<8
	return flags&clientSSL != 0
}

// PrefixMeta names a bare driver error as the meta pool. An error that
// already contains the prefix meta.tls: is unchanged, so startup and migrate
// do not stack prefixes. A hostname such as metadb.example is not that prefix.
func PrefixMeta(err error) error {
	if err == nil || strings.Contains(err.Error(), "meta.tls:") {
		return err
	}
	return fmt.Errorf("meta.tls: %w", err)
}

// IsVerifyFailure reports a certificate check failure. It is not a reason to
// retry the dial or to describe the source as unreachable.
func IsVerifyFailure(err error) bool {
	if err == nil {
		return false
	}
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &unknown) || errors.As(err, &host) || errors.As(err, &invalid) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "x509:") ||
		strings.Contains(msg, "certificate is valid for") ||
		strings.Contains(msg, "certificate signed by unknown authority") ||
		strings.Contains(msg, "failed to verify certificate")
}

// Normalize trims the block and checks its shape. It does not read files.
func Normalize(prefix string, c Config) (Config, error) {
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	c.CA = strings.TrimSpace(c.CA)
	c.Cert = strings.TrimSpace(c.Cert)
	c.Key = strings.TrimSpace(c.Key)
	c.ServerName = strings.TrimSpace(c.ServerName)
	if err := validateShape(prefix, c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func validateShape(prefix string, c Config) error {
	switch c.Mode {
	case "", ModeDisabled:
		if c.CA != "" || c.Cert != "" || c.Key != "" || c.ServerName != "" {
			return fmt.Errorf("%s.tls: ca, cert, key, and server_name require mode preferred, required, verify-ca, or verify-identity", prefix)
		}
		return nil
	case ModePreferred:
		if c.CA != "" || c.Cert != "" || c.Key != "" || c.ServerName != "" {
			return fmt.Errorf("%s.tls: preferred does not take ca, cert, key, or server_name", prefix)
		}
		return nil
	case ModeRequired:
		if c.CA != "" {
			return fmt.Errorf("%s.tls.ca is only used with verify-ca and verify-identity", prefix)
		}
		if c.ServerName != "" {
			return fmt.Errorf("%s.tls.server_name is only used with verify-identity", prefix)
		}
		return certPair(prefix, c)
	case ModeVerifyCA:
		if c.ServerName != "" {
			return fmt.Errorf("%s.tls.server_name is only used with verify-identity", prefix)
		}
		if c.CA == "" {
			return fmt.Errorf("%s.tls.ca is required for verify-ca", prefix)
		}
		return certPair(prefix, c)
	case ModeVerifyIdentity:
		if c.CA == "" {
			return fmt.Errorf("%s.tls.ca is required for verify-identity", prefix)
		}
		return certPair(prefix, c)
	default:
		return fmt.Errorf("%s.tls.mode must be disabled, preferred, required, verify-ca, or verify-identity, got %q", prefix, c.Mode)
	}
}

func certPair(prefix string, c Config) error {
	if (c.Cert == "") != (c.Key == "") {
		return fmt.Errorf("%s.tls: cert and key must both be set or both be empty", prefix)
	}
	return nil
}

// ClientConfig reads the PEM files and returns the dial config.
// A nil config means plaintext. The error names paths and never PEM bytes.
func ClientConfig(prefix string, c Config, host string) (*tls.Config, error) {
	c, err := Normalize(prefix, c)
	if err != nil {
		return nil, err
	}
	if c.Mode == "" || c.Mode == ModeDisabled {
		return nil, nil
	}
	out := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.Cert != "" {
		pair, err := loadKeyPair(prefix, c.Cert, c.Key)
		if err != nil {
			return nil, err
		}
		out.Certificates = []tls.Certificate{pair}
	}
	switch c.Mode {
	case ModePreferred, ModeRequired:
		out.InsecureSkipVerify = true
		return out, nil
	case ModeVerifyCA:
		pool, err := loadCA(prefix, c.CA)
		if err != nil {
			return nil, err
		}
		out.InsecureSkipVerify = true
		out.VerifyPeerCertificate = verifyCAOnly(pool)
		return out, nil
	case ModeVerifyIdentity:
		pool, err := loadCA(prefix, c.CA)
		if err != nil {
			return nil, err
		}
		name := c.ServerName
		if name == "" {
			name = hostName(host)
		}
		if name == "" {
			return nil, fmt.Errorf("%s.tls.server_name is required when the dial host is empty", prefix)
		}
		out.RootCAs = pool
		out.ServerName = name
		return out, nil
	default:
		return nil, fmt.Errorf("%s.tls.mode must be disabled, preferred, required, verify-ca, or verify-identity, got %q", prefix, c.Mode)
	}
}

func hostName(host string) string {
	host = strings.TrimSpace(host)
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

func loadCA(prefix, path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s.tls.ca: read %s: %v", prefix, path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("%s.tls.ca: no certificates in %s", prefix, path)
	}
	return pool, nil
}

func loadKeyPair(prefix, certPath, keyPath string) (tls.Certificate, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s.tls.cert: read %s: %v", prefix, certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s.tls.key: read %s: %v", prefix, keyPath, err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// Do not wrap err: a parse error can quote the file.
		return tls.Certificate{}, fmt.Errorf("%s.tls: invalid client certificate or key", prefix)
	}
	return pair, nil
}

func verifyCAOnly(pool *x509.CertPool) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("tls: server presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("tls: server certificate is not parseable")
		}
		inter := x509.NewCertPool()
		for _, raw := range rawCerts[1:] {
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("tls: server certificate chain is not parseable")
			}
			inter.AddCert(cert)
		}
		_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter})
		return err
	}
}

// ApplyDSN rewrites a meta DSN for c. A disabled block returns dsn unchanged.
// name is the registered tls= value for modes that need a custom tls.Config;
// an empty name uses MetaTLSName.
func ApplyDSN(dsn string, c Config, name string) (string, error) {
	c, err := Normalize("meta", c)
	if err != nil {
		return "", err
	}
	if c.Mode == "" || c.Mode == ModeDisabled {
		return dsn, nil
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("meta_dsn: %w", err)
	}
	if parsed.TLSConfig != "" {
		return "", fmt.Errorf("meta_dsn already sets tls=%s; remove it and use meta.tls", parsed.TLSConfig)
	}
	if c.Mode == ModePreferred {
		parsed.TLSConfig = ModePreferred
		return parsed.FormatDSN(), nil
	}
	host := parsed.Addr
	if h, _, err := net.SplitHostPort(parsed.Addr); err == nil {
		host = h
	}
	tlsCfg, err := ClientConfig("meta", c, host)
	if err != nil {
		return "", err
	}
	if name == "" {
		name = MetaTLSName
	}
	if err := mysql.RegisterTLSConfig(name, tlsCfg); err != nil {
		return "", fmt.Errorf("meta.tls: %s", err.Error())
	}
	parsed.TLSConfig = name
	return parsed.FormatDSN(), nil
}
