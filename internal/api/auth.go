// Package api provides module-level functionality for api.
// input: HTTP authorization headers and auth config for protected routes
// output: request allow/deny decisions (bearer scheme matched case-insensitively), the anonymous GET /auth/config the Consoles read to pick their auth header; every missing, malformed or wrong credential is 401 (bearer mode adds WWW-Authenticate)
// pos: API security boundary enforcing auth policy before business handlers
// note: if this file changes, update this header and module README.md.
package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
)

type AuthMode string

const (
	AuthModeBearer AuthMode = "bearer"
	AuthModeAPIKey AuthMode = "api_key"
)

// AuthConfig 控制 /metrics 与 /api/* 的认证策略。
type AuthConfig struct {
	Enabled bool
	Mode    AuthMode

	BearerToken  string
	APIKey       string
	APIKeyHeader string

	ProtectAPI     bool
	ProtectMetrics bool
}

type serverOptions struct {
	auth       AuthConfig
	tracing    TracingConfig
	rateLimit  RateLimiterConfig
}

// TracingConfig 控制 API HTTP 入站 tracing 行为。
type TracingConfig struct {
	Enabled        bool
	ServiceName    string
	TracerProvider trace.TracerProvider
}

// ServerOption 用于覆盖 NewServer 默认配置。
type ServerOption func(*serverOptions)

// WithAuth 注入 API 鉴权配置。
func WithAuth(cfg AuthConfig) ServerOption {
	return func(opts *serverOptions) {
		opts.auth = cfg
	}
}

// WithTracing 注入 API tracing 配置。
func WithTracing(cfg TracingConfig) ServerOption {
	return func(opts *serverOptions) {
		opts.tracing = cfg
	}
}

// WithRateLimit 注入 API 速率限制配置。
func WithRateLimit(cfg RateLimiterConfig) ServerOption {
	return func(opts *serverOptions) {
		opts.rateLimit = cfg
	}
}

func defaultServerOptions() serverOptions {
	return serverOptions{
		auth: AuthConfig{
			Enabled:        false,
			Mode:           AuthModeBearer,
			APIKeyHeader:   "X-API-Key",
			ProtectAPI:     false,
			ProtectMetrics: false,
		},
		tracing: TracingConfig{
			Enabled:     false,
			ServiceName: "binlog-server",
		},
		rateLimit: RateLimiterConfig{
			Enabled:           true,
			RequestsPerSecond: 100.0,
			Burst:             200,
		},
	}
}

func normalizeAuthConfig(cfg AuthConfig) AuthConfig {
	if cfg.APIKeyHeader == "" {
		cfg.APIKeyHeader = "X-API-Key"
	}
	mode := strings.ToLower(strings.TrimSpace(string(cfg.Mode)))
	switch AuthMode(mode) {
	case AuthModeBearer:
		cfg.Mode = AuthModeBearer
	case AuthModeAPIKey:
		cfg.Mode = AuthModeAPIKey
	default:
		cfg.Mode = AuthModeBearer
	}
	return cfg
}

// authMiddleware rejects every missing, malformed or wrong credential with
// 401. The server has one shared credential and no per-caller permissions,
// so a failed check always means "not authenticated", which is 401 in
// RFC 9110; 403 stays free for a real "authenticated but not allowed" once
// users and roles exist. Both Consoles reopen Settings on 401 only.
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch s.auth.Mode {
		case AuthModeAPIKey:
			presented := strings.TrimSpace(c.GetHeader(s.auth.APIKeyHeader))
			if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(s.auth.APIKey)) != 1 {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			c.Next()
			return
		case AuthModeBearer:
			fallthrough
		default:
			token := bearerToken(c.GetHeader("Authorization"))
			if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.auth.BearerToken)) != 1 {
				c.Header("WWW-Authenticate", `Bearer realm="binlog-server"`)
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			c.Next()
			return
		}
	}
}

// bearerToken returns the credential of an "Authorization: Bearer <token>"
// header, or "" when the header is missing, uses another scheme, or carries
// no token. The scheme is case-insensitive and may be followed by any run
// of spaces or tabs (RFC 9110 section 11.1, RFC 6750 section 2.1).
func bearerToken(header string) string {
	raw := strings.TrimSpace(header)
	i := strings.IndexAny(raw, " \t")
	if i < 0 || !strings.EqualFold(raw[:i], "Bearer") {
		return ""
	}
	return strings.TrimSpace(raw[i:])
}

// authConfigResponse tells a browser Console which header carries the
// credential it collects in Settings. It never includes the credential.
type authConfigResponse struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`
	Header  string `json:"header"`
	Scheme  string `json:"scheme,omitempty"`
}

// handleAuthConfig serves GET /auth/config without auth so a Console can
// learn, before its first API call, whether to send
// "Authorization: Bearer <token>" or "<api_key_header>: <key>".
func (s *Server) handleAuthConfig(c *gin.Context) {
	resp := authConfigResponse{Enabled: s.auth.Enabled, Mode: string(AuthModeBearer), Header: "Authorization", Scheme: "Bearer"}
	if s.auth.Mode == AuthModeAPIKey {
		resp = authConfigResponse{Enabled: s.auth.Enabled, Mode: string(AuthModeAPIKey), Header: s.auth.APIKeyHeader}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, resp)
}
