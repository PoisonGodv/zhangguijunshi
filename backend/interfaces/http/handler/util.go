package handler

// Shared helpers for handlers. currentClaims reads the *jwt.Claims the Auth
// middleware stores in the gin context; extractBearer duplicates the tiny
// bearer parser so the handler package need not import the middleware package
// (keeping the dependency direction one-way: router → handler/middleware).

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// ginClaimsKey is the gin-context key the Auth middleware stores claims under.
// It is duplicated (as a literal) in middleware/auth.go on purpose to avoid a
// cross-package coupling for a single string constant.
const ginClaimsKey = "claims"

func currentClaims(c *gin.Context) *jwt.Claims {
	v, ok := c.Get(ginClaimsKey)
	if !ok {
		return nil
	}
	cl, ok := v.(*jwt.Claims)
	if !ok {
		return nil
	}
	return cl
}

// extractBearer pulls "<token>" out of "Authorization: Bearer <token>".
func extractBearer(header string) string {
	header = strings.TrimSpace(header)
	if header == "" || !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return ""
	}
	return strings.TrimSpace(header[len("bearer "):])
}
