// Command api is the single HTTP entry point for the modular monolith
// (tech plan §1.2.1 / §6.1.3). It loads layered config, initialises structured
// logging, optionally opens the DB pool, and serves the three BFF groups with
// graceful shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	httpserver "github.com/zhangguijunshi/backend/interfaces/http"
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
	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/compliance/contentfilter"
	"github.com/zhangguijunshi/backend/internal/compliance/freqcircuit"
	"github.com/zhangguijunshi/backend/internal/crm"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/internal/payment"
	"github.com/zhangguijunshi/backend/internal/reach"
	"github.com/zhangguijunshi/backend/internal/store"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/internal/subscription"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/logger"
	"github.com/zhangguijunshi/backend/pkg/redisx"
	"github.com/zhangguijunshi/backend/pkg/wxsdk"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.WithEnv(envOr("ZG_ENV", "dev")))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, err := logger.New(cfg.Log)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()
	logger.SetDefault(log)

	log.Info("starting zhanggui backend",
		zap.String("env", cfg.App.Env),
		zap.String("version", cfg.App.Version),
	)

	// The DB pool is optional at boot: in local dev without Docker/Postgres we
	// log a warning and continue with a nil pool; readyz reports it as skipped.
	var database *db.DB
	if cfg.Postgres.Host != "" {
		database, err = db.New(context.Background(), cfg.Postgres)
		if err != nil {
			log.Warn("db pool init failed; continuing without database", zap.Error(err))
			database = nil
		} else {
			pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if perr := database.Ping(pctx); perr != nil {
				log.Warn("db ping failed; continuing without database",
					zap.String("dsn", database.DSN()), zap.Error(perr))
				database.Close()
				database = nil
			} else {
				log.Info("db connected", zap.String("dsn", database.DSN()))
			}
			cancel()
		}
	}

	// Auth stack (tech plan §6.2): ES256 JWT manager + a SessionStore (Redis
	// when available, else in-memory for local dev) + the consumer auth service.
	// All degrade gracefully — a dev box with no Redis still issues real tokens.
	jwtMgr, err := buildJWTManager(cfg, log)
	if err != nil {
		return fmt.Errorf("init jwt: %w", err)
	}
	sessStore := buildSessionStore(context.Background(), cfg, log)

	var authSvc *auth.AuthService
	if jwtMgr != nil {
		wxOpts := []wxsdk.Option{}
		if cfg.WX.Code2SessionURL != "" {
			wxOpts = append(wxOpts, wxsdk.WithBaseURL(cfg.WX.Code2SessionURL))
		}
		if cfg.WX.Timeout > 0 {
			wxOpts = append(wxOpts, wxsdk.WithTimeout(cfg.WX.Timeout))
		}
		var consumerRepo auth.ConsumerRepo = auth.NewMemoryConsumerRepo()
		if database != nil {
			consumerRepo = auth.NewPGConsumerRepo(database.Pool())
		}
		authSvc = auth.New(auth.Config{
			AppID:      cfg.WX.AppID,
			Secret:     cfg.WX.Secret,
			AccessTTL:  cfg.JWT.AccessTTL,
			RefreshTTL: cfg.JWT.RefreshTTL,
		}, wxsdk.New(wxOpts...), jwtMgr, sessStore, consumerRepo)
	}

	// M1-S1 domain services (CONTRACT §8). Repositories are in-memory until
	// PostgreSQL is wired (the services depend only on the repo interfaces, so a
	// pgx implementation drops in later without touching the HTTP layer).
	var catalogRepo catalog.ProductRepo = catalog.NewMemoryProductRepo()
	if database != nil {
		catalogRepo = catalog.NewPGProductRepo(database.Pool())
	}
	catalogSvc := catalog.NewService(catalogRepo)

	// M1-S2 domain services (CONTRACT §9). Inventory enforces product-tenant
	// ownership through the catalog adapter; billing consumes stock FIFO after a
	// committed sale; membership issues/validates dynamic member codes.
	var inventoryBatchRepo inventory.BatchRepo = inventory.NewMemoryBatchRepo()
	if database != nil {
		inventoryBatchRepo = inventory.NewPGBatchRepo(database.Pool())
	}
	inventorySvc := inventory.New(inventoryBatchRepo, catalogProductLookuper{catalogSvc})
	var billingOrderRepo billing.OrderRepo = billing.NewMemoryOrderRepo()
	if database != nil {
		billingOrderRepo = billing.NewPGOrderRepo(database.Pool())
	}
	billingSvc := billing.New(
		billingOrderRepo,
		catalogPricer{catalogSvc},
		billing.WithStockConsumer(inventoryStockConsumer{inventorySvc}),
	)
	var membershipMemberRepo membership.MemberRepo = membership.NewMemoryMemberRepo()
	if database != nil {
		membershipMemberRepo = membership.NewPGMemberRepo(database.Pool())
	}
	membershipSvc := membership.New(membershipMemberRepo, membership.NewMemoryMemberCoder(nil))

	// M2-S1 domain services (CONTRACT §10). Marketing depends on the
	// membership (member existence) and margin (gross-margin rail) anti-
	// corruption ports, adapted here at the composition root. The margin guard
	// fetches the tenant's products from catalog and rejects any coupon that
	// would drop a product below the floor; an empty catalog yields no
	// validation (best-effort).
	var marketingCouponRepo marketing.CouponRepo = marketing.NewMemoryCouponRepo()
	if database != nil {
		marketingCouponRepo = marketing.NewPGCouponRepo(database.Pool())
	}
	marketingSvc := marketing.New(
		marketingCouponRepo,
		marketing.WithMemberLookuper(membershipLookuper{membershipSvc}),
		marketing.WithMarginGuard(catalogMarginGuard{
			guard:      margin.Guard{MarginFloorPct: defaultMarginFloorPct},
			catalogSvc: catalogSvc,
		}),
	)

	// M2-S2 domain services (CONTRACT §11). Reach is the single outbound send
	// channel: its three anti-corruption ports (Sender / FreqGate /
	// ContentFilter) are adapted here. The frequency circuit and content filter
	// were built in M2-S1 but left unwired — they now gate every reach.Send.
	// Groupbuy reuses the membership lookuper; crm is standalone.
	freqSvc := freqcircuit.New()
	contentFilter := contentfilter.New()
	var reachRepo reach.ReachRepo = reach.NewMemoryReachRepo()
	if database != nil {
		reachRepo = reach.NewPGReachRepo(database.Pool())
	}
	reachSvc := reach.New(
		reachRepo,
		reach.WithSender(reach.NewMemorySender()),
		reach.WithFreqGate(freqGateAdapter{freqSvc}),
		reach.WithContentFilter(cfFilterAdapter{contentFilter}),
	)
	var groupbuyRepo groupbuy.GroupbuyRepo = groupbuy.NewMemoryGroupbuyRepo()
	if database != nil {
		groupbuyRepo = groupbuy.NewPGGroupbuyRepo(database.Pool())
	}
	groupbuySvc := groupbuy.New(
		groupbuyRepo,
		groupbuy.WithMemberLookuper(membershipLookuper{membershipSvc}),
	)
	var crmRepo crm.Repo = crm.NewMemoryRepo()
	if database != nil {
		crmRepo = crm.NewPGRepo(database.Pool())
	}
	crmSvc := crm.New(crmRepo)

	// FE-MP-02 store domain (CONTRACT §16.11). C-side 门店列表 read model over
	// the stores table (migration 000001 + 000016 phone/group_qr/active). dev:
	// in-memory repo seeds two demo stores under tenant_id=1; production reads
	// the real stores rows.
	var storeRepo store.StoreRepo = store.NewMemoryStoreRepo()
	if database != nil {
		storeRepo = store.NewPGStoreRepo(database.Pool())
	}
	storeSvc := store.New(storeRepo)

	// M3-S1 AI engine (CONTRACT §12). Five-stage pipeline + ActionExecutor.
	// gateway.NewFromConfig builds a placeholder (empty-providers) gateway when
	// no provider API key is configured (dev); agent.EnhanceOrSkip then degrades
	// to rule-only (no_provider) — the pipeline still produces cold-start / rule
	// advice. The shared margin guard (defaultMarginFloorPct) is injected into
	// both rule and exec so the three defense-in-depth checks share one source.
	gw, _ := gateway.NewFromConfig(cfg.AI)
	emb := gateway.NewEmbedderFromConfig(cfg.AI)
	var strategyRepo strategy.StrategyRepo = strategy.NewMemoryStrategyRepo()
	if database != nil {
		strategyRepo = strategy.NewPGStrategyRepo(database.Pool())
	}
	strategySvc := strategy.New(strategyRepo)
	cacheL1 := cache.NewMemoryCache()
	semCache := semantic.NewMemoryCache(emb)
	signalCollector := aisignal.NewCollector("fruit", "", signalLookuperAdapter{
		catalogSvc:    catalogSvc,
		inventorySvc:  inventorySvc,
		membershipSvc: membershipSvc,
		billingSvc:    billingSvc,
	})
	ruleEngine := rule.NewEngine(rule.DefaultSeeds(), margin.Guard{MarginFloorPct: defaultMarginFloorPct})
	// M3-S3 attribution + bandit + playbook (CONTRACT §14.7). dev: in-memory
	// repos; the coupon/visit/groupbuy/repurchase ports adapt the real domain
	// services (groupbuy→strategy linkage is wired via StatsByStrategy so
	// groupbuy_success is attributed back by strategy_id, §14.9). A nil
	// rewardSource/itemResolver would disable crediting; here we wire
	// them so the 02:00 BanditUpdate cron credits real rewards.
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
	// M3-S2 pregen scheduler + M3-S3 attribution/bandit crons (CONTRACT §13.3.5
	// / §14.7). dev: tenantEnumeratorAdapter returns empty so all cron jobs are
	// no-ops until production wires a real tenant store; the cron loop still
	// starts (incremental channel + attribution/bandit ready for manual calls).
	tenEnum := tenantEnumeratorAdapter{}
	scheduler := pregen.NewScheduler(pipeline, cacheL1, semCache, tenEnum,
		pregen.WithAttribution(attrRepo, attrComputer),
		pregen.WithBanditLearner(banditLearner),
		pregen.WithItemEnumerator(strategyItemEnumeratorAdapter{strategySvc}),
	)
	go func() {
		if err := scheduler.Start(); err != nil {
			log.Warn("pregen scheduler start failed", zap.Error(err))
		}
	}()

	// M4-S1 payment + subscription (CONTRACT §15.7). dev: FakeWxPay (deterministic,
	// never-network) wires all three WeChat-Pay V3 ports; the composite
	// paymentFulfiller dispatches a successful payment to billing/membership/
	// subscription by Subject. subscription defaults to free (empty MemoryStore)
	// until a boss subscribes. 生产: replace FakeWxPay with the real V3 client and
	// the Memory repos with PG repos once PG + wx credentials are configured.
	fakeWxPay := payment.NewFakeWxPay()
	subscriptionSvc := subscription.NewService(subscription.NewMemoryStore())
	var (
		paySubMerchantRepo payment.SubMerchantRepo = payment.NewMemorySubMerchantRepo()
		payPaymentRepo     payment.PaymentRepo     = payment.NewMemoryPaymentRepo()
		payLedgerRepo      payment.LedgerRepo      = payment.NewMemoryLedgerRepo()
	)
	if database != nil {
		pool := database.Pool()
		paySubMerchantRepo = payment.NewPGSubMerchantRepo(pool)
		payPaymentRepo = payment.NewPGPaymentRepo(pool)
		payLedgerRepo = payment.NewPGLedgerRepo(pool)
	}
	paymentSvc := payment.NewService(
		paySubMerchantRepo,
		payPaymentRepo,
		payLedgerRepo,
		payment.WithApplier(fakeWxPay),
		payment.WithPreparer(fakeWxPay),
		payment.WithNotifyParser(fakeWxPay),
		payment.WithFulfiller(paymentFulfiller{bill: billingSvc, mbr: membershipSvc, sub: subscriptionSvc}),
	)

	srv := &http.Server{
		Addr: fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler: httpserver.New(httpserver.Deps{
			Cfg:          cfg,
			DB:           database,
			JWT:          jwtMgr,
			Store:        sessStore,
			AuthSvc:      authSvc,
			Catalog:      catalogSvc,
			Billing:      billingSvc,
			Inventory:    inventorySvc,
			Membership:   membershipSvc,
			Marketing:    marketingSvc,
			Reach:        reachSvc,
			Groupbuy:     groupbuySvc,
			CRM:          crmSvc,
			Strategy:     strategySvc,
			Pipeline:     pipeline,
			Executor:     executor,
			Attribution:  attrRepo,
			Payment:      paymentSvc,
			Subscription: subscriptionSvc,
			StoreSvc:     storeSvc,
			DevMock:      cfg.Multitenancy.DevMockHeader,
		}),
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
	}

	go func() {
		log.Info("http listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("http server crashed", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	if database != nil {
		database.Close()
	}
	if sessStore != nil {
		_ = sessStore.Close()
	}
	log.Info("shutdown complete")
	return nil
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}

// catalogPricer adapts *catalog.Service to billing.ProductPricer (CONTRACT §8.3
// anti-corruption layer): billing never imports catalog, so this thin adapter
// translates the priced view at the composition root.
type catalogPricer struct{ svc *catalog.Service }

func (p catalogPricer) GetByID(ctx context.Context, tenantID, id int64) (*billing.PricedProduct, error) {
	prod, err := p.svc.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// Defence-in-depth: the service already scopes to the ctx tenant, but the
	// billing contract requires the returned product to belong to tenantID.
	if prod.TenantID != tenantID {
		return nil, catalog.ErrNotFound
	}
	return &billing.PricedProduct{
		ID:         prod.ID,
		TenantID:   prod.TenantID,
		StoreID:    prod.StoreID,
		Name:       prod.Name,
		Unit:       prod.Unit,
		PriceCents: prod.PriceCents,
		CostCents:  prod.CostCents,
		Status:     prod.Status,
	}, nil
}

// catalogProductLookuper adapts *catalog.Service to inventory.ProductLookuper
// (CONTRACT §9.3 anti-corruption): inventory never imports catalog, so this thin
// adapter answers "does this product belong to the ctx tenant" at the
// composition root. A cross-tenant or unknown product reads as not-exists.
type catalogProductLookuper struct{ svc *catalog.Service }

func (l catalogProductLookuper) Exists(ctx context.Context, tenantID, productID int64) (bool, error) {
	prod, err := l.svc.GetByID(ctx, productID)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if prod.TenantID != tenantID {
		return false, nil
	}
	return true, nil
}

// inventoryStockConsumer adapts *inventory.Service to billing.StockConsumer
// (CONTRACT §9.3 anti-corruption): billing never imports inventory, so this
// thin adapter forwards FIFO deduction at the composition root. Errors are
// mapped to a plain error (billing treats any failure as a reconciliation gap).
type inventoryStockConsumer struct{ svc *inventory.Service }

func (c inventoryStockConsumer) ConsumeFIFO(ctx context.Context, storeID, productID, qty, refOrderID int64) error {
	_, err := c.svc.ConsumeFIFO(ctx, storeID, productID, qty, refOrderID)
	return err
}

// defaultMarginFloorPct is the gross-margin floor (10%) the marketing margin
// guard enforces on coupon issuance. Configurable via config in a later
// milestone; a constant keeps M2-S1 self-contained.
const defaultMarginFloorPct = 10

// membershipLookuper adapts *membership.Service to marketing.MemberLookuper
// (CONTRACT §10.3 anti-corruption): marketing never imports membership, so this
// thin adapter answers "does this member exist in this tenant" at the
// composition root. A cross-tenant or unknown member reads as not-exists.
type membershipLookuper struct{ svc *membership.Service }

func (l membershipLookuper) Exists(ctx context.Context, tenantID, memberID int64) (bool, error) {
	m, err := l.svc.GetMember(ctx, memberID)
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if m.TenantID != tenantID {
		return false, nil
	}
	return true, nil
}

// catalogMarginGuard adapts margin.Guard + *catalog.Service to
// marketing.MarginGuard (CONTRACT §10.3 anti-corruption). It fetches the
// tenant's products, adapts the coupon params to a margin.CouponRule, and
// delegates to the pure margin.ValidateCoupon. Best-effort: a catalog read
// failure skips the check (the coupon is allowed) rather than blocking issuance.
type catalogMarginGuard struct {
	guard      margin.Guard
	catalogSvc *catalog.Service
}

func (g catalogMarginGuard) ValidateCoupon(ctx context.Context, c *marketing.Coupon) error {
	prods, err := g.catalogSvc.List(ctx, catalog.ProductFilter{Limit: 500})
	if err != nil {
		return nil
	}
	products := make([]margin.ProductView, 0, len(prods))
	for _, p := range prods {
		products = append(products, margin.ProductView{
			ID:         p.ID,
			CostCents:  p.CostCents,
			PriceCents: p.PriceCents,
		})
	}
	rule := margin.CouponRule{
		Type:             c.Type,
		ThresholdCents:   c.Params.ThresholdCents,
		OffCents:         c.Params.OffCents,
		DiscountPermille: c.Params.DiscountPermille,
	}
	return margin.ValidateCoupon(g.guard, rule, products)
}

// freqGateAdapter adapts *freqcircuit.Service to reach.FreqGate (CONTRACT §11.3.2
// anti-corruption): reach never imports freqcircuit, so this thin adapter forwards
// the three-period frequency check at the composition root. The string channel is
// cast to freqcircuit.Channel — the two share the same spelling (wework_group /
// wework_1v1 / subscribe_msg / sms).
type freqGateAdapter struct{ svc *freqcircuit.Service }

func (a freqGateAdapter) Allow(ctx context.Context, channel string) (bool, string, error) {
	return a.svc.Allow(ctx, freqcircuit.Channel(channel))
}

// cfFilterAdapter adapts *contentfilter.Filter to reach.ContentFilter (CONTRACT
// §11.3.2 anti-corruption): reach never imports contentfilter, so this thin
// adapter translates the DFA + absolutism Result at the composition root.
type cfFilterAdapter struct{ f *contentfilter.Filter }

func (a cfFilterAdapter) Do(text string) reach.ContentResult {
	r := a.f.Do(text)
	return reach.ContentResult{
		Text:     r.Text,
		Passed:   r.Passed,
		HitWords: r.HitWords,
		Replaced: r.Replaced,
	}
}

// buildJWTManager constructs the ES256 signer/verifier. It loads a PEM private
// key from cfg.JWT.KeyFile when set; otherwise it derives a deterministic dev
// key (logged loudly) so a dev server still issues verifiable tokens with no
// configuration.
func buildJWTManager(cfg *config.Config, log *zap.Logger) (*jwt.Manager, error) {
	var priv *ecdsa.PrivateKey
	if cfg.JWT.KeyFile != "" {
		p, err := jwt.LoadPrivateKeyFile(cfg.JWT.KeyFile)
		if err != nil {
			return nil, err
		}
		priv = p
		log.Info("jwt key loaded from file", zap.String("key_file", cfg.JWT.KeyFile))
	} else {
		p, err := jwt.DevKey(jwt.DevSeed)
		if err != nil {
			return nil, err
		}
		priv = p
		log.Warn("using derived dev JWT key — DO NOT USE IN PRODUCTION",
			zap.String("kid", cfg.JWT.KeyID),
			zap.String("hint", "set jwt.key_file to a PEM ECDSA P-256 private key in staging/prod"),
		)
	}
	return jwt.New(jwt.Config{
		PrivateKey: priv,
		KeyID:      cfg.JWT.KeyID,
		Issuer:     cfg.JWT.Issuer,
		AccessTTL:  cfg.JWT.AccessTTL,
	})
}

// buildSessionStore returns a Redis-backed SessionStore when Redis is reachable,
// otherwise an in-memory store. Both satisfy jwt.SessionStore.
func buildSessionStore(ctx context.Context, cfg *config.Config, log *zap.Logger) jwt.SessionStore {
	if cfg.Redis.Host == "" {
		return jwt.NewMemorySessionStore()
	}
	rdb, err := redisx.New(ctx, cfg.Redis)
	if err != nil {
		log.Warn("redis client init failed; using memory session store", zap.Error(err))
		return jwt.NewMemorySessionStore()
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pctx); err != nil {
		log.Warn("redis ping failed; using memory session store",
			zap.String("addr", rdb.Addr()), zap.Error(err))
		_ = rdb.Close()
		return jwt.NewMemorySessionStore()
	}
	log.Info("redis connected; using redis session store", zap.String("addr", rdb.Addr()))
	return jwt.NewRedisSessionStore(rdb.Client)
}
