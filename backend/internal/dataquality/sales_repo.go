package dataquality

// Persistence ports for the BE-A-08/BE-A-09 job results (CONTRACT §9.3).
//
// This file defines ONLY the narrow ports plus their value types; it is
// deliberately pgx-free so the pure-function core of the package (ReverseDailySales
// / ComputeTags) stays import-light. The in-memory implementation lives in
// memory_sales_repo.go (pure Go, no pgxmock) and the pgx-backed one in
// daily_sales_pg_repo.go. Tenant is resolved from ctx by the implementations in
// the job scenario (the cron injects it via multitenancy.WithContext); the ports
// still carry tenantID/storeID explicitly as their documented contract, matching
// the SalesReverser idiom.

import (
	"context"
	"time"
)

// SourceSaleOut labels the fused daily-sales rows the reverse-sales job writes
// (the inventory sale_out source of the BE-A-08 three-source fusion).
const SourceSaleOut = "sale_out"

// SalesRepo persists the fused per-product daily sales (BE-A-08) and reads a
// product's most recent sale_date back (for the BE-A-09 滞销 rule). The cron
// reverse-computes yesterday's sale_out quantities, upserts them here, then asks
// LastSaleDate how stale each product is. storeID=0 means "all of the tenant's
// stores" — the whole-tenant fusion scope the cron sweeps.
type SalesRepo interface {
	// UpsertDailySales writes the day's fused per-product quantities, idempotent
	// on (tenantID, storeID, productID, day): re-running for the same day
	// overwrites the stored qty rather than duplicating. It returns the number
	// of product rows written.
	UpsertDailySales(ctx context.Context, tenantID, storeID int64, day time.Time, rows []SaleQty) (int, error)

	// LastSaleDate returns the most recent sale_date on record for productID
	// within (tenantID, storeID); storeID=0 scans all of the tenant's stores.
	// ok=false when the product has no recorded sale.
	LastSaleDate(ctx context.Context, tenantID, storeID, productID int64) (last time.Time, ok bool, err error)
}

// TagWriter writes a computed BE-A-09 tag set onto one product. catalog.Service
// satisfies it directly (its SetProductTags method), so the cron can hand the
// catalogue service straight to the tagging step with no adapter. Tenant is
// resolved from ctx by the implementation; the explicit tenantID is the port's
// contract.
type TagWriter interface {
	SetProductTags(ctx context.Context, tenantID, productID int64, tags []string) error
}
