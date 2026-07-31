// Package tenant implements the platform-ops (运营后台) cross-tenant READ-ONLY
// domain (M8-B). Every other domain in this codebase is single-tenant: its
// repos resolve the tenant from ctx via multitenancy.EnforceTenant and refuse
// to run without one (ARCH-01 row-level isolation). Platform operations need the
// opposite — an aggregate view ACROSS all tenants (list every merchant + a few
// per-tenant counts) — without weakening that isolation for anyone else.
//
// The isolation boundary is enforced at THREE layers so a cross-tenant read can
// never be reached by a normal boss/admin/consumer caller:
//
//  1. HTTP router (wired by the main thread): the /v1/platform/* group sits
//     behind RequireRole("platform_ops") — only platform tokens reach the
//     handler (see wiringNeeded in the M8-B delivery notes).
//  2. Service: ListTenants takes an EXPLICIT PlatformCaller (never trusts the
//     single-tenant ctx identity) and refuses unless Caller.Role ==
//     "platform_ops" AND the ctx Source is platform/system. A regular tenant
//     ctx is rejected with errcode.ErrForbidden even if it somehow reaches here.
//  3. Repo: the cross-tenant query methods (ListAllTenants) are named
//     distinctly from per-tenant methods and refuse to run unless the ctx
//     carries a platform/system Source — the Service stamps this via
//     multitenancy.SystemContext before calling. They NEVER reuse a
//     tenant-filtered query to "bypass" isolation.
//
// The domain is strictly read-only: there are no create/update/delete methods.
// It mirrors the store/membership convention (Memory + PG dual implementation,
// nil-safe degrade, pgxmock tests) and imports neither catalog nor membership —
// the per-tenant counts are aggregated directly in SQL (PG) or from seeded
// fields (memory), keeping the package anti-corruption-clean.
package tenant

import (
	"context"
	"time"
)

// PlatformRole is the ONLY RBAC role allowed to read cross-tenant data. It
// matches the role seeded for the platform-ops account (internal/auth) and the
// role the router's RequireRole guard checks. Kept as a named constant so the
// Service and the wiring share one source of truth.
const PlatformRole = "platform_ops"

// Tenant is the platform-facing read model of a merchant (the tenants table,
// migration 000001). It deliberately exposes only operational fields — license
// numbers, LBS and other compliance/internal columns are NOT surfaced here.
type Tenant struct {
	ID        int64     `json:"id"` // == tenant_id, the global isolation key
	Name      string    `json:"name"`
	Status    string    `json:"status"` // active | suspended | closed
	Plan      string    `json:"plan"`   // free | pro | flagship
	CreatedAt time.Time `json:"created_at"`
}

// TenantSummary is one row of the platform aggregate view: the tenant's base
// identity plus per-tenant counts. Counts that a given deployment cannot supply
// degrade to 0 (never an error) — see the per-field notes. All counts are
// non-negative; the PG repo computes them with correlated subqueries so a
// tenant with no rows in a table still appears with a 0 for that table.
type TenantSummary struct {
	Tenant
	ProductCount int `json:"product_count"` // products (not soft-deleted); 0 if none
	MemberCount  int `json:"member_count"`  // members (not soft-deleted); 0 if none
	OrderCount   int `json:"order_count"`   // orders (orders are never soft-deleted); 0 if none
	StoreCount   int `json:"store_count"`   // stores (not soft-deleted); 0 if none
}

// Filter narrows ListTenants. The zero value lists every tenant (all statuses,
// no name filter), first page. Status / Query are exact-/substring-match
// respectively; empty means "no constraint".
type Filter struct {
	Status string // "" = all; else exact match on tenants.status
	Query  string // "" = all; else case-insensitive substring match on name
	Limit  int    // <=0 → defaultPageSize
	Offset int    // <0 → 0
}

// defaultPageSize bounds a page when Filter.Limit is unset/non-positive.
const defaultPageSize = 20

// maxPageSize caps an explicit Filter.Limit so a caller cannot request an
// unbounded scan of the tenants table.
const maxPageSize = 200

// Normalize returns a copy of f with Limit clamped to [1, maxPageSize]
// (defaulting to defaultPageSize) and Offset floored at 0. It keeps the paging
// contract identical across the memory repo, the PG repo and the Service.
func (f Filter) Normalize() Filter {
	if f.Limit <= 0 {
		f.Limit = defaultPageSize
	}
	if f.Limit > maxPageSize {
		f.Limit = maxPageSize
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f
}

// ListResult is one page of TenantSummary plus the total matching count
// (independent of Offset/Limit, so the admin pager can render "共 N 条").
type ListResult struct {
	Items []*TenantSummary `json:"items"`
	Total int64            `json:"total"`
}

// Repo persists the platform cross-tenant read model. The real implementation
// is pgx-backed (tenants table + correlated counts); an in-memory
// implementation covers dev and tests.
//
// EVERY method here is cross-tenant by design and therefore MUST be reachable
// only from a platform/system context. The method names (ListAllTenants) are
// deliberately distinct from the per-tenant List* methods other domains use, so
// a reviewer can grep for the cross-tenant surface and no one accidentally
// reuses a tenant-filtered query to "bypass" isolation (ARCH-01).
type Repo interface {
	// ListAllTenants returns one page of tenant summaries across ALL tenants
	// matching f, ordered by id, plus the total matching count. It is a
	// platform-level operation: implementations refuse to run unless ctx
	// carries a platform/system Source (see platformCtx). A nil/absent source
	// yields an empty result rather than a cross-tenant leak.
	ListAllTenants(ctx context.Context, f Filter) (ListResult, error)
}
