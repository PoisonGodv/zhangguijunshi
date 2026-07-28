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
	"github.com/zhangguijunshi/backend/internal/aiengine/bandit"
	"github.com/zhangguijunshi/backend/internal/aiengine/cache"
	"github.com/zhangguijunshi/backend/internal/aiengine/converge"
	"github.com/zhangguijunshi/backend/internal/aiengine/output"
	"github.com/zhangguijunshi/backend/internal/aiengine/playbook"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/semantic"
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
	sem       semantic.SemanticCache
	business  string
	now       func() time.Time
	timeout   time.Duration
	log       *zap.Logger
	// M3-S3 (CONTRACT §14.7): playbook cold-start injection + bandit priority
	// boost. Both nil-safe (backward-compatible with M3-S1/S2 callers).
	pb     *playbook.Store
	bandit *bandit.Learner
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

// WithSemanticCache injects the L2 semantic cache (M3-S2 §13.3.6). nil or unset
// disables L2 — this keeps NewPipeline backward-compatible with M3-S1 callers
// (which pass no semantic cache) so main.go / m3s1_test / pipeline_test need no
// change for the signature alone.
func WithSemanticCache(sem semantic.SemanticCache) Option {
	return func(p *Pipeline) {
		p.sem = sem
	}
}

// WithPlaybookStore injects the cold-start playbook library (M3-S3 §14.7).
// When wired, a cold-start (0 routed items) injects the playbook's ColdPriority
// actions for the configured business type instead of the hard-coded generic
// fallback. nil keeps the M3-S1/S2 injectColdStart behaviour (backward-compat).
func WithPlaybookStore(s *playbook.Store) Option {
	return func(p *Pipeline) {
		p.pb = s
	}
}

// WithBanditLearner injects the epsilon-greedy rule-weight learner (M3-S3
// §14.7). When wired, routed items are priority-boosted by the bandit arm
// weight for their ReasonType (best-effort: a missing arm leaves the rule's
// original priority). nil disables boosting (backward-compatible).
func WithBanditLearner(l *bandit.Learner) Option {
	return func(p *Pipeline) {
		p.bandit = l
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

	// ①b) L2 semantic cache short-circuit (needs the bundle to build signalText).
	// Fires only when L1 missed and a semantic cache is wired; a hit returns the
	// cached strategy with Degrade="semantic_hit" without re-running the pipeline.
	if p.sem != nil {
		signalText := semantic.BuildSignalText(bundle)
		if strat, ok, _ := p.sem.TryHit(ctx, tenantID, bundle.BusinessType, signalText); ok && strat != nil {
			log.Debug("pipeline semantic cache hit", zap.String("degrade", "semantic_hit"))
			return &output.Advice{Strategy: strat, Degrade: "semantic_hit"}, nil
		}
	}

	// ② Rule route — never errors (empty Routed on panic); cold-start inject.
	routed, _ := p.router.Route(ctx, bundle)
	if routed == nil {
		routed = &rule.Routed{TenantID: tenantID}
	}
	// ②a) Bandit priority boost (M3-S3 §14.7): re-rank routed items by the
	// learner arm weight for each item's ReasonType. Best-effort — a missing
	// arm leaves the item's original priority; errors are swallowed so the
	// pipeline never breaks on the bandit path.
	applyBanditBoost(ctx, routed, p.bandit, tenantID, log)
	degrade := ""
	if len(routed.Items) == 0 {
		// Cold-start: prefer the playbook's business-specific ColdPriority
		// actions when a playbook store is wired; else fall back to the
		// M3-S1/S2 generic placeholder set (CONTRACT §14.7 / §12.3.8).
		if injected := coldStartFromPlaybook(tenantID, p.business, p.pb); injected != nil {
			routed = injected
			degrade = "cold_start"
			log.Debug("playbook cold-start injected",
				zap.String("business", p.business), zap.Int("items", len(routed.Items)))
		} else {
			routed = injectColdStart(routed.TenantID)
			degrade = "cold_start"
			log.Debug("cold-start injected", zap.Int("items", len(routed.Items)))
		}
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
	if p.sem != nil && advice != nil && advice.Strategy != nil {
		signalText := semantic.BuildSignalText(bundle)
		_ = p.sem.Put(ctx, tenantID, bundle.BusinessType, signalText, advice.Strategy, 24*time.Hour)
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

// coldStartFromPlaybook builds a business-specific cold-start Routed set from
// the wired playbook store (M3-S3 §14.7). Returns nil if no store is wired or
// the business has no playbook — the caller then falls back to the generic
// injectColdStart. The injected items mirror the playbook's ColdPriority
// action_types (non-product-bound, cost=0 so the margin floor holds trivially)
// and reuse the generic copy/param ranges per action so one-click execute +
// margin guardrail behave identically to the rule-routed path.
func coldStartFromPlaybook(tenantID int64, business string, store *playbook.Store) *rule.Routed {
	if store == nil {
		return nil
	}
	pb, ok := store.Get(business)
	if !ok || pb == nil || len(pb.ColdPriority) == 0 {
		return nil
	}
	items := make([]rule.RoutedItem, 0, len(pb.ColdPriority))
	for rank, act := range pb.ColdPriority {
		items = append(items, playbookColdItem(rank+1, act))
	}
	return &rule.Routed{TenantID: tenantID, Items: items}
}

// playbookColdItem builds one cold-start RoutedItem for an action_type, with a
// sane per-action copy + param range. All are non-product-bound (Product "-")
// so the margin floor is trivially satisfied on a brand-new store.
func playbookColdItem(rank int, actionType string) rule.RoutedItem {
	it := rule.RoutedItem{
		RuleID:         "cold_start_" + actionType,
		ActionType:     actionType,
		Target:         "member_segment",
		Product:        "-",
		Priority:       10 - rank, // larger-first; rank1 highest
		MarginFloorPct: 0,
		Reason:         "冷启动兜底: " + coldReasonFor(actionType) + " (不依赖销售数据, 新店首日可用)",
	}
	switch actionType {
	case rule.ActCouponWake:
		it.ReasonType = rule.ReasonWakeDormant
		it.ParamRange = rule.ParamRange{ThresholdCents: 3000, OffCents: 500, ExpireHours: 48}
	case rule.ActGroupSeckill:
		it.ReasonType = rule.ReasonClearanceCoupon
		it.ParamRange = rule.ParamRange{DiscountMinPermille: 500, DiscountMaxPermille: 700, LimitQtyMax: 50}
	case rule.ActGroupbuy:
		it.ReasonType = rule.ReasonBundleSlow
		it.ParamRange = rule.ParamRange{DiscountMinPermille: 800, DiscountMaxPermille: 900, LimitQtyMax: 20}
	case rule.ActMomentsPost:
		it.ReasonType = rule.ReasonHighValue
		it.ParamRange = rule.ParamRange{DiscountMinPermille: 900, DiscountMaxPermille: 900}
	case rule.ActRechargePromo:
		it.ReasonType = rule.ReasonHighValue
		it.ParamRange = rule.ParamRange{ThresholdCents: 10000, OffCents: 1000}
	default:
		it.ReasonType = rule.ReasonFestival
		it.ParamRange = rule.ParamRange{ThresholdCents: 5000, OffCents: 800, ExpireHours: 72}
	}
	return it
}

// coldReasonFor maps a cold-start action_type to a one-line boss-readable hint.
func coldReasonFor(actionType string) string {
	switch actionType {
	case rule.ActCouponWake:
		return "沉睡唤醒券"
	case rule.ActGroupSeckill:
		return "群秒杀引流"
	case rule.ActGroupbuy:
		return "拼团裂变"
	case rule.ActMomentsPost:
		return "朋友圈种草"
	case rule.ActRechargePromo:
		return "储值锁客"
	default:
		return "节日满减"
	}
}

// applyBanditBoost re-ranks routed items by the bandit arm weight (Q value) for
// each item's ReasonType (M3-S3 §14.7). It adds a weight-scaled boost to each
// item's Priority so the converge Top3 stage prefers rules the bandit has
// learned perform well. Best-effort: nil learner or a read error leaves the
// routed priorities untouched (the pipeline never breaks on the bandit path).
// An unseen rule (cold-start prior Weight=0.5) contributes a neutral boost.
func applyBanditBoost(ctx context.Context, routed *rule.Routed, learner *bandit.Learner, tenantID int64, log *zap.Logger) {
	if learner == nil || routed == nil || len(routed.Items) == 0 {
		return
	}
	weights, err := learner.Weights(ctx, tenantID)
	if err != nil {
		log.Debug("bandit weights read failed (non-fatal)", zap.Error(err))
		return
	}
	for i := range routed.Items {
		w, ok := weights[routed.Items[i].ReasonType]
		if !ok {
			continue // unseen rule → leave original priority
		}
		// Boost = (weight − 0.5 neutral prior) * 20, so a weight of 1.0 adds
		// +10 and 0.0 subtracts 10. This nudges ranking without overriding the
		// rule's declarative priority entirely.
		routed.Items[i].Priority += int((w - 0.5) * 20)
	}
}
