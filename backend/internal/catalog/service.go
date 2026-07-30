package catalog

// Service is the catalogue use-case layer. Every method first resolves the
// tenant from ctx via multitenancy.EnforceTenant (ARCH-01): tenant NEVER comes
// from a request body or parameter. The store scope falls back to the store
// carried in ctx (multitenancy.Context.StoreID) when the caller does not pass
// one explicitly.

import (
	"context"
	"errors"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service exposes catalogue use-cases.
type Service struct {
	repo ProductRepo
}

// NewService constructs a Service on top of repo.
func NewService(repo ProductRepo) *Service {
	return &Service{repo: repo}
}

// CreateInput carries the caller-supplied fields for a new product. TenantID and
// StoreID are NOT trusted from the caller: tenant comes from ctx, and store falls
// back to ctx.StoreID when this is zero.
type CreateInput struct {
	StoreID       int64 // 0 -> use ctx.StoreID
	Barcode       string
	Name          string
	Category      string
	Unit          string // empty -> "个"
	CostCents     int64
	PriceCents    int64
	ImageURL      string
	ShelfLifeDays int
	Tags          []string
	Status        string // empty -> "active"
}

// validatePrice enforces the pricing business rule: price >= cost >= 0.
func validatePrice(costCents, priceCents int64) error {
	if costCents < 0 || priceCents < 0 || priceCents < costCents {
		return errcode.ErrParam
	}
	return nil
}

// Create validates the input, applies defaults, and persists a product under the
// ctx tenant. A uniqueness violation (barcode or name+unit already taken within
// the store) is mapped to ErrParam.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Product, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}

	// Name must be non-empty. Unit may be empty (defaulted to 个 below); the
	// business rule requires a non-empty unit, which the default satisfies.
	if in.Name == "" {
		return nil, errcode.ErrParam
	}
	if err := validatePrice(in.CostCents, in.PriceCents); err != nil {
		return nil, err
	}

	storeID := in.StoreID
	if storeID == 0 {
		if mc, ok := multitenancy.FromContext(ctx); ok {
			storeID = mc.StoreID
		}
	}

	unit := in.Unit
	if unit == "" {
		unit = "个"
	}
	status := in.Status
	if status == "" {
		status = "active"
	}

	p := &Product{
		TenantID:      tenantID,
		StoreID:       storeID,
		Barcode:       in.Barcode,
		Name:          in.Name,
		Category:      in.Category,
		Unit:          unit,
		CostCents:     in.CostCents,
		PriceCents:    in.PriceCents,
		ImageURL:      in.ImageURL,
		ShelfLifeDays: in.ShelfLifeDays,
		Tags:          in.Tags,
		Status:        status,
	}
	created, err := s.repo.Create(ctx, p)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return created, nil
}

// UpdatePrice re-prices a product, enforcing price >= cost >= 0. It loads the
// product under the ctx tenant (cross-tenant -> ErrNotFound) and writes the full
// record back.
func (s *Service) UpdatePrice(ctx context.Context, id, costCents, priceCents int64) (*Product, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := validatePrice(costCents, priceCents); err != nil {
		return nil, err
	}

	p, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	p.CostCents = costCents
	p.PriceCents = priceCents
	updated, err := s.repo.Update(ctx, p)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return updated, nil
}

// GetByID loads a product within the ctx tenant. Cross-tenant -> ErrNotFound.
func (s *Service) GetByID(ctx context.Context, id int64) (*Product, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return p, nil
}

// GetByBarcode loads a product by barcode within the ctx tenant and store scope
// (store falls back to ctx.StoreID).
func (s *Service) GetByBarcode(ctx context.Context, barcode string) (*Product, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	var storeID int64
	if mc, ok := multitenancy.FromContext(ctx); ok {
		storeID = mc.StoreID
	}
	p, err := s.repo.GetByBarcode(ctx, tenantID, storeID, barcode)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return p, nil
}

// List returns products within the ctx tenant matching f.
func (s *Service) List(ctx context.Context, f ProductFilter) ([]*Product, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	products, err := s.repo.List(ctx, tenantID, f)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return products, nil
}

// UpdateTags replaces a product's BE-A-09 tag set (临期/滞销/高毛利) within the
// ctx tenant and returns the updated record. It touches ONLY the tags column —
// never price/cost/name — so it cannot trip the pricing rule or the uniqueness
// indexes. Cross-tenant / unknown -> ErrNotFound; no tenant in ctx -> ErrNoTenant.
func (s *Service) UpdateTags(ctx context.Context, productID int64, tags []string) (*Product, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.repo.UpdateTags(ctx, tenantID, productID, tags)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return p, nil
}

// SetProductTags writes a computed tag set onto one product. It satisfies
// dataquality.TagWriter, so catalog.Service can be handed straight to the BE-A-09
// tagging step with no adapter. Per ARCH-01 the tenant is resolved from ctx (the
// cron injects it via multitenancy.WithContext); the explicit tenantID is the
// port's contract and is asserted to match the ctx tenant so a mismatched caller
// fails closed with ErrNotFound (tenantID==0 defers entirely to ctx).
func (s *Service) SetProductTags(ctx context.Context, tenantID, productID int64, tags []string) error {
	ctxTenant, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return err
	}
	if tenantID != 0 && tenantID != ctxTenant {
		return ErrNotFound
	}
	_, err = s.repo.UpdateTags(ctx, ctxTenant, productID, tags)
	if err != nil {
		return mapRepoErr(err)
	}
	return nil
}

// mapRepoErr translates repository errors into errcode sentinels. ErrNotFound is
// passed through so the HTTP layer can emit 404; a uniqueness conflict becomes
// ErrParam; anything else is an internal error.
func mapRepoErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	case errors.Is(err, ErrConflict):
		return errcode.ErrParam
	default:
		return errcode.ErrInternal
	}
}
