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
	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/pkg/config"
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
	// M1-S1 domain services (nil in the minimal test fixture).
	Catalog *catalog.Service
	Billing *billing.Service
	// M1-S2 domain services (nil in the minimal test fixture).
	Inventory  *inventory.Service
	Membership *membership.Service
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

	wireWxshop(r, deps)
	for _, prefix := range []string{"/v1/boss", "/v1/admin"} {
		g := r.Group(prefix)
		g.Use(bossAdminAuthMW(deps))
		g.GET("/whoami", whoami)
	}
	wireBoss(r, deps)
	return r
}

// wireBoss mounts the merchant (boss) BFF domain routes for M1-S1: the product
// catalogue and the billing (checkout + daily flow) surfaces. All routes sit
// behind the boss auth middleware; tenant/store are resolved from the verified
// context (JWT in prod, X-Mock-* in dev), never from the request body.
func wireBoss(r *gin.Engine, deps Deps) {
	boss := r.Group("/v1/boss")
	boss.Use(bossAdminAuthMW(deps))

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
		boss.GET("/membership/members/:id", h.GetMember)
		boss.POST("/membership/members/:id/code", h.IssueCode)
		boss.POST("/membership/redeem", h.Redeem)
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
}

// wxshopAuthMW prefers real JWT auth; it falls back to MockTenant only when no
// JWT manager is wired (minimal test fixture without the auth stack).
func wxshopAuthMW(deps Deps) gin.HandlerFunc {
	if deps.JWT != nil {
		return middleware.Auth(deps.JWT, deps.Store)
	}
	return middleware.MockTenant()
}

// bossAdminAuthMW uses MockTenant in dev (dev_mock_header) or when JWT is not
// wired; otherwise it enforces real JWT auth. These surfaces have no login
// flow in M0-S2, so dev keeps the mock header demo alive.
func bossAdminAuthMW(deps Deps) gin.HandlerFunc {
	if deps.DevMock || deps.JWT == nil {
		return middleware.MockTenant()
	}
	return middleware.Auth(deps.JWT, deps.Store)
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
