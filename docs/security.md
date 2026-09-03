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
    protect_metrics: true  # Optional: also protect /metrics endpoint
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
### Encrypted Value Format

Encrypted values use the format: `enc:aes256:<base64-encoded-ciphertext>`

The base64-encoded ciphertext contains:
- A 12-byte nonce (for GCM)
- The encrypted data
- A 16-byte authentication tag (from GCM)

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

2. **Use Environment Variables or Encryption**
   - Never commit plaintext secrets to version control
   - Use `${ENV_VAR}` syntax or encrypted values

3. **Configure Rate Limiting**
   - Adjust `requests_per_second` based on your expected load
   - Set appropriate `burst` for legitimate traffic spikes

4. **Review Log Output**
   - Ensure sensitive data (passwords, tokens) are not logged
   - Configure appropriate log levels

5. **Network Security**
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
