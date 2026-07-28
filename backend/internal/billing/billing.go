// Package billing implements the order/checkout + daily-flow-summary domain
// (tech plan §5.3.3 orders/order_items, WBS BE-A-05/BE-A-06). It records paid
// orders with per-item name/unit/price/cost SNAPSHOTS (so historical profit
// attribution survives later catalog price edits) and aggregates a tenant-
// scoped DailySummary (gross/net/cost/profit + top sellers) for the boss
// dashboard.
//
// The package is deliberately infrastructure-light and anti-corruption
// guarded: it NEVER imports internal/catalog. The only view of a product it
// needs is the minimal PricedProduct mirror, fetched through the ProductPricer
// interface (the main thread adapts catalog.Service to it at wiring time). The
// OrderRepo is an interface with a concurrency-safe in-memory implementation
// so the whole flow runs in unit tests without PostgreSQL.
package billing

import (
	"context"
	"errors"
	"time"
)

// OrderItem is a single line on an order. Price/cost/name/unit are SNAPSHOTS
// taken at checkout time; they are NOT live references into the catalog, so a
// later price change never rewrites history.
type OrderItem struct {
	ProductID    int64
	NameSnapshot string
	Unit         string
	Qty          int64
	PriceCents   int64 // 成交单价(快照)
	CostCents    int64 // 成本快照(归因利润)
}

// Order is a checkout record. M1-S1 only ever creates Status=paid orders
// (paid == total, discount == 0); the other statuses exist for the schema and
// later refund/void flows.
type Order struct {
	ID, TenantID, StoreID, MemberID      int64 // MemberID 可 0(散客)
	OrderNo                              string
	Channel                              string // in_store|wxshop|wework|delivery
	Status                               string // pending|paid|refunded|voided
	TotalCents, PaidCents, DiscountCents int64
	PaymentMethod                        string
	OperatorID                           int64
	Items                                []OrderItem
	PaidAt                               *time.Time
	CreatedAt, UpdatedAt                 time.Time
}

// ErrNotFound is returned when no order matches (including cross-tenant reads,
// which are indistinguishable from absence).
var ErrNotFound = errors.New("billing: order not found")

// OrderRepo persists orders. The real implementation will be pgx-backed
// (orders/order_items tables with tenant_id row isolation); an in-memory
// implementation covers dev and tests until PostgreSQL is wired.
type OrderRepo interface {
	// Create persists an order together with its Items and returns the
	// persisted record (id/timestamps allocated by the repo).
	Create(ctx context.Context, o *Order) (*Order, error)

	// GetByID loads an order (with Items) by id within tenantID. ErrNotFound
	// when absent or owned by another tenant.
	GetByID(ctx context.Context, tenantID, id int64) (*Order, error)

	// ListByDate returns the tenant's orders for storeID created on the
	// natural day containing day (any time-of-day). storeID<=0 means all
	// stores of the tenant.
	ListByDate(ctx context.Context, tenantID, storeID int64, day time.Time) ([]*Order, error)

	// OrderTotalByMembers sums the total_cents of the tenant's PAID orders
	// whose MemberID is in memberIDs and whose paid time falls in [since,
	// until] (inclusive both ends). Empty memberIDs or since.After(until)
	// returns 0 with nil error. Cross-tenant orders are excluded by tenantID.
	OrderTotalByMembers(ctx context.Context, tenantID int64, memberIDs []int64, since, until time.Time) (int64, error)

	// RepurchaseMembers returns the count of DISTINCT members in memberIDs
	// that have at least one PAID order in [since, until] (inclusive both
	// ends) within tenantID. Empty memberIDs or since.After(until) returns
	// 0 with nil error. Cross-tenant orders are excluded.
	RepurchaseMembers(ctx context.Context, tenantID int64, memberIDs []int64, since, until time.Time) (int, error)
}

// PricedProduct is billing's minimal anti-corruption mirror of a catalog
// product (same field shape as catalog.Product). The main thread adapts
// catalog.Service into a ProductPricer at wiring time; billing unit tests use
// a fake pricer. billing never imports internal/catalog.
type PricedProduct struct {
	ID, TenantID, StoreID int64
	Name, Unit            string
	PriceCents, CostCents int64
	Status                string
}

// ProductPricer is the subset of the catalog the billing service needs to
// price an order line. Implementations must enforce tenant isolation.
type ProductPricer interface {
	// GetByID returns the priced view of a product. ErrNotFound (or an
	// equivalent not-found error) when the product does not exist within
	// tenantID.
	GetByID(ctx context.Context, tenantID, id int64) (*PricedProduct, error)
}

// CreateOrderItem is one requested line (which product, how many).
type CreateOrderItem struct {
	ProductID, Qty int64
}

// CreateOrderInput is the checkout request. Channel/PaymentMethod default to
// in_store/cash when empty.
type CreateOrderInput struct {
	StoreID       int64
	MemberID      int64
	Channel       string // 空默认 in_store
	PaymentMethod string // 空默认 cash
	Items         []CreateOrderItem
}

// DailySummary aggregates one tenant+store's paid orders for a natural day.
type DailySummary struct {
	TenantID, StoreID int64
	Date              string // YYYY-MM-DD
	OrderCount        int
	GrossCents        int64 // Σ total
	NetCents          int64 // Σ paid
	CostCents         int64 // Σ 各项 qty*cost
	ProfitCents       int64 // Net - Cost
	TopProducts       []TopProduct
}

// TopProduct is a best-seller row (by quantity sold).
type TopProduct struct {
	ProductID  int64
	Name       string
	Qty        int64
	GrossCents int64
}
