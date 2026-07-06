// Package multitenancy enforces tenant_id row-level isolation across the whole
// backend (tech plan ARCH-01). It is the single most important guard against
// catastrophic cross-tenant data leakage.
//
// The context plumbing here is the SHARED CONTRACT: every repository MUST call
// TenantID(ctx) (or EnforceTenant, see guard.go) before issuing any query, so
// that a query without a resolved tenant is impossible by construction.
package multitenancy

import (
	"context"
	"errors"
)

// Source identifies which client surface originated the request.
const (
	SourceConsumer = "consumer"
	SourceMerchant = "merchant"
	SourceAdmin    = "admin"
	SourceSystem   = "system" // background jobs / cron
)

// ErrNoTenant is returned when a repository call is made without a tenant in
// context. This is the runtime cross-tenant guard.
var ErrNoTenant = errors.New("multitenancy: tenant_id missing in context")

// ctxKey is an unexported key type so no other package can collide.
type ctxKey struct{}

// Context carries the resolved tenant/actor identity through the request
// lifecycle. merchant_id IS the tenant_id (see tech plan §2, internal/tenant).
type Context struct {
	TenantID int64  // == merchant_id; the row-level isolation key
	StoreID  int64  // optional scope within a tenant
	UserID   int64  // authenticated actor
	Role     string // RBAC role
	Source   string // consumer | merchant | admin | system
}

// WithContext returns a copy of ctx carrying c.
func WithContext(ctx context.Context, c Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext extracts the tenant Context. ok is false when absent.
func FromContext(ctx context.Context) (Context, bool) {
	c, ok := ctx.Value(ctxKey{}).(Context)
	return c, ok
}

// MustFromContext panics if no tenant is present; use only where absence is a
// programmer error (e.g. inside a repository that should never run without one).
func MustFromContext(ctx context.Context) Context {
	c, ok := FromContext(ctx)
	if !ok {
		panic(ErrNoTenant)
	}
	return c
}

// TenantID returns the tenant_id from context, or ErrNoTenant. Repositories MUST
// call this before any query — it is the single chokepoint that makes
// cross-tenant access impossible by construction.
func TenantID(ctx context.Context) (int64, error) {
	c, ok := FromContext(ctx)
	if !ok || c.TenantID == 0 {
		return 0, ErrNoTenant
	}
	return c.TenantID, nil
}
