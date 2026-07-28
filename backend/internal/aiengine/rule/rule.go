package rule

// Routed is the output of the Rule Router stage: the set of RoutedItems that
// matched the bundle and passed the margin guardrail.
type Routed struct {
	TenantID int64        `json:"tenant_id"`
	Items    []RoutedItem `json:"items"`
}

// RoutedItem is one routed action. Amounts are int64 cents. ActionType is
// derived from ReasonType via ReasonToAction (not hand-filled). CostCeilCents
// is the nominal floor price cost*(100+floor)/100; ParamRange is clamped so
// any discount the LLM picks respects the floor.
type RoutedItem struct {
	RuleID         string     `json:"rule_id"`     // clearance_fruit_3d / cold_start_wake
	ReasonType     string     `json:"reason_type"` // 7 values (ReasonXxx)
	ActionType     string     `json:"action_type"` // 6 values (ActXxx, via ReasonToAction)
	Target         string     `json:"target"`      // member_segment|product|group
	ProductID      int64      `json:"product_id"`  // 0=非商品绑定
	Product        string     `json:"product"`     // "-"=非商品绑定
	CostCents      int64      `json:"cost_cents"`  // 0=满减类不绑商品
	PriceCents     int64      `json:"price_cents"`
	CostCeilCents  int64      `json:"cost_ceil_cents"`  // cost*(100+floor)/100, 0=不校验
	MarginFloorPct int64      `json:"margin_floor_pct"` // 5=5%
	Priority       int        `json:"priority"`
	Reason         string     `json:"reason"` // 老板可读理由
	ParamRange     ParamRange `json:"param_range"`
}

// ParamRange constrains LLM polish to an interval. All int64 (no float amount);
// discount in permille (‰, 500=5折).
type ParamRange struct {
	DiscountMinPermille int64 `json:"discount_min_permille"` // 500=5折
	DiscountMaxPermille int64 `json:"discount_max_permille"` // 700=7折
	ThresholdCents      int64 `json:"threshold_cents"`       // 满X
	OffCents            int64 `json:"off_cents"`             // 减Y
	LimitQtyMax         int   `json:"limit_qty_max"`
	ExpireHours         int   `json:"expire_hours"`
}

// Rule is the JSON/DB-driven rule struct (CONTRACT §12.3.3). Conds are AND.
// The rule layer declares ReasonType in Action; ActionType is derived.
type Rule struct {
	ID             string `json:"id"`       // clearance_fruit_3d
	Business       string `json:"business"` // fruit|*（* 全业态）
	Priority       int    `json:"priority"` // 越大越先
	Enable         bool   `json:"enable"`
	Conds          []Cond `json:"conds"` // AND
	Action         Action `json:"action"`
	MarginFloorPct int64  `json:"margin_floor_pct"` // declarative; enforcement uses the common margin.Guard
}

// Cond is one trigger condition. Field is a dotted path; Op selects the
// comparison; Value is the operand (between=[lo,hi]; in/exists=membership test).
type Cond struct {
	Field string `json:"field"` // weather.*|holiday.*|members.*|inventory.*
	Op    string `json:"op"`    // eq|lt|gt|gte|lte|between|in|exists
	Value any    `json:"value"`
}

// Action is the rule's declarative outcome. ParamRange is the interval the LLM
// may polish within (clamped by the guardrail at emit time).
type Action struct {
	ReasonType string     `json:"reason_type"` // 7 values (ReasonXxx)
	Target     string     `json:"target"`
	ParamRange ParamRange `json:"param_range"`
}
