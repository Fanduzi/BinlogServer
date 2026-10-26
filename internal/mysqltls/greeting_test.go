// Package mysqltls proves the greeting bit and the verify-failure classifier.
// input: a v10 greeting payload and an error string
// output: CLIENT_SSL is read from the lower capability bytes; an x509 text is a verify failure and a dial timeout is not
// pos: unit coverage for ADR 0012 preferred and terminal TLS errors
// note: if this file changes, update this header and module README.md.
package mysqltls

import (
	"errors"
	"testing"
)

func TestGreetingOffersTLS(t *testing.T) {
	if GreetingOffersTLS(nil) || GreetingOffersTLS([]byte{9, 0}) {
		t.Fatal("short or non-v10 greeting offered TLS")
	}
	if GreetingOffersTLS(greetingPayload(false)) {
		t.Fatal("CLIENT_SSL clear was reported as TLS")
	}
	if !GreetingOffersTLS(greetingPayload(true)) {
		t.Fatal("CLIENT_SSL set was reported as plaintext")
	}
}

func TestIsVerifyFailure(t *testing.T) {
	if !IsVerifyFailure(errors.New("writeAuthHandshake: x509: certificate is valid for mysql.tls.test, not wrong.example")) {
		t.Fatal("hostname mismatch was not a verify failure")
	}
	if IsVerifyFailure(nil) || IsVerifyFailure(errors.New("dial tcp: i/o timeout")) {
		t.Fatal("a dial timeout was a verify failure")
	}
}

func greetingPayload(ssl bool) []byte {
	var b []byte
	b = append(b, 10)
	b = append(b, []byte("8.0.0")...)
	b = append(b, 0)
	b = append(b, 1, 0, 0, 0)
	b = append(b, make([]byte, 8)...)
	b = append(b, 0)
	flags := uint16(0)
	if ssl {
		flags = 1 << 11
	}
	b = append(b, byte(flags), byte(flags>>8))
	return b
}
