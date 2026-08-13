package handler

// login_limit_test.go proves the login brute-force limiter is wired into the
// boss + admin login handlers and surfaces as HTTP 429 + a stable business code
// + a Retry-After header. It builds a dedicated router with a TIGHT limiter
// (injected clock, tiny caps) so the test is deterministic and needs no sleep.
// Existing handler tests (rbac_handler_test.go) build handlers WITHOUT a
// limiter (nil → no limiting) and keep working unchanged.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/loginlimit"
)

// handlerFakeClock advances the limiter window deterministically (no sleep).
type handlerFakeClock struct{ t time.Time }

func newHandlerFakeClock() *handlerFakeClock {
	return &handlerFakeClock{t: time.Unix(1_700_000_000, 0)}
}
func (c *handlerFakeClock) now() time.Time          { return c.t }
func (c *handlerFakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// buildAuthRouterWithLimits wires boss + admin login behind a limiter with the
// given caps. Tests that isolate the ACCOUNT dimension pass a generous ipMax
// (so the IP cap never trips); tests that isolate the IP dimension pass a
// generous accMax. The frozen clock keeps every attempt in one window so the
// caps trip deterministically.
func buildAuthRouterWithLimits(t *testing.T, ipMax, accMax int) (*gin.Engine, *handlerFakeClock) {
	t.Helper()
	priv, err := jwt.DevKey("login-limit")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	repo := auth.NewMemoryAccountRepo()
	svc := auth.New(auth.Config{
		AccessTTL: time.Minute, RefreshTTL: time.Hour, PlatformTenantID: 1,
	}, nil, mgr, store, nil)
	svc.WithAccountRepo(repo)

	clock := newHandlerFakeClock()
	limiter := loginlimit.New(
		loginlimit.WithClock(clock.now),
		loginlimit.WithWindow(time.Minute),
		loginlimit.WithIPMax(ipMax),
		loginlimit.WithAccountMax(accMax),
	)
	boss := NewBossAuthHandler(svc).WithLoginLimiter(limiter)
	admin := NewAdminAuthHandler(svc).WithLoginLimiter(limiter)

	r := gin.New()
	r.POST("/v1/boss/auth/login", boss.Login)
	r.POST("/v1/admin/auth/login", admin.Login)
	return r, clock
}

// postLogin returns the full recorder so the Retry-After header test can inspect
// response headers (which doJSON discards).
func postLogin(t *testing.T, r *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestBossLoginHandler_RateLimit429 trips the ACCOUNT cap (generous IP) and
// asserts 429 + code 42902.
func TestBossLoginHandler_RateLimit429(t *testing.T) {
	r, _ := buildAuthRouterWithLimits(t, 100, 2)
	wrongBody := map[string]any{"tenant_id": 1, "phone": "13800000001", "password": "WRONG"}
	for i := 0; i < 2; i++ {
		code, _ := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login", wrongBody, "")
		require.Equal(t, http.StatusUnauthorized, code, "attempt %d wrong pwd → 401", i+1)
	}
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login", wrongBody, "")
	require.Equal(t, http.StatusTooManyRequests, code, "3rd attempt must be 429")
	require.Equal(t, float64(42902), env["code"], "must be ErrLoginRateLimit code 42902")
}

// TestBossLoginHandler_RateLimit_RetryAfterHeader checks the Retry-After header
// is set to a positive integer (seconds) when the limiter trips.
func TestBossLoginHandler_RateLimit_RetryAfterHeader(t *testing.T) {
	r, _ := buildAuthRouterWithLimits(t, 100, 2)
	wrongBody := map[string]any{"tenant_id": 1, "phone": "13800000001", "password": "WRONG"}
	postLogin(t, r, "/v1/boss/auth/login", wrongBody)
	postLogin(t, r, "/v1/boss/auth/login", wrongBody)

	w := postLogin(t, r, "/v1/boss/auth/login", wrongBody)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	ra := w.Header().Get("Retry-After")
	require.NotEmpty(t, ra, "Retry-After must be set")
	n, err := strconv.Atoi(ra)
	require.NoError(t, err, "Retry-After must be an integer")
	require.Greater(t, n, 0, "Retry-After must be positive")
}

// TestBossLoginHandler_RateLimit_IPCapTripsAcrossAccounts proves the IP cap
// catches an attacker rotating accounts from one IP: two accounts × one attempt
// each exhausts the IP budget (ipMax=2), the third account is rejected even
// though its own account window is empty (generous accMax).
func TestBossLoginHandler_RateLimit_IPCapTripsAcrossAccounts(t *testing.T) {
	r, _ := buildAuthRouterWithLimits(t, 2, 100)
	for _, phone := range []string{"13800000001", "13800000002"} {
		body := map[string]any{"tenant_id": 1, "phone": phone, "password": "WRONG"}
		code, _ := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login", body, "")
		require.Equal(t, http.StatusUnauthorized, code)
	}
	body := map[string]any{"tenant_id": 1, "phone": "13800000003", "password": "WRONG"}
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login", body, "")
	require.Equal(t, http.StatusTooManyRequests, code)
	require.Equal(t, float64(42902), env["code"])
}

// TestBossLoginHandler_SuccessClearsAccountWindow proves a successful login
// clears the ACCOUNT window (RecordSuccess) so the user is not locked out by
// their own old failures once they type the correct password. The IP window is
// intentionally generous here so it does not mask the account-clearing effect
// (RecordSuccess does not clear IP, by design — IP tracks raw volume).
func TestBossLoginHandler_SuccessClearsAccountWindow(t *testing.T) {
	r, _ := buildAuthRouterWithLimits(t, 100, 2)
	doJSON(t, r, http.MethodPost, "/v1/boss/auth/login",
		map[string]any{"tenant_id": 1, "phone": "13800000001", "password": "WRONG"}, "")
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login",
		map[string]any{"tenant_id": 1, "phone": "13800000001", "password": "boss12345"}, "")
	require.Equal(t, http.StatusOK, code, "correct password must log in")
	require.Equal(t, float64(0), env["code"])
	// Account window was cleared; two more wrong attempts must be allowed.
	for i := 0; i < 2; i++ {
		code, _ := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login",
			map[string]any{"tenant_id": 1, "phone": "13800000001", "password": "WRONG"}, "")
		require.Equal(t, http.StatusUnauthorized, code, "post-success attempt %d must be allowed (account window cleared)", i+1)
	}
}

// TestAdminLoginHandler_RateLimit429 proves the admin login surface is also
// gated. Two wrong-password attempts on "ops" → 3rd is 429.
func TestAdminLoginHandler_RateLimit429(t *testing.T) {
	r, _ := buildAuthRouterWithLimits(t, 100, 2)
	wrongBody := map[string]any{"username": "ops", "password": "WRONG"}
	doJSON(t, r, http.MethodPost, "/v1/admin/auth/login", wrongBody, "")
	doJSON(t, r, http.MethodPost, "/v1/admin/auth/login", wrongBody, "")
	code, env := doJSON(t, r, http.MethodPost, "/v1/admin/auth/login", wrongBody, "")
	require.Equal(t, http.StatusTooManyRequests, code)
	require.Equal(t, float64(42902), env["code"])
}

// TestLoginHandler_NoLimiterBackwardCompat proves a handler built WITHOUT
// WithLoginLimiter (nil limiter) imposes NO limit — existing test fixtures and
// any direct handler construction stay unlimited.
func TestLoginHandler_NoLimiterBackwardCompat(t *testing.T) {
	priv, err := jwt.DevKey("nolimit")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	repo := auth.NewMemoryAccountRepo()
	svc := auth.New(auth.Config{AccessTTL: time.Minute, RefreshTTL: time.Hour, PlatformTenantID: 1}, nil, mgr, store, nil)
	svc.WithAccountRepo(repo)
	boss := NewBossAuthHandler(svc) // no WithLoginLimiter → nil → unlimited
	r := gin.New()
	r.POST("/v1/boss/auth/login", boss.Login)
	body := map[string]any{"tenant_id": 1, "phone": "13800000001", "password": "WRONG"}
	for i := 0; i < 20; i++ {
		code, _ := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login", body, "")
		require.Equal(t, http.StatusUnauthorized, code, "nil limiter must never 429 (attempt %d)", i+1)
	}
}
