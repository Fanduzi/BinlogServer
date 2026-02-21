package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Config 定义服务运行所需的全部配置项。
type Config struct {
	// ListenAddr 是 API/UI 服务监听地址（如 :8080）。
	ListenAddr string
	// DataDir 是本地 binlog 文件落盘目录。
	DataDir    string
	// MetaDSN 是元数据 MySQL 连接串；为空则走内存模式。
	MetaDSN    string
	// Mode 支持 standalone/cluster。
	Mode       string

	// Cluster 是 cluster 运行时相关配置。
	Cluster ClusterConfig

	// Upload* 是对象存储上传配置（启用时生效）。
	UploadEndpoint  string
	UploadBucket    string
	UploadAccessKey string
	UploadSecretKey string
	UploadRegion    string
	UploadPrefix    string
	UploadUseSSL    bool
}

// ClusterConfig 定义 cluster 角色和 lease 参数。
type ClusterConfig struct {
	Role                   string
	WorkerID               string
	WorkerHealthListenAddr string
	LeaseTTLSec            int
	LeaseRenewIntervalSec  int
	LeaseGraceSec          int
	FailoverPolicy         string
}

// LoadConfig 按“默认值 < 配置文件 < 环境变量”顺序加载配置。
func LoadConfig(path string) (Config, error) {
	v := viper.New()
	v.SetEnvPrefix("BINLOG_SERVER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// 默认值 < 配置文件 < 环境变量。
	v.SetDefault("listen_addr", ":8080")
	v.SetDefault("data_dir", "./data")
	v.SetDefault("mode", "standalone")
	v.SetDefault("cluster.role", "all-in-one")
	v.SetDefault("cluster.worker_health_listen_addr", "")
	v.SetDefault("cluster.lease_ttl_sec", 15)
	v.SetDefault("cluster.lease_renew_interval_sec", 5)
	v.SetDefault("cluster.lease_grace_sec", 30)
	v.SetDefault("cluster.failover_policy", "rebuild_current_file")
	v.SetDefault("upload.use_ssl", false)

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("read config file %q: %w", path, err)
		}
	} else {
		// 未显式指定时尝试加载 ./config.yaml，不存在则忽略。
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		if err := v.ReadInConfig(); err != nil {
			var notFound viper.ConfigFileNotFoundError
			if !errors.As(err, &notFound) {
				return Config{}, fmt.Errorf("read default config file: %w", err)
			}
		}
	}

	cfg := Config{
		ListenAddr: getString(v, "listen_addr"),
		DataDir:    getString(v, "data_dir"),
		MetaDSN:    getString(v, "meta_dsn"),
		Mode:       getString(v, "mode"),
		Cluster: ClusterConfig{
			Role:                   getString(v, "cluster.role"),
			WorkerID:               getString(v, "cluster.worker_id"),
			WorkerHealthListenAddr: getString(v, "cluster.worker_health_listen_addr"),
			LeaseTTLSec:            getInt(v, "cluster.lease_ttl_sec"),
			LeaseRenewIntervalSec:  getInt(v, "cluster.lease_renew_interval_sec"),
			LeaseGraceSec:          getInt(v, "cluster.lease_grace_sec"),
			FailoverPolicy:         getString(v, "cluster.failover_policy"),
		},
		UploadEndpoint: getString(v, "upload.endpoint", "upload_endpoint"),
		UploadBucket:   getString(v, "upload.bucket", "upload_bucket"),
		UploadAccessKey: getString(v,
			"upload.access_key",
			"upload_access_key",
		),
		UploadSecretKey: getString(v,
			"upload.secret_key",
			"upload_secret_key",
		),
		UploadRegion: getString(v, "upload.region", "upload_region"),
		UploadPrefix: getString(v, "upload.prefix", "upload_prefix"),
		UploadUseSSL: getBool(v, "upload.use_ssl", "upload_use_ssl"),
	}
	return cfg, nil
}

// getString 返回首个非空配置值（按 keys 顺序）。
func getString(v *viper.Viper, keys ...string) string {
	for _, key := range keys {
		if val := strings.TrimSpace(v.GetString(key)); val != "" {
			return val
		}
	}
	return ""
}

// getBool 返回首个已设置的布尔配置值。
func getBool(v *viper.Viper, keys ...string) bool {
	for _, key := range keys {
		if v.IsSet(key) {
			return v.GetBool(key)
		}
	}
	return false
}

// getInt 返回首个已设置的整型配置值。
func getInt(v *viper.Viper, keys ...string) int {
	for _, key := range keys {
		if v.IsSet(key) {
			return v.GetInt(key)
		}
	}
	return 0
}
