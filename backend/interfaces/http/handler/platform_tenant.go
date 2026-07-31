package handler

// PlatformTenantHandler exposes the platform-ops (运营后台) cross-tenant
// READ-ONLY tenant endpoints (M8-B). It is the HTTP face of internal/tenant.
//
// Authorization model: the router gates the whole /v1/platform group behind
// RequireRole("platform_ops") (wired by the main thread), so by the time a
// request reaches this handler the caller is already a verified platform token.
// The handler does NOT re-implement that guard; instead it builds an explicit
// tenant.PlatformCaller from the verified JWT claims and hands it to the
// Service, which independently re-checks the role (defense in depth). The
// handler never trusts a tenant id from the request body — there is none; the
// cross-tenant scope comes solely from the platform role.
//
// The handler only decodes query params, maps errors and encodes responses; all
// business logic lives in tenant.Service.

import (
	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/tenant"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// PlatformTenantHandler serves the platform cross-tenant read endpoints.
type PlatformTenantHandler struct {
	svc *tenant.Service
}

// NewPlatformTenantHandler constructs a PlatformTenantHandler over svc.
func NewPlatformTenantHandler(svc *tenant.Service) *PlatformTenantHandler {
	return &PlatformTenantHandler{svc: svc}
}

// ListTenants returns one page of the cross-tenant aggregate view (all
// merchants + per-tenant counts).
// GET /v1/platform/tenants?status=&query=&limit=&offset=
//
// The caller's platform identity is taken from the verified JWT claims stored by
// the Auth middleware (currentClaims) — never from the request. A request that
// somehow arrives without claims (mis-wired route) is rejected as forbidden
// rather than treated as platform.
func (h *PlatformTenantHandler) ListTenants(c *gin.Context) {
	caller := h.caller(c)
	if caller.Role == "" {
		// No verified identity reached the handler → refuse. In a correctly
		// wired router RequireRole has already admitted only platform_ops; this
		// is the belt to the guard's braces.
		httpx.Fail(c, errcode.ErrForbidden)
		return
	}

	f := tenant.Filter{
		Status: c.Query("status"),
		Query:  c.Query("query"),
		Limit:  int(queryInt64(c, "limit")),
		Offset: int(queryInt64(c, "offset")),
	}
	res, err := h.svc.ListTenants(c.Request.Context(), caller, f)
	if err != nil {
		// A non-platform role that slipped past the router guard surfaces here
		// as ErrForbidden (40301 / 403); every other error maps via FailError.
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{
		"items": res.Items,
		"total": res.Total,
		"count": len(res.Items),
	})
}

// caller builds the explicit platform identity from the verified JWT claims the
// Auth middleware stored. When no claims are present (route mis-wired without
// auth) it returns a zero PlatformCaller whose empty Role makes the Service
// (and this handler) refuse. UserID is parsed from the subject for audit only.
func (h *PlatformTenantHandler) caller(c *gin.Context) tenant.PlatformCaller {
	claims := currentClaims(c)
	if claims == nil {
		return tenant.PlatformCaller{}
	}
	return tenant.PlatformCaller{
		Role:   claims.Role,
		UserID: parseSubject(claims.Subject),
	}
}

// parseSubject extracts the numeric id from a subject like "u_2001" (mirrors
// middleware.parseSubjectID; duplicated here so the handler package need not
// import middleware, keeping the dependency direction one-way). Unknown shapes
// yield 0 (audit-only field; access is gated on Role, not UserID).
func parseSubject(sub string) int64 {
	for i := 0; i < len(sub); i++ {
		if sub[i] == '_' {
			var n int64
			for _, r := range sub[i+1:] {
				if r < '0' || r > '9' {
					return 0
				}
				n = n*10 + int64(r-'0')
			}
			return n
		}
	}
	return 0
}
