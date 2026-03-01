// Package api provides module-level functionality for api.
// input: HTTP authorization headers and auth config for protected routes
// output: request allow/deny decisions with consistent HTTP auth status codes
// pos: API security boundary enforcing auth policy before business handlers
// note: if this file changes, update this header and module README.md.
package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
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
	auth AuthConfig
}

// ServerOption 用于覆盖 NewServer 默认配置。
type ServerOption func(*serverOptions)

// WithAuth 注入 API 鉴权配置。
func WithAuth(cfg AuthConfig) ServerOption {
	return func(opts *serverOptions) {
		opts.auth = cfg
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

func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch s.auth.Mode {
		case AuthModeAPIKey:
			presented := strings.TrimSpace(c.GetHeader(s.auth.APIKeyHeader))
			if presented == "" {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			if presented != s.auth.APIKey {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			c.Next()
			return
		case AuthModeBearer:
			fallthrough
		default:
			raw := strings.TrimSpace(c.GetHeader("Authorization"))
			if raw == "" {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			const prefix = "Bearer "
			if !strings.HasPrefix(raw, prefix) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(raw, prefix))
			if token == "" {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			if token != s.auth.BearerToken {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			c.Next()
			return
		}
	}
}
