package bootstrap

// aiengine_build.go constructs the M3-S1 five-stage AI pipeline, the exec
// executor, and the pregen/attribution/bandit scheduler as one self-contained
// unit (CONTRACT §12/§13/§14). Extracted verbatim from the previous
// cmd/api/main.go so the api / cron / worker binaries share the identical
// pipeline wiring.

import (
	"context"
	"strings"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine"
	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/attribution"
	"github.com/zhangguijunshi/backend/internal/aiengine/bandit"
	"github.com/zhangguijunshi/backend/internal/aiengine/cache"
	"github.com/zhangguijunshi/backend/internal/aiengine/converge"
	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/aiengine/output"
	"github.com/zhangguijunshi/backend/internal/aiengine/playbook"
	"github.com/zhangguijunshi/backend/internal/aiengine/pregen"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/semantic"
	aisignal "github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/dataquality"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/internal/reach"
	"github.com/zhangguijunshi/backend/internal/store"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// aiEngine bundles the M3 objects BuildServices needs to publish.
type aiEngine struct {
	strategy  *strategy.Service
	pipeline  *aiengine.Pipeline
	executor  *exec.Executor
	attrRepo  attribution.Repo
	scheduler *pregen.Scheduler
	tenants   pregen.TenantEnumerator
}

func buildAIEngine(
	cfg *config.Config,
	database *db.DB,
	catalogSvc *catalog.Service,
	inventorySvc *inventory.Service,
	membershipSvc *membership.Service,
	billingSvc *billing.Service,
	marketingSvc *marketing.Service,
	reachSvc *reach.Service,
	groupbuySvc *groupbuy.Service,
	storeSvc *store.Service,
) aiEngine {
	// gateway.NewFromConfig builds a placeholder (empty-providers) gateway when
	// no provider API key is configured (dev); agent.EnhanceOrSkip then degrades
	// to rule-only (no_provider) — the pipeline still produces cold-start / rule
	// advice.
	gw, _ := gateway.NewFromConfig(cfg.AI)
	emb := gateway.NewEmbedderFromConfig(cfg.AI)

	var strategyRepo strategy.StrategyRepo = strategy.NewMemoryStrategyRepo()
	if database != nil {
		strategyRepo = strategy.NewPGStrategyRepo(database.Pool())
	}
	strategySvc := strategy.New(strategyRepo)

	cacheL1 := cache.NewMemoryCache()
	semCache := semantic.NewMemoryCache(emb)
	// M7-B: feed fused daily_product_sales (migration 000019) into the signal
	// stage as real historical sales. Memory↔PG switch mirrors the cron repo
	// convention; nil provider (no DB, dev) keeps pre-M7-B behaviour (backward
	// compatible — WithSalesProvider(nil) is a no-op).
	var salesRepo dataquality.SalesRepo = dataquality.NewMemorySalesRepo()
	if database != nil {
		salesRepo = dataquality.NewPGSalesRepo(database.Pool())
	}
	signalCollector := aisignal.NewCollector("fruit", "", signalLookuperAdapter{
		catalogSvc:    catalogSvc,
		inventorySvc:  inventorySvc,
		membershipSvc: membershipSvc,
		billingSvc:    billingSvc,
	},
		aisignal.WithSalesProvider(salesProviderAdapter{repo: salesRepo}),
	)
	ruleEngine := rule.NewEngine(rule.DefaultSeeds(), margin.Guard{MarginFloorPct: defaultMarginFloorPct})

	// M3-S3 attribution + bandit + playbook (CONTRACT §14.7).
	var attrRepo attribution.Repo = attribution.NewMemoryRepo()
	if database != nil {
		attrRepo = attribution.NewPGRepo(database.Pool())
	}
	attrComputer := attribution.NewComputer(
		attribution.Sources{
			Coupons:    couponStatsAdapter{marketingSvc},
			Visits:     visitCounterAdapter{billingSvc},
			Groupbuy:   groupbuyStatsAdapter{groupbuySvc},
			Repurchase: repurchaseCounterAdapter{mkt: marketingSvc, bill: billingSvc},
		},
		itemLoaderAdapter{strategySvc},
	)
	var banditRepo bandit.Repo = bandit.NewMemoryRepo()
	if database != nil {
		banditRepo = bandit.NewPGRepo(database.Pool())
	}
	banditLearner := bandit.NewLearner(banditRepo, bandit.DefaultConfig(),
		bandit.WithRewardSource(attrComputer),
		bandit.WithItemResolver(itemResolverAdapter{strategySvc}),
	)
	playbookStore := playbook.NewStore()

	// aiengine 修复②: audience-resolution + coupon-template enrichment. The two
	// adapters wrap the real membership / marketing services so coupon-bearing
	// items (coupon_wake / group_seckill) get a concrete member_id / coupon_id
	// before encode — closing the "every pipeline item blocks invalid_item" gap.
	// A tenant with no members / no active coupon template degrades honestly to
	// exec blocked(audience_unresolved) rather than a fake green.
	enricher := aiengine.NewEnricher(
		aiMemberResolverAdapter{membershipSvc},
		aiCouponTemplateSelectorAdapter{marketingSvc},
		nil,
	)

	pipeline := aiengine.NewPipeline(
		signalCollector, ruleEngine, agent.New(gw), converge.NewTop3(),
		output.NewEncoder(strategySvc), cacheL1,
		aiengine.WithBusinessType("fruit"),
		aiengine.WithSemanticCache(semCache),
		aiengine.WithPlaybookStore(playbookStore),
		aiengine.WithBanditLearner(banditLearner),
		aiengine.WithEnricher(enricher),
	)
	executor := exec.NewExecutor(
		strategySvc, margin.Guard{MarginFloorPct: defaultMarginFloorPct},
		couponIssuerAdapter{marketingSvc}, msgSenderAdapter{reachSvc},
		priceUpdaterAdapter{catalogSvc}, groupbuyOpenerAdapter{groupbuySvc},
	)

	// pregen scheduler + attribution/bandit crons. tenantEnumerator returns the
	// real DB-backed enumerator when a pool is wired, else the dev-empty stub.
	tenEnum := tenantEnumerator(database)
	// aiengine 修复③: bandit-credit de-duplication store. Memory↔PG switch
	// mirrors the repo convention (database != nil → PG, else memory). The PG
	// store queries strategy_items.bandit_credited_at (migration 000023)
	// directly, constructed here like semCache — avoiding any change to
	// adapters.go. The memory store wraps the same strategy-item enumerator the
	// scheduler already uses.
	itemEnumAdapter := strategyItemEnumeratorAdapter{strategySvc}
	var creditStore pregen.BanditCreditStore = pregen.NewMemoryBanditCreditStore(itemEnumAdapter)
	if database != nil {
		creditStore = pregen.NewPGBanditCreditStore(database.Pool())
	}
	scheduler := pregen.NewScheduler(pipeline, cacheL1, semCache, tenEnum,
		pregen.WithAttribution(attrRepo, attrComputer),
		pregen.WithBanditLearner(banditLearner),
		pregen.WithItemEnumerator(itemEnumAdapter),
		pregen.WithBanditCreditStore(creditStore),
		// M6 ①: run the pipeline per active store (injects StoreID into ctx so
		// the strategy domain no longer rejects the run with storeID=0).
		pregen.WithStoreEnumerator(storeEnumeratorAdapter{storeSvc}),
	)

	return aiEngine{
		strategy:  strategySvc,
		pipeline:  pipeline,
		executor:  executor,
		attrRepo:  attrRepo,
		scheduler: scheduler,
		tenants:   tenEnum,
	}
}

// memberResolverAdapter adapts membership.Service onto aiengine.MemberResolver
// (aiengine 修复②). It resolves ONE representative active member for the routed
// segment so the one-click coupon path has a concrete member_id to issue to.
//
// Segment → filter mapping is best-effort and intentionally conservative: the
// membership MemberFilter exposes SleepBefore / MinPoints / BirthdayOn / Level /
// Query, so we map the rule seeds' well-known Target strings onto those
// dimensions and fall back to "any active member" otherwise. A representative
// (deterministic lowest-ID match) is returned — true per-member fan-out for
// group_seckill is a reach-domain concern deferred to a later milestone (see
// internal/aiengine/enrich.go package note). Returns 0 (→ exec degrades to
// blocked(audience_unresolved)) when the tenant has no matching active member.
type aiMemberResolverAdapter struct{ svc *membership.Service }

func (a aiMemberResolverAdapter) ResolveMember(ctx context.Context, tenantID, storeID int64, target string) (int64, error) {
	if a.svc == nil {
		return 0, nil
	}
	bctx := withTenantStoreCtx(ctx, tenantID, storeID)
	f := membership.MemberFilter{Limit: 1}
	seg := strings.TrimSpace(target)
	switch {
	case strings.Contains(seg, "未到店") || strings.Contains(seg, "沉睡"):
		sleep := time.Now().AddDate(0, 0, -30)
		f.SleepBefore = &sleep
	case strings.Contains(seg, "高价值"):
		f.MinPoints = 1
	case strings.Contains(seg, "生日"):
		now := time.Now()
		f.BirthdayOn = &now
	}
	members, err := a.svc.ListMembers(bctx, f)
	if err != nil {
		return 0, err
	}
	for _, m := range members {
		if m != nil && m.Status == "active" && m.ID > 0 {
			return m.ID, nil
		}
	}
	return 0, nil
}

// couponTemplateSelectorAdapter adapts marketing.Service onto
// aiengine.CouponTemplateSelector (aiengine 修复②). It picks an ACTIVE coupon
// template whose shape matches the action (group_seckill → discount; coupon_wake
// → full_reduction), falling back to any active template when no shape match
// exists. Returns 0 (→ exec degrades to blocked(audience_unresolved)) when the
// tenant has no active coupon template at all.
type aiCouponTemplateSelectorAdapter struct{ svc *marketing.Service }

func (a aiCouponTemplateSelectorAdapter) SelectCoupon(ctx context.Context, tenantID, storeID int64, actionType, reasonType string) (int64, error) {
	if a.svc == nil {
		return 0, nil
	}
	bctx := withTenantStoreCtx(ctx, tenantID, storeID)
	preferred := marketing.TypeFullReduction
	if actionType == "group_seckill" {
		preferred = marketing.TypeDiscount
	}
	// First pass: active templates of the preferred shape.
	if id := firstActiveCoupon(a.svc, bctx, preferred); id > 0 {
		return id, nil
	}
	// Fallback: any active template.
	return firstActiveCoupon(a.svc, bctx, ""), nil
}

// firstActiveCoupon returns the lowest-ID active coupon template of the given
// type ("" = any type), or 0 when none exists.
func firstActiveCoupon(svc *marketing.Service, ctx context.Context, typ string) int64 {
	coupons, err := svc.ListCoupons(ctx, marketing.CouponFilter{Status: marketing.StatusActive, Type: typ, Limit: 50})
	if err != nil {
		return 0
	}
	var best int64
	for _, c := range coupons {
		if c == nil || c.Status != marketing.StatusActive || c.ID <= 0 {
			continue
		}
		if best == 0 || c.ID < best {
			best = c.ID
		}
	}
	return best
}

// withTenantStoreCtx injects (tenantID, storeID) as a system-sourced identity so
// the enrichment adapters can call domain services that resolve tenant/store via
// multitenancy (ARCH-01: identity from ctx, never from the item payload).
func withTenantStoreCtx(ctx context.Context, tenantID, storeID int64) context.Context {
	return multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: tenantID,
		StoreID:  storeID,
		Source:   multitenancy.SourceSystem,
	})
}
