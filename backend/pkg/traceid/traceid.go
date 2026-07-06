// Package traceid propagates a per-request trace id through context. The id is
// the `trace_id` log field (tech plan §1.4) and the response envelope's
// trace_id (tech plan §6.5.1). An upstream gateway may propagate its own id via
// the X-Request-Id header; otherwise one is generated.
package traceid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type ctxKey struct{}

// New returns a fresh 32-char hex id (16 random bytes).
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure means the OS PRNG is broken — surface it loudly.
		panic("traceid: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// WithTraceID returns a copy of ctx carrying id.
func WithTraceID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// TraceID extracts the id from ctx, or "" when absent (nil-safe).
func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}
