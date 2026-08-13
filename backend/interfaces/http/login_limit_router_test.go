package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/loginlimit"
)

// clientIPTestRouter builds a router whose /__client_ip echoes c.ClientIP()
// back as the response body. Reused by the trust-policy tests below.
func clientIPTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	r := New(Deps{Cfg: &config.Config{App: config.AppConfig{Env: "test"}}})
	r.GET("/__client_ip", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/plain", []byte(c.ClientIP()))
	})
	return r
}

// TestRouter_ClientIPRejectsSpoofedForwardingHeaders proves a direct public
// caller cannot rotate X-Forwarded-For to evade the IP limiter (or poison a
// victim's budget). Only the compose-bridge nginx proxy is trusted.
func TestRouter_ClientIPRejectsSpoofedForwardingHeaders(t *testing.T) {
	r := clientIPTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/__client_ip", nil)
	req.RemoteAddr = "203.0.113.10:4567" // public caller, not in trusted CIDR
	req.Header.Set("X-Forwarded-For", "198.51.100.77")
	req.Header.Set("X-Real-IP", "198.51.100.88")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "203.0.113.10", w.Body.String())
}

// TestRouter_ClientIPTrustsComposeNginxProxy proves the legitimate compose
// nginx reverse proxy (inside the trusted bridge subnet) is still honored so
// the limiter keys on the real client address behind nginx.
func TestRouter_ClientIPTrustsComposeNginxProxy(t *testing.T) {
	r := clientIPTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/__client_ip", nil)
	req.RemoteAddr = "172.30.0.9:4567" // inside trusted CIDR (admin-web nginx)
	req.Header.Set("X-Forwarded-For", "198.51.100.77")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "198.51.100.77", w.Body.String())
}

func TestRouter_BossAndAdminLoginActuallyReturn429(t *testing.T) {
	priv, err := jwt.DevKey("router-login-limit")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })

	svc := auth.New(auth.Config{
		AccessTTL: time.Minute, RefreshTTL: time.Hour, PlatformTenantID: 1,
	}, nil, mgr, store, nil).WithAccountRepo(auth.NewMemoryAccountRepo())
	limiter := loginlimit.New(
		loginlimit.WithIPMax(100),
		loginlimit.WithAccountMax(1),
	)
	r := New(Deps{
		Cfg:          &config.Config{App: config.AppConfig{Env: "test"}},
		JWT:          mgr,
		Store:        store,
		AuthSvc:      svc,
		LoginLimiter: limiter,
	})

	cases := []struct {
		name string
		path string
		body any
	}{
		{"boss", "/v1/boss/auth/login", map[string]any{"tenant_id": 1, "phone": "13800000001", "password": "WRONG"}},
		{"admin", "/v1/admin/auth/login", map[string]any{"username": "ops", "password": "WRONG"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := doJSON(t, r, http.MethodPost, tc.path, tc.body, "")
			require.Equal(t, http.StatusUnauthorized, first.Code, first.Body.String())

			limited := doJSON(t, r, http.MethodPost, tc.path, tc.body, "")
			require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.Body.String())
			env, _ := envData(t, limited)
			require.Equal(t, 42902, env.Code)
			require.NotEmpty(t, limited.Header().Get("Retry-After"))
		})
	}
}
