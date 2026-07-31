// Package httpserver wires the three BFF route groups (/v1/wxshop,
// /v1/boss, /v1/admin) plus health and metrics endpoints (tech plan §6.1).
//
// Middleware order: recover → requestid → logging → (otelgin when enabled).
// Per-surface authentication (tech plan §6.2):
//   - wxshop: JWT always (login/refresh are public; me/logout/whoami authed).
//   - boss/admin: MockTenant in dev (dev_mock_header) or when no JWT is wired;
//     real JWT (Auth middleware) otherwise. The tenant is ALWAYS resolved from
//     verified context, never from the request body (tech plan §6.8.4).
package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/zhangguijunshi/backend/interfaces/http/handler"
	"github.com/zhangguijunshi/backend/interfaces/http/middleware"
	"github.com/zhangguijunshi/backend/internal/aiengine"
	"github.com/zhangguijunshi/backend/internal/aiengine/attribution"
	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/crm"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/inventory"
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
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
	"github.com/zhangguijunshi/backend/pkg/traceid"
)

// Deps bundles the router's external dependencies. A nil DB is tolerated
// (local dev without PostgreSQL) so the server still boots; readyz reports it.
// JWT/Store/AuthSvc are nil in the minimal M0-S1 test fixture; when JWT is
// present the wxshop surface authenticates via real tokens.
type Deps struct {
	Cfg     *config.Config
	DB      *db.DB
	JWT     *jwt.Manager
	Store   jwt.SessionStore
	AuthSvc *auth.AuthService
	// VersionStore caches each account's current token-version so the JWT
	// middleware can enforce the `ver` claim without a per-request DB hit
	// (CONTRACT §20.5). nil disables ver enforcement (best-effort allow — the
	// degraded mode for local dev without Redis). Bootstrap constructs a
	// RedisVersionStore (prod) or MemoryVersionStore (dev) and sets this field;
	// the router wraps it in an auth.VersionChecker for the Auth middleware.
	VersionStore auth.VersionStore
	// M1-S1 domain services (nil in the minimal test fixture).
	Catalog *catalog.Service
	Billing *billing.Service
	// M1-S2 domain services (nil in the minimal test fixture).
	Inventory  *inventory.Service
	Membership *membership.Service
	// M2-S1 domain services (nil in the minimal test fixture).
	Marketing *marketing.Service
	// M2-S2 domain services (nil in the minimal test fixture).
	Reach    *reach.Service
	Groupbuy *groupbuy.Service
	CRM      *crm.Service
	// M3-S1 AI engine (nil in the minimal test fixture).
	Strategy *strategy.Service
	Pipeline *aiengine.Pipeline
	Executor *exec.Executor
	// M3-S3 attribution read API (CONTRACT §14.7). nil disables the
	// /strategies/:id/attribution endpoint cleanly.
	Attribution attribution.Repo
	// M4-S1 payment / subscription (nil in the minimal test fixture).
	Payment      *payment.Service
	Subscription *subscription.Service
	// FE-MP-02 store domain (C-side 门店列表; nil in the minimal test fixture).
	StoreSvc *store.Service
	// M8-B platform-ops cross-tenant READ-ONLY aggregate (CONTRACT §23). nil in
	// the minimal test fixture → the /v1/platform group is not mounted.
	PlatformTenant *tenant.Service
	// COS is the object-storage client for boss image uploads (CONTRACT §19).
	// nil in dev (unconfigured) → the /assets endpoint reports "COS 未配置".
	COS *cos.Client
	// DevMock enables the X-Mock-Tenant header for boss/admin (dev only).
	DevMock bool
}

// New builds the gin engine with the global middleware chain and routes.
func New(deps Deps) *gin.Engine {
	env := ""
	if deps.Cfg != nil {
		env = deps.Cfg.App.Env
	}
	if env == "" || env == "dev" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(
		middleware.Recover(),
		middleware.RequestID(),
		middleware.Logging(),
	)
	if deps.Cfg != nil && deps.Cfg.OTel.Enable {
		r.Use(otelgin.Middleware(deps.Cfg.OTel.ServiceName))
	}

	// Infrastructure endpoints (no tenant, no auth).
	r.GET("/healthz", liveness)
	r.GET("/readyz", readiness(deps.DB))
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// M4-S1 WeChat-pay notify: unauthenticated callback (tenant recovered from
	// the out_trade_no in the body, CONTRACT §15.7).
	if deps.Payment != nil {
		ph := handler.NewPaymentHandler(deps.Payment)
		r.POST("/v1/wxshop/payment/notify", ph.Notify)
	}

	wireWxshop(r, deps)
	wireBoss(r, deps)
	wireAdmin(r, deps)
	wirePlatform(r, deps)
	return r
}

// wireBoss mounts the merchant (boss) BFF. The login endpoint is public
// (credential triple — no authenticated identity yet); everything else sits
// behind real JWT + RequireScene(boss) in prod, or the dev MockTenant fallback.
// Tenant/store are always resolved from the verified context, never the body.
func wireBoss(r *gin.Engine, deps Deps) {
	boss := r.Group("/v1/boss")

	// Public auth: registered BEFORE the authed middleware so it stays open.
	if deps.AuthSvc != nil {
		bh := handler.NewBossAuthHandler(deps.AuthSvc)
		boss.POST("/auth/login", bh.Login)
	}

	// Authed group: real JWT + RequireScene(boss) (prod) or MockTenant (dev).
	boss.Use(bossAdminAuthMW(deps, jwt.SceneBoss))
	boss.GET("/whoami", whoami)
	if deps.AuthSvc != nil {
		bh := handler.NewBossAuthHandler(deps.AuthSvc)
		boss.POST("/auth/logout", bh.Logout)
		// Change password (authed): account id + tenant from JWT ctx, never body.
		boss.POST("/auth/password", bh.ChangePassword)
	}
	// Boss image upload → COS (nil client ⇒ handler reports "COS 未配置").
	ah := handler.NewAssetHandler(deps.COS)
	boss.POST("/assets", ah.Upload)

	if deps.Catalog != nil {
		h := handler.NewCatalogHandler(deps.Catalog)
		boss.POST("/catalog/products", h.CreateProduct)
		boss.GET("/catalog/products", h.ListProducts)
		boss.GET("/catalog/products/barcode/:barcode", h.GetProductByBarcode)
		boss.GET("/catalog/products/:id", h.GetProduct)
		boss.PATCH("/catalog/products/:id/price", h.UpdateProductPrice)
	}
	if deps.Billing != nil {
		h := handler.NewBillingHandler(deps.Billing)
		boss.POST("/billing/orders", h.CreateOrder)
		boss.GET("/billing/orders/:id", h.GetOrder)
		boss.GET("/billing/summary/today", h.DailySummaryToday)
		boss.GET("/billing/summary/:date", h.DailySummary)
		boss.POST("/billing/orders/pending", h.CreatePendingOrder)
		boss.POST("/billing/orders/:id/pay", h.SimulatePay)
	}
	if deps.Inventory != nil {
		h := handler.NewInventoryHandler(deps.Inventory)
		boss.POST("/inventory/purchase", h.PurchaseIn)
		boss.GET("/inventory/stock", h.GetStock)
		boss.GET("/inventory/movements", h.ListMovements)
		boss.POST("/inventory/products/:id/consume", h.Consume)
	}
	if deps.Membership != nil {
		h := handler.NewMembershipHandler(deps.Membership)
		boss.POST("/membership/members", h.Join)
		boss.GET("/membership/members", h.ListMembers)
		boss.GET("/membership/members/:id", h.GetMember)
		boss.POST("/membership/members/:id/code", h.IssueCode)
		boss.POST("/membership/redeem", h.Redeem)
		boss.POST("/membership/members/:id/recharge", h.Recharge)
		boss.POST("/membership/members/:id/region-cap", h.SetRegionCap)
	}
	if deps.Marketing != nil {
		h := handler.NewMarketingHandler(deps.Marketing)
		boss.POST("/marketing/coupons", h.CreateCoupon)
		boss.GET("/marketing/coupons", h.ListCoupons)
		boss.GET("/marketing/coupons/:id", h.GetCoupon)
		boss.PATCH("/marketing/coupons/:id/status", h.UpdateStatus)
		boss.POST("/marketing/coupons/issue", h.Issue)
		boss.POST("/marketing/coupons/redeem", h.Redeem)
		boss.GET("/marketing/members/:id/coupons", h.ListMemberCoupons)
	}
	if deps.Reach != nil {
		h := handler.NewReachHandler(deps.Reach)
		boss.POST("/reach/send", h.Send)
		boss.GET("/reach/tasks", h.ListTasks)
		boss.GET("/reach/tasks/:id", h.GetTask)
		boss.POST("/reach/subscribe-auth", h.GrantSubscribe)
		boss.POST("/reach/unsubscribe", h.Unsubscribe)
		boss.POST("/reach/resubscribe", h.Resubscribe)
	}
	if deps.Groupbuy != nil {
		h := handler.NewGroupbuyHandler(deps.Groupbuy)
		boss.POST("/groupbuys", h.Create)
		boss.GET("/groupbuys/active", h.ListActive)
		boss.GET("/groupbuys/:id", h.Get)
		boss.POST("/groupbuys/:id/join", h.Join)
		boss.GET("/groupbuys/:id/participants", h.ListParticipants)
		boss.POST("/groupbuys/:id/cancel", h.Cancel)
	}
	if deps.CRM != nil {
		h := handler.NewCRMHandler(deps.CRM)
		boss.POST("/crm/groups", h.CreateGroup)
		boss.GET("/crm/groups", h.ListGroups)
		boss.GET("/crm/groups/:id", h.GetGroup)
		boss.POST("/crm/groups/:id/sop/advance", h.AdvanceSOP)
		boss.POST("/crm/groups/:id/members", h.AddGroupMembers)
		boss.POST("/crm/living-codes", h.CreateCode)
		boss.POST("/crm/living-codes/resolve", h.ResolveCode)
	}
	if deps.Pipeline != nil || deps.Executor != nil || deps.Strategy != nil {
		h := handler.NewAIEngineHandler(deps.Pipeline, deps.Executor, deps.Strategy, deps.Attribution, deps.Marketing, deps.Billing)
		boss.POST("/aiengine/today", h.Today)
		boss.GET("/aiengine/strategies", h.ListStrategies)
		boss.GET("/aiengine/strategies/:id", h.GetStrategy)
		boss.POST("/aiengine/strategies/:id/items/:itemID/execute", h.ExecuteItem)
		boss.POST("/aiengine/strategies/:id/items/:itemID/feedback", h.RecordFeedback)
		if deps.Attribution != nil {
			boss.GET("/aiengine/strategies/:id/attribution", h.GetAttribution)
		}
	}
	if deps.Payment != nil {
		ph := handler.NewPaymentHandler(deps.Payment)
		boss.POST("/payment/sub-merchants", ph.ApplySubMerchant)
		boss.GET("/payment/sub-merchants", ph.ListSubMerchants)
		boss.POST("/payment/sub-merchants/:id/query", ph.QuerySubMerchant)
		boss.POST("/payment/prepay", ph.Prepay)
		boss.GET("/payment/payments/:id", ph.GetPayment)
		boss.GET("/payment/ledger", ph.ListLedger)
	}
	if deps.Subscription != nil {
		sh := handler.NewSubscriptionHandler(deps.Subscription)
		boss.GET("/subscription", sh.GetSubscription)
		boss.POST("/subscription", sh.Subscribe)
		boss.POST("/subscription/check", sh.CheckFeature)
	}
}

// wireWxshop mounts the consumer BFF: public auth endpoints + an authed group
// (JWT when available, MockTenant fallback for the minimal test fixture).
func wireWxshop(r *gin.Engine, deps Deps) {
	wx := r.Group("/v1/wxshop")

	if deps.AuthSvc != nil {
		h := handler.NewAuthHandler(deps.AuthSvc)
		pub := wx.Group("/auth")
		pub.POST("/login", h.Login)
		pub.POST("/refresh", h.Refresh)
	}

	authed := wx.Group("")
	authed.Use(wxshopAuthMW(deps))
	authed.GET("/whoami", whoami)
	if deps.AuthSvc != nil {
		h := handler.NewAuthHandler(deps.AuthSvc)
		authed.GET("/auth/me", h.Me)
		authed.POST("/auth/logout", h.Logout)
	}

	// M-C-01 C 端业务接口 (CONTRACT §16): member profile / 余额 / 券包 / 动态会员码 /
	// 新人券 / 拼团 / 订单 / 拼团详情 / 券详情 / 积分明细 / 门店列表 (FE-MP-02 §16.11).
	// Membership+Marketing+Groupbuy are required to wire the group; Billing / Store
	// may be nil (→ /orders / /stores degrade to an empty list, §16.8).
	if deps.Membership != nil && deps.Marketing != nil && deps.Groupbuy != nil {
		h := handler.NewWxshopHandler(deps.Membership, deps.Marketing, deps.Groupbuy, deps.Billing, deps.StoreSvc)
		authed.GET("/coupons", h.Coupons)
		authed.GET("/coupons/:id", h.CouponDetail)
		authed.GET("/member/profile", h.Profile)
		authed.GET("/member/balance", h.Balance)
		authed.GET("/member/points", h.Points)
		authed.POST("/member/code", h.MemberCode)
		authed.POST("/member/claim-newcomer", h.ClaimNewcomer)
		authed.GET("/groupbuys/active", h.GroupbuysActive)
		authed.GET("/groupbuys/:id", h.GroupbuyDetail)
		authed.POST("/groupbuys/:id/join", h.GroupbuyJoin)
		authed.GET("/orders", h.Orders)
		authed.GET("/stores", h.Stores)
	}
}

// wxshopAuthMW prefers real JWT auth; it falls back to MockTenant only when no
// JWT manager is wired (minimal test fixture without the auth stack).
func wxshopAuthMW(deps Deps) gin.HandlerFunc {
	if deps.JWT != nil {
		return middleware.Auth(deps.JWT, deps.Store, auth.NewVersionChecker(deps.VersionStore))
	}
	return middleware.MockTenant()
}

// wireAdmin mounts the platform-ops (admin) BFF. Login is public; /me and
// logout sit behind real JWT + RequireScene(admin) in prod, MockTenant in dev.
// GET /v1/admin/me returns the caller's identity + permissions.menus (RBAC).
func wireAdmin(r *gin.Engine, deps Deps) {
	admin := r.Group("/v1/admin")

	// Public auth: registered BEFORE the authed middleware so it stays open.
	if deps.AuthSvc != nil {
		ah := handler.NewAdminAuthHandler(deps.AuthSvc)
		admin.POST("/auth/login", ah.Login)
	}

	// Authed group: real JWT + RequireScene(admin) (prod) or MockTenant (dev).
	admin.Use(bossAdminAuthMW(deps, jwt.SceneAdmin))
	admin.GET("/whoami", whoami)
	if deps.AuthSvc != nil {
		ah := handler.NewAdminAuthHandler(deps.AuthSvc)
		admin.GET("/me", ah.Me)
		admin.POST("/auth/logout", ah.Logout)
		// Change password (authed): account id + tenant from JWT ctx, never body.
		admin.POST("/auth/password", ah.ChangePassword)
	}
}

// wirePlatform mounts the platform-ops (运营后台) cross-tenant READ-ONLY BFF
// (M8-B, CONTRACT §23). It is the ONLY surface allowed to read across tenants.
//
// Auth: real JWT + RequireScene(admin) (platform_ops tokens are minted by
// AdminLogin, scene=admin) via bossAdminAuthMW, THEN RequireRole(platform_ops).
// The role guard is what makes this surface safe: a normal boss/admin merchant
// token carries owner/clerk/… roles and is rejected with 403 before reaching
// the handler. In dev_mock_header mode MockTenant sets no role, so the platform
// group is effectively closed — local platform debugging must use a real admin
// login (ops/ops12345) to obtain a platform_ops token. This is intentional
// security convergence, not a bug.
//
// The handler independently re-checks the role via tenant.Service (defense in
// depth); the cross-tenant scope comes solely from the verified platform role,
// never from a request-body tenant id (there is none).
func wirePlatform(r *gin.Engine, deps Deps) {
	if deps.PlatformTenant == nil {
		return
	}
	h := handler.NewPlatformTenantHandler(deps.PlatformTenant)
	plat := r.Group("/v1/platform")
	plat.Use(bossAdminAuthMW(deps, jwt.SceneAdmin))
	plat.Use(middleware.RequireRole(tenant.PlatformRole))
	plat.GET("/tenants", h.ListTenants)
}

// bossAdminAuthMW enforces real JWT + the given surface scene (RequireScene) in
// prod, or falls back to MockTenant in dev (dev_mock_header) / when no JWT is
// wired. The tenant is always resolved from the verified context. Composing
// Auth + RequireScene into one handler keeps the dev fallback a single branch.
func bossAdminAuthMW(deps Deps, scene string) gin.HandlerFunc {
	if deps.DevMock || deps.JWT == nil {
		return middleware.MockTenant()
	}
	authMW := middleware.Auth(deps.JWT, deps.Store, auth.NewVersionChecker(deps.VersionStore))
	sceneMW := middleware.RequireScene(scene)
	return func(c *gin.Context) {
		authMW(c)
		if c.IsAborted() {
			return
		}
		sceneMW(c)
	}
}

// liveness: the process is up. Always 200.
func liveness(c *gin.Context) {
	httpx.OK(c, gin.H{"status": "alive"})
}

// readiness: the process is ready to serve. DB probe when a pool is configured.
func readiness(database *db.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		checks := gin.H{}
		healthy := true
		if database == nil {
			checks["db"] = "skipped"
		} else {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			err := database.Ping(ctx)
			cancel()
			if err != nil {
				healthy = false
				checks["db"] = "down"
			} else {
				checks["db"] = "ok"
			}
		}
		if healthy {
			httpx.OK(c, checks)
			return
		}
		c.JSON(http.StatusServiceUnavailable, httpx.Envelope{
			Code:    errcode.ErrInternal.Code,
			Message: "not ready",
			Data:    checks,
			TraceID: traceid.TraceID(c.Request.Context()),
		})
	}
}

// whoami demonstrates the tenant guard over HTTP: it refuses any request whose
// context has no resolved tenant (ErrNoTenant → ErrTenantCross), proving the
// guard cannot be bypassed even by a direct handler.
func whoami(c *gin.Context) {
	tid, err := multitenancy.TenantID(c.Request.Context())
	if err != nil {
		httpx.Fail(c, errcode.ErrTenantCross)
		return
	}
	tc, _ := multitenancy.FromContext(c.Request.Context())
	source := "unresolved"
	if tc.Source != "" {
		source = tc.Source
	}
	httpx.OK(c, gin.H{"tenant_id": tid, "source": source})
}
