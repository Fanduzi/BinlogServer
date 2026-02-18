package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
}

func TestLoadConfig_FromYAMLFile(t *testing.T) {
	t.Setenv("BINLOG_SERVER_LISTEN_ADDR", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_USE_SSL", "")

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
}

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
}

func TestLoadConfig_MissingExplicitFileReturnsError(t *testing.T) {
	_, err := LoadConfig("/path/not/exist/config.yaml")
	if err == nil {
		t.Fatal("expected error when explicit config file does not exist")
	}
}

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
