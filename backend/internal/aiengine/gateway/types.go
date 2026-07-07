// Package gateway adapts OpenAI-compatible LLM providers (Qwen/DeepSeek/Doubao)
// behind one Provider interface, adds per-tenant monthly cost accounting with a
// budget circuit-breaker, and routes requests by task tier (tech plan §7.8).
//
// Design notes (M0-S2 POC):
//   - We speak HTTP directly with net/http rather than pulling
//     sashabaranov/go-openai: /chat/completions is a small, stable surface and a
//     plain client is trivial to point at an httptest fake. The plan names
//     go-openai but permits minimal deps at POC.
//   - Dify is intentionally NOT integrated: tech plan §7.5 defers it ("不在 0→1
//     引入 Dify 作为核心依赖"). When a complex multi-step agent needs visual
//     trial-and-error, Dify becomes a remote experiment and stable flows are
//     re-cast into the Go pipeline. See DifyStub.
//   - Every price in the pricing table MUST be verified against the provider's
//     current rate card before relying on it for billing/circuit-breaking.
package gateway

import (
	"context"
	"errors"
	"time"
)

// Role values for Message.Role.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is a single chat message in OpenAI format.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ResponseFormat requests structured output when non-nil (OpenAI-compatible
// providers support {type:"json_object"}; json_schema support varies — verify).
type ResponseFormat struct {
	Type string `json:"type"` // "json_object" | "text"
}

// Opts tunes a Chat call. Zero values mean "provider/model default".
type Opts struct {
	Model          string
	Temperature    float32
	MaxTokens      int
	ResponseFormat *ResponseFormat
	Timeout        time.Duration // overrides provider default when > 0
}

// Resp is the normalised result of a Chat call.
type Resp struct {
	Content          string
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CostCNY is the estimated RMB cost of this call (from the pricing table).
	CostCNY float64
}

// Provider is the unified LLM interface (tech plan §7.8.1).
type Provider interface {
	// Name is the short provider id (qwen/deepseek/doubao).
	Name() string
	// Chat sends a single completion request (no tool loop; the agent loop in
	// tech plan §7.4 is layered above this).
	Chat(ctx context.Context, msgs []Message, opts Opts) (Resp, error)
}

// TaskTier selects the model class for a request (tech plan §7.8.2).
type TaskTier string

const (
	TierLite TaskTier = "lite" // copywriting / daily recommend / intent classify
	TierV3   TaskTier = "v3"   // tool-calling / audience filter
	TierR1   TaskTier = "r1"   // complex reasoning (low frequency; NEVER for pricing)
)

// Sentinel errors.
var (
	// ErrBudgetExceeded means the tenant has hit the monthly AI cost ceiling;
	// callers MUST fall back to rules+cache (tech plan §7.8.4).
	ErrBudgetExceeded = errors.New("gateway: tenant monthly AI budget exceeded")
	// ErrNoProvider means no provider is configured for the requested name/tier.
	ErrNoProvider = errors.New("gateway: no provider configured")
)

// Errors returned by providers wrap these to signal retryability.
var (
	// ErrUpstreamTimeout is returned when a call times out.
	ErrUpstreamTimeout = errors.New("gateway: upstream timeout")
)
