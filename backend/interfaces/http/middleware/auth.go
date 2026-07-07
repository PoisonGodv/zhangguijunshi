package middleware

// Auth is the JWT authentication middleware (tech plan §6.2.4 / §6.8.4). It
// extracts the Bearer access token, verifies signature + issuer + expiry,
// checks the jti blacklist, then injects a multitenancy.Context derived from
// the verified claims. The tenant is ALWAYS taken from verified claims here —
// never from the request body — which is the HTTP-side enforcement of ARCH-01.
//
// MockTenant (tenant.go) remains as a dev-only alternative for surfaces whose
// login flow has not landed yet (boss/admin in M0-S2); once every surface can
// mint real tokens, MockTenant is removed.

import (
	"context"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/logger"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// tokenVerifier is satisfied by *jwt.Verifier (and thus *jwt.Manager).
type tokenVerifier interface {
	Verify(tokenStr string) (*jwt.Claims, error)
}

// jtiBlocker is satisfied by jwt.SessionStore.
type jtiBlocker interface {
	IsBlocked(ctx context.Context, jti string) (bool, error)
}

// Auth returns a gin middleware that authenticates via ES256 JWT. A missing or
// invalid token short-circuits with errcode.ErrTokenInvalid (40101).
func Auth(verifier tokenVerifier, blocker jtiBlocker) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := extractBearer(c.GetHeader("Authorization"))
		if raw == "" {
			httpx.Fail(c, errcode.ErrTokenInvalid)
			c.Abort()
			return
		}
		claims, err := verifier.Verify(raw)
		if err != nil {
			httpx.Fail(c, errcode.ErrTokenInvalid)
			c.Abort()
			return
		}
		if claims.ID != "" && blocker != nil {
			blocked, err := blocker.IsBlocked(c.Request.Context(), claims.ID)
			if err != nil {
				httpx.Fail(c, errcode.ErrInternal)
				c.Abort()
				return
			}
			if blocked {
				httpx.Fail(c, errcode.ErrTokenInvalid)
				c.Abort()
				return
			}
		}

		tc := multitenancy.Context{
			TenantID: claims.TenantID,
			StoreID:  claims.StoreID,
			UserID:   parseSubjectID(claims.Subject),
			Role:     claims.Role,
			Source:   sourceForScene(claims.Scene),
		}
		ctx := multitenancy.WithContext(c.Request.Context(), tc)
		ctx = logger.WithFields(ctx,
			zap.Int64(logger.FieldTenantID, tc.TenantID),
			zap.Int64(logger.FieldStoreID, tc.StoreID),
			zap.Int64(logger.FieldUserID, tc.UserID),
			zap.String("scene", claims.Scene),
		)
		c.Request = c.Request.WithContext(ctx)
		c.Set("claims", claims)
		c.Next()
	}
}

// extractBearer pulls the token out of an "Authorization: Bearer <token>" header.
func extractBearer(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return ""
	}
	return strings.TrimSpace(header[len("bearer "):])
}

// parseSubjectID extracts the numeric id from a subject like "c_123" / "u_456".
// Unknown prefixes yield 0 (no actor), which is safe for downstream guards.
func parseSubjectID(sub string) int64 {
	idx := strings.IndexByte(sub, '_')
	if idx < 0 || idx == len(sub)-1 {
		return 0
	}
	n, err := strconv.ParseInt(sub[idx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// sourceForScene maps a JWT scene to a multitenancy source.
func sourceForScene(scene string) string {
	switch scene {
	case jwt.SceneConsumer:
		return multitenancy.SourceConsumer
	case jwt.SceneBoss:
		return multitenancy.SourceMerchant
	case jwt.SceneAdmin:
		return multitenancy.SourceAdmin
	default:
		return ""
	}
}
