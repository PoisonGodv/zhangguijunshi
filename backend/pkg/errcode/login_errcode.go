package errcode

import "net/http"

// login_errcode.go — login brute-force rate-limit sentinel (MVP security
// hardening). HTTP 429 + stable business code so the client can show a
// login-specific "too many attempts" prompt distinct from a generic 429. The
// handler also sets the standard Retry-After header (seconds).
var (
	// ErrLoginRateLimit: the boss/admin login brute-force limiter tripped
	// (per-IP or per-account sliding window exceeded). HTTP 429 + code 42902.
	ErrLoginRateLimit = Err{42902, "登录尝试过多，请稍后再试", http.StatusTooManyRequests}
)
