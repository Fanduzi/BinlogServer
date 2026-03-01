// Package config provides module-level functionality for config.
// input: YAML files, environment variables, default config constants
// output: validated runtime configuration structs for downstream modules
// pos: configuration boundary translating external settings into internal options
// note: if this file changes, update this header and module README.md.
package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/viper"
)

var envPlaceholderPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Config 定义服务运行所需的全部配置项。
type Config struct {
	// ListenAddr 是 API/UI 服务监听地址（如 :8080）。
	ListenAddr string
	// DataDir 是本地 binlog 文件落盘目录。
	DataDir string
	// MetaDSN 是元数据 MySQL 连接串；为空则走内存模式。
	MetaDSN string
	// Mode 支持 standalone/cluster。
	Mode string

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

	// Log 是日志输出与轮转配置。
	Log LogConfig

	// API 是 HTTP API 层鉴权相关配置。
	API APIConfig

	// HTTP 是各监听器的超时配置。
	HTTP HTTPConfig
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

// LogConfig 定义日志级别、输出位置与轮转策略。
type LogConfig struct {
	Level          string
	Encoding       string
	File           string
	MaxSizeMB      int
	MaxBackups     int
	MaxAgeDays     int
	Compress       bool
	RotateInterval string
}

// APIConfig 定义 API 鉴权控制配置。
type APIConfig struct {
	Auth APIAuthConfig
}

// APIAuthConfig 定义 /metrics 与 /api/* 鉴权策略。
type APIAuthConfig struct {
	Enabled bool
	Mode    string

	BearerToken  string
	APIKey       string
	APIKeyHeader string

	ProtectAPI     bool
	ProtectMetrics bool
}

// HTTPConfig 定义不同 HTTP server 的超时配置。
type HTTPConfig struct {
	ControlPlane HTTPServerTimeoutConfig
	WorkerHealth HTTPServerTimeoutConfig
}

// HTTPServerTimeoutConfig 定义单个 HTTP server 的超时参数（秒）。
type HTTPServerTimeoutConfig struct {
	ReadHeaderTimeoutSec int
	ReadTimeoutSec       int
	WriteTimeoutSec      int
	IdleTimeoutSec       int
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
	v.SetDefault("log.level", "info")
	v.SetDefault("log.encoding", "json")
	v.SetDefault("log.file", "./logs/binlog-server.log")
	v.SetDefault("log.max_size_mb", 100)
	v.SetDefault("log.max_backups", 7)
	v.SetDefault("log.max_age_days", 30)
	v.SetDefault("log.compress", false)
	v.SetDefault("log.rotate_interval", "24h")
	v.SetDefault("api.auth.enabled", false)
	v.SetDefault("api.auth.mode", "bearer")
	v.SetDefault("api.auth.api_key_header", "X-API-Key")
	v.SetDefault("api.auth.protect_api", false)
	v.SetDefault("api.auth.protect_metrics", false)
	v.SetDefault("http.control_plane.read_header_timeout_sec", 5)
	v.SetDefault("http.control_plane.read_timeout_sec", 30)
	v.SetDefault("http.control_plane.write_timeout_sec", 30)
	v.SetDefault("http.control_plane.idle_timeout_sec", 120)
	v.SetDefault("http.worker_health.read_header_timeout_sec", 3)
	v.SetDefault("http.worker_health.read_timeout_sec", 10)
	v.SetDefault("http.worker_health.write_timeout_sec", 10)
	v.SetDefault("http.worker_health.idle_timeout_sec", 30)

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
		Log: LogConfig{
			Level:          getString(v, "log.level", "log_level"),
			Encoding:       getString(v, "log.encoding", "log_encoding"),
			File:           getString(v, "log.file", "log_file"),
			MaxSizeMB:      getInt(v, "log.max_size_mb", "log_max_size_mb"),
			MaxBackups:     getInt(v, "log.max_backups", "log_max_backups"),
			MaxAgeDays:     getInt(v, "log.max_age_days", "log_max_age_days"),
			Compress:       getBool(v, "log.compress", "log_compress"),
			RotateInterval: getString(v, "log.rotate_interval", "log_rotate_interval"),
		},
		API: APIConfig{
			Auth: APIAuthConfig{
				Enabled:      getBool(v, "api.auth.enabled", "api_auth_enabled"),
				Mode:         strings.ToLower(getString(v, "api.auth.mode", "api_auth_mode")),
				BearerToken:  getString(v, "api.auth.bearer_token", "api_auth_bearer_token"),
				APIKey:       getString(v, "api.auth.api_key", "api_auth_api_key"),
				APIKeyHeader: getString(v, "api.auth.api_key_header", "api_auth_api_key_header"),
				ProtectAPI:   getBool(v, "api.auth.protect_api", "api_auth_protect_api"),
				ProtectMetrics: getBool(v,
					"api.auth.protect_metrics",
					"api_auth_protect_metrics",
				),
			},
		},
		HTTP: HTTPConfig{
			ControlPlane: HTTPServerTimeoutConfig{
				ReadHeaderTimeoutSec: getInt(
					v,
					"http.control_plane.read_header_timeout_sec",
					"http_control_plane_read_header_timeout_sec",
				),
				ReadTimeoutSec: getInt(
					v,
					"http.control_plane.read_timeout_sec",
					"http_control_plane_read_timeout_sec",
				),
				WriteTimeoutSec: getInt(
					v,
					"http.control_plane.write_timeout_sec",
					"http_control_plane_write_timeout_sec",
				),
				IdleTimeoutSec: getInt(
					v,
					"http.control_plane.idle_timeout_sec",
					"http_control_plane_idle_timeout_sec",
				),
			},
			WorkerHealth: HTTPServerTimeoutConfig{
				ReadHeaderTimeoutSec: getInt(
					v,
					"http.worker_health.read_header_timeout_sec",
					"http_worker_health_read_header_timeout_sec",
				),
				ReadTimeoutSec: getInt(
					v,
					"http.worker_health.read_timeout_sec",
					"http_worker_health_read_timeout_sec",
				),
				WriteTimeoutSec: getInt(
					v,
					"http.worker_health.write_timeout_sec",
					"http_worker_health_write_timeout_sec",
				),
				IdleTimeoutSec: getInt(
					v,
					"http.worker_health.idle_timeout_sec",
					"http_worker_health_idle_timeout_sec",
				),
			},
		},
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	warnSensitivePlaintextInConfig(v)
	return cfg, nil
}

// getString 返回首个非空配置值（按 keys 顺序）。
func getString(v *viper.Viper, keys ...string) string {
	for _, key := range keys {
		if val := strings.TrimSpace(v.GetString(key)); val != "" {
			return expandEnvPlaceholders(val)
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

func expandEnvPlaceholders(raw string) string {
	return envPlaceholderPattern.ReplaceAllStringFunc(raw, func(m string) string {
		matches := envPlaceholderPattern.FindStringSubmatch(m)
		if len(matches) != 2 {
			return m
		}
		if val, ok := os.LookupEnv(matches[1]); ok {
			return val
		}
		return m
	})
}

func warnSensitivePlaintextInConfig(v *viper.Viper) {
	sensitiveKeys := []string{
		"meta_dsn",
		"upload.access_key",
		"upload.secret_key",
	}
	for _, key := range sensitiveKeys {
		if !v.InConfig(key) {
			continue
		}
		raw := strings.TrimSpace(v.GetString(key))
		if raw == "" {
			continue
		}
		if envPlaceholderPattern.MatchString(raw) {
			continue
		}
		log.Printf("config warning: key %q appears to contain plaintext sensitive value; prefer ${ENV_VAR} or environment injection", key)
	}
}

func validateConfig(cfg Config) error {
	if err := validateHTTPTimeoutConfig("http.control_plane", cfg.HTTP.ControlPlane); err != nil {
		return err
	}
	if err := validateHTTPTimeoutConfig("http.worker_health", cfg.HTTP.WorkerHealth); err != nil {
		return err
	}
	if err := validateAPIAuthConfig(cfg.API.Auth); err != nil {
		return err
	}
	return nil
}

func validateHTTPTimeoutConfig(scope string, cfg HTTPServerTimeoutConfig) error {
	if cfg.ReadHeaderTimeoutSec <= 0 {
		return fmt.Errorf("%s.read_header_timeout_sec must be > 0", scope)
	}
	if cfg.ReadTimeoutSec <= 0 {
		return fmt.Errorf("%s.read_timeout_sec must be > 0", scope)
	}
	if cfg.WriteTimeoutSec <= 0 {
		return fmt.Errorf("%s.write_timeout_sec must be > 0", scope)
	}
	if cfg.IdleTimeoutSec <= 0 {
		return fmt.Errorf("%s.idle_timeout_sec must be > 0", scope)
	}
	return nil
}

func validateAPIAuthConfig(cfg APIAuthConfig) error {
	if !cfg.Enabled {
		if cfg.ProtectAPI || cfg.ProtectMetrics {
			return errors.New("api.auth.enabled=false cannot protect api or metrics routes")
		}
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch mode {
	case "bearer":
		if strings.TrimSpace(cfg.BearerToken) == "" && (cfg.ProtectAPI || cfg.ProtectMetrics) {
			return errors.New("api.auth.bearer_token is required when protection is enabled")
		}
	case "api_key":
		if strings.TrimSpace(cfg.APIKey) == "" && (cfg.ProtectAPI || cfg.ProtectMetrics) {
			return errors.New("api.auth.api_key is required when protection is enabled")
		}
		if strings.TrimSpace(cfg.APIKeyHeader) == "" {
			return errors.New("api.auth.api_key_header must not be empty")
		}
	default:
		return fmt.Errorf("api.auth.mode must be bearer or api_key, got %q", cfg.Mode)
	}
	return nil
}
