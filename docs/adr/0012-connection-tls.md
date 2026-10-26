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

The replication SQL connection is `connectSource` (`github.com/go-mysql-org/go-mysql/client`). The binlog dump is `replication.BinlogSyncerConfig.TLSConfig` on the same `tls.Config`. go-mysql has no preferred fallback, so `preferred` reads the server greeting and dials plaintext only when that greeting's capability flags omit `CLIENT_SSL`. A greeting that cannot be read is an error and is not "no TLS". An auth error, a certificate error, or any other handshake error does not dial again. The dump probes once per run through that same path and then sets or clears `TLSConfig`. `required` and the verify modes set `TLSConfig` with no fallback.

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

That breaks an HTTP object store (MinIO, Ceph RGW on plain HTTP) that relied on the old default: uploads and HEAD fail with a TLS handshake error. Before upgrading, set `upload.use_ssl: false` on every HTTP endpoint. Configs that already set `true` or `false` do not change. The shipped MinIO e2e configs already set `false`.

`upload.endpoint` is `host[:port]` with no scheme, which is what the S3 client accepts. A value containing `://` is a startup error: `upload.endpoint must be host[:port] without a URL scheme; upload.use_ssl defaults to true (HTTPS); set upload.use_ssl: false for a plain HTTP endpoint`. That is the validation error for an endpoint someone wrote as `http://...` while the new default is TLS.

## `upload.tls.ca_file`

Optional PEM file. Empty uses the system trust store. When set, it is the only trust store for this process's object client. The hostname is still checked. The file is read when the client is built. A missing file or a file with no certificates is a startup error that names the path and not the PEM. `upload.tls.ca_file` with `upload.use_ssl: false` is a startup error. A repeated TLS transport failure (x509, or an HTTP response to an HTTPS client) is logged at warn on every upload attempt and degrades `/api/health` (`UPLOAD_TLS`, HTTP 503). A missing bucket or other probe failure stays detail with status ok.

## Secrets

`ca`, `cert`, and `key` are filesystem paths. The key bytes are never written to a log line, an error string, or an API response. A key that cannot be parsed returns `source.tls: invalid client certificate or key` or the `meta.tls` form of that sentence, with the path and not the PEM. The API already clears `source.password`; it returns the paths, not the file contents.
