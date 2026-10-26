# internal/mysqltls Module

## Files

- `tls.go`: ADR 0012 TLS block. `Normalize` checks the mode shape and does not read files. `ClientConfig` reads `ca`/`cert`/`key` and returns a `*tls.Config` for go-mysql (nil means plaintext). `ApplyDSN` rewrites a meta DSN for `github.com/go-sql-driver/mysql`: disabled leaves the DSN unchanged, `preferred` sets `tls=preferred`, and the other modes register `binlog-meta` (or the caller's name). Errors name the path and never the PEM. `GreetingOffersTLS` reads `CLIENT_SSL` from a server greeting. `IsVerifyFailure` marks an x509 check failure. `PrefixMeta` prefixes a bare driver error with `meta.tls:` and does not stack that prefix. A hostname that contains `meta` (`metadb.example`) is still prefixed.
- `tls_test.go`: mode shape, defaults, DSN rewrite, CA verification without a hostname check, and key material kept out of errors.
- `greeting_test.go`: `CLIENT_SSL` on a v10 greeting, and x509 versus a dial timeout.

## Exports

- `Config`, `Normalize`, `ClientConfig`, `ApplyDSN`, `Disabled`, `Preferred`, `GreetingOffersTLS`, `IsVerifyFailure`, `PrefixMeta`.
- Modes: `disabled`, `preferred`, `required`, `verify-ca`, `verify-identity`.

## Dependencies

- Upstream: `internal/config` (meta block), `internal/replication` (source dial and binlog syncer), `internal/app` (meta DSN rewrite before the pool opens), `internal/tasks` (the struct stored on the task).
- Downstream: `crypto/tls`, `github.com/go-sql-driver/mysql`.

## Update Rule

- A mode, a DSN rewrite, or a secret-handling rule change updates this file and `docs/adr/0012-connection-tls.md`.
