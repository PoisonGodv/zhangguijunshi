// Package httpserver wires the three BFF route groups (/v1/wxshop,
// /v1/boss, /v1/admin) plus health and metrics endpoints (tech plan §6.1).
//
// Middleware order: recover → requestid → logging → (otelgin when enabled).
// Group-specific middleware (MockTenant now; JWT+RBAC in M0-S2) is applied per
// route group. The tenant is ALWAYS resolved from verified context, never from
// the request body (tech plan §6.8.4).
package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/zhangguijunshi/backend/interfaces/http/middleware"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
	"github.com/zhangguijunshi/backend/pkg/traceid"
)

// Deps bundles the router's external dependencies. A nil DB is tolerated
// (local dev without PostgreSQL) so the server still boots; readyz reports it.
type Deps struct {
	Cfg *config.Config
	DB  *db.DB
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

	// Three BFF route groups (tech plan §6.1.1). Each carries a dev-only
	// MockTenant middleware + a /whoami demo that proves the multitenancy
	// guard is the authority on tenant resolution through HTTP.
	for _, prefix := range []string{"/v1/wxshop", "/v1/boss", "/v1/admin"} {
		g := r.Group(prefix)
		g.Use(middleware.MockTenant())
		g.GET("/whoami", whoami)
	}
	return r
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
	httpx.OK(c, gin.H{"tenant_id": tid, "source": "mock-tenant-header"})
}
