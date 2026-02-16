package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	ListenAddr string
	DataDir    string
	MetaDSN    string
	Mode       string

	Cluster ClusterConfig

	UploadEndpoint  string
	UploadBucket    string
	UploadAccessKey string
	UploadSecretKey string
	UploadRegion    string
	UploadPrefix    string
	UploadUseSSL    bool
}

type ClusterConfig struct {
	Role                  string
	WorkerID              string
	LeaseTTLSec           int
	LeaseRenewIntervalSec int
	LeaseGraceSec         int
	FailoverPolicy        string
}

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
		ListenAddr:     getString(v, "listen_addr"),
		DataDir:        getString(v, "data_dir"),
		MetaDSN:        getString(v, "meta_dsn"),
		Mode:           getString(v, "mode"),
		Cluster: ClusterConfig{
			Role:                  getString(v, "cluster.role"),
			WorkerID:              getString(v, "cluster.worker_id"),
			LeaseTTLSec:           getInt(v, "cluster.lease_ttl_sec"),
			LeaseRenewIntervalSec: getInt(v, "cluster.lease_renew_interval_sec"),
			LeaseGraceSec:         getInt(v, "cluster.lease_grace_sec"),
			FailoverPolicy:        getString(v, "cluster.failover_policy"),
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

func getString(v *viper.Viper, keys ...string) string {
	for _, key := range keys {
		if val := strings.TrimSpace(v.GetString(key)); val != "" {
			return val
		}
	}
	return ""
}

func getBool(v *viper.Viper, keys ...string) bool {
	for _, key := range keys {
		if v.IsSet(key) {
			return v.GetBool(key)
		}
	}
	return false
}

func getInt(v *viper.Viper, keys ...string) int {
	for _, key := range keys {
		if v.IsSet(key) {
			return v.GetInt(key)
		}
	}
	return 0
}
