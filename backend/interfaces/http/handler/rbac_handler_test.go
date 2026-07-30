package handler

// rbac_handler_test.go exercises the boss/admin auth handlers end-to-end over
// HTTP: login (success + failure), /v1/admin/me menu delivery, and logout.
// Handlers are built directly (gin.New), bypassing the router, so only the
// packages in this task are touched. A dev-key JWT manager verifies issued
// tokens; the seeded MemoryAccountRepo provides the demo accounts
// (CONTRACT §19 / BE-A-01).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

func init() { gin.SetMode(gin.TestMode) }

// buildAuthRouter wires an AuthService with a dev-key JWT manager + memory
// store + seeded account repo, and mounts the boss/admin auth handlers on
// distinct paths (mirroring the future router grouping). Returns the router and
// manager so tests can mint claims for /me.
func buildAuthRouter(t *testing.T, platformTenantID int64) (*gin.Engine, *jwt.Manager, jwt.SessionStore) {
	t.Helper()
	priv, err := jwt.DevKey("rbac-handler")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	repo := auth.NewMemoryAccountRepo()
	svc := auth.New(auth.Config{
		AccessTTL:        time.Minute,
		RefreshTTL:       time.Hour,
		PlatformTenantID: platformTenantID,
	}, nil, mgr, store, nil)
	svc.WithAccountRepo(repo)

	boss := NewBossAuthHandler(svc)
	admin := NewAdminAuthHandler(svc)

	r := gin.New()
	r.POST("/v1/boss/auth/login", boss.Login)
	r.POST("/v1/boss/auth/logout", boss.Logout)
	r.POST("/v1/admin/auth/login", admin.Login)
	r.POST("/v1/admin/auth/logout", admin.Logout)
	// /me requires a verified identity; inject claims from a Bearer token the
	// same way the Auth middleware would.
	r.GET("/v1/admin/me", injectClaims(mgr), admin.Me)
	return r, mgr, store
}

// injectClaims is a test-local stand-in for the Auth middleware: it verifies
// the Bearer token and stores *jwt.Claims under "claims".
func injectClaims(mgr *jwt.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := extractBearer(c.GetHeader("Authorization"))
		if tok == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		claims, err := mgr.Verify(tok)
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("claims", claims)
		c.Next()
	}
}

// doJSON posts/puts a JSON body to the router and decodes the httpx envelope.
func doJSON(t *testing.T, r *gin.Engine, method, path string, body any, bearer string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var env map[string]any
	if w.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	}
	return w.Code, env
}

func TestBossLoginHandler_Success(t *testing.T) {
	r, mgr, _ := buildAuthRouter(t, 1)
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login", map[string]any{
		"tenant_id": 1, "phone": "13800000001", "password": "boss12345",
	}, "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, float64(0), env["code"])

	data, ok := env["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Bearer", data["token_type"])
	access, ok := data["access_token"].(string)
	require.True(t, ok && access != "")
	claims, err := mgr.Verify(access)
	require.NoError(t, err)
	require.Equal(t, jwt.SceneBoss, claims.Scene)
	require.Equal(t, "owner", claims.Role)
	require.Equal(t, int64(1), claims.TenantID)
}

func TestBossLoginHandler_WrongPassword(t *testing.T) {
	r, _, _ := buildAuthRouter(t, 1)
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login", map[string]any{
		"tenant_id": 1, "phone": "13800000001", "password": "nope",
	}, "")
	require.Equal(t, http.StatusUnauthorized, code)
	require.Equal(t, float64(40102), env["code"])
}

func TestBossLoginHandler_BadParam(t *testing.T) {
	r, _, _ := buildAuthRouter(t, 1)
	// Missing phone -> binding fails -> ErrParam.
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/login", map[string]any{
		"tenant_id": 1, "password": "boss12345",
	}, "")
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, float64(40001), env["code"])
}

func TestAdminLoginHandler_Success(t *testing.T) {
	r, mgr, _ := buildAuthRouter(t, 1)
	code, env := doJSON(t, r, http.MethodPost, "/v1/admin/auth/login", map[string]any{
		"username": "ops", "password": "ops12345",
	}, "")
	require.Equal(t, http.StatusOK, code)
	data := env["data"].(map[string]any)
	claims, err := mgr.Verify(data["access_token"].(string))
	require.NoError(t, err)
	require.Equal(t, jwt.SceneAdmin, claims.Scene)
	require.Equal(t, "platform_ops", claims.Role)
	require.Equal(t, int64(0), claims.StoreID)
}

func TestAdminMeHandler_DeliversMenus(t *testing.T) {
	r, mgr, _ := buildAuthRouter(t, 1)
	// Mint an admin token for a platform_ops identity.
	token, _, err := mgr.Sign(t.Context(), jwt.IssueInput{
		Subject: "u_2011", Scene: jwt.SceneAdmin, TenantID: 1, StoreID: 0, Role: "platform_ops",
	})
	require.NoError(t, err)

	code, env := doJSON(t, r, http.MethodGet, "/v1/admin/me", nil, token)
	require.Equal(t, http.StatusOK, code)
	data := env["data"].(map[string]any)
	require.Equal(t, "platform_ops", data["role"])
	perms := data["permissions"].(map[string]any)
	menus, ok := perms["menus"].([]any)
	require.True(t, ok, "permissions.menus must be an array")
	require.Len(t, menus, 4)
	require.Contains(t, menus, "platform/merchants")
	require.Contains(t, menus, "platform/tenants")
}

func TestAdminMeHandler_NoTokenUnauthorized(t *testing.T) {
	r, _, _ := buildAuthRouter(t, 1)
	code, _ := doJSON(t, r, http.MethodGet, "/v1/admin/me", nil, "")
	require.Equal(t, http.StatusUnauthorized, code)
}

func TestBossLogoutHandler(t *testing.T) {
	r, _, _ := buildAuthRouter(t, 1)
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/logout", map[string]any{
		"refresh_token": "some-refresh",
	}, "")
	require.Equal(t, http.StatusOK, code)
	data := env["data"].(map[string]any)
	require.Equal(t, true, data["logged_out"])
}

func TestAdminLogoutHandler(t *testing.T) {
	r, _, _ := buildAuthRouter(t, 1)
	code, _ := doJSON(t, r, http.MethodPost, "/v1/admin/auth/logout", map[string]any{
		"refresh_token": "some-refresh",
	}, "")
	require.Equal(t, http.StatusOK, code)
}
