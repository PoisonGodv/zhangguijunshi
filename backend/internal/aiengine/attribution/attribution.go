// Package attribution implements AI-10 (CONTRACT §14.3.1 / tech plan ch07
// §7.9): strategy-execution effect attribution backfill. After a strategy_item
// is executed, the Computer derives proxy metrics at T+24h/T+72h — coupon
// redemption rate, visit uplift, groupbuy success — and writes them into the
// pre-existing strategy_attribution table (migration 000007, EAV shape:
// tenant_id / strategy_item_id / metric / value / measured_at). It also folds
// those measurements into a normalised [0,1] reward for the bandit package and
// exposes a read API substrate for the boss dashboard.
//
// The package is deliberately anti-corruption-clean: it imports only
// internal/strategy (the StrategyItem shape) and declares its own narrow
// ports — CouponStats → marketing, VisitCounter → billing, GroupbuyStats →
// groupbuy, ItemLoader → strategy.Service, PGPool → *pgxpool.Pool. The main
// thread adapts the real services onto those ports; nil ports are skipped
// safely (dev wires no PG / no marketing linkage). reward is clipped to [0,1]
// with math.Max/math.Min; all time-dependent paths take explicit
// executedAt/measuredAt (never time.Now()).
package attribution

import (
	"context"
	"errors"
	"time"
)

// metric constants — one-to-one with the strategy_attribution.metric CHECK
// constraint in migration 000007. repurchase / moments_engage have no data
// source this sprint and are NOT emitted by Compute (dashboard shows "—").
const (
	MetricCouponRedeem    = "coupon_redeem"    // 本期真实计算
	MetricVisitUplift     = "visit_uplift"     // 本期真实计算
	MetricGroupbuySuccess = "groupbuy_success" // 本期真实计算(best-effort, 无 linkage 则不 emit)
	MetricRepurchase      = "repurchase"       // 本期不产出(无数据源, 延后)
	MetricMomentsEngage   = "moments_engage"   // 本期不产出(无数据源, 延后)
)

// validMetrics mirrors the migration 000007 CHECK constraint for early input
// validation; the DB CHECK remains authoritative.
var validMetrics = map[string]bool{
	MetricCouponRedeem:    true,
	MetricVisitUplift:     true,
	MetricGroupbuySuccess: true,
	MetricRepurchase:      true,
	MetricMomentsEngage:   true,
}

// Measurement is one (item, metric, value, measured_at) fact; it maps 1:1 onto
// a strategy_attribution row.
type Measurement struct {
	ID             int64     `json:"id"`
	TenantID       int64     `json:"tenant_id"`
	StrategyItemID int64     `json:"strategy_item_id"`
	Metric         string    `json:"metric"`
	Value          float64   `json:"value"`
	MeasuredAt     time.Time `json:"measured_at"`
}

// MeasurementFilter narrows an attribution listing. Empty slices mean "no
// filter"; Limit<=0 defaults to 50.
type MeasurementFilter struct {
	ItemIDs []int64  // 空=不限
	Metrics []string // 空=不限
	Limit   int      // <=0 默认 50
}

// Sentinel errors. Cross-tenant access is reported as absence (empty slice /
// zero value), indistinguishable from a genuinely missing record (ARCH-01).
var (
	ErrParam    = errors.New("attribution: invalid param")
	ErrNotFound = errors.New("attribution: not found")
)

// Repo persists Measurement rows (strategy_attribution table, migration
// 000007). The in-memory dev/test implementation is MemoryRepo; the pgx-backed
// production implementation is PGRepo. Both satisfy this interface
// structurally; downstream packages (pregen, handler) depend on the interface
// so they are agnostic to the backing store (CONTRACT §14.3.1). Every method
// treats a record owned by another tenant as absent (ARCH-01).
type Repo interface {
	// Record upserts one (tenant, item, metric, measured_at) fact. The
	// UNIQUE(tenant_id, strategy_item_id, metric, measured_at) constraint makes
	// re-runs idempotent (overwrite in place). Returns the persisted row.
	Record(ctx context.Context, tenantID, itemID int64, metric string, value float64, measuredAt time.Time) (*Measurement, error)
	// List returns measurements matching f for tenantID. Cross-tenant rows are
	// never returned. Limit<=0 defaults to 50.
	List(ctx context.Context, tenantID int64, f MeasurementFilter) ([]Measurement, error)
	// LatestPerMetric returns the most recent value per metric for one item;
	// metrics with no measurement are omitted (NOT zero).
	LatestPerMetric(ctx context.Context, tenantID, itemID int64) (map[string]float64, error)
}
