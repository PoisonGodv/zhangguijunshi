// Package catalog implements the product catalogue domain (M1-S1, CONTRACT §8.3).
// It owns the Product aggregate, the ProductRepo port, and a Service that
// enforces multi-tenant row isolation (ARCH-01) plus the pricing business rules
// (price >= cost >= 0). The repository is defined as an interface so the
// production pgx implementation and the in-memory dev/test implementation are
// interchangeable without touching the service or HTTP layers.
//
// Amounts are always int64 cents (分); floating point is forbidden.
package catalog

import (
	"context"
	"errors"
	"time"
)

// Product is the catalogue aggregate. All money fields are int64 cents.
type Product struct {
	ID, TenantID, StoreID         int64
	Barcode, Name, Category, Unit string
	CostCents, PriceCents         int64
	ImageURL                      string
	ShelfLifeDays                 int // 0 = 未设置
	Tags                          []string
	Status                        string // "active" | "archived"
	CreatedAt, UpdatedAt          time.Time
}

// ErrNotFound is returned by repositories when no product matches. A cross-tenant
// lookup also returns ErrNotFound (never "exists but forbidden") so an attacker
// cannot probe other tenants' catalogue.
var ErrNotFound = errors.New("catalog: product not found")

// ErrConflict is returned by repositories when a uniqueness rule is violated
// ((tenant_id, store_id, barcode) for non-empty barcode, or
// (tenant_id, store_id, name, unit)). The service maps it to errcode.ErrParam.
var ErrConflict = errors.New("catalog: product uniqueness conflict")

// ProductFilter narrows a List query. TenantID is NOT part of the filter: it is
// resolved from ctx by every repo/service call (ARCH-01) and injected separately.
type ProductFilter struct {
	StoreID  int64  // >0 时按店过滤
	Query    string // 名称/条码模糊
	Category string
	Limit    int // <=0 用默认 50
	Offset   int
}

// ProductRepo persists products. Implementations MUST enforce tenant isolation:
// every method takes tenantID (resolved from ctx) and treats a product owned by a
// different tenant as non-existent. The real implementation is pgx-backed
// (products table with tenant_id row isolation); the in-memory implementation
// covers dev and unit tests until PostgreSQL is wired.
type ProductRepo interface {
	// Create inserts p and returns the persisted record (with ID/timestamps).
	Create(ctx context.Context, p *Product) (*Product, error)
	// Update replaces the whole product addressed by p.ID within p.TenantID.
	Update(ctx context.Context, p *Product) (*Product, error)
	// GetByID loads a product by id within tenantID. ErrNotFound if absent or cross-tenant.
	GetByID(ctx context.Context, tenantID, id int64) (*Product, error)
	// GetByBarcode loads a product by barcode within (tenantID, storeID). ErrNotFound if absent or cross-tenant.
	GetByBarcode(ctx context.Context, tenantID, storeID int64, barcode string) (*Product, error)
	// List returns products within tenantID matching f.
	List(ctx context.Context, tenantID int64, f ProductFilter) ([]*Product, error)
	// UpdateTags replaces the tag set (临期/滞销/高毛利) of the product addressed
	// by productID within tenantID and returns the updated record. ErrNotFound
	// if absent or cross-tenant. Used by the BE-A-09 tag write-back (the
	// dataquality job); it touches ONLY the tags column, never price/cost.
	UpdateTags(ctx context.Context, tenantID, productID int64, tags []string) (*Product, error)
}
