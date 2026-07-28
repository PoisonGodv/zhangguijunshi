// Package signal implements the Signal Collect stage (CONTRACT §12.3.2) of the
// five-stage AI pipeline. It aggregates a merchant's "today" data — inventory,
// members, transactions, weather, holiday — into a strongly typed Bundle that
// the downstream Rule Router consumes.
//
// signal imports NO domain packages (membership/inventory/billing/catalog); the
// main thread adapts them to the Lookuper port defined in this package. Amounts
// are int64 cents (no float); weather temperature, stock quantity and days may
// be float/int. The Lookuper is best-effort: when the backend cannot enumerate
// (M3-S1 membership/inventory List are partial), it returns zero values / empty
// slices + nil error so the Pipeline can fall back to cold-start.
package signal

import (
	"context"
	"time"
)

// Bundle is the aggregated today-signal snapshot fed to the Rule Router.
type Bundle struct {
	TenantID     int64          `json:"tenant_id"`
	StoreID      int64          `json:"store_id"`
	BusinessType string         `json:"business_type"` // fruit|convenience|maternity|fresh|apparel
	City         string         `json:"city"`
	Date         time.Time      `json:"date"`
	Weather      Weather        `json:"weather"`
	Holiday      Holiday        `json:"holiday"`
	Inventory    []InventorySig `json:"inventory"`
	Members      MemberStats    `json:"members"`
	DataQuality  int            `json:"data_quality"` // 0-100, trace only
}

// Weather is the in-memory mocked weather signal (real API deferred).
type Weather struct {
	Condition string  `json:"condition"` // rain|hot|cold|snow|normal
	TempC     float64 `json:"temp_c"`    // temperature may be float (non-amount)
	Forecast  string  `json:"forecast"`
}

// Holiday is the in-memory mocked holiday signal (lunar/festival table deferred).
type Holiday struct {
	Name       string `json:"name"`    // 端午|中秋|母亲节|无
	DaysTo     int    `json:"days_to"` // 距今天数, -1=无
	IsFestival bool   `json:"is_festival"`
}

// InventorySig is one product's inventory signal. Amounts are int64 cents.
type InventorySig struct {
	ProductID    int64   `json:"product_id"`
	Name         string  `json:"name"`
	CostCents    int64   `json:"cost_cents"`
	PriceCents   int64   `json:"price_cents"`
	Stock        float64 `json:"stock"`
	DaysToExpiry int     `json:"days_to_expiry"` // -1=非临期
	SlowMoving   bool    `json:"slow_moving"`
	HighMargin   bool    `json:"high_margin"`
}

// MemberStats is the member-aggregate signal (best-effort; fields may be zero).
type MemberStats struct {
	Total         int     `json:"total"`          // 会员总数
	Sleep30       int     `json:"sleep_30"`       // 30天未到店人数
	HighValue     []int64 `json:"high_value"`     // 高价值会员ID
	BirthdayToday []int64 `json:"birthday_today"` // 当天生日会员ID
}

// TxnSig is the daily transaction aggregate (best-effort).
type TxnSig struct {
	RevenueCents int64 `json:"revenue_cents"` // 当日营收
	OrderCount   int   `json:"order_count"`
	NewMembers   int   `json:"new_members"`
}

// Lookuper is the anti-corruption port (defined in signal; the main thread
// adapts membership/inventory/billing/catalog/dataquality to it). M3-S1 is
// best-effort: implementations that cannot enumerate return zero values /
// empty slices + nil error to trigger upstream cold-start.
type Lookuper interface {
	// Inventory returns nearing-expiry / slow-moving / high-margin items.
	Inventory(ctx context.Context, tenantID, storeID int64) ([]InventorySig, error)
	// Members returns member-aggregate stats (zero values where unavailable).
	Members(ctx context.Context, tenantID int64) (MemberStats, error)
	// DailyTxn returns the daily transaction aggregate for date (YYYY-MM-DD).
	DailyTxn(ctx context.Context, tenantID, storeID int64, date string) (TxnSig, error)
}

// Collector aggregates today's signals into a Bundle.
type Collector interface {
	Collect(ctx context.Context, tenantID, storeID int64) (*Bundle, error)
}
