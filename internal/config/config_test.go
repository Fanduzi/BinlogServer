// Package config provides module-level functionality for config.
// input: YAML files, environment variables, default config constants
// output: config loading regression coverage including unresolved protected-auth secret rejection
// pos: configuration boundary translating external settings into internal options
// note: if this file changes, update this header and module README.md.
package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setRequiredAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BINLOG_SERVER_API_AUTH_ENABLED", "true")
	t.Setenv("BINLOG_SERVER_API_AUTH_MODE", "bearer")
	t.Setenv("BINLOG_SERVER_API_AUTH_BEARER_TOKEN", "test-token")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_API", "true")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_METRICS", "true")
}

// TestLoadConfig_DefaultValues 验证相关行为。
func TestLoadConfig_DefaultValues(t *testing.T) {
	setRequiredAuthEnv(t)
	t.Setenv("BINLOG_SERVER_LISTEN_ADDR", "")
	t.Setenv("BINLOG_SERVER_DATA_DIR", "")
	t.Setenv("BINLOG_SERVER_META_DSN", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_ENDPOINT", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_BUCKET", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_ACCESS_KEY", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_SECRET_KEY", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_REGION", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_PREFIX", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_USE_SSL", "")
	t.Setenv("BINLOG_SERVER_LOG_LEVEL", "")
	t.Setenv("BINLOG_SERVER_LOG_ENCODING", "")
	t.Setenv("BINLOG_SERVER_LOG_FILE", "")
	t.Setenv("BINLOG_SERVER_LOG_MAX_SIZE_MB", "")
	t.Setenv("BINLOG_SERVER_LOG_MAX_BACKUPS", "")
	t.Setenv("BINLOG_SERVER_LOG_MAX_AGE_DAYS", "")
	t.Setenv("BINLOG_SERVER_LOG_COMPRESS", "")
	t.Setenv("BINLOG_SERVER_LOG_ROTATE_INTERVAL", "")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("expected :8080, got %s", cfg.ListenAddr)
	}
	if cfg.DataDir != "./data" {
		t.Fatalf("expected ./data, got %s", cfg.DataDir)
	}
	if cfg.MetaDSN != "" {
		t.Fatalf("expected empty meta dsn, got %s", cfg.MetaDSN)
	}
	if cfg.UploadEndpoint != "" || cfg.UploadBucket != "" {
		t.Fatalf("expected upload config empty by default, got endpoint=%q bucket=%q", cfg.UploadEndpoint, cfg.UploadBucket)
	}
	if cfg.UploadUseSSL {
		t.Fatal("expected upload ssl disabled by default")
	}
	if cfg.Log.Level != "info" {
		t.Fatalf("expected default log level info, got %q", cfg.Log.Level)
	}
	if cfg.Log.Encoding != "json" {
		t.Fatalf("expected default log encoding json, got %q", cfg.Log.Encoding)
	}
	if cfg.Log.File != "./logs/binlog-server.log" {
		t.Fatalf("expected default log file, got %q", cfg.Log.File)
	}
	if cfg.Log.MaxSizeMB != 100 {
		t.Fatalf("expected default log max_size_mb=100, got %d", cfg.Log.MaxSizeMB)
	}
	if cfg.Log.RotateInterval != "24h" {
		t.Fatalf("expected default log rotate_interval=24h, got %q", cfg.Log.RotateInterval)
	}
	if cfg.HTTP.ControlPlane.ReadHeaderTimeoutSec <= 0 || cfg.HTTP.ControlPlane.ReadTimeoutSec <= 0 || cfg.HTTP.ControlPlane.WriteTimeoutSec <= 0 || cfg.HTTP.ControlPlane.IdleTimeoutSec <= 0 {
		t.Fatalf("expected positive control-plane HTTP timeout defaults, got %+v", cfg.HTTP.ControlPlane)
	}
	if cfg.HTTP.WorkerHealth.ReadHeaderTimeoutSec <= 0 || cfg.HTTP.WorkerHealth.ReadTimeoutSec <= 0 || cfg.HTTP.WorkerHealth.WriteTimeoutSec <= 0 || cfg.HTTP.WorkerHealth.IdleTimeoutSec <= 0 {
		t.Fatalf("expected positive worker-health HTTP timeout defaults, got %+v", cfg.HTTP.WorkerHealth)
	}
	if cfg.Meta.Timeout.ReadSec <= 0 || cfg.Meta.Timeout.WriteSec <= 0 || cfg.Meta.Timeout.LeaseSec <= 0 || cfg.Meta.Timeout.UploadSec <= 0 {
		t.Fatalf("expected positive meta timeout defaults, got %+v", cfg.Meta.Timeout)
	}
	if cfg.Tracing.Enabled || cfg.Tracing.Exporter != "disabled" || cfg.Tracing.SampleRatio != 0.1 {
		t.Fatalf("unexpected tracing defaults: %+v", cfg.Tracing)
	}
	if !cfg.API.Auth.Enabled || cfg.API.Auth.Mode != "bearer" || cfg.API.Auth.BearerToken != "test-token" {
		t.Fatalf("unexpected API auth defaults/overrides: %+v", cfg.API.Auth)
	}
}

// TestLoadConfig_FromYAMLFile 验证相关行为。
func TestLoadConfig_FromYAMLFile(t *testing.T) {
	t.Setenv("BINLOG_SERVER_API_AUTH_ENABLED", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_MODE", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_BEARER_TOKEN", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_API_KEY", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_API_KEY_HEADER", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_API", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_METRICS", "")
	t.Setenv("BINLOG_SERVER_LISTEN_ADDR", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_USE_SSL", "")
	t.Setenv("BINLOG_SERVER_LOG_LEVEL", "")
	t.Setenv("BINLOG_SERVER_LOG_ENCODING", "")
	t.Setenv("BINLOG_SERVER_LOG_FILE", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
listen_addr: "127.0.0.1:19090"
data_dir: "/tmp/binlogs"
meta_dsn: "user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true"
upload:
  endpoint: "s3.example.com"
  bucket: "binlog-backup"
  access_key: "ak"
  secret_key: "sk"
  region: "cn-north-1"
  prefix: "prod"
  use_ssl: true
api:
  auth:
    enabled: true
    mode: "bearer"
    bearer_token: "yaml-token"
    protect_api: true
    protect_metrics: false
http:
  control_plane:
    read_header_timeout_sec: 6
    read_timeout_sec: 31
    write_timeout_sec: 32
    idle_timeout_sec: 121
  worker_health:
    read_header_timeout_sec: 4
    read_timeout_sec: 11
    write_timeout_sec: 12
    idle_timeout_sec: 33
meta:
  timeout:
    read_sec: 4
    write_sec: 9
    lease_sec: 6
    upload_sec: 21
tracing:
  enabled: true
  exporter: "otlp-http"
  endpoint: "http://127.0.0.1:4318/v1/traces"
  sample_ratio: 0.5
  service_name: "binlog-server-dev"
log:
  level: "debug"
  encoding: "console"
  file: "/tmp/binlog-server.log"
  max_size_mb: 64
  max_backups: 10
  max_age_days: 14
  compress: true
  rotate_interval: "12h"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:19090" {
		t.Fatalf("unexpected listen addr: %s", cfg.ListenAddr)
	}
	if cfg.DataDir != "/tmp/binlogs" {
		t.Fatalf("unexpected data dir: %s", cfg.DataDir)
	}
	if cfg.MetaDSN == "" {
		t.Fatal("expected meta dsn from yaml")
	}
	if cfg.UploadEndpoint != "s3.example.com" || cfg.UploadBucket != "binlog-backup" {
		t.Fatalf("unexpected upload config: endpoint=%s bucket=%s", cfg.UploadEndpoint, cfg.UploadBucket)
	}
	if !cfg.UploadUseSSL {
		t.Fatal("expected upload ssl enabled from yaml")
	}
	if cfg.Log.Level != "debug" {
		t.Fatalf("expected log level debug, got %q", cfg.Log.Level)
	}
	if cfg.Log.Encoding != "console" {
		t.Fatalf("expected log encoding console, got %q", cfg.Log.Encoding)
	}
	if cfg.Log.File != "/tmp/binlog-server.log" {
		t.Fatalf("expected log file from yaml, got %q", cfg.Log.File)
	}
	if cfg.Log.MaxSizeMB != 64 || cfg.Log.MaxBackups != 10 || cfg.Log.MaxAgeDays != 14 {
		t.Fatalf("unexpected log rotate settings: %+v", cfg.Log)
	}
	if !cfg.Log.Compress {
		t.Fatal("expected log compress enabled from yaml")
	}
	if cfg.Log.RotateInterval != "12h" {
		t.Fatalf("expected log rotate_interval 12h, got %q", cfg.Log.RotateInterval)
	}
	if cfg.API.Auth.BearerToken != "yaml-token" || cfg.API.Auth.ProtectMetrics {
		t.Fatalf("unexpected api auth config: %+v", cfg.API.Auth)
	}
	if cfg.HTTP.ControlPlane.ReadHeaderTimeoutSec != 6 || cfg.HTTP.ControlPlane.ReadTimeoutSec != 31 || cfg.HTTP.ControlPlane.WriteTimeoutSec != 32 || cfg.HTTP.ControlPlane.IdleTimeoutSec != 121 {
		t.Fatalf("unexpected control-plane http timeout config: %+v", cfg.HTTP.ControlPlane)
	}
	if cfg.HTTP.WorkerHealth.ReadHeaderTimeoutSec != 4 || cfg.HTTP.WorkerHealth.ReadTimeoutSec != 11 || cfg.HTTP.WorkerHealth.WriteTimeoutSec != 12 || cfg.HTTP.WorkerHealth.IdleTimeoutSec != 33 {
		t.Fatalf("unexpected worker-health http timeout config: %+v", cfg.HTTP.WorkerHealth)
	}
	if cfg.Meta.Timeout.ReadSec != 4 || cfg.Meta.Timeout.WriteSec != 9 || cfg.Meta.Timeout.LeaseSec != 6 || cfg.Meta.Timeout.UploadSec != 21 {
		t.Fatalf("unexpected meta timeout config: %+v", cfg.Meta.Timeout)
	}
	if !cfg.Tracing.Enabled || cfg.Tracing.Exporter != "otlp-http" || cfg.Tracing.SampleRatio != 0.5 || cfg.Tracing.ServiceName != "binlog-server-dev" {
		t.Fatalf("unexpected tracing config: %+v", cfg.Tracing)
	}
}

// TestLoadConfig_EnvOverridesYAML 验证相关行为。
func TestLoadConfig_EnvOverridesYAML(t *testing.T) {
	setRequiredAuthEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
listen_addr: "127.0.0.1:19090"
upload:
  use_ssl: false
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	t.Setenv("BINLOG_SERVER_LISTEN_ADDR", "127.0.0.1:28080")
	t.Setenv("BINLOG_SERVER_UPLOAD_USE_SSL", "true")
	t.Setenv("BINLOG_SERVER_LOG_LEVEL", "warn")
	t.Setenv("BINLOG_SERVER_LOG_ENCODING", "json")
	t.Setenv("BINLOG_SERVER_LOG_FILE", "/var/log/binlog-server.log")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:28080" {
		t.Fatalf("expected env override listen addr, got %s", cfg.ListenAddr)
	}
	if !cfg.UploadUseSSL {
		t.Fatal("expected env override upload.use_ssl=true")
	}
	if cfg.Log.Level != "warn" {
		t.Fatalf("expected env override log.level=warn, got %q", cfg.Log.Level)
	}
	if cfg.Log.Encoding != "json" {
		t.Fatalf("expected env override log.encoding=json, got %q", cfg.Log.Encoding)
	}
	if cfg.Log.File != "/var/log/binlog-server.log" {
		t.Fatalf("expected env override log.file, got %q", cfg.Log.File)
	}
	if cfg.Meta.Timeout.ReadSec != 3 || cfg.Meta.Timeout.WriteSec != 5 || cfg.Meta.Timeout.LeaseSec != 2 || cfg.Meta.Timeout.UploadSec != 30 {
		t.Fatalf("expected default meta timeout fallback for legacy yaml, got %+v", cfg.Meta.Timeout)
	}
}

// TestLoadConfig_InvalidTracingSampleRatio 验证 tracing 采样率越界时返回错误。
func TestLoadConfig_InvalidTracingSampleRatio(t *testing.T) {
	setRequiredAuthEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
tracing:
  enabled: true
  exporter: "otlp-http"
  endpoint: "http://127.0.0.1:4318/v1/traces"
  sample_ratio: 1.5
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid tracing.sample_ratio")
	}
	if !strings.Contains(err.Error(), "tracing.sample_ratio") {
		t.Fatalf("expected tracing.sample_ratio validation error, got %v", err)
	}
}

// TestLoadConfig_UnsupportedEnabledTracingExporter 验证启用 tracing 时仅支持 otlp-http。
func TestLoadConfig_UnsupportedEnabledTracingExporter(t *testing.T) {
	setRequiredAuthEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
tracing:
  enabled: true
  exporter: "jaeger"
  endpoint: "http://127.0.0.1:4318/v1/traces"
  sample_ratio: 0.5
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for unsupported enabled tracing exporter")
	}
	if !strings.Contains(err.Error(), "otlp-http") {
		t.Fatalf("expected otlp-http constraint in error, got %v", err)
	}
}

// TestLoadConfig_MissingExplicitFileReturnsError 验证相关行为。
func TestLoadConfig_MissingExplicitFileReturnsError(t *testing.T) {
	_, err := LoadConfig("/path/not/exist/config.yaml")
	if err == nil {
		t.Fatal("expected error when explicit config file does not exist")
	}
}

// TestLoadConfig_ClusterDefaults 验证相关行为。
func TestLoadConfig_ClusterDefaults(t *testing.T) {
	setRequiredAuthEnv(t)
	t.Setenv("BINLOG_SERVER_MODE", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_ROLE", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_WORKER_ID", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_LEASE_TTL_SEC", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_LEASE_RENEW_INTERVAL_SEC", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_LEASE_GRACE_SEC", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_FAILOVER_POLICY", "")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.Mode != "standalone" {
		t.Fatalf("expected mode standalone, got %q", cfg.Mode)
	}
	if cfg.Cluster.Role != "all-in-one" {
		t.Fatalf("expected cluster role all-in-one, got %q", cfg.Cluster.Role)
	}
	if cfg.Cluster.WorkerHealthListenAddr != "" {
		t.Fatalf("expected empty worker_health_listen_addr, got %q", cfg.Cluster.WorkerHealthListenAddr)
	}
	if cfg.Cluster.LeaseTTLSec != 15 {
		t.Fatalf("expected lease_ttl_sec=15, got %d", cfg.Cluster.LeaseTTLSec)
	}
	if cfg.Cluster.LeaseRenewIntervalSec != 5 {
		t.Fatalf("expected lease_renew_interval_sec=5, got %d", cfg.Cluster.LeaseRenewIntervalSec)
	}
	if cfg.Cluster.LeaseGraceSec != 30 {
		t.Fatalf("expected lease_grace_sec=30, got %d", cfg.Cluster.LeaseGraceSec)
	}
	if cfg.Cluster.FailoverPolicy != "rebuild_current_file" {
		t.Fatalf("expected failover_policy rebuild_current_file, got %q", cfg.Cluster.FailoverPolicy)
	}
}

// TestLoadConfig_ClusterFromYAML 验证相关行为。
func TestLoadConfig_ClusterFromYAML(t *testing.T) {
	setRequiredAuthEnv(t)
	t.Setenv("BINLOG_SERVER_MODE", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_ROLE", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_WORKER_ID", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_LEASE_TTL_SEC", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_LEASE_RENEW_INTERVAL_SEC", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_LEASE_GRACE_SEC", "")
	t.Setenv("BINLOG_SERVER_CLUSTER_FAILOVER_POLICY", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
mode: "cluster"
cluster:
  role: "worker"
  worker_id: "worker-a"
  worker_health_listen_addr: "127.0.0.1:19081"
  lease_ttl_sec: 20
  lease_renew_interval_sec: 6
  lease_grace_sec: 45
  failover_policy: "rebuild_current_file"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.Mode != "cluster" {
		t.Fatalf("expected mode cluster, got %q", cfg.Mode)
	}
	if cfg.Cluster.Role != "worker" {
		t.Fatalf("expected cluster role worker, got %q", cfg.Cluster.Role)
	}
	if cfg.Cluster.WorkerID != "worker-a" {
		t.Fatalf("expected cluster worker_id worker-a, got %q", cfg.Cluster.WorkerID)
	}
	if cfg.Cluster.WorkerHealthListenAddr != "127.0.0.1:19081" {
		t.Fatalf("expected worker_health_listen_addr 127.0.0.1:19081, got %q", cfg.Cluster.WorkerHealthListenAddr)
	}
	if cfg.Cluster.LeaseTTLSec != 20 {
		t.Fatalf("expected lease_ttl_sec=20, got %d", cfg.Cluster.LeaseTTLSec)
	}
	if cfg.Cluster.LeaseRenewIntervalSec != 6 {
		t.Fatalf("expected lease_renew_interval_sec=6, got %d", cfg.Cluster.LeaseRenewIntervalSec)
	}
	if cfg.Cluster.LeaseGraceSec != 45 {
		t.Fatalf("expected lease_grace_sec=45, got %d", cfg.Cluster.LeaseGraceSec)
	}
	if cfg.Cluster.FailoverPolicy != "rebuild_current_file" {
		t.Fatalf("expected failover_policy rebuild_current_file, got %q", cfg.Cluster.FailoverPolicy)
	}
}

// TestLoadConfig_ExpandsEnvPlaceholders 验证敏感字段支持 ${ENV_VAR} 占位符展开。
func TestLoadConfig_ExpandsEnvPlaceholders(t *testing.T) {
	setRequiredAuthEnv(t)
	t.Setenv("TEST_META_DSN", "user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true")
	t.Setenv("TEST_UPLOAD_SK", "super-secret")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
meta_dsn: "${TEST_META_DSN}"
upload:
  secret_key: "${TEST_UPLOAD_SK}"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.MetaDSN != "user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true" {
		t.Fatalf("unexpected expanded meta dsn: %q", cfg.MetaDSN)
	}
	if cfg.UploadSecretKey != "super-secret" {
		t.Fatalf("unexpected expanded upload secret key: %q", cfg.UploadSecretKey)
	}
}

// TestLoadConfig_WarnsPlaintextSensitiveValues 验证配置文件明文敏感项会触发告警。
func TestLoadConfig_WarnsPlaintextSensitiveValues(t *testing.T) {
	setRequiredAuthEnv(t)
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
meta_dsn: "user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true"
upload:
  secret_key: "plain-secret"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	logs := buf.String()
	if !strings.Contains(logs, `key "meta_dsn" appears to contain plaintext`) {
		t.Fatalf("expected meta_dsn warning, logs=%q", logs)
	}
	if !strings.Contains(logs, `key "upload.secret_key" appears to contain plaintext`) {
		t.Fatalf("expected upload.secret_key warning, logs=%q", logs)
	}
}

// TestLoadConfig_E2EStyleConfigWithoutAuthToken 验证 e2e 风格配置在未提供 auth token 时可正常加载。
func TestLoadConfig_E2EStyleConfigWithoutAuthToken(t *testing.T) {
	t.Setenv("BINLOG_SERVER_API_AUTH_ENABLED", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_MODE", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_BEARER_TOKEN", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_API_KEY", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_API_KEY_HEADER", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_API", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_METRICS", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
listen_addr: "127.0.0.1:18080"
data_dir: "./tmp/e2e/data"
meta_dsn: ""
upload:
  use_ssl: false
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.API.Auth.Enabled {
		t.Fatalf("expected api.auth.enabled default=false for e2e-friendly startup, got %+v", cfg.API.Auth)
	}
	if cfg.API.Auth.ProtectAPI || cfg.API.Auth.ProtectMetrics {
		t.Fatalf("expected api auth route protection defaults disabled, got %+v", cfg.API.Auth)
	}
}

// TestLoadConfig_InvalidHTTPTimeoutValidation 验证非法 HTTP 超时配置会被拒绝。
func TestLoadConfig_InvalidHTTPTimeoutValidation(t *testing.T) {
	setRequiredAuthEnv(t)
	t.Setenv("BINLOG_SERVER_HTTP_CONTROL_PLANE_READ_TIMEOUT_SEC", "0")

	_, err := LoadConfig("")
	if err == nil {
		t.Fatal("expected error when http.control_plane.read_timeout_sec <= 0")
	}
	if !strings.Contains(err.Error(), "http.control_plane.read_timeout_sec must be > 0") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoadConfig_InvalidAPIAuthValidation 验证鉴权保护开启时必须提供凭证。
func TestLoadConfig_InvalidAPIAuthValidation(t *testing.T) {
	t.Setenv("BINLOG_SERVER_API_AUTH_ENABLED", "true")
	t.Setenv("BINLOG_SERVER_API_AUTH_MODE", "bearer")
	t.Setenv("BINLOG_SERVER_API_AUTH_BEARER_TOKEN", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_API", "true")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_METRICS", "true")

	_, err := LoadConfig("")
	if err == nil {
		t.Fatal("expected error when bearer auth enabled without token")
	}
	if !strings.Contains(err.Error(), "api.auth.bearer_token is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoadConfig_RejectsUnresolvedProtectedAuthSecret verifies deployment templates cannot start protected routes with a missing environment secret.
func TestLoadConfig_RejectsUnresolvedProtectedAuthSecret(t *testing.T) {
	t.Setenv("BINLOG_SERVER_API_AUTH_ENABLED", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_MODE", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_BEARER_TOKEN", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_API", "")
	t.Setenv("BINLOG_SERVER_API_AUTH_PROTECT_METRICS", "")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`api:
  auth:
    enabled: true
    mode: bearer
    bearer_token: "${BINLOG_SERVER_API_AUTH_BEARER_TOKEN}"
    protect_api: true
    protect_metrics: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "api.auth.bearer_token is required") {
		t.Fatalf("expected unresolved bearer token error, got %v", err)
	}
}
