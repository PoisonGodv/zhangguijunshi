package bootstrap

// aiengine_build.go constructs the M3-S1 five-stage AI pipeline, the exec
// executor, and the pregen/attribution/bandit scheduler as one self-contained
// unit (CONTRACT §12/§13/§14). Extracted verbatim from the previous
// cmd/api/main.go so the api / cron / worker binaries share the identical
// pipeline wiring.

import (
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

	pipeline := aiengine.NewPipeline(
		signalCollector, ruleEngine, agent.New(gw), converge.NewTop3(),
		output.NewEncoder(strategySvc), cacheL1,
		aiengine.WithBusinessType("fruit"),
		aiengine.WithSemanticCache(semCache),
		aiengine.WithPlaybookStore(playbookStore),
		aiengine.WithBanditLearner(banditLearner),
	)
	executor := exec.NewExecutor(
		strategySvc, margin.Guard{MarginFloorPct: defaultMarginFloorPct},
		couponIssuerAdapter{marketingSvc}, msgSenderAdapter{reachSvc},
		priceUpdaterAdapter{catalogSvc}, groupbuyOpenerAdapter{groupbuySvc},
	)

	// pregen scheduler + attribution/bandit crons. tenantEnumerator returns the
	// real DB-backed enumerator when a pool is wired, else the dev-empty stub.
	tenEnum := tenantEnumerator(database)
	scheduler := pregen.NewScheduler(pipeline, cacheL1, semCache, tenEnum,
		pregen.WithAttribution(attrRepo, attrComputer),
		pregen.WithBanditLearner(banditLearner),
		pregen.WithItemEnumerator(strategyItemEnumeratorAdapter{strategySvc}),
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
