// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces
// output: REST API responses and status codes for task/cluster operations
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// RateLimiterConfig 定义速率限制配置。
type RateLimiterConfig struct {
	Enabled          bool
	RequestsPerSecond float64 // 每秒请求数
	Burst            int     // 突发容量
}

// maxLimiterEntries 限制 IP 限流器 map 的最大条目数，防止无界增长（OOM / DoS）。
const maxLimiterEntries = 10000

// ipEntry 存储限流器及最近访问时间，用于 LRU 淘汰。
// lastSeen 使用 atomic.Int64（unix nano）以便在快路径（读锁）下无锁更新。
type ipEntry struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // unix nano; updated atomically on every access
}

// IPRateLimiter 实现基于 IP 的速率限制器。
type IPRateLimiter struct {
	mu      sync.RWMutex
	limiter map[string]*ipEntry
	config  RateLimiterConfig
}

// NewIPRateLimiter 创建基于 IP 的速率限制器。
func NewIPRateLimiter(cfg RateLimiterConfig) *IPRateLimiter {
	return &IPRateLimiter{
		limiter: make(map[string]*ipEntry),
		config:  cfg,
	}
}

// getLimiter 获取或创建指定 IP 的限流器。使用双检锁减少写锁竞争。
func (rl *IPRateLimiter) getLimiter(ip string) *rate.Limiter {
	// 快路径：读锁检查。
	rl.mu.RLock()
	entry, exists := rl.limiter[ip]
	rl.mu.RUnlock()
	if exists {
		entry.lastSeen.Store(time.Now().UnixNano())
		return entry.limiter
	}

	// 慢路径：写锁创建。
	rl.mu.Lock()
	defer rl.mu.Unlock()
	// 二次检查，避免重复创建。
	if entry, exists = rl.limiter[ip]; exists {
		entry.lastSeen.Store(time.Now().UnixNano())
		return entry.limiter
	}
	// 防止无界增长：超限时按 lastSeen 淘汰最旧的一半条目（LRU）。
	if len(rl.limiter) >= maxLimiterEntries {
		type kv struct {
			key      string
			lastSeen int64
		}
		kvs := make([]kv, 0, len(rl.limiter))
		for k, v := range rl.limiter {
			kvs = append(kvs, kv{k, v.lastSeen.Load()})
		}
		sort.Slice(kvs, func(i, j int) bool {
			return kvs[i].lastSeen < kvs[j].lastSeen
		})
		for i := 0; i < maxLimiterEntries/2; i++ {
			delete(rl.limiter, kvs[i].key)
		}
	}
	e := &ipEntry{
		limiter: rate.NewLimiter(rate.Limit(rl.config.RequestsPerSecond), rl.config.Burst),
	}
	e.lastSeen.Store(time.Now().UnixNano())
	rl.limiter[ip] = e
	return e.limiter
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
// 仅使用 RemoteAddr（TCP 连接源地址，不可伪造），不读取 X-Forwarded-For / X-Real-IP，
// 避免客户端通过伪造请求头绕过限流。若未来在受信任的反向代理后部署，需重新评估此策略。
func extractClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
