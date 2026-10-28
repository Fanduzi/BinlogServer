# internal/livemysql Module

## Files

- `mysql.go`: docker MySQL 8 fixture for `BINLOG_TEST_TLS=1`. `Require` skips when that variable is empty. `FreePort` takes and releases a `127.0.0.1` port so the fixture can claim it. `WriteCerts` builds a one-day CA, a server certificate, and a second CA (`wrong-ca.pem`) for the untrusted-CA negative cases. `StartMySQL` publishes `127.0.0.1` only. The TLS server mounts the cert directory. The plaintext server sets an empty `--tls-version`.

## Exports

- `Require`, `FreePort`, `WriteCerts`, `StartMySQL`.

## Dependencies

- Upstream: `internal/replication`, `internal/app`, `internal/upload` live TLS tests.
- Downstream: `docker`, `openssl`, the local `mysql:8.0` image.

## Update Rule

- A change to the fixture or the certificate layout updates this file.
