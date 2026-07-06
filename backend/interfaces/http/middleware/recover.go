package middleware

import (
	"runtime/debug"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/logger"
	"github.com/zhangguijunshi/backend/pkg/traceid"
)

// Recover catches panics in the handler chain, logs a stack trace, and returns
// a canonical ErrInternal envelope so a panic never crashes the process or
// leaks a raw stack to the client.
func Recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logger.FromCtx(c.Request.Context()).Error("panic recovered",
					zap.Any("panic", r),
					zap.String("stack", string(debug.Stack())),
				)
				if !c.Writer.Written() {
					c.AbortWithStatusJSON(errcode.ErrInternal.HTTP, httpx.Envelope{
						Code:    errcode.ErrInternal.Code,
						Message: errcode.ErrInternal.Message,
						Data:    nil,
						TraceID: traceid.TraceID(c.Request.Context()),
					})
				}
			}
		}()
		c.Next()
	}
}
