// Package agent implements the LLM Enhance stage (CONTRACT §12.3.5) of the
// five-stage AI pipeline. It takes the Rule Router's Routed items and polishes
// each into an EnhancedItem: a boss-readable copy (≤40 字), a params map clamped
// to the rule's ParamRange and re-validated against the shared margin guardrail,
// and a conservative expected-effect string.
//
// EnhanceOrSkip NEVER returns an error (CONTRACT §12.5.6). Every LLM failure
// mode — budget circuit-breaker, no provider configured, upstream timeout,
// transport error, malformed JSON, or a recovered panic — degrades silently to a
// deterministic template item. The pipeline therefore always has output to feed
// Converge, and the boss never sees a spinner where "今日3件事" should be. The
// degradation reason is written through the shared *degrade pointer (write-once:
// an upstream stage that already claimed cold_start is respected).
package agent

import (
	"context"

	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
)

// Enhanced is the Agent stage's output: one EnhancedItem per RoutedItem, in the
// same order (Converge re-orders later).
type Enhanced struct {
	TenantID int64          `json:"tenant_id"`
	Items    []EnhancedItem `json:"items"`
}

// EnhancedItem pairs a RoutedItem with its polished copy / params / effect. Copy
// is ≤40 字 (template fallback or LLM polish, re-filtered downstream). Params is
// the discount/threshold/off map, already clamped into the rule's ParamRange and
// re-validated against the margin floor. MarginOK is true after the agent's
// second guardrail pass (the rule stage already clamped DiscountMinPermille to
// the floor, and the agent clamps any LLM excursion back to it).
type EnhancedItem struct {
	Routed         rule.RoutedItem `json:"routed"`
	Copy           string          `json:"copy"`            // ≤40 字
	Params         map[string]any  `json:"params"`          // clamped to ParamRange + margin floor
	ExpectedEffect string          `json:"expected_effect"` // "核销35份 挽回损耗¥180"
	MarginOK       bool            `json:"margin_ok"`
}

// Stage is the Agent stage port. EnhanceOrSkip always returns a non-nil
// *Enhanced (at worst a Routed-only template item per input) and never returns
// an error. degrade is the pipeline-wide degradation marker: when the whole
// call never hits the LLM once AND *degrade is still empty, the agent writes
// the failure reason (budget|no_provider|llm) into it (write-once, so an
// upstream cold_start label is preserved).
type Stage interface {
	EnhanceOrSkip(ctx context.Context, tenantID int64, r *rule.Routed, degrade *string) *Enhanced
}
