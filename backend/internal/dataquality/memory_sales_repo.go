package dataquality

// memory_sales_repo.go is the pure-Go, concurrency-safe in-memory SalesRepo for
// dev and tests (no pgx / pgxmock dependency). It mirrors the production upsert
// contract of daily_product_sales (migration 000019): the uniqueness cell is
// (tenant, store, product, sale_date) and a repeat write for the same day
// overwrites the stored quantity, so the reverse-sales job is idempotent when a
// day is re-run.

import (
	"context"
	"sync"
	"time"
)

// salesKey is the uniqueness key mirroring idx_daily_product_sales_uq. The date
// is the natural-day string ("2006-01-02") so a cell is addressed calendar-wise
// regardless of the time-of-day / location of the passed day.
type salesKey struct {
	tenant, store, product int64
	date                   string
}

// salesRow is one stored daily-sales cell.
type salesRow struct {
	qty int64
	day time.Time
}

// MemorySalesRepo stores fused daily-sales cells keyed by (tenant, store,
// product, date).
type MemorySalesRepo struct {
	mu sync.RWMutex
	m  map[salesKey]salesRow
}

// NewMemorySalesRepo constructs an empty repo.
func NewMemorySalesRepo() *MemorySalesRepo {
	return &MemorySalesRepo{m: make(map[salesKey]salesRow)}
}

// dateKey normalises a day to its natural-day string key.
func dateKey(day time.Time) string { return day.Format("2006-01-02") }

// UpsertDailySales implements SalesRepo. Each row overwrites the cell for
// (tenantID, storeID, row.ProductID, day); re-running a day is idempotent. It
// returns the number of product rows written (len(rows)). A nil receiver or an
// empty rows slice is a safe no-op returning 0.
func (r *MemorySalesRepo) UpsertDailySales(_ context.Context, tenantID, storeID int64, day time.Time, rows []SaleQty) (int, error) {
	if r == nil || len(rows) == 0 {
		return 0, nil
	}
	date := dateKey(day)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		r.m[salesKey{tenant: tenantID, store: storeID, product: row.ProductID, date: date}] = salesRow{qty: row.Qty, day: day}
	}
	return len(rows), nil
}

// LastSaleDate implements SalesRepo. It scans the tenant's cells for productID
// (storeID=0 = all of the tenant's stores) and returns the latest recorded
// sale_date. ok=false when the product has no recorded sale. A nil receiver
// returns (zero, false, nil).
func (r *MemorySalesRepo) LastSaleDate(_ context.Context, tenantID, storeID, productID int64) (time.Time, bool, error) {
	if r == nil {
		return time.Time{}, false, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var (
		best  time.Time
		found bool
	)
	for k, v := range r.m {
		if k.tenant != tenantID || k.product != productID {
			continue
		}
		if storeID != 0 && k.store != storeID {
			continue
		}
		if !found || v.day.After(best) {
			best = v.day
			found = true
		}
	}
	return best, found, nil
}
