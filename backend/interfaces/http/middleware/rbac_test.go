package middleware

// rbac_test.go exercises RequireScene / RequireRole in isolation: a synthetic
// *jwt.Claims is injected under the gin-context key "claims" (mirroring what
// the Auth middleware sets upstream), then the middleware's allow/deny status
// code is asserted. No real JWT signing is needed — these middlewares read the
// claims the Auth middleware already verified (CONTRACT §19 / BE-A-01).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/jwt"
)

func init() { gin.SetMode(gin.TestMode) }

// runMiddleware builds a router that injects claims (when non-nil) then runs
// mw, and returns the recorder for a GET /x request. The terminal handler
// writes 200 so a pass-through is observable.
func runMiddleware(t *testing.T, claims *jwt.Claims, mw gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		if claims != nil {
			c.Set("claims", claims)
		}
		c.Next()
	}, mw, func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)
	return w
}

func TestRequireScene_Pass(t *testing.T) {
	claims := &jwt.Claims{Scene: jwt.SceneBoss, Role: "owner", TenantID: 1}
	w := runMiddleware(t, claims, RequireScene(jwt.SceneBoss))
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRequireScene_WrongSceneRejected(t *testing.T) {
	claims := &jwt.Claims{Scene: jwt.SceneConsumer, Role: "owner", TenantID: 1}
	w := runMiddleware(t, claims, RequireScene(jwt.SceneBoss))
	require.Equal(t, http.StatusUnauthorized, w.Code) // 40101
}

func TestRequireScene_NoClaimsRejected(t *testing.T) {
	w := runMiddleware(t, nil, RequireScene(jwt.SceneBoss))
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireRole_Pass(t *testing.T) {
	claims := &jwt.Claims{Scene: jwt.SceneBoss, Role: "manager", TenantID: 1}
	w := runMiddleware(t, claims, RequireRole("owner", "manager"))
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRequireRole_WrongRoleForbidden(t *testing.T) {
	claims := &jwt.Claims{Scene: jwt.SceneBoss, Role: "clerk", TenantID: 1}
	w := runMiddleware(t, claims, RequireRole("owner", "manager"))
	require.Equal(t, http.StatusForbidden, w.Code) // 40301
}

func TestRequireRole_NoClaimsUnauthorized(t *testing.T) {
	// A missing claims entry (Auth did not run) is unauthenticated, not merely
	// forbidden: ErrTokenInvalid -> 401.
	w := runMiddleware(t, nil, RequireRole("owner"))
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireRole_SingleRolePass(t *testing.T) {
	claims := &jwt.Claims{Scene: jwt.SceneAdmin, Role: "platform_ops", TenantID: 1}
	w := runMiddleware(t, claims, RequireRole("platform_ops"))
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRequireRole_UnknownRoleForbidden(t *testing.T) {
	claims := &jwt.Claims{Scene: jwt.SceneAdmin, Role: "superuser", TenantID: 1}
	w := runMiddleware(t, claims, RequireRole("platform_ops"))
	require.Equal(t, http.StatusForbidden, w.Code)
}
