package pregen

// bandit_credit.go implements the bandit-credit de-duplication store (aiengine
// 修复③). BanditUpdate runs every 02:00 and enumerates executed items over a
// 72h lookback (attributionLookback) — that wide window is intentional resilience
// so a MISSED tick still catches up on the next run. The defect was that nothing
// de-duplicated within the window: the same item was re-credited on every
// consecutive tick, inflating arm.Trials and dragging the Q estimate toward one
// repeatedly-applied reward.
//
// The fix keeps the 72h window (do NOT tighten to [last-run, now)) and moves
// de-duplication into the cron caller via a persisted marker:
//
//   - ListUncreditedExecuted returns only executed items whose
//     bandit_credited_at IS NULL (migration 000023), so an already-credited item
//     is skipped on every subsequent tick;
//   - MarkCredited stamps bandit_credited_at=now() right after a successful
//     CreditItem, removing the item from the next enumeration.
//
// bandit.Learner.Update's accumulate semantics are untouched — de-dup is purely a
// caller responsibility (CONTRACT §14.7). Two implementations ship (CONTRACT
// §14.5.4 dual-implementation invariant):
//
//   - MemoryBanditCreditStore: dev/test. Wraps a StrategyItemEnumerator and
//     keeps an in-process set of credited item ids. Behaviourally identical to
//     PG within a run (the set is process-local, so a restart re-credits once —
//     acceptable for dev; the durable path is PG).
//   - PGBanditCreditStore: production. Queries strategy_items directly (narrow
//     Pool, pgxmock-tested) with the bandit_credited_at IS NULL filter and the
//     write-back UPDATE. Constructed in bootstrap like semCache (memory↔pg
//     switch on database != nil), avoiding any change to adapters.go.

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// BanditCreditStore is the de-duplication port the BanditUpdate cron uses to
// credit each executed item EXACTLY ONCE across consecutive ticks (aiengine
// 修复③). It is nil-safe at the Scheduler: a nil store falls back to the legacy
// (non-deduplicating) StrategyItemEnumerator path so M3-S2/S3 callers stay
// backward-compatible.
type BanditCreditStore interface {
	// ListUncreditedExecuted returns executed items for tenantID within the
	// lookback window (since) that have NOT yet been credited to the bandit.
	// Cross-tenant rows are never returned. limit caps the result (<=0 → default).
	ListUncreditedExecuted(ctx context.Context, tenantID int64, since time.Time, limit int) ([]ExecutedItem, error)

	// MarkCredited stamps the item's bandit_credited_at marker so subsequent
	// ticks skip it. Best-effort: a failure is logged by the caller and does not
	// roll back the already-applied CreditItem (the item simply gets re-credited
	// once on the next tick — the same bounded over-count the 72h window already
	// tolerates for a missed tick).
	MarkCredited(ctx context.Context, tenantID, itemID int64, at time.Time) error
}

// ---- Memory implementation ----

// MemoryBanditCreditStore is the dev/test BanditCreditStore. It delegates
// enumeration to a wrapped StrategyItemEnumerator and filters out item ids it
// has already marked credited (an in-process set). The set is process-local: a
// restart clears it (dev-only; PG is the durable path).
type MemoryBanditCreditStore struct {
	enum StrategyItemEnumerator
	mu   sync.Mutex
	seen map[int64]time.Time // itemID → credited-at
}

// NewMemoryBanditCreditStore wraps enum (the dev strategy-item enumerator) into
// a de-duplicating store. A nil enum makes ListUncreditedExecuted return nothing.
func NewMemoryBanditCreditStore(enum StrategyItemEnumerator) *MemoryBanditCreditStore {
	return &MemoryBanditCreditStore{enum: enum, seen: make(map[int64]time.Time)}
}

// ListUncreditedExecuted implements BanditCreditStore: it lists executed items
// via the wrapped enumerator and drops any already-credited id.
func (m *MemoryBanditCreditStore) ListUncreditedExecuted(ctx context.Context, tenantID int64, since time.Time, limit int) ([]ExecutedItem, error) {
	if m == nil || m.enum == nil {
		return nil, nil
	}
	items, err := m.enum.ListExecutedSince(ctx, tenantID, since, limit)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := items[:0:0]
	for _, ei := range items {
		if ei.Item == nil {
			continue
		}
		if _, done := m.seen[ei.Item.ID]; done {
			continue // already credited on an earlier tick
		}
		out = append(out, ei)
	}
	return out, nil
}

// MarkCredited implements BanditCreditStore: record the item id so the next
// ListUncreditedExecuted skips it.
func (m *MemoryBanditCreditStore) MarkCredited(_ context.Context, _, itemID int64, at time.Time) error {
	if m == nil || itemID <= 0 {
		return nil
	}
	m.mu.Lock()
	m.seen[itemID] = at
	m.mu.Unlock()
	return nil
}

// ---- PG implementation ----

// pgCreditPool is the narrow database surface PGBanditCreditStore needs. It is
// the multi-row Query + Exec subset that *pgxpool.Pool and pgxmock both satisfy
// (same范式 as bandit.PGPool / semantic.Pool); the package never hard-imports
// pgxpool.
type pgCreditPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SQL against strategy_items (migration 000023 adds bandit_credited_at). The
// list joins strategies for store_id (ExecutedItem.StoreID) and filters to
// executed + within-window + NOT-yet-credited. The marker write-back is a
// tenant-scoped UPDATE.
const (
	sqlListUncredited = `SELECT si.id, si.executed_at, s.store_id
FROM strategy_items si
JOIN strategies s ON s.id = si.strategy_id
WHERE si.tenant_id = $1
  AND si.execution_status = 'executed'
  AND si.executed_at >= $2
  AND si.bandit_credited_at IS NULL
ORDER BY si.executed_at DESC
LIMIT $3`

	sqlMarkCredited = `UPDATE strategy_items SET bandit_credited_at = $3 WHERE tenant_id = $1 AND id = $2`
)

// PGBanditCreditStore is the production BanditCreditStore backed by
// strategy_items.bandit_credited_at (migration 000023). A nil pool makes every
// method a safe no-op (list → empty, mark → nil) so a misconfigured main thread
// degrades to "credit nothing" rather than crashing the cron.
type PGBanditCreditStore struct {
	pool pgCreditPool
}

// NewPGBanditCreditStore constructs a PGBanditCreditStore over pool.
func NewPGBanditCreditStore(pool pgCreditPool) *PGBanditCreditStore {
	return &PGBanditCreditStore{pool: pool}
}

// ListUncreditedExecuted implements BanditCreditStore over PG.
func (p *PGBanditCreditStore) ListUncreditedExecuted(ctx context.Context, tenantID int64, since time.Time, limit int) ([]ExecutedItem, error) {
	if p == nil || p.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := p.pool.Query(ctx, sqlListUncredited, tenantID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExecutedItem
	for rows.Next() {
		var (
			id         int64
			executedAt *time.Time
			storeID    int64
		)
		if err := rows.Scan(&id, &executedAt, &storeID); err != nil {
			return nil, err
		}
		out = append(out, ExecutedItem{
			Item:    &strategy.StrategyItem{ID: id, TenantID: tenantID, ExecutedAt: executedAt},
			StoreID: storeID,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkCredited implements BanditCreditStore over PG (tenant-scoped write-back).
func (p *PGBanditCreditStore) MarkCredited(ctx context.Context, tenantID, itemID int64, at time.Time) error {
	if p == nil || p.pool == nil || itemID <= 0 {
		return nil
	}
	_, err := p.pool.Exec(ctx, sqlMarkCredited, tenantID, itemID, at)
	return err
}

// compile-time guards: both implementations satisfy the port.
var (
	_ BanditCreditStore = (*MemoryBanditCreditStore)(nil)
	_ BanditCreditStore = (*PGBanditCreditStore)(nil)
)
