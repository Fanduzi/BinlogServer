# TLS for the source, the meta DB, and S3

Status: Accepted. Makes production-readiness G2 met: the replication connection and the meta connection can require and verify TLS, and S3 uses TLS unless the operator turns it off.

## Config

One TLS block, two places. No other MySQL connection gets a block. `verify.drill_dsn` is not dialed by the worker and stays a plain DSN.

```yaml
meta:
  tls:
    mode: disabled   # disabled | preferred | required | verify-ca | verify-identity
    ca: ""           # PEM file. Required for verify-ca and verify-identity
    cert: ""         # client certificate PEM. Optional; must be set with key
    key: ""          # client private key PEM. Optional; must be set with cert
    server_name: ""  # verify-identity only. Default is the host being dialed
```

The source block is the same object on the task, not in the process YAML:

```json
"source": { "host": "db.example.com", "port": 3306, "user": "repl", "password": "...",
  "tls": { "mode": "verify-identity", "ca": "/etc/binlog-server/ca.pem", "server_name": "db.example.com" } }
```

Environment for the process block follows the existing viper rule: `meta.tls.mode` is `BINLOG_SERVER_META_TLS_MODE`, and the same for `ca`, `cert`, `key`, `server_name`.

## Modes

Same five names on both connections, matching MySQL client `--ssl-mode`.

| mode | encryption | server certificate | hostname |
| --- | --- | --- | --- |
| `disabled` (omit the block) | no | — | — |
| `preferred` | yes if the server offers TLS, else plaintext | not checked | not checked |
| `required` | yes, or the dial fails | not checked | not checked |
| `verify-ca` | yes, or the dial fails | must chain to `ca` | not checked |
| `verify-identity` | yes, or the dial fails | must chain to `ca` | must match `server_name`, or the dial host if `server_name` is empty |

`ca` is rejected on `disabled`, `preferred`, and `required`. `server_name` is rejected except on `verify-identity`. `cert` and `key` are rejected on `disabled` and `preferred`; on the other modes they are both set or both empty. A client certificate is sent when both are set. `preferred` has no client certificate: the meta driver implements it as the built-in `tls=preferred` fallback, which cannot attach one.

## Where it is applied

Meta uses `github.com/go-sql-driver/mysql`. When `meta.tls.mode` is `disabled` or omitted, `meta_dsn` is passed through unchanged, including a `tls=` the operator already put in the DSN. Any other mode rejects a DSN that already contains `tls=` (`meta_dsn already sets tls=...; remove it and use meta.tls`). `preferred` appends `tls=preferred`. `required`, `verify-ca`, and `verify-identity` register one `tls.Config` under the name `binlog-meta` and append `tls=binlog-meta`. `verify-ca` sets `InsecureSkipVerify` and checks the chain in `VerifyPeerCertificate` with the configured CA only, so the dial host is not compared to the certificate. `verify-identity` sets `RootCAs` and `ServerName` and leaves hostname verification on.

The replication SQL connection is `connectSource` (`github.com/go-mysql-org/go-mysql/client`). The binlog dump is `replication.BinlogSyncerConfig.TLSConfig` on the same `tls.Config`. go-mysql has no preferred fallback. If a `tls.Config` is set and the greeting omits `CLIENT_SSL`, go-mysql closes the socket before it writes the client handshake. That close is an aborted connect. MySQL's host cache counts it (`Aborted_connects`, `performance_schema.host_cache.SUM_CONNECT_ERRORS`). A later successful handshake can reset the consecutive counter, and this design does not rely on that reset: concurrent preferred tasks abort before any of them succeeds, and with `max_connect_errors=2` four of them block the host (ERROR 1129).

`preferred` therefore decides on the socket it keeps. It reads one greeting packet. `GreetingOffersTLS` is that decision. `CLIENT_SSL` set: the same socket continues the TLS handshake (the greeting bytes are replayed into go-mysql). `CLIENT_SSL` clear: the same socket continues a plaintext handshake, with no `tls.Config`, so the server never sees a disconnect. A greeting that cannot be read is returned; it is not "no TLS" and there is no second dial. An auth error or a certificate error is returned from that one socket; there is no plaintext redial.

`preferred` trusts that greeting. An attacker who can change the packet can clear `CLIENT_SSL`, and the connection continues in plaintext. The meta driver's `tls=preferred` has the same downgrade. A deployment that needs protection uses `required`, `verify-ca`, or `verify-identity`.

A cached probe was the other option: one clean handshake per endpoint, single-flight, reused by later dials, dropped on a TLS failure. It still needs a lock and an expiry, and every dial that misses the cache can abort before the cache is filled. Reading the greeting on the connection that will authenticate needs neither, and it cannot abort.

The dump still probes once per run through `connectSource` and then sets or clears `TLSConfig`. That probe completes a handshake. It is an extra connection, not an aborted one. `required` and the verify modes set `TLSConfig` with no fallback.

A certificate check failure while killing a leftover dump is terminal: the task is `FAILED`, `last_error` is the x509 text, and the log is warn. A dial timeout on that kill stays `RETRY_BACKOFF` with the leftover-dump warning.

`migrate` accepts the same `meta.tls` block. `./migrate up --config /path/config.yaml` loads `meta_dsn` and applies `meta.tls` with `ApplyDSN`. `--dsn` and `META_DSN` override the file's `meta_dsn`. The tls block still comes from the file. Without `--config`, the DSN is unchanged, including a `tls=` already in it. `disabled` leaves that `tls=` in place. Any other mode rejects a DSN that already sets `tls=`.

Paths are read when that process dials. Meta files are checked at startup because this process opens the pool. Source files are checked when the worker dials, not when the API accepts the task: in cluster mode the control plane is not the process that holds the CA. A bad mode or a half-set cert/key is still rejected by create and update.

## Standalone and cluster

Source TLS is part of the task. Cluster stores it in `source_json`. Standalone stores it in the encrypted task record and restores it on restart. An update that omits `source.tls` keeps the stored block, so a console edit that does not know the field does not drop it. `"tls": {"mode": "disabled"}` clears it.

Meta TLS is process config. Standalone with an empty `meta_dsn` rejects a `meta.tls` mode other than disabled, because there is no connection to apply it to. Cluster and standalone-with-meta use the same rewritten DSN, whether the pool is `OpenClusterMeta` or `NewMySQLTaskStore`.

## Defaults

| connection | default | why |
| --- | --- | --- |
| source | `disabled` | Every existing task dials plaintext. A new block must not change that. |
| meta | `disabled` | A config with no `meta.tls` keys must keep working, including a `tls=` already in `meta_dsn`. |
| S3 `upload.use_ssl` | `true` | G2 requires S3 to use TLS unless the operator opts out. |

## `upload.use_ssl` upgrade

The default changes from `false` to `true`. A config that sets `upload.endpoint` and omits `upload.use_ssl` used to speak HTTP. It now speaks HTTPS.

That breaks an HTTP object store (MinIO, Ceph RGW on plain HTTP) that relied on the old default: uploads and HEAD fail the TLS handshake. The error keeps the transport text (`server gave HTTP response to HTTPS client`, or `first record does not look like a TLS handshake`) and tells the operator to set `upload.use_ssl: false` or fix the endpoint. It does not say `upload.tls.ca_file`. Before upgrading, set `upload.use_ssl: false` on every HTTP endpoint. Configs that already set `true` or `false` do not change. The shipped MinIO e2e configs already set `false`.

`upload.endpoint` is `host[:port]` with no scheme, which is what the S3 client accepts. A value containing `://` is a startup error: `upload.endpoint must be host[:port] without a URL scheme; upload.use_ssl defaults to true (HTTPS); set upload.use_ssl: false for a plain HTTP endpoint`. That is the validation error for an endpoint someone wrote as `http://...` while the new default is TLS.

## `upload.tls.ca_file`

Optional PEM file. Empty uses the system trust store. When set, it is the only trust store for this process's object client. The hostname is still checked. The file is read when the client is built. A missing file or a file with no certificates is a startup error that names the path and not the PEM. `upload.tls.ca_file` with `upload.use_ssl: false` is a startup error. The HTTPS client starts from minio-go's default transport (environment proxy, dial and header timeouts) and, when `ca_file` is set, replaces only `RootCAs`. A TLS handshake error is returned on the first attempt, so a bad CA at startup is the x509 text and `UPLOAD_TLS` immediately, not a 2s timeout. minio-go retries every TLS failure except a one-level `x509.UnknownAuthorityError` and the exact sentence `http: server gave HTTP response to HTTPS client`. The client returns that one-level error only so the retry stops, then puts the original error back before the caller sees it. Each call keeps that error on its own context, so two concurrent failures do not swap text. The original text is kept, and one hint is added from the error type:

| class | how it is recognized | hint |
| --- | --- | --- |
| plain HTTP with `use_ssl: true` | `server gave HTTP response to HTTPS client`, or a TLS first-record error | set `upload.use_ssl: false` or fix the endpoint |
| hostname mismatch | `x509.HostnameError` | hostname does not match the endpoint |
| unknown authority | `x509.UnknownAuthorityError` | set `upload.tls.ca_file` |
| protocol version or a remote TLS alert | `protocol version not supported`, `unsupported protocol version`, or `remote error: tls:` | original text, no hint. This is `UPLOAD_TLS` and is counted. The startup probe returns that text, not a 2s timeout |
| anything else | | original text, no hint |

A hostname mismatch does not get the `ca_file` hint. An unknown authority does not get the `use_ssl` hint. A repeated TLS transport failure is logged at warn on every upload attempt and degrades `/api/health` (`UPLOAD_TLS`, HTTP 503). That reason is the current failure: a later successful upload or retry whose stored checksum matches (the resolve path, not an empty-checksum report), or a probe detail that replaces the TLS line, clears it and `/api/health` is 200 again. A non-TLS upload failure does not clear it. A missing bucket or other probe failure stays detail with status ok. A probe deadline whose text is already a TLS failure stays the TLS text.

## Secrets

`ca`, `cert`, and `key` are filesystem paths. The key bytes are never written to a log line, an error string, or an API response. A key that cannot be parsed returns `source.tls: invalid client certificate or key` or the `meta.tls` form of that sentence, with the path and not the PEM. The API already clears `source.password`; it returns the paths, not the file contents.
