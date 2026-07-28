// Package aiengine is the root of the AI engine pipeline. It wires the five
// stages — Signal Collect → Rule Router → LLM Enhance → Converge Top3 → Encode
// — behind one Pipeline.Run entry point that the boss-facing handler calls to
// produce today's "3件事" advice. The stage implementations live in
// sub-packages (signal, rule, agent, converge, output, cache); this file is
// the orchestrator only (CONTRACT §12.3.8).
package aiengine

import (
	"context"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/cache"
	"github.com/zhangguijunshi/backend/internal/aiengine/converge"
	"github.com/zhangguijunshi/backend/internal/aiengine/output"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
	"go.uber.org/zap"
)

// pipelineTimeout is the hard per-tenant ceiling for the whole pipeline (tech
// plan §7.1.3: 8s). Override in tests via WithTimeout.
const pipelineTimeout = 8 * time.Second

// Pipeline is the five-stage total controller. Run is the only entry point the
// handler calls. Each stage degrades on failure rather than aborting; only
// output.Encode persistence failure returns an error (CONTRACT §12.3.8).
type Pipeline struct {
	collector signal.Collector
	router    rule.Engine
	stage     agent.Stage
	conv      converge.Top3
	enc       output.Encoder
	ch        cache.Cache
	business  string
	now       func() time.Time
	timeout   time.Duration
	log       *zap.Logger
}

// Option configures a Pipeline.
type Option func(*Pipeline)

// WithBusinessType overrides the default cold-start business type ("fruit").
func WithBusinessType(b string) Option {
	return func(p *Pipeline) {
		if b != "" {
			p.business = b
		}
	}
}

// WithNowFunc overrides the pipeline clock (deterministic date / cache key).
func WithNowFunc(f func() time.Time) Option {
	return func(p *Pipeline) {
		if f != nil {
			p.now = f
		}
	}
}

// WithTimeout overrides the per-run pipeline timeout (tests).
func WithTimeout(d time.Duration) Option {
	return func(p *Pipeline) {
		if d > 0 {
			p.timeout = d
		}
	}
}

// WithLogger overrides the default no-op logger.
func WithLogger(l *zap.Logger) Option {
	return func(p *Pipeline) {
		if l != nil {
			p.log = l
		}
	}
}

// NewPipeline wires the five stages plus the L1 cache. collector/router/stage/
// conv/enc are required (non-nil); ch may be nil to disable caching. The
// business type defaults to "fruit" (CONTRACT §12.3.8).
func NewPipeline(
	collector signal.Collector,
	router rule.Engine,
	stage agent.Stage,
	conv converge.Top3,
	enc output.Encoder,
	ch cache.Cache,
	opts ...Option,
) *Pipeline {
	p := &Pipeline{
		collector: collector,
		router:    router,
		stage:     stage,
		conv:      conv,
		enc:       enc,
		ch:        ch,
		business:  "fruit",
		now:       time.Now,
		timeout:   pipelineTimeout,
		log:       zap.NewNop(),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Run produces today's advice. It first enforces the tenant from ctx (tenant is
// never trusted from a field), then runs under an 8s timeout:
//  0. cache.TryHit → hit returns Advice{Degrade:"cache_hit"};
//  1. signal.Collect (failure → empty bundle, rule_only path);
//  2. rule.Route (0 items → cold-start injection, Degrade="cold_start");
//  3. agent.EnhanceOrSkip (LLM polish, all-fail → degrade marker);
//  4. converge.Top3;
//  5. output.Encode (persist + cache.Put, swallow put error).
func (p *Pipeline) Run(ctx context.Context) (*output.Advice, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	storeID := int64(0)
	if mc, ok := multitenancy.FromContext(ctx); ok {
		storeID = mc.StoreID
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	date := p.now().Format("2006-01-02")
	log := p.log.With(zap.Int64("tenant_id", tenantID), zap.Int64("store_id", storeID))

	// 0) L1 cache short-circuit.
	if p.ch != nil {
		if strat, ok := p.ch.TryHit(ctx, tenantID, storeID, date); ok {
			log.Debug("pipeline cache hit", zap.String("degrade", "cache_hit"))
			return &output.Advice{Strategy: strat, Degrade: "cache_hit"}, nil
		}
	}

	// ① Signal collect — failure degrades to an empty bundle (rule_only path).
	bundle, err := p.collector.Collect(ctx, tenantID, storeID)
	if err != nil || bundle == nil {
		log.Warn("signal collect degraded", zap.Error(err))
		bundle = &signal.Bundle{
			TenantID:     tenantID,
			StoreID:      storeID,
			BusinessType: p.business,
			Date:         p.now(),
		}
	}

	// ② Rule route — never errors (empty Routed on panic); cold-start inject.
	routed, _ := p.router.Route(ctx, bundle)
	if routed == nil {
		routed = &rule.Routed{TenantID: tenantID}
	}
	degrade := ""
	if len(routed.Items) == 0 {
		routed = injectColdStart(routed.TenantID)
		degrade = "cold_start"
		log.Debug("cold-start injected", zap.Int("items", len(routed.Items)))
	}

	// ③ Agent enhance (write-once degrade: cold_start preserved on LLM miss).
	enhanced := p.stage.EnhanceOrSkip(ctx, tenantID, routed, &degrade)
	log.Debug("enhance done", zap.Int("items", len(enhanced.Items)), zap.String("degrade", degrade))

	// ④ Converge Top3.
	top3 := p.conv.PickTop3(ctx, enhanced)

	// ⑤ Encode + persist; cache.Put failure is swallowed.
	advice, err := p.enc.Encode(ctx, tenantID, storeID, top3, bundle, degrade)
	if err != nil {
		log.Error("encode failed", zap.Error(err))
		return nil, err
	}
	if p.ch != nil && advice != nil && advice.Strategy != nil {
		p.ch.Put(ctx, tenantID, storeID, date, advice.Strategy)
	}
	return advice, nil
}

// injectColdStart builds 1-2 placeholder RoutedItems so "今日3件事" is never
// empty when no rule matched (CONTRACT §12.3.8 cold-start). The placeholders
// are non-product-bound coupon_wake actions (满减/触达, cost=0) so the margin
// floor holds trivially and the boss sees a sensible "wake dormant members /
// festival warming" nudge on a brand-new store's first day.
func injectColdStart(tenantID int64) *rule.Routed {
	return &rule.Routed{
		TenantID: tenantID,
		Items: []rule.RoutedItem{
			{
				RuleID:         "cold_start_wake",
				ReasonType:     rule.ReasonWakeDormant,
				ActionType:     rule.ActCouponWake,
				Target:         "member_segment",
				Product:        "-",
				Priority:       5,
				MarginFloorPct: 0,
				Reason:         "冷启动兜底: 沉睡唤醒 (不依赖销售数据, 新店首日可用)",
				ParamRange: rule.ParamRange{
					ThresholdCents: 3000, // 满30元
					OffCents:       500,  // 减5元
					ExpireHours:    48,
				},
			},
			{
				RuleID:         "cold_start_festival",
				ReasonType:     rule.ReasonFestival,
				ActionType:     rule.ActCouponWake,
				Target:         "member_segment",
				Product:        "-",
				Priority:       4,
				MarginFloorPct: 0,
				Reason:         "冷启动兜底: 节日满减关怀 (不依赖销售数据)",
				ParamRange: rule.ParamRange{
					ThresholdCents: 5000, // 满50元
					OffCents:       800,  // 减8元
					ExpireHours:    72,
				},
			},
		},
	}
}
