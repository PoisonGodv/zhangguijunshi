package handler

// boss_auth.go exposes the boss (merchant) phone+password auth endpoints
// (CONTRACT §19 / BE-A-01). POST /v1/boss/auth/login takes the credential
// triple (tenant_id, phone, password) in the body — there is no authenticated
// identity at login, so tenant is necessarily client-supplied here; every
// subsequent boss request takes tenant from the JWT, never the body (ARCH-01).
// Logout reuses the shared logoutRequest shape and the same svc.Logout as the
// consumer surface.

import (
	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// BossAuthHandler exposes the boss auth endpoints.
type BossAuthHandler struct {
	svc *auth.AuthService
}

// NewBossAuthHandler constructs a BossAuthHandler.
func NewBossAuthHandler(svc *auth.AuthService) *BossAuthHandler {
	return &BossAuthHandler{svc: svc}
}

type bossLoginRequest struct {
	TenantID int64  `json:"tenant_id" binding:"required,min=1"`
	Phone    string `json:"phone" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// changePasswordRequest is the body for both boss and admin password-change.
// Only old/new passwords come from the body; the account id and tenant are taken
// from the verified JWT context (ARCH-01) — never trusted from the body.
type changePasswordRequest struct {
	OldPassword string `json:"old_password" binding:"required"`
	NewPassword string `json:"new_password" binding:"required"`
}

// Login authenticates a staff member by phone + password.
// POST /v1/boss/auth/login
func (h *BossAuthHandler) Login(c *gin.Context) {
	var req bossLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	res, err := h.svc.BossLogin(c.Request.Context(), req.TenantID, req.Phone, req.Password)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, res)
}

// Logout blocks the access token's jti and revokes the refresh token. The
// refresh token is accepted from X-Refresh-Token (preferred) or the JSON body.
// POST /v1/boss/auth/logout
func (h *BossAuthHandler) Logout(c *gin.Context) {
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
// context, NEVER the body (ARCH-01). On success the client MUST re-login: the
// version bump invalidates the current session by design (forcing a fresh
// credential check after a password change is the secure default); this handler
// does not re-sign a fresh pair.
// POST /v1/boss/auth/password
func (h *BossAuthHandler) ChangePassword(c *gin.Context) {
	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	tc, ok := multitenancy.FromContext(c.Request.Context())
	if !ok || tc.UserID == 0 {
		// No resolved identity — Auth middleware did not run / token had no sub.
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
