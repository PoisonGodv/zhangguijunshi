package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/pkg/logger"
)

// Logging emits one structured access log per request. Fields: method, path,
// status, size, latency. The trace_id / tenant_id fields are added implicitly
// by logger.FromCtx when the requestid / tenant middlewares enriched the ctx.
func Logging() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		logger.FromCtx(c.Request.Context()).Info("http",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Int("size", c.Writer.Size()),
			zap.Duration("latency", time.Since(start)),
		)
	}
}
