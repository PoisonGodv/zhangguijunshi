// Package bootstrap owns the composition root for the modular monolith
// (CONTRACT §19). It builds the shared infra (Postgres pool, Redis client,
// JWT manager, session store) and wires every domain repo + service + the
// M3-S1 AI pipeline + the pregen scheduler once, so the api / worker / cron
// binaries all compose the SAME object graph instead of re-deriving it from
// three copies of cmd/api/main.go.
//
// Nothing in this package starts a server, a scheduler loop, or a consumer
// loop — BuildServices only constructs. The entry binaries decide what to
// RUN:
//   - cmd/api    → StartScheduler (dev in-process pregen) + serve HTTP
//   - cmd/cron   → NewScheduler + register the periodic jobs, run forever
//   - cmd/worker → jobqueue.NewConsumer(...).Run(ctx)
//
// Every external dependency degrades gracefully (CONTRACT §0): a dev box with
// no Postgres / Redis still boots — repos fall back to in-memory and the
// session store / job queue fall back to no-ops.
package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/internal/aiengine"
	"github.com/zhangguijunshi/backend/internal/aiengine/attribution"
	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/aiengine/pregen"
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
	"github.com/zhangguijunshi/backend/internal/tenant"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/cos"
	"github.com/zhangguijunshi/backend/pkg/db"
	"github.com/zhangguijunshi/backend/pkg/jobqueue"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/logger"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
	"github.com/zhangguijunshi/backend/pkg/redisx"
	"github.com/zhangguijunshi/backend/pkg/wxsdk"
)

// Infra is the external-infrastructure half of the composition root. Any of
// DB / RDB may be nil in local dev; Services + every binary must tolerate that.
type Infra struct {
	DB   *db.DB
	RDB  *redisx.Client
	JWT  *jwt.Manager
	Sess jwt.SessionStore
}

// Services is the fully-wired domain graph. Field names mirror the Deps
// struct consumed by interfaces/http so cmd/api can hand it over 1:1; the
// pregen scheduler and the jobs/queue handles are additionally exposed for the
// cron / worker binaries (CONTRACT §19).
type Services struct {
	Cfg *config.Config
	Log *zap.Logger

	// Infra (exposed so binaries can close them on shutdown).
	DB   *db.DB
	RDB  *redisx.Client
	JWT  *jwt.Manager
	Sess jwt.SessionStore

	// Auth + C-side auth (wxshop login) / boss+admin password login share one
	// AuthService; the AccountRepo drives the real BE-A-01 login.
	AuthSvc      *auth.AuthService
	AccountRepo  auth.AccountRepo
	VersionStore auth.VersionStore

	// Domain services.
	Catalog      *catalog.Service
	Billing      *billing.Service
	Inventory    *inventory.Service
	Membership   *membership.Service
	Marketing    *marketing.Service
	Reach        *reach.Service
	Groupbuy     *groupbuy.Service
	CRM          *crm.Service
	Strategy     *strategy.Service
	Pipeline     *aiengine.Pipeline
	Executor     *exec.Executor
	Attribution  attribution.Repo
	Payment      *payment.Service
	Subscription *subscription.Service
	StoreSvc     *store.Service
	// PlatformTenant is the platform-ops (运营后台) cross-tenant READ-ONLY
	// aggregate service (M8-B, CONTRACT §23). Memory-backed in dev (seeded demo
	// tenants), PG-backed in prod. nil only in the minimal test fixture.
	PlatformTenant *tenant.Service

	// COS is the object-storage client for boss image uploads. nil when the COS
	// credentials are absent (dev); the asset handler degrades to a clean error.
	COS *cos.Client

	// Scheduler is the pregen cron (batch / incremental / attribution /
	// bandit). It is CONSTRUCTED here but not started; the api process starts
	// it when cfg.Scheduler.RunInAPI is true (dev), the cron binary always.
	Scheduler *pregen.Scheduler

	// Tenants enumerates active tenants for the cron loops (real when a DB is
	// wired, dev-empty otherwise). Shared by cron's per-tenant jobs.
	Tenants TenantEnumerator

	// JobProducer enqueues async jobs onto the Redis Stream (nil-safe no-op when
	// Redis is absent). The worker / cron binaries build their own Consumer from
	// RDB + Cfg.JobQueue. CONTRACT §19.
	JobProducer *jobqueue.Producer
}

// TenantEnumerator is pregen's tenant-enumeration port (the 03:00 batch + the
// cron binary's per-tenant jobs iterate it). It is satisfied by the real
// DB-backed enumerator or the dev-empty stub.
type TenantEnumerator = pregen.TenantEnumerator

// BuildInfra opens the optional Postgres pool, the optional Redis client, the
// ES256 JWT manager and the session store. It never fails on a missing
// Postgres/Redis — those degrade to nil with a warning (CONTRACT §0).
func BuildInfra(ctx context.Context, cfg *config.Config, log *zap.Logger) (*Infra, error) {
	infra := &Infra{}

	// Postgres: optional at boot (local dev without Docker). readyz reports a
	// nil pool as "skipped".
	if cfg.Postgres.Host != "" {
		database, err := db.New(ctx, cfg.Postgres)
		if err != nil {
			log.Warn("db pool init failed; continuing without database", zap.Error(err))
		} else {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if perr := database.Ping(pctx); perr != nil {
				log.Warn("db ping failed; continuing without database",
					zap.String("dsn", database.DSN()), zap.Error(perr))
				database.Close()
			} else {
				log.Info("db connected", zap.String("dsn", database.DSN()))
				infra.DB = database
			}
			cancel()
		}
	}

	// Redis: optional. The session store and the job queue both degrade to
	// no-ops / in-memory when it is absent.
	if cfg.Redis.Host != "" {
		rdb, err := redisx.New(ctx, cfg.Redis)
		if err != nil {
			log.Warn("redis client init failed; continuing without redis", zap.Error(err))
		} else {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if perr := rdb.Ping(pctx); perr != nil {
				log.Warn("redis ping failed; continuing without redis",
					zap.String("addr", rdb.Addr()), zap.Error(perr))
				_ = rdb.Close()
			} else {
				log.Info("redis connected", zap.String("addr", rdb.Addr()))
				infra.RDB = rdb
			}
			cancel()
		}
	}

	// Auth stack (tech plan §6.2): ES256 JWT manager + a SessionStore (Redis
	// when available, else in-memory for local dev). All degrade gracefully — a
	// dev box with no Redis still issues real tokens.
	jwtMgr, err := buildJWTManager(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("init jwt: %w", err)
	}
	infra.JWT = jwtMgr
	infra.Sess = buildSessionStore(infra.RDB)

	return infra, nil
}

// BuildServices constructs the entire domain graph on top of infra. When
// infra is nil it is built first (so tests and the binaries share one call).
func BuildServices(ctx context.Context, cfg *config.Config, log *zap.Logger, infra *Infra) (*Services, error) {
	if infra == nil {
		var err error
		infra, err = BuildInfra(ctx, cfg, log)
		if err != nil {
			return nil, err
		}
	}
	database := infra.DB
	rdb := infra.RDB
	jwtMgr := infra.JWT
	sessStore := infra.Sess

	// M6 ②: log which external integrations came up with real credentials vs
	// degraded to fake/nil (WX / COS / LLM / WX Pay / PG / Redis). Nil-safe on
	// every arg; no secrets are logged, only presence/absence. One call covers
	// the api / worker / cron binaries (all go through BuildServices).
	LogClientDiagnostics(log, cfg, database, rdb)

	s := &Services{Cfg: cfg, Log: log, DB: database, RDB: rdb, JWT: jwtMgr, Sess: sessStore}

	// ---- Async job queue (CONTRACT §19). Producer is nil-safe: with no Redis
	// Enqueue degrades to a no-op log, so business flows are never blocked. ----
	var jobCmd redis.Cmdable
	if rdb != nil {
		jobCmd = rdb.Client
	}
	s.JobProducer = jobqueue.NewProducer(jobCmd, cfg.JobQueue)

	// ---- Auth: consumer (wxshop) + boss/admin password login (BE-A-01). ----
	// The AccountRepo switches to PG when a pool is wired, else the seeded
	// in-memory repo (demo owner + platform_ops for dev smoke).
	if jwtMgr != nil {
		wxOpts := []wxsdk.Option{}
		if cfg.WX.Code2SessionURL != "" {
			wxOpts = append(wxOpts, wxsdk.WithBaseURL(cfg.WX.Code2SessionURL))
		}
		if cfg.WX.Timeout > 0 {
			wxOpts = append(wxOpts, wxsdk.WithTimeout(cfg.WX.Timeout))
		}
		var consumerRepo auth.ConsumerRepo = auth.NewMemoryConsumerRepo()
		var accountRepo auth.AccountRepo = auth.NewMemoryAccountRepo()
		if database != nil {
			consumerRepo = auth.NewPGConsumerRepo(database.Pool())
			accountRepo = auth.NewPGAccountRepo(database.Pool())
		}
		platformTenantID := cfg.Multitenancy.PlatformTenantID
		if platformTenantID <= 0 {
			platformTenantID = 1
		}
		authSvc := auth.New(auth.Config{
			AppID:            cfg.WX.AppID,
			Secret:           cfg.WX.Secret,
			AccessTTL:        cfg.JWT.AccessTTL,
			RefreshTTL:       cfg.JWT.RefreshTTL,
			PlatformTenantID: platformTenantID,
		}, wxsdk.New(wxOpts...), jwtMgr, sessStore, consumerRepo)
		s.AuthSvc = authSvc.WithAccountRepo(accountRepo)
		s.AccountRepo = accountRepo

		// VersionStore (CONTRACT §20.6): caches each account's current
		// token-version so the Auth middleware can enforce the JWT `ver` claim
		// (revoke-on-password-change) WITHOUT a per-request DB hit. Redis when a
		// client is wired (cross-process safe under horizontal api scale), else
		// in-memory (single-process dev). nil-safe at the middleware layer: a
		// nil store disables the ver check (best-effort availability).
		var versionStore auth.VersionStore = auth.NewMemoryVersionStore()
		if jobCmd != nil {
			versionStore = auth.NewRedisVersionStore(jobCmd)
		}
		s.AuthSvc = s.AuthSvc.WithVersionStore(versionStore)
		s.VersionStore = versionStore
	}

	// ---- M1-S1 catalog + billing (CONTRACT §8). ----
	var catalogRepo catalog.ProductRepo = catalog.NewMemoryProductRepo()
	if database != nil {
		catalogRepo = catalog.NewPGProductRepo(database.Pool())
	}
	catalogSvc := catalog.NewService(catalogRepo)
	s.Catalog = catalogSvc

	// ---- M1-S2 inventory / billing / membership (CONTRACT §9). ----
	var inventoryBatchRepo inventory.BatchRepo = inventory.NewMemoryBatchRepo()
	if database != nil {
		inventoryBatchRepo = inventory.NewPGBatchRepo(database.Pool())
	}
	inventorySvc := inventory.New(inventoryBatchRepo, catalogProductLookuper{catalogSvc})
	s.Inventory = inventorySvc

	var billingOrderRepo billing.OrderRepo = billing.NewMemoryOrderRepo()
	if database != nil {
		billingOrderRepo = billing.NewPGOrderRepo(database.Pool())
	}
	billingSvc := billing.New(
		billingOrderRepo,
		catalogPricer{catalogSvc},
		billing.WithStockConsumer(inventoryStockConsumer{inventorySvc}),
	)
	s.Billing = billingSvc

	var membershipMemberRepo membership.MemberRepo = membership.NewMemoryMemberRepo()
	if database != nil {
		membershipMemberRepo = membership.NewPGMemberRepo(database.Pool())
	}
	membershipSvc := membership.New(membershipMemberRepo, membership.NewMemoryMemberCoder(nil))
	s.Membership = membershipSvc

	// ---- M2-S1 marketing (CONTRACT §10). ----
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
	s.Marketing = marketingSvc

	// ---- M2-S2 reach / groupbuy / crm (CONTRACT §11). ----
	freqSvc := freqcircuit.New()
	contentFilter := contentfilter.New()
	var reachRepo reach.ReachRepo = reach.NewMemoryReachRepo()
	if database != nil {
		reachRepo = reach.NewPGReachRepo(database.Pool())
	}
	reachOpts := []reach.Option{
		reach.WithSender(reach.NewMemorySender()),
		reach.WithFreqGate(freqGateAdapter{freqSvc}),
		reach.WithContentFilter(cfFilterAdapter{contentFilter}),
	}
	// Deferred (future-dated) sends enqueue onto the job queue only when Redis
	// is present; otherwise reach falls back to immediate delivery (dev).
	if rdb != nil {
		reachOpts = append(reachOpts, reach.WithScheduledProducer(reachScheduledProducer{prod: s.JobProducer}))
	}
	reachSvc := reach.New(reachRepo, reachOpts...)
	s.Reach = reachSvc

	var groupbuyRepo groupbuy.GroupbuyRepo = groupbuy.NewMemoryGroupbuyRepo()
	if database != nil {
		groupbuyRepo = groupbuy.NewPGGroupbuyRepo(database.Pool())
	}
	groupbuySvc := groupbuy.New(
		groupbuyRepo,
		groupbuy.WithMemberLookuper(membershipLookuper{membershipSvc}),
	)
	s.Groupbuy = groupbuySvc

	var crmRepo crm.Repo = crm.NewMemoryRepo()
	if database != nil {
		crmRepo = crm.NewPGRepo(database.Pool())
	}
	crmSvc := crm.New(crmRepo)
	s.CRM = crmSvc

	// ---- FE-MP-02 store domain (CONTRACT §16.11). ----
	var storeRepo store.StoreRepo = store.NewMemoryStoreRepo()
	if database != nil {
		storeRepo = store.NewPGStoreRepo(database.Pool())
	}
	storeSvc := store.New(storeRepo)
	s.StoreSvc = storeSvc

	// ---- M8-B platform-ops cross-tenant READ-ONLY aggregate (CONTRACT §23). ----
	// Memory-backed (seeded demo tenants) in dev, PG-backed in prod. Depends only
	// on the pool — per-tenant counts are correlated subqueries, so no other
	// domain repo is coupled in.
	var platformTenantRepo tenant.Repo = tenant.NewMemoryRepo()
	if database != nil {
		platformTenantRepo = tenant.NewPGRepo(database.Pool())
	}
	s.PlatformTenant = tenant.New(platformTenantRepo)

	// ---- COS object-storage client (boss image uploads; nil when unconfigured).
	s.COS = buildCOS(cfg.COS)

	// ---- M3-S1 AI engine + M3-S2/S3 pipeline + scheduler (CONTRACT §12/§13/§14). ----
	ai := buildAIEngine(cfg, database, catalogSvc, inventorySvc, membershipSvc, billingSvc, marketingSvc, reachSvc, groupbuySvc, storeSvc)
	s.Strategy = ai.strategy
	s.Pipeline = ai.pipeline
	s.Executor = ai.executor
	s.Attribution = ai.attrRepo
	s.Scheduler = ai.scheduler
	s.Tenants = ai.tenants

	// ---- M4-S1 payment + subscription (CONTRACT §15). Real V3 client when the
	// merchant credentials are configured, else the deterministic FakeWxPay. ----
	subscriptionSvc := subscription.NewService(subscription.NewMemoryStore())
	s.Subscription = subscriptionSvc
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
	ports := buildPaymentPorts(cfg, log)
	paymentSvc := payment.NewService(
		paySubMerchantRepo,
		payPaymentRepo,
		payLedgerRepo,
		payment.WithApplier(ports.applier),
		payment.WithPreparer(ports.preparer),
		payment.WithNotifyParser(ports.notifyParser),
		payment.WithFulfiller(paymentFulfiller{bill: billingSvc, mbr: membershipSvc, sub: subscriptionSvc}),
	)
	s.Payment = paymentSvc

	return s, nil
}

// Close releases the infra owned by the graph (DB pool, Redis client, session
// store). Safe to call with any nil members; binaries defer it.
func (s *Services) Close() {
	if s == nil {
		return
	}
	if s.DB != nil {
		s.DB.Close()
	}
	if s.RDB != nil {
		_ = s.RDB.Close()
	}
	if s.Sess != nil {
		_ = s.Sess.Close()
	}
}

// StartScheduler starts the in-process pregen cron. Called only by cmd/api when
// cfg.Scheduler.RunInAPI is true (dev single-binary); prod leaves it to the
// cron binary so the two never double-tick (CONTRACT §19).
func (s *Services) StartScheduler() {
	if s == nil || s.Scheduler == nil {
		return
	}
	go func() {
		if err := s.Scheduler.Start(); err != nil {
			s.Log.Warn("pregen scheduler start failed", zap.Error(err))
		}
	}()
}

// BuildLogger is a convenience for the binaries: structured logger + set as the
// package default, matching the previous cmd/api/main.go behaviour.
func BuildLogger(cfg *config.Config) (*zap.Logger, error) {
	log, err := logger.New(cfg.Log)
	if err != nil {
		return nil, err
	}
	logger.SetDefault(log)
	return log, nil
}

// buildCOS returns a COS client when the credentials are configured, else nil
// (the asset handler reports "COS 未配置" instead of panicking).
func buildCOS(cfg config.COSConfig) *cos.Client {
	if cfg.SecretID == "" || cfg.SecretKey == "" || (cfg.Bucket == "" && cfg.Endpoint == "") {
		return nil
	}
	return cos.New(cfg)
}

// tenantEnumerator selects the tenant enumeration strategy: a real DB-backed
// enumerator when a pool is wired (prod), else the dev-empty stub so every
// per-tenant loop is a clean no-op in local dev.
func tenantEnumerator(database *db.DB) pregen.TenantEnumerator {
	if database != nil {
		return dbTenantEnumerator{pool: database.Pool()}
	}
	return tenantEnumeratorAdapter{}
}

// dbTenantEnumerator reads the active tenant ids from the tenants table
// (migration 000001). It drives the pregen 03:00 batch and the cron binary's
// per-tenant jobs (groupbuy finalize / dataquality). Suspended/closed/soft-
// deleted tenants are excluded.
type dbTenantEnumerator struct{ pool *pgxpool.Pool }

func (e dbTenantEnumerator) ActiveTenantIDs(ctx context.Context) ([]int64, error) {
	if e.pool == nil {
		return nil, nil
	}
	rows, err := e.pool.Query(ctx, `SELECT id FROM tenants WHERE status = 'active' AND deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: enumerate tenants: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("bootstrap: scan tenant id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// reachScheduledProducer adapts the jobqueue Producer onto reach's
// ScheduledSendProducer port (CONTRACT §19). It serialises the SendInput into a
// JobReachDueSend job; the tenant is recovered from ctx (reach.Send has already
// enforced it) so the worker can rebuild a tenant-scoped context on delivery.
type reachScheduledProducer struct{ prod *jobqueue.Producer }

// EnqueueScheduledSend implements reach.ScheduledSendProducer. taskID is the id
// of the pending ReachTask row reach.Send persisted just before enqueueing
// (CONTRACT §20 / M6 ③); it is stamped into the payload so the worker finalizes
// THAT row on delivery instead of inserting a duplicate.
func (a reachScheduledProducer) EnqueueScheduledSend(ctx context.Context, in reach.SendInput, sendAt time.Time, taskID int64) error {
	payload, err := json.Marshal(reach.ScheduledSendPayload{In: in, TaskID: taskID})
	if err != nil {
		return fmt.Errorf("reach scheduled send: marshal payload: %w", err)
	}
	tenantID, _ := multitenancy.TenantID(ctx)
	return a.prod.Enqueue(ctx, jobqueue.Job{
		Type:     jobqueue.JobReachDueSend,
		Payload:  payload,
		TenantID: tenantID,
		RunAt:    sendAt,
	})
}
