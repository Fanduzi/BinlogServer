// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces
// output: REST API responses and status codes for task/cluster operations
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func TestNewIPRateLimiter(t *testing.T) {
	cfg := RateLimiterConfig{Enabled: true, RequestsPerSecond: 10, Burst: 5}
	rl := NewIPRateLimiter(cfg)
	if rl == nil {
		t.Fatal("expected non-nil IPRateLimiter")
	}
	if rl.config.Burst != 5 {
		t.Errorf("expected Burst=5, got %d", rl.config.Burst)
	}
}

func TestIPRateLimiter_Allow_Disabled(t *testing.T) {
	rl := NewIPRateLimiter(RateLimiterConfig{Enabled: false})
	// Disabled limiter always allows.
	for i := 0; i < 1000; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatal("disabled limiter should always allow")
		}
	}
}

func TestIPRateLimiter_Allow_Burst(t *testing.T) {
	rl := NewIPRateLimiter(RateLimiterConfig{Enabled: true, RequestsPerSecond: 0.001, Burst: 3})
	// Burst of 3: first 3 allowed, 4th denied.
	for i := 0; i < 3; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed within burst", i+1)
		}
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("4th request should be denied (burst exhausted)")
	}
}

func TestIPRateLimiter_Allow_PerIP(t *testing.T) {
	rl := NewIPRateLimiter(RateLimiterConfig{Enabled: true, RequestsPerSecond: 0.001, Burst: 1})
	// Each IP gets its own bucket.
	if !rl.Allow("192.168.1.1") {
		t.Fatal("first request for IP-A should be allowed")
	}
	if !rl.Allow("192.168.1.2") {
		t.Fatal("first request for IP-B should be allowed")
	}
	// Second requests for each IP should be denied.
	if rl.Allow("192.168.1.1") {
		t.Fatal("second request for IP-A should be denied")
	}
	if rl.Allow("192.168.1.2") {
		t.Fatal("second request for IP-B should be denied")
	}
}

func TestIPRateLimiter_Concurrent(t *testing.T) {
	// MEDIUM-3: verify getLimiter double-checked lock is race-free.
	rl := NewIPRateLimiter(RateLimiterConfig{Enabled: true, RequestsPerSecond: 1000, Burst: 1000})
	const goroutines = 50
	const ipsPerGoroutine = 20
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ipsPerGoroutine; i++ {
				ip := fmt.Sprintf("10.%d.%d.1", g, i)
				rl.Allow(ip)
				rl.Allow(ip) // second call hits fast path
			}
		}()
	}
	wg.Wait()
}

func TestExtractClientIP_RemoteAddr(t *testing.T) {
	req := newTestRequest(t, http.MethodGet, "/")
	req.RemoteAddr = "192.168.0.1:54321"
	if got := extractClientIP(req); got != "192.168.0.1" {
		t.Errorf("got %q, want 192.168.0.1", got)
	}
}

func TestExtractClientIP_RemoteAddr_IPv6(t *testing.T) {
	req := newTestRequest(t, http.MethodGet, "/")
	req.RemoteAddr = "[::1]:54321"
	if got := extractClientIP(req); got != "::1" {
		t.Errorf("got %q, want ::1", got)
	}
}

// TestExtractClientIP_IgnoresForwardedHeaders verifies that X-Forwarded-For and
// X-Real-IP are NOT trusted (no reverse proxy in front of this service).
func TestExtractClientIP_IgnoresForwardedHeaders(t *testing.T) {
	req := newTestRequest(t, http.MethodGet, "/")
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")
	// Should use RemoteAddr, not the spoofed headers.
	if got := extractClientIP(req); got != "10.0.0.1" {
		t.Errorf("got %q, want 10.0.0.1 (headers must be ignored)", got)
	}
}

func TestRateLimitMiddleware_Disabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rl := NewIPRateLimiter(RateLimiterConfig{Enabled: false})

	router := gin.New()
	router.Use(RateLimitMiddleware(rl))
	router.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := newTestRequest(t, http.MethodGet, "/ping")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRateLimitMiddleware_Nil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RateLimitMiddleware(nil))
	router.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := newTestRequest(t, http.MethodGet, "/ping")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRateLimitMiddleware_Exceeded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rl := NewIPRateLimiter(RateLimiterConfig{Enabled: true, RequestsPerSecond: 0.001, Burst: 1})

	router := gin.New()
	router.Use(RateLimitMiddleware(rl))
	router.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	// First request — should pass.
	w1 := httptest.NewRecorder()
	req1 := newTestRequest(t, http.MethodGet, "/ping")
	req1.RemoteAddr = "10.0.0.1:1234"
	router.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Errorf("first request: expected 200, got %d", w1.Code)
	}

	// Second request from same IP — should be rate limited.
	w2 := httptest.NewRecorder()
	req2 := newTestRequest(t, http.MethodGet, "/ping")
	req2.RemoteAddr = "10.0.0.1:1235"
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second request: expected 429, got %d", w2.Code)
	}
}
