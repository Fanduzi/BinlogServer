// input: auth middleware with bearer and api-key configs
// output: regression test that every rejected credential is 401, never 403
// pos: API auth contract test; both Consoles reopen Settings only on 401
// note: if this file changes, update this header and module README.md.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"binlog_server/internal/tasks"
)

func TestAuthRejectsBadCredentialsWith401(t *testing.T) {
	bearer := NewServer(tasks.NewScheduler(), WithAuth(AuthConfig{
		Enabled: true, Mode: AuthModeBearer, BearerToken: "secret-token",
		ProtectAPI: true, ProtectMetrics: true,
	}))
	apiKey := NewServer(tasks.NewScheduler(), WithAuth(AuthConfig{
		Enabled: true, Mode: AuthModeAPIKey, APIKey: "secret-key",
		ProtectAPI: true, ProtectMetrics: true,
	}))

	cases := []struct {
		name       string
		handler    http.Handler
		path       string
		header     string
		value      string
		want       int
		wantBearer bool
	}{
		{"bearer missing", bearer, "/api/tasks", "", "", http.StatusUnauthorized, true},
		{"bearer wrong token", bearer, "/api/tasks", "Authorization", "Bearer wrong", http.StatusUnauthorized, true},
		{"bearer no prefix", bearer, "/api/tasks", "Authorization", "secret-token", http.StatusUnauthorized, true},
		{"bearer other scheme", bearer, "/api/tasks", "Authorization", "Basic c2VjcmV0", http.StatusUnauthorized, true},
		{"bearer empty token", bearer, "/api/tasks", "Authorization", "Bearer   ", http.StatusUnauthorized, true},
		{"bearer wrong on metrics", bearer, "/metrics", "Authorization", "Bearer wrong", http.StatusUnauthorized, true},
		{"bearer ok", bearer, "/api/tasks", "Authorization", "Bearer secret-token", http.StatusOK, false},
		{"api key missing", apiKey, "/api/tasks", "", "", http.StatusUnauthorized, false},
		{"api key wrong", apiKey, "/api/tasks", "X-API-Key", "wrong", http.StatusUnauthorized, false},
		{"api key ok", apiKey, "/api/tasks", "X-API-Key", "secret-key", http.StatusOK, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}
			resp := httptest.NewRecorder()
			tc.handler.ServeHTTP(resp, req)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d", resp.Code, tc.want)
			}
			got := resp.Header().Get("WWW-Authenticate")
			if tc.wantBearer && got != `Bearer realm="binlog-server"` {
				t.Fatalf("WWW-Authenticate = %q, want bearer challenge", got)
			}
			if !tc.wantBearer && got != "" {
				t.Fatalf("unexpected WWW-Authenticate %q", got)
			}
		})
	}
}
