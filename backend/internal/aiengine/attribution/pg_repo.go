package attribution

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface PGRepo needs for single-row upserts
// (Record uses INSERT ... ON CONFLICT ... RETURNING via QueryRow). It mirrors
// semantic.PGVectorSemanticCache's Pool idiom so the main thread can inject the
// same *pgxpool.Pool / pgx.Tx into both. Multi-row reads (List /
// LatestPerMetric) use Query, which the injected pool (*pgxpool.Pool /
// pgxmock.PgxPoolIface) also provides; PGRepo asserts to pgQuerier at call
// time so the frozen PGPool surface stays minimal.
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// pgQuerier is the multi-row read surface PGRepo additionally needs. Both
// *pgxpool.Pool and pgxmock.PgxPoolIface satisfy it, so the runtime assertion
// in List/LatestPerMetric succeeds for production and tests; a pool that only
// satisfies PGPool (no Query) yields an empty result rather than a panic.
type pgQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SQL statements against strategy_attribution (migration 000007). Record is an
// upsert on UNIQUE(tenant_id,strategy_item_id,metric,measured_at) so repeated
// backfill of the same window overwrites rather than errors.
const (
	// sqlRecord upserts one measurement and returns the row id.
	sqlRecord = `INSERT INTO strategy_attribution (tenant_id, strategy_item_id, metric, value, measured_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, strategy_item_id, metric, measured_at)
DO UPDATE SET value = EXCLUDED.value
RETURNING id`

	// sqlList selects a tenant's measurements with optional item/metric
	// filters. Empty filter slices are passed as NULL arrays and match all.
	sqlList = `SELECT id, tenant_id, strategy_item_id, metric, value, measured_at
FROM strategy_attribution
WHERE tenant_id = $1
  AND ($2::bigint[] IS NULL OR strategy_item_id = ANY($2))
  AND ($3::text[] IS NULL OR metric = ANY($3))
ORDER BY strategy_item_id, metric, measured_at
LIMIT $4`

	// sqlLatest selects the most recent value per metric for one item
	// (DISTINCT ON keeps the highest measured_at per metric).
	sqlLatest = `SELECT DISTINCT ON (metric) metric, value
FROM strategy_attribution
WHERE tenant_id = $1 AND strategy_item_id = $2
ORDER BY metric, measured_at DESC`
)

// PGRepo is the production Repo backed by PostgreSQL (strategy_attribution,
// migration 000007). A nil pool is a defensive no-op returning empty results
// (dev wires MemoryRepo).
type PGRepo struct {
	pool PGPool
}

// NewPGRepo constructs a PGRepo over pool.
func NewPGRepo(pool PGPool) *PGRepo {
	return &PGRepo{pool: pool}
}

// Record upserts one measurement under
// UNIQUE(tenantID,itemID,metric,measuredAt) and returns the stored row (id
// backfilled from RETURNING). ErrParam on an unknown metric; a nil pool is a
// no-op returning ErrNotFound (nothing persisted).
func (r *PGRepo) Record(ctx context.Context, tenantID, itemID int64, metric string, value float64, measuredAt time.Time) (*Measurement, error) {
	if !validMetrics[metric] {
		return nil, ErrParam
	}
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	m := &Measurement{
		TenantID:       tenantID,
		StrategyItemID: itemID,
		Metric:         metric,
		Value:          value,
		MeasuredAt:     measuredAt,
	}
	if err := r.pool.QueryRow(ctx, sqlRecord, tenantID, itemID, metric, value, measuredAt).Scan(&m.ID); err != nil {
		return nil, fmt.Errorf("attribution: pg record: %w", err)
	}
	return m, nil
}

// List returns the tenant's measurements matching f. Empty ItemIDs/Metrics
// slices are sent as NULL and match all; Limit<=0 defaults to 50. Cross-tenant
// rows are excluded by the tenant_id predicate. A nil pool (or one without
// Query) returns an empty slice.
func (r *PGRepo) List(ctx context.Context, tenantID int64, f MeasurementFilter) ([]Measurement, error) {
	out := make([]Measurement, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(pgQuerier)
	if !ok {
		return out, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	var itemIDs []int64
	if len(f.ItemIDs) > 0 {
		itemIDs = f.ItemIDs
	}
	var metrics []string
	if len(f.Metrics) > 0 {
		metrics = f.Metrics
	}
	rows, err := q.Query(ctx, sqlList, tenantID, itemIDs, metrics, limit)
	if err != nil {
		return nil, fmt.Errorf("attribution: pg list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m Measurement
		if err := rows.Scan(&m.ID, &m.TenantID, &m.StrategyItemID, &m.Metric, &m.Value, &m.MeasuredAt); err != nil {
			return nil, fmt.Errorf("attribution: pg list scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attribution: pg list rows: %w", err)
	}
	return out, nil
}

// LatestPerMetric returns the most recent value per metric for one item;
// metrics with no row are omitted (not zero). Cross-tenant items yield an
// empty map. A nil pool (or one without Query) returns an empty map.
func (r *PGRepo) LatestPerMetric(ctx context.Context, tenantID, itemID int64) (map[string]float64, error) {
	out := make(map[string]float64)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(pgQuerier)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlLatest, tenantID, itemID)
	if err != nil {
		return nil, fmt.Errorf("attribution: pg latest: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var metric string
		var value float64
		if err := rows.Scan(&metric, &value); err != nil {
			return nil, fmt.Errorf("attribution: pg latest scan: %w", err)
		}
		out[metric] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attribution: pg latest rows: %w", err)
	}
	return out, nil
}
