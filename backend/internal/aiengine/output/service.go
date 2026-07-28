package output

import (
	"context"
	"encoding/json"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// copyMaxRunes is the hard cap on StrategyItem.Copy (CONTRACT §12.3.7 / schema
// maxLength 40). Counted in runes so Chinese copy is bounded correctly.
const copyMaxRunes = 40

// Encode folds the converged Top3 plus the signal Bundle into a strategy and
// persists it. Ranks are 1..N in the given order; copy is content-filtered and
// rune-truncated, falling back to the agent template when the filter hard-blocks
// or the copy is empty; margin_ok is forced true (the agent already clamped to
// the floor); action_type / reason_type are carried from the EnhancedItem onto
// each StrategyItem; the Bundle is marshalled into Strategy.Signals as the
// "today" snapshot; and GeneratedBy is derived from the degrade marker.
func (s *Service) Encode(ctx context.Context, tenantID, storeID int64, top3 []*agent.EnhancedItem, bundle *signal.Bundle, degrade string) (*Advice, error) {
	now := s.now()
	st := &strategy.Strategy{
		TenantID:         tenantID,
		StoreID:          storeID,
		ForDate:          now.Format("2006-01-02"),
		Signals:          marshalBundle(bundle),
		DataQualityScore: bundleQuality(bundle),
		GeneratedBy:      generatedBy(degrade),
		LLMTier:          llmTier(degrade),
		Status:           strategy.StatusGenerated,
		Items:            make([]strategy.StrategyItem, 0, len(top3)),
	}

	for i, ei := range top3 {
		if ei == nil {
			continue
		}
		st.Items = append(st.Items, strategy.StrategyItem{
			Rank:            i + 1,
			ActionType:      ei.Routed.ActionType,
			ReasonType:      ei.Routed.ReasonType,
			Target:          targetMap(ei.Routed),
			ProductID:       ei.Routed.ProductID,
			Params:          cloneParams(ei.Params),
			Copy:            s.safeCopy(ei.Copy, ei.Routed.ActionType),
			Reason:          ei.Routed.Reason,
			ExpectedEffect:  effectMap(ei.ExpectedEffect),
			MarginOK:        true, // forced: agent clamped to floor; full-reduction actions pass trivially
			ExecutionStatus: strategy.ItemPending,
		})
	}

	saved, err := s.svc.SaveAdvice(ctx, st)
	if err != nil {
		return nil, err
	}
	return &Advice{Strategy: saved, Degrade: degrade}, nil
}

// safeCopy runs the contentfilter, then truncates to copyMaxRunes. When the
// filter hard-blocks (sensitive-word hit) or the result is empty, it falls back
// to the agent's template copy for the ActionType (templates are pre-cleaned).
func (s *Service) safeCopy(copy, actionType string) string {
	text := copy
	if s.filter != nil {
		res := s.filter.Do(copy)
		if !res.Passed {
			text = agent.TemplateCopy(actionType)
		} else {
			text = res.Text
		}
	}
	if text == "" {
		text = agent.TemplateCopy(actionType)
	}
	return truncateRunes(text, copyMaxRunes)
}

// truncateRunes returns the first n runes of s (no allocation when already short).
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// generatedBy derives strategies.generated_by from the degrade marker
// (CONTRACT §12.3.7): empty → live_llm; cold_start/budget/no_provider/llm →
// rule_only. cache_hit never reaches Encode (the pipeline short-circuits).
func generatedBy(degrade string) string {
	if degrade == "" {
		return strategy.GenLiveLLM
	}
	return strategy.GenRuleOnly
}

// llmTier reports the tier that produced the advice: "lite" for live LLM, empty
// for rule_only (CONTRACT §12.3.1 "rule_only 时空").
func llmTier(degrade string) string {
	if degrade == "" {
		return "lite"
	}
	return ""
}

// marshalBundle snapshots the signal Bundle into the strategy.Signals JSONB
// column. A nil bundle or marshal error yields nil (strategy stores NULL).
func marshalBundle(bundle *signal.Bundle) json.RawMessage {
	if bundle == nil {
		return nil
	}
	b, err := json.Marshal(bundle)
	if err != nil {
		return nil
	}
	return b
}

// bundleQuality surfaces the Bundle's 0-100 data-quality score for trace.
func bundleQuality(bundle *signal.Bundle) int {
	if bundle == nil {
		return 0
	}
	return bundle.DataQuality
}

// targetMap renders the audience filter JSONB. It carries the routed target
// segment plus the bound product id when the action is product-linked.
func targetMap(ri rule.RoutedItem) map[string]any {
	if ri.Target == "" && ri.ProductID == 0 {
		return nil
	}
	m := map[string]any{}
	if ri.Target != "" {
		m["segment"] = ri.Target
	}
	if ri.ProductID > 0 {
		m["product_id"] = ri.ProductID
	}
	return m
}

// effectMap stores the expected-effect string under a summary key.
func effectMap(effect string) map[string]any {
	if effect == "" {
		return nil
	}
	return map[string]any{"summary": effect}
}

// cloneParams returns a shallow copy of params so the stored strategy does not
// share a map with the caller's EnhancedItem.
func cloneParams(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = v
	}
	return out
}
