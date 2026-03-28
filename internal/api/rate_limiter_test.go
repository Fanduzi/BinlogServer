// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces
// output: REST API responses and status codes for task/cluster operations
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

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
	// Burst=3, rps=0.001 (effectively no refill during test).
	rl := NewIPRateLimiter(RateLimiterConfig{Enabled: true, RequestsPerSecond: 0.001, Burst: 3})
	ip := "10.0.0.1"

	// First 3 should be allowed (burst).
	for i := 0; i < 3; i++ {
		if !rl.Allow(ip) {
			t.Fatalf("request %d should be allowed (within burst)", i+1)
		}
	}
	// 4th should be denied.
	if rl.Allow(ip) {
		t.Fatal("request beyond burst should be denied")
	}
}

func TestIPRateLimiter_Allow_PerIP(t *testing.T) {
	rl := NewIPRateLimiter(RateLimiterConfig{Enabled: true, RequestsPerSecond: 0.001, Burst: 1})

	// Each IP gets its own bucket — both first requests should be allowed.
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

func TestExtractClientIP_XForwardedFor(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"single IP", "1.2.3.4", "1.2.3.4"},
		{"multiple IPs", "1.2.3.4, 5.6.7.8", "1.2.3.4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Forwarded-For", tc.header)
			got := extractClientIP(req)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractClientIP_XRealIP(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Real-IP", "9.8.7.6")
	if got := extractClientIP(req); got != "9.8.7.6" {
		t.Errorf("got %q, want 9.8.7.6", got)
	}
}

func TestExtractClientIP_RemoteAddr(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.0.1:54321"
	if got := extractClientIP(req); got != "192.168.0.1" {
		t.Errorf("got %q, want 192.168.0.1", got)
	}
}

func TestRateLimitMiddleware_Disabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rl := NewIPRateLimiter(RateLimiterConfig{Enabled: false})

	router := gin.New()
	router.Use(RateLimitMiddleware(rl))
	router.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/ping", nil)
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
	req, _ := http.NewRequest(http.MethodGet, "/ping", nil)
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
	req1, _ := http.NewRequest(http.MethodGet, "/ping", nil)
	req1.RemoteAddr = "10.0.0.1:1234"
	router.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Errorf("first request: expected 200, got %d", w1.Code)
	}

	// Second request same IP — should be rate limited.
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodGet, "/ping", nil)
	req2.RemoteAddr = "10.0.0.1:1235"
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second request: expected 429, got %d", w2.Code)
	}
}
