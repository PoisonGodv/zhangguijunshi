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
	OK                = Err{0, "ok", http.StatusOK}
	ErrParam          = Err{40001, "参数错误", http.StatusBadRequest}
	ErrTokenInvalid   = Err{40101, "登录已失效，请重新登录", http.StatusUnauthorized}
	ErrForbidden      = Err{40301, "无权操作该资源", http.StatusForbidden}
	ErrTenantCross    = Err{40302, "跨租户访问被拒", http.StatusForbidden}
	ErrIdempotent     = Err{40901, "重复请求已处理", http.StatusConflict}
	ErrRateLimit      = Err{42901, "操作过于频繁，请稍后再试", http.StatusTooManyRequests}
	ErrAccountLock    = Err{42301, "账号已锁定，15 分钟后重试", http.StatusLocked}
	ErrMarginGuard    = Err{60001, "折扣低于毛利护栏，已拦截", http.StatusOK}
	ErrFreqCircuit    = Err{60002, "群发频次已达上限", http.StatusOK}
	ErrBalanceOver    = Err{60003, "储值余额超过单店上限", http.StatusOK}
	ErrBalanceShort   = Err{60004, "储值余额不足", http.StatusOK}
	ErrCouponQuota    = Err{60005, "券已发完", http.StatusOK}
	ErrCouponInactive = Err{60006, "券未上线或不在有效期", http.StatusOK}
	ErrCouponExpired  = Err{60007, "券已过期", http.StatusOK}
	ErrCouponUsed     = Err{60008, "券已核销", http.StatusOK}
	// M2-S2 reach / groupbuy / crm business rejections (CONTRACT §11). HTTP 200 +
	// 6xxx code so the client can branch on `code` (e.g. distinguish "blocked by
	// frequency circuit" from "blocked by unsubscribe").
	ErrContentFilter   = Err{60009, "内容命中敏感词，已拦截", http.StatusOK}
	ErrSubscribeQuota  = Err{60010, "订阅消息授权次数已用尽", http.StatusOK}
	ErrUnsubscribed    = Err{60011, "会员已退订该通道", http.StatusOK}
	ErrTemplateUnknown = Err{60012, "触达模板不存在", http.StatusOK}
	ErrGroupbuyFull    = Err{60013, "拼团已满", http.StatusOK}
	ErrGroupbuyClosed  = Err{60014, "拼团已结束或不可参团", http.StatusOK}
	ErrAlreadyJoined   = Err{60015, "该会员已参团", http.StatusOK}
	ErrSOPTerminal     = Err{60016, "社群SOP已到终态", http.StatusOK}
	// M3-S1 AI engine / strategy (CONTRACT §12.7). HTTP 200 + 6xxx for business
	// rejections (blocked / degrade is NOT failure); 40404 for not-found.
	ErrStrategyNotFound    = Err{40404, "策略不存在", http.StatusNotFound}
	ErrMarginGuardExec     = Err{60017, "执行时毛利护栏拦截", http.StatusOK}
	ErrBudgetDegrade       = Err{60018, "AI预算熔断，已降级为规则建议", http.StatusOK}
	ErrLLMDegrade          = Err{60019, "AI增强失败，已降级为规则建议", http.StatusOK}
	ErrColdStart           = Err{60020, "数据不足，已使用冷启动剧本", http.StatusOK}
	ErrActionBlocked       = Err{60021, "动作被合规拦截，未执行", http.StatusOK}
	ErrActionNotExecutable = Err{60022, "该动作暂不可一键执行", http.StatusOK}
	ErrInternal            = Err{50001, "服务器内部错误", http.StatusInternalServerError}
	ErrUpstream            = Err{50201, "上游服务异常", http.StatusBadGateway}
)
