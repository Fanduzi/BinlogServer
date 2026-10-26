// Package config provides module-level functionality for config.
// input: YAML files, environment variables, default config constants, optional --encryption-key
// output: validated runtime configuration structs that reject unresolved protected-auth secrets, reject an unset ${ENV} in meta_dsn by naming the variable, reject booleans other than true or false, reject non-numeric integers and numbers instead of treating them as 0, warn on plaintext secrets only when the file itself is plaintext, default protect flags to true when auth is enabled and unset, and retain EncryptionKey for meta source-password encryption; health.evaluator (off|shadow|on), health.sweep_interval_sec and health.rehash_bytes_per_sec for the ADR 0009 P2-3 evaluator, and health.unreachable_after_sec, health.unreachable_after_task_sec and health.signal_stale_after_sec for the P2-4 signal kinds, and health.no_progress_after_sec (default 120, not negative) for TASK_NO_PROGRESS (P2-6d), and health.stale_after_sec (default 0 = 5×sweep, not negative) for HEALTH_STALE (P2-6.1, §28), and health.node_gone_after_sec (default 0 = 3×sweep, not negative) for chain delegation (P2-6.2, §30); verify.every_sec, verify.budget_bytes_per_sec, verify.rotation_periods, verify.recent_share_percent (1–100, 0 rejected), verify.pass_deadline_sec, verify.pass_max_bytes, verify.object_recent_sec, verify.drill_dsn, verify.drill_every_sec and verify.drill_check_sql for the phase 3 verifier; meta.tls default disabled and upload.use_ssl default true (ADR 0012)
// note ADR 0011: cluster.side_kill is off or auto, default off
// pos: configuration boundary translating external settings into internal options
// note: if this file changes, update this header and module README.md.
package config

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"binlog_server/internal/assurance"
	"binlog_server/internal/mysqltls"

	"github.com/spf13/viper"
)

var envPlaceholderPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Config 定义服务运行所需的全部配置项。
type Config struct {
	// ListenAddr 是 API/UI 服务监听地址（如 :8080）。
	ListenAddr string
	// DataDir 是本地 binlog 文件落盘目录。
	DataDir string
	// MetaDSN 是元数据 MySQL 连接串；为空则控制面只在内存，进程退出后任务元数据丢失。
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
	// UploadGraceSec (upload.grace_sec, default 900) is how long a sealed
	// segment may stay not UPLOADED before I7_UPLOAD_LATE (P2-5a, #348);
	// UploadGraceTaskSec overrides it per task id.
	UploadGraceSec     int
	UploadGraceTaskSec map[string]int

	// Log 是日志输出与轮转配置。
	Log LogConfig

	// API 是 HTTP API 层鉴权相关配置。
	API APIConfig

	// HTTP 是各监听器的超时配置。
	HTTP HTTPConfig
	// Meta 是内部依赖调用（存储/lease/上传）的超时配置。
	Meta MetaConfig
	// Health is the ADR 0009 phase 2 evaluator (P2-3).
	Health HealthConfig
	// Verify is the ADR 0009 phase 3 verifier.
	Verify VerifyConfig
	// Tracing 是 OpenTelemetry tracing 配置（默认关闭）。
	Tracing TracingConfig
	// Standalone is the no-meta_dsn task record (ADR 0010).
	Standalone StandaloneConfig

	// EncryptionKey 来自 --encryption-key / LoadConfigWithEncryption，不从 YAML 读取。
	// 空值表示不加密 backup_tasks.source_json 中的源库密码。
	EncryptionKey string
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
	// SideKill is cluster.side_kill: off (default) or auto. auto is a direct
	// connection only. Empty means off.
	SideKill string
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

// HealthConfig configures the health evaluator (ADR 0009 phase 2, P2-3).
type HealthConfig struct {
	// Evaluator is off, shadow (compute and log, write nothing) or on.
	Evaluator string
	// SweepIntervalSec is the period of the sweep that marks every task dirty.
	SweepIntervalSec int
	// RehashBytesPerSec is the global byte budget of the sha256 re-checker.
	RehashBytesPerSec int64
	// UnreachableAfterSec: SOURCE_UNREACHABLE opens once a task with
	// desired RUN stays in STARTING/RETRY_BACKOFF this long (P2-4, default 90).
	UnreachableAfterSec int
	// UnreachableAfterTaskSec overrides UnreachableAfterSec per task id
	// (health.unreachable_after_task_sec: {"<task id>": seconds}).
	UnreachableAfterTaskSec map[string]int
	// NoProgressAfterSec: TASK_NO_PROGRESS (CRITICAL) opens once a task
	// with desired RUN has a local run that has not advanced under data_dir
	// (nor been confirmed caught up with the source), or no worker runs it,
	// for this long (P2-6d, design 0009 §24.9, default 120; 0 = default).
	NoProgressAfterSec int
	// StaleAfterSec: HEALTH_STALE opens once a task's task_health row is
	// this old (P2-6.1, design 0009 §28). 0 means 5×sweep. Not negative.
	StaleAfterSec int
	// NodeGoneAfterSec: on non-shared storage, chain.local becomes
	// incomplete once the last chain owner's heartbeat is this old
	// (P2-6.2, design 0009 §30). 0 means 3×sweep. Not negative.
	NodeGoneAfterSec int
	// SignalStaleAfterSec: an open signal row with no failure seen by this
	// process for this long is shown as INFO (P2-4, default 86400).
	SignalStaleAfterSec int
	// UploadLateCriticalAfterSec: UPLOAD_LATE is CRITICAL once a segment
	// has been sealed this long (P2-5.2, default 86400); the _task_sec map
	// overrides it per task id.
	UploadLateCriticalAfterSec     int
	UploadLateCriticalAfterTaskSec map[string]int
	// ObjectListIntervalSec is how long one object listing of a task is
	// reused (default 300); ObjectListLimit caps each prefix (default 10000).
	ObjectListIntervalSec int
	ObjectListLimit       int
}

// VerifyConfig is the phase 3 verifier (design 0009-phase3 §9).
// DrillDSN empty leaves the restore drill off. The worker does not dial it.
type VerifyConfig struct {
	EverySec           int
	BudgetBytesPerSec  int64
	RotationPeriods    int
	RecentSharePercent int
	PassDeadlineSec    int
	PassMaxBytes       int64
	ObjectRecentSec    int
	DrillDSN           string
	DrillEverySec      int
	DrillCheckSQL      string
}

// StandaloneConfig configures the encrypted standalone task record
// (ADR 0010, #385). The key itself is never read from YAML: it comes from
// BINLOG_SERVER_TASK_KEY or the key file named here.
type StandaloneConfig struct {
	// TaskRecords is on (default) or off.
	TaskRecords string
	// TaskKeyFile is a 0600/0400 file holding the 32-byte key (base64 or hex).
	TaskKeyFile string
	// TaskKeyPreviousFile holds the key a record file may still be on during rotation.
	TaskKeyPreviousFile string
}

// MetaConfig 定义内部调用边界配置。
type MetaConfig struct {
	Timeout MetaTimeoutConfig
	// TLS is meta.tls (ADR 0012). Empty mode is plaintext.
	TLS mysqltls.Config
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
	v.SetDefault("cluster.side_kill", "off")
	v.SetDefault("upload.use_ssl", true)
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
	v.SetDefault("standalone.task_records", "on")
	v.SetDefault("health.evaluator", "off")
	v.SetDefault("health.sweep_interval_sec", 60)
	v.SetDefault("health.rehash_bytes_per_sec", 4<<20)
	v.SetDefault("health.unreachable_after_sec", 90)
	v.SetDefault("health.no_progress_after_sec", 120)
	v.SetDefault("health.signal_stale_after_sec", 86400)
	v.SetDefault("health.stale_after_sec", 0)
	v.SetDefault("health.node_gone_after_sec", 0)
	v.SetDefault("health.upload_late_critical_after_sec", 86400)
	v.SetDefault("health.object_list_interval_sec", 300)
	v.SetDefault("health.object_list_limit", 10000)
	v.SetDefault("upload.grace_sec", 900)
	v.SetDefault("verify.every_sec", 86400)
	v.SetDefault("verify.budget_bytes_per_sec", 4<<20)
	v.SetDefault("verify.rotation_periods", 20)
	v.SetDefault("verify.recent_share_percent", 25)
	v.SetDefault("verify.pass_deadline_sec", 0)
	v.SetDefault("verify.pass_max_bytes", 0)
	v.SetDefault("verify.object_recent_sec", 86400)
	v.SetDefault("verify.drill_dsn", "")
	v.SetDefault("verify.drill_every_sec", 604800)
	v.SetDefault("verify.drill_check_sql", "")
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

	var loadErr error
	cfg := Config{
		ListenAddr: getString(v, decryptor, "listen_addr"),
		DataDir:    getString(v, decryptor, "data_dir"),
		MetaDSN:    getString(v, decryptor, "meta_dsn"),
		Mode:       getString(v, decryptor, "mode"),
		Cluster: ClusterConfig{
			Role:                   getString(v, decryptor, "cluster.role"),
			WorkerID:               getString(v, decryptor, "cluster.worker_id"),
			WorkerHealthListenAddr: getString(v, decryptor, "cluster.worker_health_listen_addr"),
			LeaseTTLSec:            getInt(&loadErr, v, "cluster.lease_ttl_sec"),
			LeaseRenewIntervalSec:  getInt(&loadErr, v, "cluster.lease_renew_interval_sec"),
			LeaseGraceSec:          getInt(&loadErr, v, "cluster.lease_grace_sec"),
			FailoverPolicy:         getString(v, decryptor, "cluster.failover_policy"),
			SideKill:               getString(v, decryptor, "cluster.side_kill"),
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
		UploadRegion:       getString(v, decryptor, "upload.region", "upload_region"),
		UploadPrefix:       getString(v, decryptor, "upload.prefix", "upload_prefix"),
		UploadUseSSL:       getBool(&loadErr, v, "upload.use_ssl", "upload_use_ssl"),
		UploadGraceSec:     getInt(&loadErr, v, "upload.grace_sec", "upload_grace_sec"),
		UploadGraceTaskSec: healthTaskOverrides(&loadErr, "upload.grace_task_sec", v.GetStringMap("upload.grace_task_sec")),
		Log: LogConfig{
			Level:          getString(v, decryptor, "log.level", "log_level"),
			Encoding:       getString(v, decryptor, "log.encoding", "log_encoding"),
			File:           getString(v, decryptor, "log.file", "log_file"),
			MaxSizeMB:      getInt(&loadErr, v, "log.max_size_mb", "log_max_size_mb"),
			MaxBackups:     getInt(&loadErr, v, "log.max_backups", "log_max_backups"),
			MaxAgeDays:     getInt(&loadErr, v, "log.max_age_days", "log_max_age_days"),
			Compress:       getBool(&loadErr, v, "log.compress", "log_compress"),
			RotateInterval: getString(v, decryptor, "log.rotate_interval", "log_rotate_interval"),
		},
		API: APIConfig{
			Auth: APIAuthConfig{
				Enabled:      getBool(&loadErr, v, "api.auth.enabled", "api_auth_enabled"),
				Mode:         strings.ToLower(getString(v, decryptor, "api.auth.mode", "api_auth_mode")),
				BearerToken:  getString(v, decryptor, "api.auth.bearer_token", "api_auth_bearer_token"),
				APIKey:       getString(v, decryptor, "api.auth.api_key", "api_auth_api_key"),
				APIKeyHeader: getString(v, decryptor, "api.auth.api_key_header", "api_auth_api_key_header"),
				ProtectAPI:   getBool(&loadErr, v, "api.auth.protect_api", "api_auth_protect_api"),
				ProtectMetrics: getBool(&loadErr, v,
					"api.auth.protect_metrics",
					"api_auth_protect_metrics",
				),
			},
			RateLimit: RateLimitConfig{
				Enabled:           getBool(&loadErr, v, "api.rate_limit.enabled", "api_rate_limit_enabled"),
				RequestsPerSecond: getFloat64(&loadErr, v, "api.rate_limit.requests_per_second", "api_rate_limit_requests_per_second"),
				Burst:             getInt(&loadErr, v, "api.rate_limit.burst", "api_rate_limit_burst"),
			},
		},
		HTTP: HTTPConfig{
			ControlPlane: HTTPServerTimeoutConfig{
				ReadHeaderTimeoutSec: getInt(
					&loadErr,
					v,
					"http.control_plane.read_header_timeout_sec",
					"http_control_plane_read_header_timeout_sec",
				),
				ReadTimeoutSec: getInt(
					&loadErr,
					v,
					"http.control_plane.read_timeout_sec",
					"http_control_plane_read_timeout_sec",
				),
				WriteTimeoutSec: getInt(
					&loadErr,
					v,
					"http.control_plane.write_timeout_sec",
					"http_control_plane_write_timeout_sec",
				),
				IdleTimeoutSec: getInt(
					&loadErr,
					v,
					"http.control_plane.idle_timeout_sec",
					"http_control_plane_idle_timeout_sec",
				),
			},
			WorkerHealth: HTTPServerTimeoutConfig{
				ReadHeaderTimeoutSec: getInt(
					&loadErr,
					v,
					"http.worker_health.read_header_timeout_sec",
					"http_worker_health_read_header_timeout_sec",
				),
				ReadTimeoutSec: getInt(
					&loadErr,
					v,
					"http.worker_health.read_timeout_sec",
					"http_worker_health_read_timeout_sec",
				),
				WriteTimeoutSec: getInt(
					&loadErr,
					v,
					"http.worker_health.write_timeout_sec",
					"http_worker_health_write_timeout_sec",
				),
				IdleTimeoutSec: getInt(
					&loadErr,
					v,
					"http.worker_health.idle_timeout_sec",
					"http_worker_health_idle_timeout_sec",
				),
			},
		},
		Meta: MetaConfig{
			TLS: mysqltls.Config{
				Mode:       getString(v, nil, "meta.tls.mode"),
				CA:         getString(v, nil, "meta.tls.ca"),
				Cert:       getString(v, nil, "meta.tls.cert"),
				Key:        getString(v, nil, "meta.tls.key"),
				ServerName: getString(v, nil, "meta.tls.server_name"),
			},
			Timeout: MetaTimeoutConfig{
				ReadSec: getInt(&loadErr, v, "meta.timeout.read_sec", "meta_timeout_read_sec"),
				WriteSec: getInt(
					&loadErr,
					v,
					"meta.timeout.write_sec",
					"meta_timeout_write_sec",
				),
				LeaseSec: getInt(&loadErr, v, "meta.timeout.lease_sec", "meta_timeout_lease_sec"),
				UploadSec: getInt(
					&loadErr,
					v,
					"meta.timeout.upload_sec",
					"meta_timeout_upload_sec",
				),
			},
		},
		Health: HealthConfig{
			Evaluator:                      strings.ToLower(strings.TrimSpace(getString(v, decryptor, "health.evaluator", "health_evaluator"))),
			SweepIntervalSec:               getInt(&loadErr, v, "health.sweep_interval_sec", "health_sweep_interval_sec"),
			RehashBytesPerSec:              int64(getInt(&loadErr, v, "health.rehash_bytes_per_sec", "health_rehash_bytes_per_sec")),
			UnreachableAfterSec:            getInt(&loadErr, v, "health.unreachable_after_sec", "health_unreachable_after_sec"),
			UnreachableAfterTaskSec:        healthTaskOverrides(&loadErr, "health.unreachable_after_task_sec", v.GetStringMap("health.unreachable_after_task_sec")),
			NoProgressAfterSec:             getInt(&loadErr, v, "health.no_progress_after_sec", "health_no_progress_after_sec"),
			StaleAfterSec:                  getInt(&loadErr, v, "health.stale_after_sec", "health_stale_after_sec"),
			NodeGoneAfterSec:               getInt(&loadErr, v, "health.node_gone_after_sec", "health_node_gone_after_sec"),
			SignalStaleAfterSec:            getInt(&loadErr, v, "health.signal_stale_after_sec", "health_signal_stale_after_sec"),
			UploadLateCriticalAfterSec:     getInt(&loadErr, v, "health.upload_late_critical_after_sec", "health_upload_late_critical_after_sec"),
			UploadLateCriticalAfterTaskSec: healthTaskOverrides(&loadErr, "health.upload_late_critical_after_task_sec", v.GetStringMap("health.upload_late_critical_after_task_sec")),
			ObjectListIntervalSec:          getInt(&loadErr, v, "health.object_list_interval_sec", "health_object_list_interval_sec"),
			ObjectListLimit:                getInt(&loadErr, v, "health.object_list_limit", "health_object_list_limit"),
		},
		Verify: VerifyConfig{
			EverySec:           getInt(&loadErr, v, "verify.every_sec", "verify_every_sec"),
			BudgetBytesPerSec:  int64(getInt(&loadErr, v, "verify.budget_bytes_per_sec", "verify_budget_bytes_per_sec")),
			RotationPeriods:    getInt(&loadErr, v, "verify.rotation_periods", "verify_rotation_periods"),
			RecentSharePercent: getInt(&loadErr, v, "verify.recent_share_percent", "verify_recent_share_percent"),
			PassDeadlineSec:    getInt(&loadErr, v, "verify.pass_deadline_sec", "verify_pass_deadline_sec"),
			PassMaxBytes:       int64(getInt(&loadErr, v, "verify.pass_max_bytes", "verify_pass_max_bytes")),
			ObjectRecentSec:    getInt(&loadErr, v, "verify.object_recent_sec", "verify_object_recent_sec"),
			DrillDSN:           getString(v, decryptor, "verify.drill_dsn", "verify_drill_dsn"),
			DrillEverySec:      getInt(&loadErr, v, "verify.drill_every_sec", "verify_drill_every_sec"),
			DrillCheckSQL:      getString(v, decryptor, "verify.drill_check_sql", "verify_drill_check_sql"),
		},
		Standalone: StandaloneConfig{
			TaskRecords:         strings.ToLower(strings.TrimSpace(v.GetString("standalone.task_records"))),
			TaskKeyFile:         strings.TrimSpace(v.GetString("standalone.task_key_file")),
			TaskKeyPreviousFile: strings.TrimSpace(v.GetString("standalone.task_key_previous_file")),
		},
		Tracing: TracingConfig{
			Enabled:     getBool(&loadErr, v, "tracing.enabled", "tracing_enabled"),
			Exporter:    strings.ToLower(getString(v, decryptor, "tracing.exporter", "tracing_exporter")),
			Endpoint:    getString(v, decryptor, "tracing.endpoint", "tracing_endpoint"),
			SampleRatio: getFloat64(&loadErr, v, "tracing.sample_ratio", "tracing_sample_ratio"),
			ServiceName: getString(v, decryptor, "tracing.service_name", "tracing_service_name"),
		},
		EncryptionKey: encryptionKey,
	}
	if loadErr != nil {
		return Config{}, loadErr
	}
	if err := rejectUnsetMetaDSN(cfg.MetaDSN); err != nil {
		return Config{}, err
	}
	applyEnabledAuthProtectDefaults(v, &cfg.API.Auth)
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	if err := validateUploadGrace(cfg); err != nil {
		return Config{}, err
	}
	if err := validateHealthConfig(cfg.Health); err != nil {
		return Config{}, err
	}
	if err := validateVerifyConfig(cfg.Verify); err != nil {
		return Config{}, err
	}
	if err := validateStandaloneConfig(cfg.Standalone); err != nil {
		return Config{}, err
	}
	if err := validateMetaTLS(cfg); err != nil {
		return Config{}, err
	}
	if err := validateUploadEndpoint(cfg); err != nil {
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

// getBool 返回首个已设置的布尔配置值。只接受 true/false，其他字面量记入 errp。
func getBool(errp *error, v *viper.Viper, keys ...string) bool {
	for _, key := range keys {
		if !v.IsSet(key) {
			continue
		}
		b, err := strictBool(keys[0], v.Get(key))
		if err != nil {
			noteLoadErr(errp, err)
			return false
		}
		return b
	}
	return false
}

func strictBool(key string, raw any) (bool, error) {
	switch n := raw.(type) {
	case bool:
		return n, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return false, invalidBoolean(key, strings.TrimSpace(n))
		}
	default:
		return false, invalidBoolean(key, strings.TrimSpace(fmt.Sprint(raw)))
	}
}

func invalidBoolean(key, text string) error {
	return fmt.Errorf("%s: invalid boolean %q", key, text)
}

// applyEnabledAuthProtectDefaults treats unset protect flags as true when auth is enabled.
// Do not SetDefault(protect_*, true): that would fail enabled=false validation.
func applyEnabledAuthProtectDefaults(v *viper.Viper, auth *APIAuthConfig) {
	if auth == nil || !auth.Enabled {
		return
	}
	if !isBoolConfigured(v, "api.auth.protect_api", "api_auth_protect_api") {
		auth.ProtectAPI = true
	}
	if !isBoolConfigured(v, "api.auth.protect_metrics", "api_auth_protect_metrics") {
		auth.ProtectMetrics = true
	}
}

func isBoolConfigured(v *viper.Viper, keys ...string) bool {
	for _, key := range keys {
		if v.InConfig(key) {
			return true
		}
		envKey := "BINLOG_SERVER_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
		if val, ok := os.LookupEnv(envKey); ok && strings.TrimSpace(val) != "" {
			return true
		}
	}
	return false
}

// getInt 返回首个已设置的整型配置值。非整数字面量记入 errp，不再变成 0。
func getInt(errp *error, v *viper.Viper, keys ...string) int {
	for _, key := range keys {
		if !v.IsSet(key) {
			continue
		}
		n, err := strictInt(keys[0], v.Get(key))
		if err != nil {
			noteLoadErr(errp, err)
			return 0
		}
		return n
	}
	return 0
}

// getFloat64 返回首个已设置的浮点配置值。非数字记入 errp，不再变成 0。
func getFloat64(errp *error, v *viper.Viper, keys ...string) float64 {
	for _, key := range keys {
		if !v.IsSet(key) {
			continue
		}
		n, err := strictFloat(keys[0], v.Get(key))
		if err != nil {
			noteLoadErr(errp, err)
			return 0
		}
		return n
	}
	return 0
}

func noteLoadErr(errp *error, err error) {
	if errp != nil && *errp == nil && err != nil {
		*errp = err
	}
}

func strictInt(key string, raw any) (int, error) {
	switch n := raw.(type) {
	case int:
		return n, nil
	case int8:
		return int(n), nil
	case int16:
		return int(n), nil
	case int32:
		return int(n), nil
	case int64:
		if n > math.MaxInt || n < math.MinInt {
			return 0, invalidInteger(key, strconv.FormatInt(n, 10))
		}
		return int(n), nil
	case uint:
		if uint64(n) > uint64(math.MaxInt) {
			return 0, invalidInteger(key, strconv.FormatUint(uint64(n), 10))
		}
		return int(n), nil
	case uint8:
		return int(n), nil
	case uint16:
		return int(n), nil
	case uint32:
		if uint64(n) > uint64(math.MaxInt) {
			return 0, invalidInteger(key, strconv.FormatUint(uint64(n), 10))
		}
		return int(n), nil
	case uint64:
		if n > uint64(math.MaxInt) {
			return 0, invalidInteger(key, strconv.FormatUint(n, 10))
		}
		return int(n), nil
	case float32:
		return strictInt(key, float64(n))
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n > float64(math.MaxInt) || n < float64(math.MinInt) {
			return 0, invalidInteger(key, strconv.FormatFloat(n, 'f', -1, 64))
		}
		return int(n), nil
	case string:
		text := strings.TrimSpace(n)
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil || parsed > math.MaxInt || parsed < math.MinInt {
			return 0, invalidInteger(key, text)
		}
		return int(parsed), nil
	default:
		return 0, invalidInteger(key, strings.TrimSpace(fmt.Sprint(raw)))
	}
}

func strictFloat(key string, raw any) (float64, error) {
	switch n := raw.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, invalidNumber(key, strconv.FormatFloat(n, 'f', -1, 64))
		}
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case uint:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case string:
		text := strings.TrimSpace(n)
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, invalidNumber(key, text)
		}
		return parsed, nil
	default:
		if n, err := strictInt(key, raw); err == nil {
			return float64(n), nil
		}
		return 0, invalidNumber(key, strings.TrimSpace(fmt.Sprint(raw)))
	}
}

func invalidInteger(key, text string) error {
	return fmt.Errorf("%s: invalid integer %q", key, text)
}

func invalidNumber(key, text string) error {
	return fmt.Errorf("%s: invalid number %q", key, text)
}

// rejectUnsetMetaDSN fails when meta_dsn still contains ${VAR} because VAR is
// unset. An empty string, including a placeholder whose variable is set to "",
// is standalone mode and is not an error.
func rejectUnsetMetaDSN(raw string) error {
	for _, match := range envPlaceholderPattern.FindAllStringSubmatch(raw, -1) {
		if len(match) != 2 {
			continue
		}
		if _, ok := os.LookupEnv(match[1]); ok {
			continue
		}
		return fmt.Errorf("%s is not set; leave meta_dsn empty for standalone mode", match[1])
	}
	return nil
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
		"verify.drill_dsn",
	}
	// AutomaticEnv expands ${ENV} before GetString, so a file token looks like
	// the secret itself. Read the file again with env expansion off.
	rawFile := viper.New()
	if path := v.ConfigFileUsed(); path != "" {
		rawFile.SetConfigFile(path)
		if err := rawFile.ReadInConfig(); err != nil {
			return
		}
	}
	for _, key := range sensitiveKeys {
		if !rawFile.InConfig(key) {
			continue
		}
		raw := strings.TrimSpace(rawFile.GetString(key))
		if raw == "" {
			continue
		}
		if envPlaceholderPattern.MatchString(raw) {
			continue
		}
		log.Printf("config warning: key %q appears to contain plaintext sensitive value; prefer ${ENV_VAR} or environment injection", key)
	}
}

// validateMetaTLS checks meta.tls. Files are read only when this process will dial.
func validateMetaTLS(cfg Config) error {
	normalized, err := mysqltls.Normalize("meta", cfg.Meta.TLS)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.MetaDSN) == "" {
		if !mysqltls.Disabled(normalized) {
			return fmt.Errorf("meta.tls is set but meta_dsn is empty")
		}
		return nil
	}
	if mysqltls.Disabled(normalized) {
		return nil
	}
	_, err = mysqltls.ClientConfig("meta", normalized, "localhost")
	return err
}

// validateUploadEndpoint rejects a scheme. The S3 client wants host[:port],
// and upload.use_ssl (default true) is what selects HTTPS.
func validateUploadEndpoint(cfg Config) error {
	ep := strings.TrimSpace(cfg.UploadEndpoint)
	if ep == "" || !strings.Contains(ep, "://") {
		return nil
	}
	return fmt.Errorf("upload.endpoint must be host[:port] without a URL scheme (got %q); upload.use_ssl defaults to true (HTTPS); set upload.use_ssl: false for a plain HTTP endpoint", ep)
}

func validateConfig(cfg Config) error {
	if err := validateHTTPTimeoutConfig("http.control_plane", cfg.HTTP.ControlPlane); err != nil {
		return err
	}
	if err := validateHTTPTimeoutConfig("http.worker_health", cfg.HTTP.WorkerHealth); err != nil {
		return err
	}
	if err := ValidateAPIAuthConfig(cfg.API.Auth); err != nil {
		return err
	}
	if err := validateMetaTimeoutConfig("meta.timeout", cfg.Meta.Timeout); err != nil {
		return err
	}
	if err := validateTracingConfig(cfg.Tracing); err != nil {
		return err
	}
	if err := validateSideKill(cfg.Cluster.SideKill); err != nil {
		return err
	}
	return nil
}

func validateSideKill(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "off", "auto":
		return nil
	default:
		return fmt.Errorf("cluster.side_kill must be off or auto, got %q", mode)
	}
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

// ValidateAPIAuthConfig validates route protection, auth mode, and required resolved credentials.
func ValidateAPIAuthConfig(cfg APIAuthConfig) error {
	if !cfg.Enabled {
		if cfg.ProtectAPI || cfg.ProtectMetrics {
			return errors.New("api.auth.enabled=false cannot protect api or metrics routes")
		}
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch mode {
	case "bearer":
		if unresolvedSecret(cfg.BearerToken) && (cfg.ProtectAPI || cfg.ProtectMetrics) {
			return errors.New("api.auth.bearer_token is required when protection is enabled")
		}
	case "api_key":
		if unresolvedSecret(cfg.APIKey) && (cfg.ProtectAPI || cfg.ProtectMetrics) {
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

func unresolvedSecret(value string) bool {
	return strings.TrimSpace(value) == "" || envPlaceholderPattern.MatchString(value)
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

// validateStandaloneConfig checks standalone.task_records.
func validateStandaloneConfig(c StandaloneConfig) error {
	switch c.TaskRecords {
	case "", "on", "off":
		return nil
	default:
		return fmt.Errorf("standalone.task_records must be on or off, got %q", c.TaskRecords)
	}
}

// validateVerifyConfig checks the phase 3 verifier keys. recent_share_percent
// rejects 0 here; CheckRecentSharePercent itself treats 0 as the default.
func validateVerifyConfig(v VerifyConfig) error {
	if v.EverySec <= 0 {
		return fmt.Errorf("verify.every_sec must be positive, got %d", v.EverySec)
	}
	if v.BudgetBytesPerSec < 0 {
		return fmt.Errorf("verify.budget_bytes_per_sec must not be negative, got %d", v.BudgetBytesPerSec)
	}
	if v.RotationPeriods < 0 {
		return fmt.Errorf("verify.rotation_periods must not be negative, got %d", v.RotationPeriods)
	}
	if v.RecentSharePercent == 0 || assurance.CheckRecentSharePercent(v.RecentSharePercent) != nil {
		return fmt.Errorf("verify.recent_share_percent must be 1–100, got %d", v.RecentSharePercent)
	}
	if v.PassDeadlineSec < 0 {
		return fmt.Errorf("verify.pass_deadline_sec must not be negative, got %d", v.PassDeadlineSec)
	}
	if v.PassMaxBytes < 0 {
		return fmt.Errorf("verify.pass_max_bytes must not be negative, got %d", v.PassMaxBytes)
	}
	if v.ObjectRecentSec < 0 {
		return fmt.Errorf("verify.object_recent_sec must not be negative, got %d", v.ObjectRecentSec)
	}
	if v.DrillEverySec < 0 {
		return fmt.Errorf("verify.drill_every_sec must not be negative, got %d", v.DrillEverySec)
	}
	return nil
}

// validateHealthConfig checks the P2-3 evaluator keys.
func validateHealthConfig(h HealthConfig) error {
	switch h.Evaluator {
	case "", "off", "shadow", "on":
	default:
		return fmt.Errorf("health.evaluator must be off, shadow or on, got %q", h.Evaluator)
	}
	if h.SweepIntervalSec < 0 || h.RehashBytesPerSec < 0 {
		return fmt.Errorf("health.sweep_interval_sec and health.rehash_bytes_per_sec must not be negative")
	}
	if h.UnreachableAfterSec < 0 || h.SignalStaleAfterSec < 0 {
		return fmt.Errorf("health.unreachable_after_sec and health.signal_stale_after_sec must not be negative")
	}
	if h.NoProgressAfterSec < 0 {
		return fmt.Errorf("health.no_progress_after_sec must not be negative, got %d", h.NoProgressAfterSec)
	}
	if h.StaleAfterSec < 0 {
		return fmt.Errorf("health.stale_after_sec must not be negative, got %d", h.StaleAfterSec)
	}
	if h.NodeGoneAfterSec < 0 {
		return fmt.Errorf("health.node_gone_after_sec must not be negative, got %d", h.NodeGoneAfterSec)
	}
	for id, sec := range h.UnreachableAfterTaskSec {
		if sec <= 0 {
			return fmt.Errorf("health.unreachable_after_task_sec[%s] must be positive, got %d", id, sec)
		}
	}
	if h.UploadLateCriticalAfterSec < 0 || h.ObjectListIntervalSec < 0 || h.ObjectListLimit < 0 {
		return fmt.Errorf("health.upload_late_critical_after_sec, health.object_list_interval_sec and health.object_list_limit must not be negative")
	}
	for id, sec := range h.UploadLateCriticalAfterTaskSec {
		if sec <= 0 {
			return fmt.Errorf("health.upload_late_critical_after_task_sec[%s] must be positive, got %d", id, sec)
		}
	}
	return nil
}

// healthTaskOverrides reads a per-task integer map. A value that is not a
// whole number is an invalid-integer error, not a silent -1.
func healthTaskOverrides(errp *error, key string, raw map[string]any) map[string]int {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]int, len(raw))
	for id, val := range raw {
		n, err := strictInt(key+"["+id+"]", val)
		if err != nil {
			noteLoadErr(errp, err)
			continue
		}
		out[id] = n
	}
	return out
}

// validateUploadGrace checks upload.grace_sec and upload.grace_task_sec (P2-5a).
func validateUploadGrace(cfg Config) error {
	if cfg.UploadGraceSec < 0 {
		return fmt.Errorf("upload.grace_sec must not be negative, got %d", cfg.UploadGraceSec)
	}
	for id, sec := range cfg.UploadGraceTaskSec {
		if sec <= 0 {
			return fmt.Errorf("upload.grace_task_sec[%s] must be positive, got %d", id, sec)
		}
	}
	return nil
}
