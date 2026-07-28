package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/margin"
)

// enhanceOne polishes a single RoutedItem. It returns the EnhancedItem, whether
// the LLM produced it (hit), and on miss the degradation reason
// (budget|no_provider|llm). It never returns an error and never returns a
// zero EnhancedItem — the worst case is a template item.
func (s *Service) enhanceOne(ctx context.Context, tenantID int64, ri rule.RoutedItem) (EnhancedItem, bool, string) {
	// No gateway (nil) → no provider to call. Treat as no_provider.
	if s.gw == nil {
		return templateItem(ri), false, "no_provider"
	}

	resp, err := s.gw.Chat(ctx, tenantID, s.tier, buildMessages(ri), gateway.Opts{
		Temperature: 0.3,
		ResponseFormat: &gateway.ResponseFormat{
			Type: "json_object",
		},
	})
	if err != nil {
		return templateItem(ri), false, classifyChatErr(err)
	}

	ei, ok := parseLLMJSON(resp.Content, ri)
	if !ok {
		return templateItem(ri), false, "llm"
	}
	return ei, true, ""
}

// classifyChatErr maps a gateway.Chat error to the pipeline degrade reason.
// ErrNoProvider → no_provider (CONTRACT §12.5.10); ErrBudgetExceeded → budget
// (the circuit-breaker fired); anything else (upstream timeout, transport,
// context cancellation) → llm.
func classifyChatErr(err error) string {
	if errors.Is(err, gateway.ErrNoProvider) {
		return "no_provider"
	}
	if errors.Is(err, gateway.ErrBudgetExceeded) {
		return "budget"
	}
	return "llm"
}

// llmEnvelope is the strict JSON shape the prompt asks the model for.
type llmEnvelope struct {
	Copy           string         `json:"copy"`
	Params         map[string]any `json:"params"`
	ExpectedEffect string         `json:"expected_effect"`
}

// parseLLMJSON parses the model's content into an EnhancedItem and runs the
// second margin guardrail pass on discount actions (CONTRACT §12.5.4). On any
// parse failure it returns ok=false so the caller falls back to a template.
func parseLLMJSON(content string, ri rule.RoutedItem) (EnhancedItem, bool) {
	raw := strings.TrimSpace(content)
	if raw == "" {
		return EnhancedItem{}, false
	}
	var env llmEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		// Tolerate a model that wrapped the JSON in prose: lift the first
		// {...} block and retry once.
		lo := strings.IndexByte(raw, '{')
		hi := strings.LastIndexByte(raw, '}')
		if lo < 0 || hi <= lo {
			return EnhancedItem{}, false
		}
		if err := json.Unmarshal([]byte(raw[lo:hi+1]), &env); err != nil {
			return EnhancedItem{}, false
		}
	}

	params := env.Params
	if params == nil {
		params = make(map[string]any)
	}
	// Second guardrail pass: re-validate any discount against the shared floor
	// (the RoutedItem carries the frozen MarginFloorPct from rule.ApplyGuardrail).
	// On breach, clamp the discount back to the floor permille (CONTRACT §12.3.5
	// "破底 clampToFloor"). MarginOK is forced true after clamping.
	if ri.CostCents > 0 && ri.PriceCents > 0 {
		clampDiscountToFloor(ri, params)
	}

	return EnhancedItem{
		Routed:         ri,
		Copy:           env.Copy,
		Params:         params,
		ExpectedEffect: env.ExpectedEffect,
		MarginOK:       true,
	}, true
}

// clampDiscountToFloor ensures the discount in params keeps the margin at or
// above the floor. It coerces the LLM-supplied permille to int64, clamps into
// [DiscountMinPermille, DiscountMaxPermille], re-checks via the margin library,
// and on breach forces the floor permille. The (possibly corrected) permille
// and derived post-discount price are written back into params.
func clampDiscountToFloor(ri rule.RoutedItem, params map[string]any) {
	pr := ri.ParamRange
	permille := permilleFromParams(params, pr.DiscountMinPermille)
	if permille < pr.DiscountMinPermille {
		permille = pr.DiscountMinPermille
	}
	if pr.DiscountMaxPermille > 0 && permille > pr.DiscountMaxPermille {
		permille = pr.DiscountMaxPermille
	}
	newPrice := ri.PriceCents * permille / 1000
	guard := margin.Guard{MarginFloorPct: ri.MarginFloorPct}
	if err := margin.ValidatePriceAdj(guard, ri.CostCents, newPrice); err != nil {
		permille = pr.DiscountMinPermille
		newPrice = ri.PriceCents * permille / 1000
	}
	params["discount_permille"] = permille
	params["price_after_cents"] = newPrice
}

// permilleFromParams extracts a discount permille from the LLM params under any
// of the common keys, defaulting to def when absent or malformed. JSON numbers
// arrive as float64, so coerce.
func permilleFromParams(params map[string]any, def int64) int64 {
	for _, k := range []string{"discount_permille", "discount", "discount_permille_l"} {
		if v, ok := params[k]; ok {
			if n, ok := toInt64(v); ok && n > 0 {
				return n
			}
		}
	}
	return def
}

// toInt64 coerces numeric kinds (float64 from JSON, int variants) to int64.
func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case float32:
		return int64(x), true
	case int:
		return int64(x), true
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
	}
	return 0, false
}

// buildMessages constructs the system + user prompt for one RoutedItem. The
// system prompt fixes the hard constraints (range-bound pricing, ≤40-字 copy,
// no absolutism words, strict JSON, conservative effect). The user prompt
// feeds the frozen action / cost / price / ParamRange / margin floor.
func buildMessages(ri rule.RoutedItem) []gateway.Message {
	system := "你是「掌柜军师」的营销文案润色引擎。你只对规则引擎已选定的动作做文案润色、参数取值与预期效果估算。" +
		"硬约束:1.禁止从零定价,折扣/满减必须在 params_range 区间内且折扣后价不低于成本*(1+margin_floor),不确定取最保守;" +
		"2.copy 不超过 40 个汉字,口语化,不含绝对化用语(最/第一/永久/包治/最佳等);" +
		"3.必须返回严格 JSON,字段 copy/params/expected_effect;" +
		"4.expected_effect 给可量化预期(核销X份/到店Y人/挽回¥Z/召回N人),数值保守。"
	pr := ri.ParamRange
	user := fmt.Sprintf(
		"[规则选定的动作]\naction_type: %s\nreason_type: %s\nproduct: %s\ncost_cents: %d\nprice_cents: %d\n"+
			"params_range: {discount_min_permille:%d,discount_max_permille:%d,threshold_cents:%d,off_cents:%d,limit_qty_max:%d,expire_hours:%d}\n"+
			"margin_floor_pct: %d\n请产出一条建议并返回严格 JSON。",
		ri.ActionType, ri.ReasonType, ri.Product, ri.CostCents, ri.PriceCents,
		pr.DiscountMinPermille, pr.DiscountMaxPermille, pr.ThresholdCents, pr.OffCents, pr.LimitQtyMax, pr.ExpireHours,
		ri.MarginFloorPct,
	)
	return []gateway.Message{
		{Role: gateway.RoleSystem, Content: system},
		{Role: gateway.RoleUser, Content: user},
	}
}
