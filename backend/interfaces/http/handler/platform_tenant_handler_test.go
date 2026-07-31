package handler

// platform_tenant_handler_test.go exercises the PlatformTenantHandler over HTTP,
// mirroring the way the main thread will wire it: a verified-claims injector
// (standing in for middleware.Auth) followed by middleware.RequireRole(
// "platform_ops") followed by the handler. This proves BOTH authorization
// layers: the router guard (RequireRole → 403 for non-platform tokens) and the
// Service's explicit-caller gate (a non-platform role that somehow reached the
// handler is refused as ErrForbidden). doJSON / extractBearer / injectClaims
// are reused from rbac_handler_test.go (same package).

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/interfaces/http/middleware"
	"github.com/zhangguijunshi/backend/internal/tenant"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// buildPlatformRouter wires the platform tenant handler behind a verified-claims
// injector + RequireRole("platform_ops"), exactly as the router will. It returns
// the router and a JWT manager for minting test tokens.
func buildPlatformRouter(t *testing.T) (*gin.Engine, *jwt.Manager) {
	t.Helper()
	priv, err := jwt.DevKey("platform-tenant-handler")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)

	svc := tenant.New(tenant.NewMemoryRepo()) // seeded with 2 demo tenants
	h := NewPlatformTenantHandler(svc)

	r := gin.New()
	platform := r.Group("/v1/platform")
	platform.Use(injectClaims(mgr)) // stand-in for middleware.Auth
	platform.Use(middleware.RequireRole(tenant.PlatformRole))
	platform.GET("/tenants", h.ListTenants)
	return r, mgr
}

// mintToken signs a token with the given scene/role for tests.
func mintToken(t *testing.T, mgr *jwt.Manager, scene, role string, tenantID int64) string {
	t.Helper()
	tok, _, err := mgr.Sign(t.Context(), jwt.IssueInput{
		Subject: "u_2001", Scene: scene, TenantID: tenantID, Role: role,
	})
	require.NoError(t, err)
	return tok
}

func TestPlatformTenantHandler_ListOK(t *testing.T) {
	r, mgr := buildPlatformRouter(t)
	tok := mintToken(t, mgr, jwt.SceneAdmin, tenant.PlatformRole, 1)

	code, env := doJSON(t, r, http.MethodGet, "/v1/platform/tenants", nil, tok)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, float64(0), env["code"])

	data, ok := env["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(2), data["total"], "two seeded demo tenants")
	items, ok := data["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 2)
	first := items[0].(map[string]any)
	require.Equal(t, float64(1), first["id"])
	require.Contains(t, first, "product_count")
	require.Contains(t, first, "member_count")
	require.Contains(t, first, "order_count")
	require.Contains(t, first, "store_count")
}

func TestPlatformTenantHandler_ListPaging(t *testing.T) {
	r, mgr := buildPlatformRouter(t)
	tok := mintToken(t, mgr, jwt.SceneAdmin, tenant.PlatformRole, 1)

	code, env := doJSON(t, r, http.MethodGet, "/v1/platform/tenants?limit=1&offset=0", nil, tok)
	require.Equal(t, http.StatusOK, code)
	data := env["data"].(map[string]any)
	require.Equal(t, float64(2), data["total"], "total independent of the page window")
	require.Len(t, data["items"].([]any), 1)
	require.Equal(t, float64(1), data["count"])
}

// TestPlatformTenantHandler_NonPlatformForbidden proves the router guard: a
// verified boss/owner token (not platform_ops) is rejected by RequireRole with
// 40301 / 403 BEFORE reaching the handler.
func TestPlatformTenantHandler_NonPlatformForbidden(t *testing.T) {
	r, mgr := buildPlatformRouter(t)
	for _, role := range []string{"owner", "manager", "clerk", "cashier"} {
		tok := mintToken(t, mgr, jwt.SceneBoss, role, 1)
		code, env := doJSON(t, r, http.MethodGet, "/v1/platform/tenants", nil, tok)
		require.Equal(t, http.StatusForbidden, code, "role %q must be forbidden", role)
		require.Equal(t, float64(40301), env["code"])
	}
}

// TestPlatformTenantHandler_NoTokenUnauthorized proves an unauthenticated request
// is rejected (401) by the claims injector.
func TestPlatformTenantHandler_NoTokenUnauthorized(t *testing.T) {
	r, _ := buildPlatformRouter(t)
	code, _ := doJSON(t, r, http.MethodGet, "/v1/platform/tenants", nil, "")
	require.Equal(t, http.StatusUnauthorized, code)
}

// TestPlatformTenantHandler_ServiceGate proves the Service's explicit-caller
// gate independently of the router guard: driving the handler directly with a
// non-platform claims entry (bypassing RequireRole) still yields 403 because
// tenant.Service.ListTenants refuses a non-platform PlatformCaller.
func TestPlatformTenantHandler_ServiceGate(t *testing.T) {
	svc := tenant.New(tenant.NewMemoryRepo())
	h := NewPlatformTenantHandler(svc)

	r := gin.New()
	// No RequireRole here — simulate a mis-wired route; only claims injection.
	priv, err := jwt.DevKey("platform-tenant-service-gate")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	r.GET("/tenants", injectClaims(mgr), h.ListTenants)

	tok := mintToken(t, mgr, jwt.SceneBoss, "owner", 1)
	code, env := doJSON(t, r, http.MethodGet, "/tenants", nil, tok)
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, float64(40301), env["code"])
}

// TestPlatformTenantHandler_NoClaimsForbidden proves a request that reaches the
// handler with NO verified claims (route wired without auth) is refused as
// forbidden rather than treated as platform.
func TestPlatformTenantHandler_NoClaimsForbidden(t *testing.T) {
	svc := tenant.New(tenant.NewMemoryRepo())
	h := NewPlatformTenantHandler(svc)
	r := gin.New()
	r.GET("/tenants", h.ListTenants) // no auth at all

	code, env := doJSON(t, r, http.MethodGet, "/tenants", nil, "")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, float64(40301), env["code"])
}

func TestParseSubject(t *testing.T) {
	require.Equal(t, int64(2001), parseSubject("u_2001"))
	require.Equal(t, int64(0), parseSubject(""))
	require.Equal(t, int64(0), parseSubject("nounderscore"))
	require.Equal(t, int64(0), parseSubject("u_"))
	require.Equal(t, int64(0), parseSubject("u_12a"))
}
