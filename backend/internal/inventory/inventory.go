// Package inventory implements the stock domain (M1-S2, CONTRACT §9.3, BE-A-04).
// It owns the Batch aggregate (per-purchase stock lots), the Movement audit
// ledger, the BatchRepo port, and a Service that enforces multi-tenant row
// isolation (ARCH-01) plus FIFO (first-in-first-out) stock consumption. The
// repository is defined as an interface so the production pgx implementation
// and the in-memory dev/test implementation are interchangeable without
// touching the service or HTTP layers.
//
// Cross-domain access is defensive by contract: inventory NEVER imports
// catalog; it depends only on the narrow ProductLookuper interface, which the
// main thread adapts onto the real product service.
//
// Amounts are always int64 cents (分); floating point is forbidden.
package inventory

import (
	"context"
	"errors"
	"time"
)

// Batch is a stock lot created by a single purchase-in. Quantity is the CURRENT
// remaining amount; it decreases as FIFO consumption deducts from the lot.
// Money fields are int64 cents.
type Batch struct {
	ID, TenantID, StoreID, ProductID int64
	Quantity                         int64 // 当前余量
	InPriceCents                     int64
	ExpiryDate                       *time.Time // nil = 未设保质期
	ExpiryWarnDate                   *time.Time
	Status                           string // active|nearing|expired|cleared
	CreatedAt, UpdatedAt             time.Time
}

// Movement is one append-only stock ledger entry (强审计, 禁软删). Quantity is
// positive for inbound flows and negative for outbound flows. RefOrderID links
// a sale_out line to the order that caused it (0 = none); BatchID links it to
// the lot it drew from (0 = none).
type Movement struct {
	ID, TenantID, StoreID, ProductID int64
	BatchID                          int64  // 0 = 无关联批次
	Type                             string // purchase_in|sale_out|stocktake_adj|return_in|loss|transfer
	Quantity                         int64  // 正入负出
	RefOrderID                       int64  // 0 = 无关联订单
	OperatorID                       int64
	Note                             string
	CreatedAt                        time.Time
}

// Movement types.
const (
	MovementTypePurchaseIn = "purchase_in"
	MovementTypeSaleOut    = "sale_out"
)

// Batch statuses. active lots participate in FIFO and stock counts; cleared
// marks a lot that has been fully consumed; nearing/expired are assigned later
// by the BE-A-09 tags job (this domain only records the dates).
const (
	BatchStatusActive  = "active"
	BatchStatusCleared = "cleared"
)

// ErrNotFound is returned by repositories when no batch/product matches. A
// cross-tenant lookup also returns ErrNotFound (never "exists but forbidden")
// so an attacker cannot probe other tenants' stock.
var ErrNotFound = errors.New("inventory: batch/product not found")

// ErrInsufficientStock is returned by ConsumeFIFO when the total remaining
// quantity across all active lots of a product is below the requested amount.
// The service guarantees NO partial deduction when this is returned.
var ErrInsufficientStock = errors.New("inventory: insufficient stock")

// MovementFilter narrows a ListMovements query. TenantID is NOT part of the
// filter: it is resolved from ctx by every repo/service call (ARCH-01) and
// injected separately.
type MovementFilter struct {
	StoreID, ProductID int64
	Type               string
	Day                *time.Time // 非 nil 时按自然日过滤
	Limit, Offset      int
}

// BatchRepo persists batches and movements. Implementations MUST enforce tenant
// isolation: every method takes tenantID (resolved from ctx) and treats a batch
// owned by a different tenant as non-existent. The real implementation is
// pgx-backed (inventory_batches / inventory_movements tables with tenant_id row
// isolation); the in-memory implementation covers dev and unit tests until
// PostgreSQL is wired.
type BatchRepo interface {
	// CreateBatch inserts b and returns the persisted record (with ID/timestamps).
	CreateBatch(ctx context.Context, b *Batch) (*Batch, error)
	// GetBatch loads a batch by id within tenantID. ErrNotFound if absent or cross-tenant.
	GetBatch(ctx context.Context, tenantID, id int64) (*Batch, error)
	// UpdateBatch persists the new quantity/status of b, addressed by ID within b.TenantID.
	UpdateBatch(ctx context.Context, b *Batch) (*Batch, error)
	// ListActiveBatchesFIFO returns the active, non-empty lots of one product
	// within (tenantID, storeID), oldest first (FIFO), ordered by CreatedAt asc.
	ListActiveBatchesFIFO(ctx context.Context, tenantID, storeID, productID int64) ([]*Batch, error)
	// AppendMovement appends an immutable ledger entry and returns it (with ID).
	AppendMovement(ctx context.Context, m *Movement) (*Movement, error)
	// ListMovements returns ledger entries within tenantID matching f.
	ListMovements(ctx context.Context, tenantID int64, f MovementFilter) ([]*Movement, error)
}

// StockConsumer is an OPTIONAL repo capability for atomic FIFO deduction. It is
// NOT part of BatchRepo (CONTRACT §9.3 fixes BatchRepo's six methods verbatim):
// it lives behind a separate interface that the Service uses by type assertion
// when the repo provides it, falling back to a read-check-write sequence
// otherwise. The in-memory repo implements it under a single write lock, which
// is what makes concurrent ConsumeFIFO calls race-free (no oversell). A
// production pgx repo would implement it as one SELECT ... FOR UPDATE txn.
//
// Contract: consume exactly qty units of productID within (tenantID, storeID),
// oldest lot first, persisting each touched batch's new quantity/status and
// appending one sale_out movement (built by mkMovement, which stamps
// time/operator) per touched batch. If the total available quantity is below
// qty it MUST return ErrInsufficientStock and apply NOTHING (no partial
// deduction, no movement). On success it returns the updated batches in the
// order they were deducted.
type StockConsumer interface {
	ConsumeFIFOAtomic(ctx context.Context, tenantID, storeID, productID, qty int64, mkMovement func(batchID, deducted int64) *Movement) ([]*Batch, error)
}

// ProductLookuper is inventory's minimal view of the catalogue (防腐层). The
// main thread adapts it onto catalog.Service; unit tests inject a fake. The
// service uses it to confirm a product belongs to the ctx tenant before
// accepting stock for it.
type ProductLookuper interface {
	Exists(ctx context.Context, tenantID, productID int64) (bool, error)
}

// PurchaseInInput carries the caller-supplied fields for a stock-in. TenantID
// is NOT trusted from the caller (it comes from ctx); StoreID falls back to the
// store carried in ctx when zero.
type PurchaseInInput struct {
	StoreID, ProductID, Quantity, InPriceCents int64
	ExpiryDate                                 *time.Time
	Note                                       string
}
