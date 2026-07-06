// Package httpx holds the unified HTTP response envelope (tech plan §6.5.1).
// It is a leaf package (depends only on gin + errcode + traceid) so both the
// middleware and the router can import it without forming a cycle.
package httpx

import (
	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/traceid"
)

// Envelope is the single response shape for every endpoint, success or fail.
type Envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
	TraceID string `json:"trace_id,omitempty"`
}

// OK writes a success envelope (code 0) carrying data.
func OK(c *gin.Context, data any) {
	c.JSON(errcode.OK.HTTP, Envelope{
		Code:    errcode.OK.Code,
		Message: errcode.OK.Message,
		Data:    data,
		TraceID: traceid.TraceID(c.Request.Context()),
	})
}

// Fail writes a failure envelope for the given sentinel error.
func Fail(c *gin.Context, e errcode.Err) {
	c.JSON(e.HTTP, Envelope{
		Code:    e.Code,
		Message: e.Message,
		Data:    nil,
		TraceID: traceid.TraceID(c.Request.Context()),
	})
}
