package handler

// change_password_handler_test.go exercises the boss/admin ChangePassword
// handlers end-to-end over HTTP (CONTRACT §20.5). A test-local injector stands
// in for the real Auth middleware: it verifies the Bearer token and sets BOTH
// the gin "claims" key AND the multitenancy.Context that ChangePassword reads
// (mirroring what middleware.Auth injects in prod). The handler package does not
// import the middleware package by design, so the injector is duplicated here
// for tests only. doJSON / extractBearer are reused from rbac_handler_test.go.

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// subjectID parses "u_2001" -> 2001 (mirrors middleware.parseSubjectID).
func subjectID(sub string) int64 {
	idx := strings.IndexByte(sub, '_')
	if idx < 0 || idx == len(sub)-1 {
		return 0
	}
	n, _ := strconv.ParseInt(sub[idx+1:], 10, 64)
	return n
}

// injectAuthCtx is the test stand-in for middleware.Auth: verify Bearer, set
// "claims" + multitenancy.Context (TenantID/UserID/Role/Source). nil/invalid
// token -> 401.
func injectAuthCtx(mgr *jwt.Manager) gin.HandlerFunc {
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
		tc := multitenancy.Context{
			TenantID: claims.TenantID,
			StoreID:  claims.StoreID,
			UserID:   subjectID(claims.Subject),
			Role:     claims.Role,
			Source:   claims.Scene,
		}
		ctx := multitenancy.WithContext(c.Request.Context(), tc)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// buildChangePwdRouter wires a minimal boss + admin change-password router for
// the handler tests. Returns the router and manager. The seeded owner is id 2001
// (memory repo's first seed) with password "boss12345".
func buildChangePwdRouter(t *testing.T) (*gin.Engine, *jwt.Manager) {
	t.Helper()
	priv, err := jwt.DevKey("chg-handler")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	repo := auth.NewMemoryAccountRepo()
	svc := auth.New(auth.Config{
		AccessTTL:        time.Minute,
		RefreshTTL:       time.Hour,
		PlatformTenantID: 1,
	}, nil, mgr, store, nil)
	svc.WithAccountRepo(repo)
	svc.WithVersionStore(auth.NewMemoryVersionStore())

	boss := NewBossAuthHandler(svc)
	admin := NewAdminAuthHandler(svc)
	r := gin.New()
	r.POST("/v1/boss/auth/password", injectAuthCtx(mgr), boss.ChangePassword)
	r.POST("/v1/admin/auth/password", injectAuthCtx(mgr), admin.ChangePassword)
	return r, mgr
}

// mintToken signs a boss token for the seeded owner (id 2001, tenant 1).
func mintBossToken(t *testing.T, mgr *jwt.Manager) string {
	t.Helper()
	tok, _, err := mgr.Sign(t.Context(), jwt.IssueInput{
		Subject: "u_2001", Scene: jwt.SceneBoss, TenantID: 1, StoreID: 1, Role: "owner", Version: 0,
	})
	require.NoError(t, err)
	return tok
}

func TestBossChangePasswordHandler_Success(t *testing.T) {
	r, mgr := buildChangePwdRouter(t)
	token := mintBossToken(t, mgr)
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/password", map[string]any{
		"old_password": "boss12345", "new_password": "newboss12345",
	}, token)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, float64(0), env["code"])
	data := env["data"].(map[string]any)
	require.Equal(t, true, data["changed"])
	require.Equal(t, float64(1), data["version"], "version bumped 0 -> 1")
}

func TestBossChangePasswordHandler_WrongOld(t *testing.T) {
	r, mgr := buildChangePwdRouter(t)
	token := mintBossToken(t, mgr)
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/password", map[string]any{
		"old_password": "WRONG", "new_password": "newboss12345",
	}, token)
	require.Equal(t, http.StatusUnauthorized, code)
	require.Equal(t, float64(40102), env["code"], "wrong old collapses to ErrLoginFailed")
}

func TestBossChangePasswordHandler_BadParam(t *testing.T) {
	r, mgr := buildChangePwdRouter(t)
	token := mintBossToken(t, mgr)
	// Missing new_password -> binding fails -> ErrParam.
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/password", map[string]any{
		"old_password": "boss12345",
	}, token)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, float64(40001), env["code"])
}

func TestBossChangePasswordHandler_NewEqualsOld(t *testing.T) {
	r, mgr := buildChangePwdRouter(t)
	token := mintBossToken(t, mgr)
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/password", map[string]any{
		"old_password": "boss12345", "new_password": "boss12345",
	}, token)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, float64(40001), env["code"])
}

func TestBossChangePasswordHandler_NoTokenUnauthorized(t *testing.T) {
	r, _ := buildChangePwdRouter(t)
	code, _ := doJSON(t, r, http.MethodPost, "/v1/boss/auth/password", map[string]any{
		"old_password": "boss12345", "new_password": "newboss12345",
	}, "")
	require.Equal(t, http.StatusUnauthorized, code)
}

// TestAdminChangePasswordHandler_Success signs an admin token for the seeded
// platform_ops account (username "ops", id 2002) and changes its password. The
// platform ops account has no password originally? It does: "ops12345".
func TestAdminChangePasswordHandler_Success(t *testing.T) {
	r, mgr := buildChangePwdRouter(t)
	// Seeded platform_ops is id 2002 (second memory seed), password "ops12345".
	tok, _, err := mgr.Sign(t.Context(), jwt.IssueInput{
		Subject: "u_2002", Scene: jwt.SceneAdmin, TenantID: 1, StoreID: 0, Role: "platform_ops", Version: 0,
	})
	require.NoError(t, err)
	code, env := doJSON(t, r, http.MethodPost, "/v1/admin/auth/password", map[string]any{
		"old_password": "ops12345", "new_password": "newops12345",
	}, tok)
	require.Equal(t, http.StatusOK, code)
	data := env["data"].(map[string]any)
	require.Equal(t, true, data["changed"])
	require.Equal(t, float64(1), data["version"])
}

// TestBossChangePasswordHandler_NoBodyFields proves a request with NO body fields
// is rejected as ErrParam (both fields required), not a 500.
func TestBossChangePasswordHandler_EmptyBody(t *testing.T) {
	r, mgr := buildChangePwdRouter(t)
	token := mintBossToken(t, mgr)
	code, env := doJSON(t, r, http.MethodPost, "/v1/boss/auth/password", map[string]any{}, token)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, float64(40001), env["code"])
}
