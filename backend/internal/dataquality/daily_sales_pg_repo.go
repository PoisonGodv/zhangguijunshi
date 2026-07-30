package dataquality

// daily_sales_pg_repo.go is the pgx-backed SalesRepo over daily_product_sales
// (migration 000019). It follows the catalog.PGPool / attribution.PGPool idiom:
// a narrow PGPool surface, a nil pool that degrades to a safe no-op (writes
// return 0, reads return ok=false) rather than panicking, and tenant isolation
// baked into every statement's WHERE tenant_id.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface PGSalesRepo needs: UpsertDailySales uses
// Exec and LastSaleDate uses QueryRow. Structurally identical to catalog.PGPool
// so the main thread can share one *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SQL against daily_product_sales (migration 000019). The upsert is idempotent
// on the unique index idx_daily_product_sales_uq (tenant_id, store_id,
// product_id, sale_date); sale_date is bound as a 'YYYY-MM-DD' string so the
// DATE column never sees a time-of-day / location. LastSaleDate uses the
// ($2 = 0 OR store_id = $2) idiom so storeID=0 scans all of the tenant's stores.
const (
	sqlUpsertDailySales = `INSERT INTO daily_product_sales (tenant_id, store_id, product_id, sale_date, qty, source)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, store_id, product_id, sale_date)
DO UPDATE SET qty = EXCLUDED.qty, updated_at = now()`

	sqlLastSaleDate = `SELECT sale_date
FROM daily_product_sales
WHERE tenant_id = $1 AND ($2 = 0 OR store_id = $2) AND product_id = $3
ORDER BY sale_date DESC
LIMIT 1`
)

// PGSalesRepo is the pgx-backed SalesRepo. A nil pool makes writes return 0 and
// reads return ok=false (dev wires MemorySalesRepo).
type PGSalesRepo struct{ pool PGPool }

// NewPGSalesRepo constructs a PGSalesRepo over pool.
func NewPGSalesRepo(pool PGPool) *PGSalesRepo { return &PGSalesRepo{pool: pool} }

// UpsertDailySales writes each row with an ON CONFLICT upsert and returns the
// number of rows written. A nil pool or an empty rows slice is a no-op returning
// 0; the first per-row error aborts and is returned wrapped alongside the count
// written so far.
func (r *PGSalesRepo) UpsertDailySales(ctx context.Context, tenantID, storeID int64, day time.Time, rows []SaleQty) (int, error) {
	if r == nil || r.pool == nil || len(rows) == 0 {
		return 0, nil
	}
	date := day.Format("2006-01-02")
	n := 0
	for _, row := range rows {
		if _, err := r.pool.Exec(ctx, sqlUpsertDailySales, tenantID, storeID, row.ProductID, date, row.Qty, SourceSaleOut); err != nil {
			return n, fmt.Errorf("dataquality: pg upsert daily sales: %w", err)
		}
		n++
	}
	return n, nil
}

// LastSaleDate returns the product's most recent sale_date within (tenantID,
// storeID); storeID=0 scans all of the tenant's stores. ok=false when the
// product has no recorded sale (pgx.ErrNoRows → (zero, false, nil)). A nil pool
// returns (zero, false, nil).
func (r *PGSalesRepo) LastSaleDate(ctx context.Context, tenantID, storeID, productID int64) (time.Time, bool, error) {
	if r == nil || r.pool == nil {
		return time.Time{}, false, nil
	}
	var d time.Time
	err := r.pool.QueryRow(ctx, sqlLastSaleDate, tenantID, storeID, productID).Scan(&d)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("dataquality: pg last sale date: %w", err)
	}
	return d, true, nil
}
