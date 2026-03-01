// input: YAML files, environment variables, default config constants
// output: validated runtime configuration structs for downstream modules
// pos: configuration boundary translating external settings into internal options
// note: if this file changes, update this header and module AGENTS.md.
package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadConfig_DefaultValues 验证相关行为。
func TestLoadConfig_DefaultValues(t *testing.T) {
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
}

// TestLoadConfig_FromYAMLFile 验证相关行为。
func TestLoadConfig_FromYAMLFile(t *testing.T) {
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
}

// TestLoadConfig_EnvOverridesYAML 验证相关行为。
func TestLoadConfig_EnvOverridesYAML(t *testing.T) {
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
