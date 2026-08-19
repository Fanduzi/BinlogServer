// Package config provides module-level functionality for config.
// input: YAML files, environment variables, default config constants
// output: validated runtime configuration structs including data_dir persistence semantics
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
	// MetaDSN 是元数据 MySQL 连接串；为空则把控制面持久化到 data_dir。
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
	// Meta 是内部依赖调用（存储/lease/上传）的超时配置。
	Meta MetaConfig
	// Tracing 是 OpenTelemetry tracing 配置（默认关闭）。
	Tracing TracingConfig
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
	Auth      APIAuthConfig
	RateLimit RateLimitConfig
}

// RateLimitConfig 定义 API 速率限制配置。
type RateLimitConfig struct {
	Enabled           bool
	RequestsPerSecond float64
	Burst             int
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

// MetaConfig 定义内部调用边界配置。
type MetaConfig struct {
	Timeout MetaTimeoutConfig
}

// MetaTimeoutConfig 定义内部依赖调用超时（秒）。
type MetaTimeoutConfig struct {
	ReadSec   int
	WriteSec  int
	LeaseSec  int
	UploadSec int
}

// TracingConfig 定义 OpenTelemetry tracing 配置。
type TracingConfig struct {
	Enabled     bool
	Exporter    string
	Endpoint    string
	SampleRatio float64
	ServiceName string
}

// LoadConfig 按"默认值 < 配置文件 < 环境变量"顺序加载配置。
// 这是 LoadConfigWithEncryption 的包装，不使用加密。
func LoadConfig(path string) (Config, error) {
	return LoadConfigWithEncryption(path, "")
}

// LoadConfigWithEncryption 按"默认值 < 配置文件 < 环境变量"顺序加载配置。
// encryptionKey 用于解密以 enc:aes256: 为前缀的配置值。
func LoadConfigWithEncryption(path string, encryptionKey string) (Config, error) {
	var decryptor *Decryptor
	if encryptionKey != "" {
		var err error
		decryptor, err = NewDecryptor(encryptionKey)
		if err != nil {
			return Config{}, fmt.Errorf("create decryptor: %w", err)
		}
	}

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
	v.SetDefault("api.rate_limit.enabled", true)
	v.SetDefault("api.rate_limit.requests_per_second", 100.0)
	v.SetDefault("api.rate_limit.burst", 200)
	v.SetDefault("http.control_plane.read_header_timeout_sec", 5)
	v.SetDefault("http.control_plane.read_timeout_sec", 30)
	v.SetDefault("http.control_plane.write_timeout_sec", 30)
	v.SetDefault("http.control_plane.idle_timeout_sec", 120)
	v.SetDefault("http.worker_health.read_header_timeout_sec", 3)
	v.SetDefault("http.worker_health.read_timeout_sec", 10)
	v.SetDefault("http.worker_health.write_timeout_sec", 10)
	v.SetDefault("http.worker_health.idle_timeout_sec", 30)
	v.SetDefault("meta.timeout.read_sec", 3)
	v.SetDefault("meta.timeout.write_sec", 5)
	v.SetDefault("meta.timeout.lease_sec", 2)
	v.SetDefault("meta.timeout.upload_sec", 30)
	v.SetDefault("tracing.enabled", false)
	v.SetDefault("tracing.exporter", "disabled")
	v.SetDefault("tracing.endpoint", "http://127.0.0.1:4318/v1/traces")
	v.SetDefault("tracing.sample_ratio", 0.1)
	v.SetDefault("tracing.service_name", "binlog-server")

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
		ListenAddr: getString(v, decryptor, "listen_addr"),
		DataDir:    getString(v, decryptor, "data_dir"),
		MetaDSN:    getString(v, decryptor, "meta_dsn"),
		Mode:       getString(v, decryptor, "mode"),
		Cluster: ClusterConfig{
			Role:                   getString(v, decryptor, "cluster.role"),
			WorkerID:               getString(v, decryptor, "cluster.worker_id"),
			WorkerHealthListenAddr: getString(v, decryptor, "cluster.worker_health_listen_addr"),
			LeaseTTLSec:            getInt(v, "cluster.lease_ttl_sec"),
			LeaseRenewIntervalSec:  getInt(v, "cluster.lease_renew_interval_sec"),
			LeaseGraceSec:          getInt(v, "cluster.lease_grace_sec"),
			FailoverPolicy:         getString(v, decryptor, "cluster.failover_policy"),
		},
		UploadEndpoint: getString(v, decryptor, "upload.endpoint", "upload_endpoint"),
		UploadBucket:   getString(v, decryptor, "upload.bucket", "upload_bucket"),
		UploadAccessKey: getString(v, decryptor,
			"upload.access_key",
			"upload_access_key",
		),
		UploadSecretKey: getString(v, decryptor,
			"upload.secret_key",
			"upload_secret_key",
		),
		UploadRegion: getString(v, decryptor, "upload.region", "upload_region"),
		UploadPrefix: getString(v, decryptor, "upload.prefix", "upload_prefix"),
		UploadUseSSL: getBool(v, "upload.use_ssl", "upload_use_ssl"),
		Log: LogConfig{
			Level:          getString(v, decryptor, "log.level", "log_level"),
			Encoding:       getString(v, decryptor, "log.encoding", "log_encoding"),
			File:           getString(v, decryptor, "log.file", "log_file"),
			MaxSizeMB:      getInt(v, "log.max_size_mb", "log_max_size_mb"),
			MaxBackups:     getInt(v, "log.max_backups", "log_max_backups"),
			MaxAgeDays:     getInt(v, "log.max_age_days", "log_max_age_days"),
			Compress:       getBool(v, "log.compress", "log_compress"),
			RotateInterval: getString(v, decryptor, "log.rotate_interval", "log_rotate_interval"),
		},
		API: APIConfig{
			Auth: APIAuthConfig{
				Enabled:      getBool(v, "api.auth.enabled", "api_auth_enabled"),
				Mode:         strings.ToLower(getString(v, decryptor, "api.auth.mode", "api_auth_mode")),
				BearerToken:  getString(v, decryptor, "api.auth.bearer_token", "api_auth_bearer_token"),
				APIKey:       getString(v, decryptor, "api.auth.api_key", "api_auth_api_key"),
				APIKeyHeader: getString(v, decryptor, "api.auth.api_key_header", "api_auth_api_key_header"),
				ProtectAPI:   getBool(v, "api.auth.protect_api", "api_auth_protect_api"),
				ProtectMetrics: getBool(v,
					"api.auth.protect_metrics",
					"api_auth_protect_metrics",
				),
			},
			RateLimit: RateLimitConfig{
				Enabled:           getBool(v, "api.rate_limit.enabled", "api_rate_limit_enabled"),
				RequestsPerSecond: getFloat64(v, "api.rate_limit.requests_per_second", "api_rate_limit_requests_per_second"),
				Burst:             getInt(v, "api.rate_limit.burst", "api_rate_limit_burst"),
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
		Meta: MetaConfig{
			Timeout: MetaTimeoutConfig{
				ReadSec: getInt(v, "meta.timeout.read_sec", "meta_timeout_read_sec"),
				WriteSec: getInt(
					v,
					"meta.timeout.write_sec",
					"meta_timeout_write_sec",
				),
				LeaseSec: getInt(v, "meta.timeout.lease_sec", "meta_timeout_lease_sec"),
				UploadSec: getInt(
					v,
					"meta.timeout.upload_sec",
					"meta_timeout_upload_sec",
				),
			},
		},
		Tracing: TracingConfig{
			Enabled:     getBool(v, "tracing.enabled", "tracing_enabled"),
			Exporter:    strings.ToLower(getString(v, decryptor, "tracing.exporter", "tracing_exporter")),
			Endpoint:    getString(v, decryptor, "tracing.endpoint", "tracing_endpoint"),
			SampleRatio: getFloat64(v, "tracing.sample_ratio", "tracing_sample_ratio"),
			ServiceName: getString(v, decryptor, "tracing.service_name", "tracing_service_name"),
		},
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	warnSensitivePlaintextInConfig(v)
	return cfg, nil
}

// getString 返回首个非空配置值（按 keys 顺序）。
// 支持 enc:aes256: 前缀的解密。
func getString(v *viper.Viper, decryptor *Decryptor, keys ...string) string {
	for _, key := range keys {
		if val := strings.TrimSpace(v.GetString(key)); val != "" {
			val = expandEnvPlaceholders(val)
			if decryptor != nil {
				decrypted, err := decryptor.DecryptIfEncrypted(val)
				if err != nil {
					log.Printf("config warning: failed to decrypt key %q: %v", key, err)
					continue
				}
				val = decrypted
			}
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

// getFloat64 返回首个已设置的浮点配置值。
func getFloat64(v *viper.Viper, keys ...string) float64 {
	for _, key := range keys {
		if v.IsSet(key) {
			return v.GetFloat64(key)
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
		"api.auth.bearer_token",
		"api.auth.api_key",
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
	if err := validateMetaTimeoutConfig("meta.timeout", cfg.Meta.Timeout); err != nil {
		return err
	}
	if err := validateTracingConfig(cfg.Tracing); err != nil {
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

func validateMetaTimeoutConfig(scope string, cfg MetaTimeoutConfig) error {
	if cfg.ReadSec <= 0 {
		return fmt.Errorf("%s.read_sec must be > 0", scope)
	}
	if cfg.WriteSec <= 0 {
		return fmt.Errorf("%s.write_sec must be > 0", scope)
	}
	if cfg.LeaseSec <= 0 {
		return fmt.Errorf("%s.lease_sec must be > 0", scope)
	}
	if cfg.UploadSec <= 0 {
		return fmt.Errorf("%s.upload_sec must be > 0", scope)
	}
	return nil
}

func validateTracingConfig(cfg TracingConfig) error {
	if strings.TrimSpace(cfg.Exporter) == "" {
		return errors.New("tracing.exporter must not be empty")
	}
	switch cfg.Exporter {
	case "disabled", "otlp-http", "otlp-grpc", "jaeger":
	default:
		return fmt.Errorf("tracing.exporter must be one of disabled|otlp-http|otlp-grpc|jaeger, got %q", cfg.Exporter)
	}
	if cfg.Enabled && cfg.Exporter != "disabled" && cfg.Exporter != "otlp-http" {
		return fmt.Errorf("P5b currently supports tracing.exporter=otlp-http when enabled, got %q", cfg.Exporter)
	}
	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return errors.New("tracing.sample_ratio must be within [0,1]")
	}
	if strings.TrimSpace(cfg.ServiceName) == "" {
		return errors.New("tracing.service_name must not be empty")
	}
	if cfg.Enabled && cfg.Exporter != "disabled" && strings.TrimSpace(cfg.Endpoint) == "" {
		return errors.New("tracing.endpoint must not be empty when tracing is enabled")
	}
	return nil
}
