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

	UploadEndpoint  string
	UploadBucket    string
	UploadAccessKey string
	UploadSecretKey string
	UploadRegion    string
	UploadPrefix    string
	UploadUseSSL    bool
}

func LoadConfig(path string) (Config, error) {
	v := viper.New()
	v.SetEnvPrefix("BINLOG_SERVER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// 默认值 < 配置文件 < 环境变量。
	v.SetDefault("listen_addr", ":8080")
	v.SetDefault("data_dir", "./data")
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
