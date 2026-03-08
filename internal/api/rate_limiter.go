// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces
// output: REST API responses and status codes for task/cluster operations
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// RateLimiterConfig 定义速率限制配置。
type RateLimiterConfig struct {
	Enabled          bool
	RequestsPerSecond float64 // 每秒请求数
	Burst            int     // 突发容量
}

// IPRateLimiter 实现基于 IP 的速率限制器。
type IPRateLimiter struct {
	mu       sync.RWMutex
	limiter  map[string]*rate.Limiter
	config   RateLimiterConfig
}

// NewIPRateLimiter 创建基于 IP 的速率限制器。
func NewIPRateLimiter(cfg RateLimiterConfig) *IPRateLimiter {
	return &IPRateLimiter{
		limiter: make(map[string]*rate.Limiter),
		config:  cfg,
	}
}

// getLimiter 获取或创建指定 IP 的限流器。
func (rl *IPRateLimiter) getLimiter(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.limiter[ip]
	if !exists {
		limiter = rate.NewLimiter(rate.Limit(rl.config.RequestsPerSecond), rl.config.Burst)
		rl.limiter[ip] = limiter
	}
	return limiter
}

// Allow 检查指定 IP 是否允许请求。
func (rl *IPRateLimiter) Allow(ip string) bool {
	if !rl.config.Enabled {
		return true
	}
	return rl.getLimiter(ip).Allow()
}

// RateLimitMiddleware 返回速率限制中间件（Gin 版本）。
func RateLimitMiddleware(rl *IPRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rl == nil || !rl.config.Enabled {
			c.Next()
			return
		}

		ip := extractClientIP(c.Request)
		if !rl.Allow(ip) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded",
				"code":  429,
			})
			return
		}

		c.Next()
	}
}

// extractClientIP 从请求中提取客户端 IP。
func extractClientIP(r *http.Request) string {
	// 优先检查 X-Forwarded-For 头。
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For 可能包含多个 IP，取第一个。
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}

	// 检查 X-Real-IP 头。
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// 回退到 RemoteAddr。
	ip := r.RemoteAddr
	for i := 0; i < len(ip); i++ {
		if ip[i] == ':' {
			return ip[:i]
		}
	}
	return ip
}
