package middleware

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/logger"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// MockTenant is a DEV-ONLY middleware that resolves the tenant from
// X-Mock-Tenant / X-Mock-Store / X-Mock-User / X-Mock-Role headers so the
// multitenancy guard (ARCH-01) can be exercised end-to-end through HTTP before
// JWT auth (ARCH-02, M0-S2) lands. It MUST NOT be wired in staging/prod —
// there the tenant comes exclusively from verified JWT claims.
//
// When X-Mock-Tenant is absent the chain continues with no tenant, and the
// downstream guard (multitenancy.TenantID) is the authority that rejects.
func MockTenant() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("X-Mock-Tenant")
		if raw == "" {
			c.Next()
			return
		}
		tid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || tid <= 0 {
			httpx.Fail(c, errcode.ErrParam)
			c.Abort()
			return
		}
		tc := multitenancy.Context{
			TenantID: tid,
			StoreID:  headerInt64(c, "X-Mock-Store"),
			UserID:   headerInt64(c, "X-Mock-User"),
			Role:     c.GetHeader("X-Mock-Role"),
			Source:   multitenancy.SourceMerchant,
		}
		ctx := multitenancy.WithContext(c.Request.Context(), tc)
		ctx = logger.WithFields(ctx,
			zap.Int64(logger.FieldTenantID, tc.TenantID),
			zap.Int64(logger.FieldStoreID, tc.StoreID),
			zap.Int64(logger.FieldUserID, tc.UserID),
		)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func headerInt64(c *gin.Context, key string) int64 {
	v, _ := strconv.ParseInt(c.GetHeader(key), 10, 64)
	return v
}
