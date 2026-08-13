package handler

// admin_auth.go exposes the platform-admin (运营后台) auth endpoints (CONTRACT
// §19 / BE-A-01). POST /v1/admin/auth/login takes username + password (the
// tenant is the server-configured PlatformTenantID, never client-supplied).
// GET /v1/admin/me returns identity + permissions.menus derived from the
// verified JWT (rbac.BuildMeResult). Logout reuses the shared logoutRequest.
//
// Login brute-force hardening (MVP security): each Login call is gated by the
// same shared sliding-window limiter as boss (pkg/loginlimit), keyed by client
// IP and by username. The password is NEVER part of the key or any log. A
// successful login clears the account window. The limiter is nil in the minimal
// test fixture; the router injects the shared instance.

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/loginlimit"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// AdminAuthHandler exposes the platform-admin auth endpoints.
type AdminAuthHandler struct {
	svc   *auth.AuthService
	limit *loginlimit.Limiter // nil → no rate limiting (minimal test fixture)
}

// NewAdminAuthHandler constructs an AdminAuthHandler.
func NewAdminAuthHandler(svc *auth.AuthService) *AdminAuthHandler {
	return &AdminAuthHandler{svc: svc}
}

// WithLoginLimiter injects the shared login brute-force limiter (same instance
// as boss). nil leaves login unlimited (backward compatible).
func (h *AdminAuthHandler) WithLoginLimiter(l *loginlimit.Limiter) *AdminAuthHandler {
	h.limit = l
	return h
}

type adminLoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// adminAccountKey builds the rate-limit key for an admin login attempt. It
// carries ONLY the username — never the password.
func adminAccountKey(username string) string {
	return "admin:" + username
}

// Login authenticates a platform_ops account by username + password.
// POST /v1/admin/auth/login
func (h *AdminAuthHandler) Login(c *gin.Context) {
	var req adminLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	account := adminAccountKey(req.Username)
	if h.limit != nil {
		if allowed, retry := h.limit.Take(c.ClientIP(), account); !allowed {
			if retry > 0 {
				c.Header("Retry-After", strconv.Itoa(retry))
			}
			httpx.Fail(c, errcode.ErrLoginRateLimit)
			return
		}
	}
	res, err := h.svc.AdminLogin(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	if h.limit != nil {
		h.limit.RecordSuccess(account)
	}
	httpx.OK(c, res)
}

// Me returns the caller's identity and permissions.menus resolved from the
// verified JWT.
// GET /v1/admin/me
func (h *AdminAuthHandler) Me(c *gin.Context) {
	claims := currentClaims(c)
	if claims == nil {
		httpx.Fail(c, errcode.ErrTokenInvalid)
		return
	}
	httpx.OK(c, auth.BuildMeResult(claims))
}

// Logout blocks the access token's jti and revokes the refresh token.
// POST /v1/admin/auth/logout
func (h *AdminAuthHandler) Logout(c *gin.Context) {
	refresh := c.GetHeader("X-Refresh-Token")
	if refresh == "" {
		var req logoutRequest
		_ = c.ShouldBindJSON(&req)
		refresh = req.RefreshToken
	}
	access := extractBearer(c.GetHeader("Authorization"))
	if err := h.svc.Logout(c.Request.Context(), access, refresh); err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"logged_out": true})
}

// ChangePassword verifies the old password, writes a new hash, and bumps the
// account's token version (invalidating all previously-issued tokens via the
// middleware ver check). The account id + tenant come from the verified JWT
// context, NEVER the body (ARCH-01). Reuses the shared changePasswordRequest
// shape defined in boss_auth.go. On success the client MUST re-login (the
// version bump invalidates the current session by design).
// POST /v1/admin/auth/password
func (h *AdminAuthHandler) ChangePassword(c *gin.Context) {
	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	tc, ok := multitenancy.FromContext(c.Request.Context())
	if !ok || tc.UserID == 0 {
		httpx.Fail(c, errcode.ErrTokenInvalid)
		return
	}
	acc, err := h.svc.ChangePassword(c.Request.Context(), tc.TenantID, tc.UserID, req.OldPassword, req.NewPassword)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"changed": true, "version": acc.Version})
}
