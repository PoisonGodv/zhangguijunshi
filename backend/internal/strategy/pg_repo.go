package strategy

// pg_repo.go is the pgx-backed StrategyRepo (CONTRACT §17.4 / §12.3.1), the
// production-ready peer of MemoryStrategyRepo. It mirrors the idiom of
// internal/payment/pg_repo.go verbatim: a narrow PGPool surface (QueryRow /
// Exec), a queryer type-assertion for multi-row reads, scan helpers over a
// rowScanner, nil-pool safe degradation, and pgx.ErrNoRows → ErrNotFound.
//
// Tenant isolation lives in every statement's WHERE tenant_id = $N. JSONB
// columns (signals / target / params / expected_effect) round-trip as []byte;
// nullable columns (llm_tier / target / product_id / copy / reason /
// expected_effect / executed_at) use database/sql Null types.
//
// SaveAdvice is the atomic upsert: the strategies row is rewritten in place via
// INSERT ... ON CONFLICT (tenant_id, store_id, for_date) DO UPDATE (the row keeps
// its id + original created_at); the old items AND their feedback are then
// discarded (DELETE feedback by strategy's items, then DELETE items) and fresh
// items inserted — exactly the "overwrite" semantics of MemoryStrategyRepo. (The
// feedback delete is required for faithful parity: strategy_feedback has an FK to
// strategy_items without ON DELETE CASCADE, so orphaned feedback would otherwise
// block the item delete. strategy_attribution is owned by aiengine.attribution
// and is left untouched.)
//
// UpdateItemStatus is a MarkSuccess-style read-then-conditional-update so the
// execution_status state machine (isAllowedItemTransition) is enforced race-safe
// and illegal transitions return ErrParam. RecordFeedback verifies the item is
// owned by the tenant (cross-tenant → ErrNotFound) before the
// UNIQUE(tenant, item) ON CONFLICT upsert.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface PGStrategyRepo needs (CONTRACT §17.2.1).
// Structurally identical to payment.PGPool / attribution.PGPool so the main
// thread can share one *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, query string, args ...any) pgx.Row
	Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool and
// pgxmock expose on top of PGPool. PGPool's signature is frozen (no Query), so
// list / load-items methods type-assert the concrete pool to queryer; a pool
// lacking Query returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, query string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// SQL for strategies (migration 000007). SaveAdvice upserts by
// (tenant_id, store_id, for_date) — the uniq_strategy_daily unique index backs
// the ON CONFLICT target. The DO UPDATE setlist rewrites every regenerated
// field while preserving id + created_at (both absent from the setlist).
// for_date is read back as ::text so it scans straight into the Strategy.ForDate
// string without a time.Time round-trip.
const (
	sqlStrategyUpsert = `INSERT INTO strategies (tenant_id, store_id, for_date, signals, data_quality_score, generated_by, llm_tier, total_cost_microcents, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (tenant_id, store_id, for_date) DO UPDATE SET
    signals = EXCLUDED.signals,
    data_quality_score = EXCLUDED.data_quality_score,
    generated_by = EXCLUDED.generated_by,
    llm_tier = EXCLUDED.llm_tier,
    total_cost_microcents = EXCLUDED.total_cost_microcents,
    status = EXCLUDED.status
RETURNING id, created_at`
	sqlStrategyGetByID = `SELECT id, tenant_id, store_id, for_date::text, signals, data_quality_score, generated_by, llm_tier, total_cost_microcents, status, created_at
FROM strategies
WHERE tenant_id = $1 AND id = $2`
	sqlStrategyGetToday = `SELECT id, tenant_id, store_id, for_date::text, signals, data_quality_score, generated_by, llm_tier, total_cost_microcents, status, created_at
FROM strategies
WHERE tenant_id = $1 AND store_id = $2 AND for_date = $3`
	sqlStrategyList = `SELECT id, tenant_id, store_id, for_date::text, signals, data_quality_score, generated_by, llm_tier, total_cost_microcents, status, created_at
FROM strategies
WHERE tenant_id = $1 AND ($2 = 0 OR store_id = $2) AND ($3 = '' OR for_date = $3::date) AND ($4 = '' OR status = $4)
ORDER BY id DESC
LIMIT $5 OFFSET $6`
)

// SQL for strategy_items (migration 000007). SaveAdvice rebuilds items by first
// discarding the old feedback + items (see sqlFeedbackDeleteByStrategy), then
// inserting fresh rows. GetItem / UpdateItemStatus / existence checks are all
// tenant-scoped. UpdateItemStatus rewrites execution_status + executed_at and
// returns the full row.
const (
	sqlItemInsert = `INSERT INTO strategy_items (tenant_id, strategy_id, rank, action_type, reason_type, target, product_id, params, copy, reason, expected_effect, margin_ok, execution_status, executed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING id`
	sqlItemsDeleteByStrategy = `DELETE FROM strategy_items WHERE tenant_id = $1 AND strategy_id = $2`
	sqlItemsByStrategy       = `SELECT id, tenant_id, strategy_id, rank, action_type, reason_type, target, product_id, params, copy, reason, expected_effect, margin_ok, execution_status, executed_at
FROM strategy_items
WHERE tenant_id = $1 AND strategy_id = $2
ORDER BY rank`
	sqlItemGetByID = `SELECT id, tenant_id, strategy_id, rank, action_type, reason_type, target, product_id, params, copy, reason, expected_effect, margin_ok, execution_status, executed_at
FROM strategy_items
WHERE tenant_id = $1 AND id = $2`
	sqlItemGetStatus    = `SELECT execution_status FROM strategy_items WHERE tenant_id = $1 AND id = $2`
	sqlItemExists       = `SELECT 1 FROM strategy_items WHERE tenant_id = $1 AND id = $2`
	sqlItemUpdateStatus = `UPDATE strategy_items SET execution_status = $1, executed_at = $2
WHERE tenant_id = $3 AND id = $4
RETURNING id, tenant_id, strategy_id, rank, action_type, reason_type, target, product_id, params, copy, reason, expected_effect, margin_ok, execution_status, executed_at`
)

// SQL for strategy_feedback (migration 000007). sqlFeedbackDeleteByStrategy
// clears the old feedback for a strategy's items BEFORE the items are deleted
// (the FK strategy_feedback.strategy_item_id → strategy_items.id has no ON DELETE
// CASCADE, so this order avoids an orphan-FK violation on regeneration).
// sqlFeedbackUpsert targets UNIQUE(tenant_id, strategy_item_id): repeat feedback
// overwrites useful + created_at in place, keeping the feedback id.
const (
	sqlFeedbackDeleteByStrategy = `DELETE FROM strategy_feedback WHERE tenant_id = $1 AND strategy_item_id IN (SELECT id FROM strategy_items WHERE tenant_id = $2 AND strategy_id = $3)`
	sqlFeedbackUpsert           = `INSERT INTO strategy_feedback (tenant_id, strategy_item_id, useful, created_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, strategy_item_id) DO UPDATE SET useful = EXCLUDED.useful, created_at = EXCLUDED.created_at
RETURNING id, tenant_id, strategy_item_id, useful, created_at`
)

// ===== scan helpers =====

// scanStrategy decodes one strategies row (skip uuid; for_date::text → string;
// signals JSONB → []byte → json.RawMessage; nullable llm_tier → "").
func scanStrategy(s rowScanner) (*Strategy, error) {
	var st Strategy
	var signals []byte
	var llmTier sql.NullString
	if err := s.Scan(
		&st.ID, &st.TenantID, &st.StoreID, &st.ForDate, &signals, &st.DataQualityScore,
		&st.GeneratedBy, &llmTier, &st.TotalCostMicroCents, &st.Status, &st.CreatedAt,
	); err != nil {
		return nil, err
	}
	st.Signals = json.RawMessage(signals)
	st.LLMTier = llmTier.String
	return &st, nil
}

// scanItem decodes one strategy_items row (JSONB target/params/expected_effect →
// maps; nullable product_id → 0; nullable copy/reason → ""; nullable executed_at
// → *time.Time).
func scanItem(s rowScanner) (*StrategyItem, error) {
	var it StrategyItem
	var target, params, expectedEffect []byte
	var productID sql.NullInt64
	var copyText, reason sql.NullString
	var executedAt sql.NullTime
	if err := s.Scan(
		&it.ID, &it.TenantID, &it.StrategyID, &it.Rank, &it.ActionType, &it.ReasonType,
		&target, &productID, &params, &copyText, &reason, &expectedEffect, &it.MarginOK,
		&it.ExecutionStatus, &executedAt,
	); err != nil {
		return nil, err
	}
	if len(target) > 0 {
		_ = json.Unmarshal(target, &it.Target)
	}
	it.ProductID = productID.Int64
	if len(params) > 0 {
		_ = json.Unmarshal(params, &it.Params)
	}
	it.Copy = copyText.String
	it.Reason = reason.String
	if len(expectedEffect) > 0 {
		_ = json.Unmarshal(expectedEffect, &it.ExpectedEffect)
	}
	if executedAt.Valid {
		t := executedAt.Time
		it.ExecutedAt = &t
	}
	return &it, nil
}

// scanFeedback decodes one strategy_feedback row.
func scanFeedback(s rowScanner) (*StrategyFeedback, error) {
	var f StrategyFeedback
	if err := s.Scan(&f.ID, &f.TenantID, &f.StrategyItemID, &f.Useful, &f.CreatedAt); err != nil {
		return nil, err
	}
	return &f, nil
}

// jsonbArg marshals m to JSONB bytes; a nil map yields a nil arg (SQL NULL). Used
// for nullable JSONB columns (target / expected_effect).
func jsonbArg(m map[string]any) (any, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// paramsArg marshals the params map for the NOT NULL params column: a nil map
// becomes "{}" so the column constraint is satisfied.
func paramsArg(m map[string]any) (any, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ===== PGStrategyRepo =====

// PGStrategyRepo is the pgx-backed StrategyRepo. A nil pool makes reads return
// ErrNotFound, SaveAdvice / UpdateItemStatus / RecordFeedback return ErrNotFound,
// and List return empty — graceful degradation, never a panic.
type PGStrategyRepo struct{ pool PGPool }

// NewPGStrategyRepo constructs a PGStrategyRepo over pool.
func NewPGStrategyRepo(pool PGPool) *PGStrategyRepo {
	return &PGStrategyRepo{pool: pool}
}

// loadItems returns deep-copied items belonging to (tenantID, strategyID),
// ordered by Rank. A pool lacking Query returns an empty slice.
func (r *PGStrategyRepo) loadItems(ctx context.Context, tenantID, strategyID int64) ([]StrategyItem, error) {
	out := make([]StrategyItem, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlItemsByStrategy, tenantID, strategyID)
	if err != nil {
		return nil, fmt.Errorf("strategy: pg load items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("strategy: pg load items scan: %w", err)
		}
		out = append(out, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("strategy: pg load items rows: %w", err)
	}
	return out, nil
}

// SaveAdvice atomically upserts the advice for (tenant, store, forDate):
// (1) upsert the strategies row (ON CONFLICT preserves id + created_at);
// (2) discard old feedback for this strategy's items;
// (3) discard old items;
// (4) insert fresh items, backfilling their IDs onto the input pointer.
// A deep clone is returned.
func (r *PGStrategyRepo) SaveAdvice(ctx context.Context, s *Strategy) (*Strategy, error) {
	if r == nil || r.pool == nil || s == nil {
		return nil, ErrNotFound
	}
	signals := []byte("{}")
	if len(s.Signals) > 0 {
		signals = s.Signals
	}
	var llmTierArg any
	if s.LLMTier != "" {
		llmTierArg = s.LLMTier
	}
	// (1) upsert strategy row, echo back id + preserved created_at.
	if err := r.pool.QueryRow(ctx, sqlStrategyUpsert,
		s.TenantID, s.StoreID, s.ForDate, signals, s.DataQualityScore, s.GeneratedBy,
		llmTierArg, s.TotalCostMicroCents, s.Status,
	).Scan(&s.ID, &s.CreatedAt); err != nil {
		return nil, fmt.Errorf("strategy: pg save advice upsert: %w", err)
	}
	// (2) discard old feedback for the strategy's items (mirrors memory; must
	// precede the item delete because the FK lacks ON DELETE CASCADE).
	if _, err := r.pool.Exec(ctx, sqlFeedbackDeleteByStrategy, s.TenantID, s.TenantID, s.ID); err != nil {
		return nil, fmt.Errorf("strategy: pg save advice delete feedback: %w", err)
	}
	// (3) discard old items.
	if _, err := r.pool.Exec(ctx, sqlItemsDeleteByStrategy, s.TenantID, s.ID); err != nil {
		return nil, fmt.Errorf("strategy: pg save advice delete items: %w", err)
	}
	// (4) insert fresh items, backfilling ownership + IDs.
	for i := range s.Items {
		it := &s.Items[i]
		it.StrategyID = s.ID
		it.TenantID = s.TenantID
		if it.ExecutionStatus == "" {
			it.ExecutionStatus = ItemPending
		}
		targetArg, err := jsonbArg(it.Target)
		if err != nil {
			return nil, fmt.Errorf("strategy: pg save advice marshal target: %w", err)
		}
		paramsBytes, err := paramsArg(it.Params)
		if err != nil {
			return nil, fmt.Errorf("strategy: pg save advice marshal params: %w", err)
		}
		expectedArg, err := jsonbArg(it.ExpectedEffect)
		if err != nil {
			return nil, fmt.Errorf("strategy: pg save advice marshal expected: %w", err)
		}
		var productIDArg any
		if it.ProductID != 0 {
			productIDArg = it.ProductID
		}
		if err := r.pool.QueryRow(ctx, sqlItemInsert,
			it.TenantID, it.StrategyID, it.Rank, it.ActionType, it.ReasonType,
			targetArg, productIDArg, paramsBytes, it.Copy, it.Reason, expectedArg,
			it.MarginOK, it.ExecutionStatus, it.ExecutedAt,
		).Scan(&it.ID); err != nil {
			return nil, fmt.Errorf("strategy: pg save advice insert item: %w", err)
		}
	}
	return CloneStrategy(s), nil
}

// Get loads one strategy (with items) by id within tenantID. ErrNotFound when
// absent or owned by another tenant.
func (r *PGStrategyRepo) Get(ctx context.Context, tenantID, id int64) (*Strategy, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	st, err := scanStrategy(r.pool.QueryRow(ctx, sqlStrategyGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("strategy: pg get: %w", err)
	}
	items, err := r.loadItems(ctx, tenantID, st.ID)
	if err != nil {
		return nil, err
	}
	st.Items = items
	return st, nil
}

// GetToday loads the advice for (tenant, store, date). ErrNotFound/跨租户.
func (r *PGStrategyRepo) GetToday(ctx context.Context, tenantID, storeID int64, date string) (*Strategy, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	st, err := scanStrategy(r.pool.QueryRow(ctx, sqlStrategyGetToday, tenantID, storeID, date))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("strategy: pg get today: %w", err)
	}
	items, err := r.loadItems(ctx, tenantID, st.ID)
	if err != nil {
		return nil, err
	}
	st.Items = items
	return st, nil
}

// List returns strategies matching the filter for tenantID, ordered by ID
// descending, with each strategy's items re-attached. Zero-value filter fields
// are not applied; Limit<=0 defaults to 50.
func (r *PGStrategyRepo) List(ctx context.Context, tenantID int64, f StrategyFilter) ([]*Strategy, error) {
	out := make([]*Strategy, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	rows, err := q.Query(ctx, sqlStrategyList, tenantID, f.StoreID, f.Date, f.Status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("strategy: pg list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		st, err := scanStrategy(rows)
		if err != nil {
			return nil, fmt.Errorf("strategy: pg list scan: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("strategy: pg list rows: %w", err)
	}
	for _, st := range out {
		items, err := r.loadItems(ctx, tenantID, st.ID)
		if err != nil {
			return nil, err
		}
		st.Items = items
	}
	return out, nil
}

// GetItem loads one item by id within tenantID. ErrNotFound/跨租户.
func (r *PGStrategyRepo) GetItem(ctx context.Context, tenantID, itemID int64) (*StrategyItem, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	it, err := scanItem(r.pool.QueryRow(ctx, sqlItemGetByID, tenantID, itemID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("strategy: pg get item: %w", err)
	}
	return it, nil
}

// UpdateItemStatus applies one execution_status transition: read current status,
// enforce the state machine (isAllowedItemTransition → ErrParam on illegal),
// then conditional UPDATE returning the full row. Absent/cross-tenant items
// yield ErrNotFound.
func (r *PGStrategyRepo) UpdateItemStatus(ctx context.Context, tenantID, itemID int64, status string, executedAt *time.Time, _ time.Time) (*StrategyItem, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	var current string
	if err := r.pool.QueryRow(ctx, sqlItemGetStatus, tenantID, itemID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("strategy: pg update item status load: %w", err)
	}
	if !isAllowedItemTransition(current, status) {
		return nil, ErrParam
	}
	it, err := scanItem(r.pool.QueryRow(ctx, sqlItemUpdateStatus, status, executedAt, tenantID, itemID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("strategy: pg update item status: %w", err)
	}
	return it, nil
}

// RecordFeedback upserts the boss's feedback for one item. UNIQUE(tenant, item)
// → repeat overwrites Useful/CreatedAt, keeping the feedback id. The item is
// first verified to exist within the tenant (cross-tenant → ErrNotFound) so a
// caller cannot react to another tenant's item.
func (r *PGStrategyRepo) RecordFeedback(ctx context.Context, tenantID, strategyItemID int64, useful bool, now time.Time) (*StrategyFeedback, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	var one int
	if err := r.pool.QueryRow(ctx, sqlItemExists, tenantID, strategyItemID).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("strategy: pg record feedback item check: %w", err)
	}
	fb, err := scanFeedback(r.pool.QueryRow(ctx, sqlFeedbackUpsert, tenantID, strategyItemID, useful, now))
	if err != nil {
		return nil, fmt.Errorf("strategy: pg record feedback: %w", err)
	}
	return fb, nil
}
