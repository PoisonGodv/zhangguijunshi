// Package output implements the Encode stage (CONTRACT §12.3.7): it folds the
// converged Top3 EnhancedItems plus the signal Bundle into a persisted
// strategy.Strategy (via strategy.Service.SaveAdvice) and returns an Advice
// carrying the strategy and the pipeline's degradation marker.
//
// Encode is the only stage that writes through to the strategy repository. It
// assigns ranks 1-3, re-filters each copy through contentfilter (falling back
// to the agent's template when the filter hard-blocks), forces margin_ok=true
// on every item (the agent already clamped to the floor), marshals the signal
// Bundle into Strategy.Signals as a snapshot, and derives Strategy.GeneratedBy
// from the degrade marker (empty → live_llm; cold_start/budget/no_provider/llm
// → rule_only).
package output

import (
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// Advice is the pipeline's final product: the persisted Strategy for today plus
// the degradation marker the boss UI surfaces (empty = live_llm; one of
// budget|llm|no_provider|cold_start|cache_hit otherwise). cache_hit is set by
// the pipeline short-circuit, not by Encode.
type Advice struct {
	Strategy *strategy.Strategy `json:"strategy"`
	Degrade  string             `json:"degrade,omitempty"`
}
