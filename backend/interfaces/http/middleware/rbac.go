package middleware

// rbac.go adds scene/role authorization guards (CONTRACT §19 / BE-A-01) that
// run AFTER the Auth middleware has verified the JWT and stored *jwt.Claims
// under the gin-context key "claims". RequireScene narrows by client surface
// (boss/admin/wxshop); RequireRole narrows by RBAC role. Both read claims
// directly — they do NOT import the handler package, keeping the dependency
// direction one-way (router -> middleware/handler).

import (
	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// RequireScene returns a middleware that rejects the request unless the verified
// token's Scene equals scene (e.g. only boss tokens reach /v1/boss/*). A missing
// claims entry (Auth middleware did not run) or a mismatch is ErrTokenInvalid
// (40101 / 401) — a token from the wrong surface is treated as unauthenticated
// rather than merely forbidden.
func RequireScene(scene string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := readClaims(c)
		if !ok || claims.Scene != scene {
			httpx.Fail(c, errcode.ErrTokenInvalid)
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireRole returns a middleware that rejects the request unless the verified
// token's Role is one of roles. A missing claims entry is ErrTokenInvalid (401);
// a present-but-unauthorized identity is ErrForbidden (40301 / 403).
func RequireRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := readClaims(c)
		if !ok {
			httpx.Fail(c, errcode.ErrTokenInvalid)
			c.Abort()
			return
		}
		if !roleAllowed(roles, claims.Role) {
			httpx.Fail(c, errcode.ErrForbidden)
			c.Abort()
			return
		}
		c.Next()
	}
}

// readClaims pulls the *jwt.Claims the Auth middleware stored under "claims". It
// reads the key as a literal (duplicated in handler/util.go and auth.go on
// purpose) so this package need not import handler.
func readClaims(c *gin.Context) (*jwt.Claims, bool) {
	v, exists := c.Get("claims")
	if !exists {
		return nil, false
	}
	cl, ok := v.(*jwt.Claims)
	return cl, ok
}

// roleAllowed reports whether role is in roles. A small, allocation-free
// linear scan — role lists are short (1-3 entries).
func roleAllowed(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}
