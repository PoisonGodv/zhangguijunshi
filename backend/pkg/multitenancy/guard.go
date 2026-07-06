package multitenancy

// This file implements the runtime side of ARCH-01 (multi-tenant row-level
// isolation). Where context.go only PLUMBS the tenant through the call stack,
// guard.go ENFORCES it: every repository must obtain its tenant id through
// EnforceTenant / TenantGuard.Wrap before issuing any query, and background
// jobs must construct their context through SystemContext so they too declare
// the tenant they operate on instead of running "naked".
//
// Together with lint.go (the static guard), this makes a query without a
// resolved tenant impossible by construction at both compile and run time.

import (
	"context"
)

// EnforceTenant is the single runtime chokepoint every repository MUST call
// before issuing any database / cache / object-store query. It returns the
// resolved tenant_id (== merchant_id) or ErrNoTenant when context carries no
// tenant or a zero tenant_id.
//
// EnforceTenant is a semantic alias of TenantID: the name "EnforceTenant" is
// used at repository entry points to signal intent ("this call refuses to run
// without a tenant"), while TenantID is the lower-level accessor exposed on
// context.go. Implementations MUST stay equivalent so the two are
// interchangeable; the tests in guard_test.go assert that equivalence.
func EnforceTenant(ctx context.Context) (int64, error) {
	return TenantID(ctx)
}

// TenantGuard is a repository-scoped wrapper around EnforceTenant. Injecting a
// *TenantGuard into a repository (rather than letting it call the package-level
// function) makes the dependency explicit, testable, and visible in code review
// — reviewers can grep for `.Wrap(ctx)` to enumerate every tenant-aware entry
// point in the codebase.
//
// Usage:
//
//	type ProductRepo struct {
//	    guard *multitenancy.TenantGuard
//	    db    *pgxpool.Pool
//	}
//	func (r *ProductRepo) GetByID(ctx context.Context, id int64) (*Product, error) {
//	    tenantID, err := r.guard.Wrap(ctx)        // refuses to run without tenant
//	    if err != nil {
//	        return nil, err
//	    }
//	    return r.query(ctx, tenantID, id)
//	}
type TenantGuard struct{}

// NewGuard returns a TenantGuard. It performs no allocation of state, but
// returns a pointer so callers embed *TenantGuard (consistent with the
// constructor convention in CONTRACT.md §4.5).
func NewGuard() *TenantGuard {
	return &TenantGuard{}
}

// Wrap resolves and returns the tenant_id from ctx, delegating to EnforceTenant.
// A repository calls Wrap at the very top of each method; a non-nil error means
// the call MUST short-circuit and touch no backing store.
func (g *TenantGuard) Wrap(ctx context.Context) (tenantID int64, err error) {
	return EnforceTenant(ctx)
}

// SystemContext builds a context.Context for background / cron / system jobs.
//
// Background workers have no inbound HTTP request to carry a tenant, so without
// this helper they would be tempted to run "naked" queries — exactly the
// cross-tenant catastrophe ARCH-01 exists to prevent. SystemContext forces the
// job to DECLARE which tenant it operates on, and tags the context with
// Source=SourceSystem so downstream code (logs, audit, scopes) can distinguish
// system-originated writes from user-originated ones.
//
// A zero tenantID is rejected by EnforceTenant downstream just like any other
// context, so system jobs cannot accidentally fan out across all tenants.
func SystemContext(tenantID int64) context.Context {
	return WithContext(context.Background(), Context{
		TenantID: tenantID,
		Source:   SourceSystem,
	})
}
