# Security Guide

This document describes the security features and best practices for Binlog Server.

## Authentication

Binlog Server supports two authentication modes for protecting API endpoints:

### Bearer Token Authentication

Set the following configuration:

```yaml
api:
  auth:
    enabled: true
    mode: bearer
    bearer_token: "your-secret-token"
    protect_api: true
    protect_metrics: true  # defaults to true when api.auth.enabled=true and unset
```

Or via environment variables:

```bash
export BINLOG_SERVER_API_AUTH_ENABLED=true
export BINLOG_SERVER_API_AUTH_MODE=bearer
export BINLOG_SERVER_API_AUTH_BEARER_TOKEN="your-secret-token"
export BINLOG_SERVER_API_AUTH_PROTECT_API=true
```

### API Key Authentication

Set the following configuration:

```yaml
api:
  auth:
    enabled: true
    mode: api_key
    api_key: "your-api-key"
    api_key_header: "X-API-Key"  # Default header name
    protect_api: true
```

Or via environment variables:

```bash
export BINLOG_SERVER_API_AUTH_ENABLED=true
export BINLOG_SERVER_API_AUTH_MODE=api_key
export BINLOG_SERVER_API_AUTH_API_KEY="your-api-key"
export BINLOG_SERVER_API_AUTH_PROTECT_API=true
```

### Using Authentication

When authentication is enabled, include the token in your requests:

**Bearer Token:**
```bash
curl -H "Authorization: Bearer your-secret-token" http://localhost:8080/api/tasks
```

**API Key:**
```bash
curl -H "X-API-Key: your-api-key" http://localhost:8080/api/tasks
```

### Web Console

Open `/ui/` in a browser. The address bar cannot set `Authorization`, and the server does not require it for `/ui/*`. The Console calls `/api/*`. A 401 opens Settings. Paste the bearer token. The page stores it and sends `Authorization: Bearer <token>` on later API calls.

Unauthenticated `/api/*` still returns 401. `/metrics` follows `protect_metrics`. `/swagger/*` uses the same middleware when `api.auth.enabled=true` and has no token field; use curl or a reverse proxy. `/healthz` stays unauthenticated.

The Settings field is a bearer token. `api_key` mode is for clients that send the configured header. The Console does not send that header.

## Key Management

### Environment Variables (Recommended)

Store sensitive values in environment variables rather than in config files:

```bash
export BINLOG_SERVER_API_AUTH_BEARER_TOKEN="${API_TOKEN}"
export BINLOG_SERVER_UPLOAD_SECRET_KEY="${S3_SECRET_KEY}"
```

### Encrypted Configuration Values

For enhanced security, you can encrypt sensitive values in your configuration file using AES-256-GCM:

1. **Generate an encryption key** (32 bytes for AES-256):
   ```bash
   openssl rand -base64 32 | head -c 32
   ```

2. **Encrypt a value** (use the encrypt tool or your preferred method)

3. **Use the encrypted value** in your config:
   ```yaml
   api:
     auth:
       bearer_token: "enc:aes256:BASE64_ENCODED_CIPHERTEXT"
   ```

4. **Provide the encryption key** when starting the server:
   ```bash
   ./binlog-server --config config.yaml --encryption-key "your-32-byte-encryption-key"
   ```
   The same key encrypts task source passwords in `backup_tasks.source_json`. Without a key, those passwords remain plaintext JSON.

### Encrypted Value Format

Encrypted values use the format: `enc:aes256:<base64-encoded-ciphertext>`

The base64-encoded ciphertext contains:
- A 12-byte nonce (for GCM)
- The encrypted data
- A 16-byte authentication tag (from GCM)

## Standalone task record key

Without `meta_dsn`, task records are encrypted in `{data_dir}/.tasks.enc` (ADR 0010). A fresh install that never sets a key starts, `/healthz` stays `ok`, and `/api/health` is degraded:

```text
{"status":"degraded","reasons":["TASK_RECORDS_FAILED/key_missing (task _cluster)"]}
```

Generate a key before the first start. `task-key generate` writes a new mode `0600` file and prints the path and key id. It does not print the key. Do not capture its stdout and treat that text as key material.

```bash
./binlog-server task-key generate --out ./task.key
export BINLOG_SERVER_STANDALONE_TASK_KEY_FILE="$PWD/task.key"
```

The same path can be set as `standalone.task_key_file` in the YAML file. An existing `--out` path is refused, so a second generate does not overwrite a live key.

The file must be a regular file with mode `0600` or `0400`. A wider mode is `TASK_RECORDS_FAILED/key_invalid`. The process does not change the mode for you.

Keep the file outside `data_dir` and back it up separately. The encrypted task file is useless without this key, and a copy of `data_dir` should not also contain the key.

`BINLOG_SERVER_TASK_KEY` is the alternative: 32 bytes as base64 or 64 hex characters, for a container secret. The process unsets the variable after reading it. A file is still preferred, because a running process environment is visible to the same user and to root.

### Rotation

1. Generate a new key file. Leave the current key in place.
2. Point `standalone.task_key_previous_file` (or `BINLOG_SERVER_TASK_KEY_PREVIOUS`) at the old key.
3. Point `standalone.task_key_file` (or `BINLOG_SERVER_STANDALONE_TASK_KEY_FILE`) at the new key.
4. Restart. Startup re-encrypts `.tasks.enc` with the new key.
5. After that restart has finished, remove the previous-key setting. Keep the old file until you have confirmed the new file loads.

### Storage

Do not commit the key, do not put it in shell history, and do not store it under `data_dir`. Use a root-owned `0600` file outside the data directory, or a container secret via `BINLOG_SERVER_TASK_KEY`.

## Rate Limiting

Binlog Server includes built-in rate limiting to protect against abuse:

```yaml
api:
  rate_limit:
    enabled: true           # Default: true
    requests_per_second: 100  # Default: 100
    burst: 200              # Default: 200
```

Rate limiting is applied per-client-IP and covers all endpoints.

## Production Checklist

Before deploying to production:

1. **Enable Authentication**
   - Set `api.auth.enabled: true`
   - Configure a strong, unique token
   - Protect both API and metrics endpoints
   - With auth enabled, `/ui/*` loads without a credential so the Console can collect the bearer token. `/swagger/*` uses the same auth middleware as `/api/*`

2. **Pass `--encryption-key`**
   - `PRODUCTION=true` refuses to start when `--encryption-key` is empty
   - Use 32 bytes so AES-256 can encrypt source passwords
   - Pass the key as a process flag. The production YAML template does not store it

3. **Use Environment Variables or Encryption**
   - Never commit plaintext secrets to version control
   - Use `${ENV_VAR}` syntax or encrypted values

4. **Configure Rate Limiting**
   - Adjust `requests_per_second` based on your expected load
   - Set appropriate `burst` for legitimate traffic spikes

5. **Review Log Output**
   - Ensure sensitive data (passwords, tokens) are not logged
   - Configure appropriate log levels

6. **Set the standalone task key** when there is no `meta_dsn`
   - `./binlog-server task-key generate --out <path>` then `standalone.task_key_file` or `BINLOG_SERVER_STANDALONE_TASK_KEY_FILE`
   - Mode `0600` or `0400`, stored outside `data_dir`
   - Rotate with `standalone.task_key_previous_file` and keep the old key until the restart re-encrypts `.tasks.enc`

7. **Network Security**
   - Use TLS for production deployments
   - Consider running behind a reverse proxy with additional security features

## Security Warnings

When authentication is disabled, Binlog Server will display a warning at startup:

```
⚠️  SECURITY WARNING: API authentication is DISABLED
   For production, set api.auth.enabled=true and configure your auth method.
   See docs/security.md for details.
```

The control-plane also fail-closes when `listen_addr` is not loopback (`:8080` and `0.0.0.0:8080` count as non-loopback). Local demo on `127.0.0.1`/`localhost`/`::1` may keep auth disabled.

In production mode (when `PRODUCTION=true` environment variable is set), the server will refuse to start without authentication enabled. This check is independent of listen_addr and is not weakened.

When `--encryption-key` is set, task source passwords are stored in `backup_tasks.source_json` as `enc:aes256:` ciphertext and decrypted only for internal use. API responses still redact `source.password`. Outside production, an empty key still loads existing plaintext rows. When `PRODUCTION=true`, an empty `--encryption-key` stops the process before it listens. That check stays in place when `api.auth.enabled` is already true.
