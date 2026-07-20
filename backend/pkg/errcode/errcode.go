// Package errcode defines the unified error-code table for the API gateway
// (tech plan §6.5.3). Each sentinel carries a numeric business code (whose
// leading digits encode the HTTP status), a user-facing message, and the HTTP
// status to emit. Business-rule rejections (e.g. margin guard) use HTTP 200
// with a 6xxx code so the client can branch on `code` for tailored prompts.
package errcode

import "net/http"

// Err is a sentinel error: a stable business code + message + HTTP status.
// It satisfies the error interface so it can flow through normal error paths.
type Err struct {
	Code    int
	Message string
	HTTP    int
}

func (e Err) Error() string { return e.Message }

// Sentinel errors. Format: HTTP×100 + business subcode (see tech plan §6.5.2).
var (
	OK              = Err{0, "ok", http.StatusOK}
	ErrParam        = Err{40001, "参数错误", http.StatusBadRequest}
	ErrTokenInvalid = Err{40101, "登录已失效，请重新登录", http.StatusUnauthorized}
	ErrForbidden    = Err{40301, "无权操作该资源", http.StatusForbidden}
	ErrTenantCross  = Err{40302, "跨租户访问被拒", http.StatusForbidden}
	ErrIdempotent   = Err{40901, "重复请求已处理", http.StatusConflict}
	ErrRateLimit    = Err{42901, "操作过于频繁，请稍后再试", http.StatusTooManyRequests}
	ErrAccountLock  = Err{42301, "账号已锁定，15 分钟后重试", http.StatusLocked}
	ErrMarginGuard  = Err{60001, "折扣低于毛利护栏，已拦截", http.StatusOK}
	ErrFreqCircuit  = Err{60002, "群发频次已达上限", http.StatusOK}
	ErrBalanceOver  = Err{60003, "储值余额超过单店上限", http.StatusOK}
	ErrBalanceShort = Err{60004, "储值余额不足", http.StatusOK}
	ErrInternal     = Err{50001, "服务器内部错误", http.StatusInternalServerError}
	ErrUpstream     = Err{50201, "上游服务异常", http.StatusBadGateway}
)
