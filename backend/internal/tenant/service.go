package tenant

// service.go is the platform-ops read orchestrator (M8-B). Unlike every other
// domain Service — which resolves the tenant from ctx via
// multitenancy.EnforceTenant and operates on ONE tenant — this Service operates
// ACROSS all tenants and therefore inverts the usual trust model:
//
//   - It does NOT read the caller's tenant from ctx. The caller's identity is
//     passed EXPLICITLY as a PlatformCaller; a missing/wrong role is rejected
//     before any repo call. This is the "explicit platform identifier" the
//     cross-tenant design requires — a normal single-tenant ctx can never, by
//     itself, authorize a cross-tenant read.
//   - It stamps a platform identity (Source=system, TenantID=0) onto a derived
//     ctx before calling the repo. The repo's cross-tenant methods refuse to run
//     without that platform Source (ensurePlatform), so even a caller that
//     reaches the repo directly with a tenant ctx gets an empty/forbidden
//     result, never a leak (ARCH-01).
//
// The Service depends only on the Repo interface, so the whole flow is
// deterministic in unit tests (in-memory repo).

import (
	"context"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// PlatformCaller is the EXPLICIT platform identity a caller must present to
// read cross-tenant data. It is passed by value (not read from ctx) so the
// authorization decision is visible at the call site and cannot be smuggled in
// via a forged single-tenant context. Role is the gate: only PlatformRole is
// admitted. UserID is carried for audit/logging only and does not affect access.
type PlatformCaller struct {
	Role   string // must equal PlatformRole to be admitted
	UserID int64  // platform actor, audit only (0 = unknown)
}

// Service is the platform cross-tenant read orchestrator.
type Service struct {
	repo Repo
}

// New constructs a platform tenant Service over repo.
func New(repo Repo) *Service {
	return &Service{repo: repo}
}

// ListTenants returns one page of cross-tenant summaries (all merchants + their
// per-tenant counts) for an authorized platform caller.
//
// Authorization is explicit and defense-in-depth:
//  1. caller.Role must equal PlatformRole, else ErrForbidden (40301 / 403). The
//     caller is passed in by the handler from verified JWT claims — never from
//     the request body or the single-tenant ctx identity.
//  2. The ctx handed to the repo is re-stamped with a platform identity
//     (platformCtx), which the repo independently re-checks (ensurePlatform).
//
// f is normalized (page size clamped) before hitting the repo. A nil repo
// degrades to an empty page rather than panicking (nil-safe philosophy).
func (s *Service) ListTenants(ctx context.Context, caller PlatformCaller, f Filter) (ListResult, error) {
	if caller.Role != PlatformRole {
		return ListResult{}, errcode.ErrForbidden
	}
	if s == nil || s.repo == nil {
		return ListResult{Items: []*TenantSummary{}}, nil
	}
	return s.repo.ListAllTenants(s.platformCtx(ctx, caller), f.Normalize())
}

// platformCtx derives a context that carries the platform identity instead of
// the caller's single-tenant one: TenantID=0 (no row-level scope) and
// Source=system (the platform/system marker the repo requires). It is built on
// top of the inbound ctx so request-scoped values (trace id, logger, deadline)
// survive; only the multitenancy identity is overridden. The repo's
// ensurePlatform re-validates this Source, so the stamp and the check stay in
// lockstep.
func (s *Service) platformCtx(ctx context.Context, caller PlatformCaller) context.Context {
	return multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: 0, // explicitly cross-tenant: no row-level scope
		UserID:   caller.UserID,
		Role:     caller.Role,
		Source:   multitenancy.SourceSystem,
	})
}
