package store

// Service is the C-side store read orchestrator (FE-MP-02, CONTRACT
// §16.11). Every method first resolves the tenant from ctx via
// multitenancy.EnforceTenant — tenant is NEVER trusted from a request field
// (ARCH-01). The service depends only on the StoreRepo interface, so the
// whole flow is deterministic in unit tests (in-memory repo).

import (
	"context"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service is the store orchestrator.
type Service struct {
	repo StoreRepo
}

// New constructs a store Service.
func New(repo StoreRepo) *Service {
	return &Service{repo: repo}
}

// ListStores returns the active stores of the ctx tenant. Cross-tenant stores
// are never visible (repo enforces this); a tenant with no stores returns an
// empty slice (absence == cross-tenant, never a leak — ARCH-01).
func (s *Service) ListStores(ctx context.Context) ([]*Store, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListStores(ctx, tenantID)
}
