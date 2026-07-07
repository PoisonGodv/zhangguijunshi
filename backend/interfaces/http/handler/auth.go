// Package handler groups the HTTP request handlers for the three BFF surfaces.
// For M0-S2 only the consumer (wxshop) auth flow is implemented; boss/admin
// handlers land with their login milestones.
package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// AuthHandler exposes the wxshop auth endpoints.
type AuthHandler struct {
	svc *auth.AuthService
}

// NewAuthHandler constructs an AuthHandler.
func NewAuthHandler(svc *auth.AuthService) *AuthHandler {
	return &AuthHandler{svc: svc}
}

type wxLoginRequest struct {
	Code     string `json:"code" binding:"required"`
	TenantID int64  `json:"tenant_id" binding:"required,min=1"`
}

// Login runs wx.login → code2session → JWT for the consumer mini-program.
// POST /v1/wxshop/auth/login
func (h *AuthHandler) Login(c *gin.Context) {
	var req wxLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	res, err := h.svc.WxLogin(c.Request.Context(), req.TenantID, req.Code)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, res)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Refresh re-signs an access token from a refresh token. The refresh token is
// accepted from X-Refresh-Token (preferred, per tech plan §6.2.5) or the JSON
// body.
// POST /v1/wxshop/auth/refresh
func (h *AuthHandler) Refresh(c *gin.Context) {
	tok := c.GetHeader("X-Refresh-Token")
	if tok == "" {
		var req refreshRequest
		if err := c.ShouldBindJSON(&req); err == nil {
			tok = req.RefreshToken
		}
	}
	if tok == "" {
		httpx.Fail(c, errcode.ErrTokenInvalid)
		return
	}
	res, err := h.svc.Refresh(c.Request.Context(), tok)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, res)
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Logout blocks the access token's jti and revokes the refresh token.
// POST /v1/wxshop/auth/logout
func (h *AuthHandler) Logout(c *gin.Context) {
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

// Me returns the current caller's identity as resolved from the verified JWT.
// GET /v1/wxshop/auth/me
func (h *AuthHandler) Me(c *gin.Context) {
	claims := currentClaims(c)
	if claims == nil {
		httpx.Fail(c, errcode.ErrTokenInvalid)
		return
	}
	httpx.OK(c, gin.H{
		"tenant_id": claims.TenantID,
		"store_id":  claims.StoreID,
		"subject":   claims.Subject,
		"scene":     claims.Scene,
		"role":      claims.Role,
		// openid is intentionally omitted: it is not needed client-side and
		// reducing PII in responses is defence-in-depth.
	})
}
