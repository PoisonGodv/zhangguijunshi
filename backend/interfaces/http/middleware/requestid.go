// Package middleware assembles the gin middleware chain for the API gateway:
// recover → requestid → logging → (otelgin) → group-specific (tenant/auth/...).
package middleware

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/pkg/logger"
	"github.com/zhangguijunshi/backend/pkg/traceid"
)

// RequestID injects a trace id into the request context, the logger context,
// and the X-Request-Id response header. An inbound X-Request-Id is honoured so
// an upstream gateway can propagate its own id (tech plan §6.5.2).
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-Id")
		if rid == "" {
			rid = traceid.New()
		}
		ctx := traceid.WithTraceID(c.Request.Context(), rid)
		ctx = logger.WithFields(ctx, zap.String(logger.FieldTraceID, rid))
		c.Request = c.Request.WithContext(ctx)
		c.Header("X-Request-Id", rid)
		c.Next()
	}
}
