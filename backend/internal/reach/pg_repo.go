package reach

// pg_repo.go is the pgx-backed ReachRepo (CONTRACT §17.4 / §11.3.2), the
// production-ready peer of MemoryReachRepo. It mirrors the idiom of
// internal/payment/pg_repo.go verbatim: a narrow PGPool surface
// (QueryRow / Exec), a queryer type-assertion for the list read (so the frozen
// PGPool interface stays minimal), scan helpers over a rowScanner, nil-pool safe
// degradation, and pgx.ErrNoRows → ErrNotFound / ErrSubscribeExhausted mapping.
//
// Tenant isolation lives in every statement's WHERE tenant_id = $N. Nullable
// reach_tasks columns (blocked_reason / strategy_id / sent_at) are scanned via
// database/sql Null types. The two atomic critical sections match the memory
// repo: ConsumeSubscribe is a single conditional UPDATE
// (used_quota < granted_quota) that maps "no row" → ErrSubscribeExhausted;
// GrantSubscribe is a single ON CONFLICT upsert over the UNIQUE index
// uq_subauth_tenant_member_tpl (added in migration 000021), so concurrent
// first-grants accumulate onto one row instead of inserting duplicates.
// Unsubscribe is a true ON CONFLICT upsert (UNIQUE(tenant, member, channel)).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface PGReachRepo needs (CONTRACT §17.2.1).
// Structurally identical to payment.PGPool / attribution.PGPool so the main
// thread can share one *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, query string, args ...any) pgx.Row
	Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool and
// pgxmock expose on top of PGPool. PGPool's signature is frozen (no Query), so
// list methods type-assert the concrete pool to queryer; a pool lacking Query
// returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, query string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// SQL for reach_tasks (migration 000006). CreateTask defaults CreatedAt /
// ScheduledAt in Go (mirroring MemoryReachRepo) and echoes back the allocated
// id. ListTasks uses the ($N = 0 OR col = $N) / ($N = ” OR col = $N) trick so a
// single fixed statement honours the optional storeID / status filters.
// UpdateTaskStatus maps an empty blockedReason → NULL (NULLIF) and a zero sentAt
// → "keep previous" (COALESCE over a NULL arg), matching memory semantics.
const (
	sqlReachTaskCreate = `INSERT INTO reach_tasks (tenant_id, store_id, channel, template_code, target_count, status, blocked_reason, strategy_id, scheduled_at, sent_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING id`
	sqlReachTaskGetByID = `SELECT id, tenant_id, store_id, channel, template_code, target_count, status, blocked_reason, strategy_id, scheduled_at, sent_at, created_at
FROM reach_tasks
WHERE tenant_id = $1 AND id = $2`
	sqlReachTaskList = `SELECT id, tenant_id, store_id, channel, template_code, target_count, status, blocked_reason, strategy_id, scheduled_at, sent_at, created_at
FROM reach_tasks
WHERE tenant_id = $1 AND ($2 = 0 OR store_id = $2) AND ($3 = '' OR status = $3)
ORDER BY id DESC`
	sqlReachTaskUpdateStatus = `UPDATE reach_tasks
SET status = $1, blocked_reason = NULLIF($2, ''), sent_at = COALESCE($3, sent_at)
WHERE tenant_id = $4 AND id = $5
RETURNING id, tenant_id, store_id, channel, template_code, target_count, status, blocked_reason, strategy_id, scheduled_at, sent_at, created_at`
	// sqlReachTaskUpdate finalizes an existing row (worker delivery of a deferred
	// send): status / blocked_reason / sent_at / target_count, matched by
	// tenant_id + id. Same NULLIF / COALESCE semantics as UpdateTaskStatus — an
	// empty blocked_reason clears the column and a NULL sent_at keeps the prior
	// value. scheduled_at is intentionally NOT rewritten (it records when the
	// send was booked). tenant_id in the WHERE keeps row isolation.
	sqlReachTaskUpdate = `UPDATE reach_tasks
SET status = $1, blocked_reason = NULLIF($2, ''), sent_at = COALESCE($3, sent_at), target_count = $4
WHERE tenant_id = $5 AND id = $6
RETURNING id, tenant_id, store_id, channel, template_code, target_count, status, blocked_reason, strategy_id, scheduled_at, sent_at, created_at`
)

// SQL for subscribe_auth (migration 000006 + 000021). Migration 000021 adds the
// UNIQUE index uq_subauth_tenant_member_tpl on (tenant_id, member_id,
// template_id) — after first collapsing any pre-existing duplicate rows — so
// GrantSubscribe is now a single atomic ON CONFLICT upsert (bug fix: the old
// read-then-write raced on concurrent first-grants and inserted duplicate rows,
// corrupting the one-time quota accounting). ConsumeSubscribe is a single atomic
// conditional UPDATE: the WHERE used_quota < granted_quota guard plus the table's
// CHECK (used_quota <= granted_quota) make the increment race-safe, and "no row"
// (absent or exhausted) maps to ErrSubscribeExhausted.
const (
	sqlSubAuthGrantUpsert = `INSERT INTO subscribe_auth (tenant_id, member_id, template_id, granted_quota, used_quota, granted_at)
VALUES ($1, $2, $3, $4, 0, $5)
ON CONFLICT (tenant_id, member_id, template_id) DO UPDATE SET granted_quota = subscribe_auth.granted_quota + EXCLUDED.granted_quota, granted_at = EXCLUDED.granted_at
RETURNING id, tenant_id, member_id, template_id, granted_quota, used_quota, granted_at`
	sqlSubAuthConsume = `UPDATE subscribe_auth SET used_quota = used_quota + 1
WHERE tenant_id = $1 AND member_id = $2 AND template_id = $3 AND used_quota < granted_quota
RETURNING id, tenant_id, member_id, template_id, granted_quota, used_quota, granted_at`
	sqlSubAuthGet = `SELECT id, tenant_id, member_id, template_id, granted_quota, used_quota, granted_at
FROM subscribe_auth
WHERE tenant_id = $1 AND member_id = $2 AND template_id = $3`
)

// SQL for unsubscribe_records (migration 000006). UNIQUE(tenant_id, member_id,
// channel) backs the ON CONFLICT upsert: a repeat unsubscribe refreshes reason +
// timestamp in place (memory semantics). IsUnsubscribed is an existence probe;
// Resubscribe is an idempotent DELETE.
const (
	sqlUnsubscribeUpsert = `INSERT INTO unsubscribe_records (tenant_id, member_id, channel, reason, unsubscribed_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, member_id, channel) DO UPDATE SET reason = EXCLUDED.reason, unsubscribed_at = EXCLUDED.unsubscribed_at
RETURNING id, tenant_id, member_id, channel, reason, unsubscribed_at`
	sqlUnsubscribedExists = `SELECT 1 FROM unsubscribe_records WHERE tenant_id = $1 AND member_id = $2 AND channel = $3 LIMIT 1`
	sqlResubscribe        = `DELETE FROM unsubscribe_records WHERE tenant_id = $1 AND member_id = $2 AND channel = $3`
)

// ===== scan helpers =====

// scanReachTask decodes one reach_tasks row. blocked_reason / strategy_id /
// sent_at are nullable; a NULL strategy_id reads back as 0 (non-AI), a NULL
// sent_at as the zero time, matching MemoryReachRepo.
func scanReachTask(s rowScanner) (*ReachTask, error) {
	var t ReachTask
	var blockedReason sql.NullString
	var strategyID sql.NullInt64
	var sentAt sql.NullTime
	if err := s.Scan(
		&t.ID, &t.TenantID, &t.StoreID, &t.Channel, &t.TemplateCode, &t.TargetCount,
		&t.Status, &blockedReason, &strategyID, &t.ScheduledAt, &sentAt, &t.CreatedAt,
	); err != nil {
		return nil, err
	}
	t.BlockedReason = blockedReason.String
	t.StrategyID = strategyID.Int64
	if sentAt.Valid {
		t.SentAt = sentAt.Time
	}
	return &t, nil
}

// scanSubscribeAuth decodes one subscribe_auth row.
func scanSubscribeAuth(s rowScanner) (*SubscribeAuth, error) {
	var a SubscribeAuth
	if err := s.Scan(
		&a.ID, &a.TenantID, &a.MemberID, &a.TemplateID, &a.GrantedQuota, &a.UsedQuota, &a.GrantedAt,
	); err != nil {
		return nil, err
	}
	return &a, nil
}

// scanUnsubscribe decodes one unsubscribe_records row (nullable reason → "").
func scanUnsubscribe(s rowScanner) (*UnsubscribeRecord, error) {
	var u UnsubscribeRecord
	var reason sql.NullString
	if err := s.Scan(
		&u.ID, &u.TenantID, &u.MemberID, &u.Channel, &reason, &u.UnsubscribedAt,
	); err != nil {
		return nil, err
	}
	u.Reason = reason.String
	return &u, nil
}

// ===== PGReachRepo =====

// PGReachRepo is the pgx-backed ReachRepo. A nil pool makes reads return
// ErrNotFound, lists return empty, IsUnsubscribed return (false, nil),
// ConsumeSubscribe return ErrSubscribeExhausted, and Resubscribe return nil —
// the faithful "absent" semantics each method carries in MemoryReachRepo, so a
// misconfigured main thread degrades gracefully instead of panicking.
type PGReachRepo struct{ pool PGPool }

// NewPGReachRepo constructs a PGReachRepo over pool.
func NewPGReachRepo(pool PGPool) *PGReachRepo {
	return &PGReachRepo{pool: pool}
}

// CreateTask persists a new task (id + timestamps allocated by the repo, mirroring
// MemoryReachRepo). The caller's TenantID is trusted because the Service always
// resolves it from ctx. strategy_id 0 and a zero sent_at are stored as NULL.
func (r *PGReachRepo) CreateTask(ctx context.Context, t *ReachTask) (*ReachTask, error) {
	if r == nil || r.pool == nil || t == nil {
		return nil, ErrNotFound
	}
	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.ScheduledAt.IsZero() {
		t.ScheduledAt = t.CreatedAt
	}
	var strategyIDArg any = t.StrategyID
	if t.StrategyID == 0 {
		strategyIDArg = nil
	}
	var sentAtArg any
	if !t.SentAt.IsZero() {
		sentAtArg = t.SentAt
	}
	if err := r.pool.QueryRow(ctx, sqlReachTaskCreate,
		t.TenantID, t.StoreID, t.Channel, t.TemplateCode, t.TargetCount, t.Status,
		t.BlockedReason, strategyIDArg, t.ScheduledAt, sentAtArg, t.CreatedAt,
	).Scan(&t.ID); err != nil {
		return nil, fmt.Errorf("reach: pg task create: %w", err)
	}
	return cloneTask(t), nil
}

// GetTask loads a task by id within tenantID. ErrNotFound when absent or owned by
// another tenant.
func (r *PGReachRepo) GetTask(ctx context.Context, tenantID, id int64) (*ReachTask, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	t, err := scanReachTask(r.pool.QueryRow(ctx, sqlReachTaskGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reach: pg task get: %w", err)
	}
	return t, nil
}

// ListTasks returns tasks for tenantID, optionally narrowed by storeID (>0) and
// status (non-empty), ordered by ID descending. A pool lacking Query returns an
// empty slice.
func (r *PGReachRepo) ListTasks(ctx context.Context, tenantID, storeID int64, status string) ([]*ReachTask, error) {
	out := make([]*ReachTask, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlReachTaskList, tenantID, storeID, status)
	if err != nil {
		return nil, fmt.Errorf("reach: pg task list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanReachTask(rows)
		if err != nil {
			return nil, fmt.Errorf("reach: pg task list scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reach: pg task list rows: %w", err)
	}
	return out, nil
}

// UpdateTaskStatus rewrites a task's status / blocked_reason / sent_at (matched
// by ID + tenant). An empty blockedReason clears the column (NULLIF); a zero
// sentAt leaves the previous value (COALESCE over NULL). ErrNotFound when absent
// or cross-tenant.
func (r *PGReachRepo) UpdateTaskStatus(ctx context.Context, tenantID, id int64, status, blockedReason string, sentAt time.Time) (*ReachTask, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	var sentAtArg any
	if !sentAt.IsZero() {
		sentAtArg = sentAt
	}
	t, err := scanReachTask(r.pool.QueryRow(ctx, sqlReachTaskUpdateStatus, status, blockedReason, sentAtArg, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reach: pg task update status: %w", err)
	}
	return t, nil
}

// UpdateTask rewrites an existing task row matched by (t.TenantID, t.ID):
// status / blocked_reason / target_count, and sent_at when t.SentAt is non-zero
// (a zero SentAt is passed as NULL and COALESCE keeps the stored value, matching
// MemoryReachRepo). The deferred-send worker uses it to finalize the pending row
// persisted at enqueue time instead of creating a duplicate. ErrNotFound when the
// row is absent or owned by another tenant.
func (r *PGReachRepo) UpdateTask(ctx context.Context, t *ReachTask) (*ReachTask, error) {
	if r == nil || r.pool == nil || t == nil {
		return nil, ErrNotFound
	}
	var sentAtArg any
	if !t.SentAt.IsZero() {
		sentAtArg = t.SentAt
	}
	got, err := scanReachTask(r.pool.QueryRow(ctx, sqlReachTaskUpdate,
		t.Status, t.BlockedReason, sentAtArg, t.TargetCount, t.TenantID, t.ID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reach: pg task update: %w", err)
	}
	return got, nil
}

// GrantSubscribe idempotently accumulates quota for (tenant, member, tpl) as a
// single atomic ON CONFLICT upsert: a new auth is inserted with GrantedQuota =
// quota, UsedQuota = 0; an existing auth (matched by the UNIQUE index
// uq_subauth_tenant_member_tpl added in migration 000021) gets GrantedQuota +=
// quota with granted_at refreshed. The single statement is race-safe — two
// concurrent first-grants can no longer both insert (the old read-then-write
// did, corrupting the one-time quota accounting). Mirrors MemoryReachRepo, whose
// single mutex already serialised the accumulate.
func (r *PGReachRepo) GrantSubscribe(ctx context.Context, tenantID, memberID int64, tplID string, quota int, now time.Time) (*SubscribeAuth, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	return scanSubscribeAuth(r.pool.QueryRow(ctx, sqlSubAuthGrantUpsert, tenantID, memberID, tplID, quota, now))
}

// ConsumeSubscribe atomically increments UsedQuota iff used<granted via a single
// conditional UPDATE. "No row returned" (absent auth or exhausted) maps to
// ErrSubscribeExhausted with no state change — exactly the memory semantics
// (absent ⇒ exhausted). The CHECK(used_quota<=granted_quota) is a DB backstop.
func (r *PGReachRepo) ConsumeSubscribe(ctx context.Context, tenantID, memberID int64, tplID string, _ time.Time) (*SubscribeAuth, error) {
	if r == nil || r.pool == nil {
		return nil, ErrSubscribeExhausted
	}
	a, err := scanSubscribeAuth(r.pool.QueryRow(ctx, sqlSubAuthConsume, tenantID, memberID, tplID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSubscribeExhausted
		}
		return nil, fmt.Errorf("reach: pg consume subscribe: %w", err)
	}
	return a, nil
}

// GetSubscribe loads the authorization for (tenant, member, tpl). ErrNotFound
// when absent.
func (r *PGReachRepo) GetSubscribe(ctx context.Context, tenantID, memberID int64, tplID string) (*SubscribeAuth, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	a, err := scanSubscribeAuth(r.pool.QueryRow(ctx, sqlSubAuthGet, tenantID, memberID, tplID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reach: pg get subscribe: %w", err)
	}
	return a, nil
}

// Unsubscribe upserts the one (tenant, member, channel) opt-out row. A repeat
// unsubscribe refreshes reason + timestamp in place (ON CONFLICT over the
// UNIQUE(tenant, member, channel)).
func (r *PGReachRepo) Unsubscribe(ctx context.Context, tenantID, memberID int64, channel, reason string, now time.Time) (*UnsubscribeRecord, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	u, err := scanUnsubscribe(r.pool.QueryRow(ctx, sqlUnsubscribeUpsert, tenantID, memberID, channel, reason, now))
	if err != nil {
		return nil, fmt.Errorf("reach: pg unsubscribe: %w", err)
	}
	return u, nil
}

// IsUnsubscribed reports whether an active opt-out row exists for (tenant,
// member, channel). A nil pool reads as "not unsubscribed" (absent ⇒ false),
// matching memory.
func (r *PGReachRepo) IsUnsubscribed(ctx context.Context, tenantID, memberID int64, channel string) (bool, error) {
	if r == nil || r.pool == nil {
		return false, nil
	}
	var one int
	if err := r.pool.QueryRow(ctx, sqlUnsubscribedExists, tenantID, memberID, channel).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("reach: pg is unsubscribed: %w", err)
	}
	return true, nil
}

// Resubscribe clears the opt-out row for (tenant, member, channel). Idempotent:
// a missing row (or a nil pool) is a no-op success.
func (r *PGReachRepo) Resubscribe(ctx context.Context, tenantID, memberID int64, channel string) error {
	if r == nil || r.pool == nil {
		return nil
	}
	if _, err := r.pool.Exec(ctx, sqlResubscribe, tenantID, memberID, channel); err != nil {
		return fmt.Errorf("reach: pg resubscribe: %w", err)
	}
	return nil
}
