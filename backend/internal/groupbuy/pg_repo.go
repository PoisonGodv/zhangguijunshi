package groupbuy

// pg_repo.go is the pgx-backed GroupbuyRepo (CONTRACT §17 / §11.3.1) over
// migration 000006's groupbuys + groupbuy_participants tables (plus the
// strategy_id column added by migration 000011, which the frozen Groupbuy
// struct / ListByStrategy method already carry). It mirrors the authoritative
// payment/pg_repo.go idiom: narrow PGPool (QueryRow / Exec), multi-row reads
// type-asserting to queryer, tenant_id = $N in every statement's WHERE, scan
// helpers for nullable / JSONB columns, and a nil pool that degrades to
// ErrNotFound / empty rather than panicking.
//
// JoinAtomic is the atomic critical section (CONTRACT §11.5.4). The in-memory
// repo serialises it under one mutex; this implementation reproduces its exact
// sentinel precedence (already-joined → full → closed) via read-then-mutate,
// then performs the two writes as: INSERT participant (UNIQUE(tenant_id,
// groupbuy_id, member_id) is the race-safety net → ErrAlreadyJoined) followed
// by a single conditional UPDATE that increments joined and, on reaching
// required_count, flips the squad to success. Cross-tenant access is reported
// as ErrNotFound (absent and foreign are indistinguishable — ARCH-01).

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

// PGPool is the narrow database surface the PG repo needs (CONTRACT §17.2.1).
// Structurally identical to payment.PGPool so the main thread can share one
// *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool
// and pgxmock expose on top of PGPool. The PGPool interface signature is frozen
// (no Query), so list methods type-assert the concrete pool to queryer; a pool
// lacking Query returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, q string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// txBeginner is the optional transaction capability used by JoinAtomic. Both
// *pgxpool.Pool and pgxmock.PgxPoolIface satisfy it; if a pool lacks it the join
// call degrades to the pre-existing two-step autocommit logic rather than
// panicking. groupbuy deliberately does NOT import pkg/db (that would couple a
// domain package to infra), so the transaction is driven through this local
// type-assert over the pool — the same idiom as inventory.ConsumeFIFOAtomic and
// membership.redeemAtomic.
type txBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// SQL for groupbuys (migration 000006 + strategy_id from 000011 + description/
// original_price_cents from 000024, CONTRACT §24.6). product_id is nullable
// (NULL when the squad binds no product); params is JSONB. Every read / update
// is scoped by tenant_id. JoinIncr is the atomic critical section: a single
// conditional UPDATE that increments joined and, on reaching required_count,
// flips status to success — the CASE references the pre-update joined value,
// and the WHERE guard (joined < required_count AND status = 'active') makes a
// concurrent join that raced the read reject with no rows (mapped to
// ErrGroupbuyFull, matching memory's "succeeded squad → full").
//
// UpdateStatus carries a DB-side `AND status = 'active'` guard so a transition
// can only land on a still-active squad. The Go-side isAllowedGroupbuyTransition
// pre-check only ever permits transitions *from* active, so the two agree; the
// guard closes the TOCTOU window where a concurrent finaliser/cancel flips the
// squad to a terminal status between the GetByID pre-read and the UPDATE. A
// guard miss (pre-read succeeded, UPDATE matched no rows) is mapped to
// ErrGroupbuyClosed — not ErrNotFound — so FinalizeExpired/Cancel silently skip
// a squad that was already settled, matching the memory repo's under-lock
// re-validation semantics.
const (
	sqlGBCreate = `INSERT INTO groupbuys (tenant_id, store_id, product_id, title, description, original_price_cents, params, required_count, joined, status, start_at, end_at, strategy_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING id`
	sqlGBGetByID = `SELECT id, tenant_id, store_id, product_id, title, description, original_price_cents, params, required_count, joined, status, start_at, end_at, strategy_id, created_at, updated_at
FROM groupbuys
WHERE tenant_id = $1 AND id = $2`
	sqlGBListActiveAll   = `SELECT id, tenant_id, store_id, product_id, title, description, original_price_cents, params, required_count, joined, status, start_at, end_at, strategy_id, created_at, updated_at FROM groupbuys WHERE tenant_id = $1 AND status = 'active' ORDER BY id`
	sqlGBListActiveStore = `SELECT id, tenant_id, store_id, product_id, title, description, original_price_cents, params, required_count, joined, status, start_at, end_at, strategy_id, created_at, updated_at FROM groupbuys WHERE tenant_id = $1 AND store_id = $2 AND status = 'active' ORDER BY id`
	sqlGBListByStrategy  = `SELECT id, tenant_id, store_id, product_id, title, description, original_price_cents, params, required_count, joined, status, start_at, end_at, strategy_id, created_at, updated_at FROM groupbuys WHERE tenant_id = $1 AND strategy_id = $2 ORDER BY id`
	sqlGBUpdateStatus    = `UPDATE groupbuys SET status = $1, updated_at = $2 WHERE tenant_id = $3 AND id = $4 AND status = 'active' RETURNING id, tenant_id, store_id, product_id, title, description, original_price_cents, params, required_count, joined, status, start_at, end_at, strategy_id, created_at, updated_at`
	sqlGBJoinIncr        = `UPDATE groupbuys SET joined = joined + 1, status = CASE WHEN joined + 1 >= required_count THEN 'success' ELSE status END, updated_at = $1 WHERE tenant_id = $2 AND id = $3 AND joined < required_count AND status = 'active' RETURNING id, tenant_id, store_id, product_id, title, description, original_price_cents, params, required_count, joined, status, start_at, end_at, strategy_id, created_at, updated_at`
)

// SQL for groupbuy_participants (migration 000006). UNIQUE(tenant_id,
// groupbuy_id, member_id) is the DB-side guard against double-join. No
// updated_at → no trigger.
const (
	sqlPartInsert      = `INSERT INTO groupbuy_participants (tenant_id, groupbuy_id, member_id, status, joined_at) VALUES ($1, $2, $3, $4, $5) RETURNING id, tenant_id, groupbuy_id, member_id, status, joined_at`
	sqlPartGetByMember = `SELECT id, tenant_id, groupbuy_id, member_id, status, joined_at FROM groupbuy_participants WHERE tenant_id = $1 AND groupbuy_id = $2 AND member_id = $3`
	sqlPartListByGB    = `SELECT id, tenant_id, groupbuy_id, member_id, status, joined_at FROM groupbuy_participants WHERE tenant_id = $1 AND groupbuy_id = $2 ORDER BY id`
	sqlPartMarkPaid    = `UPDATE groupbuy_participants SET status = 'paid' WHERE tenant_id = $1 AND groupbuy_id = $2 AND member_id = $3 AND status = 'joined' RETURNING id, tenant_id, groupbuy_id, member_id, status, joined_at`
)

// ===== scan helpers =====

// scanGroupbuy decodes one groupbuys row. product_id is nullable (NULL → 0);
// params is JSONB decoded into GroupbuyParams. description/original_price_cents
// are NOT NULL with ” / 0 defaults (migration 000024).
func scanGroupbuy(s rowScanner) (*Groupbuy, error) {
	var g Groupbuy
	var productID sql.NullInt64
	var paramsRaw []byte
	if err := s.Scan(
		&g.ID, &g.TenantID, &g.StoreID, &productID, &g.Title, &g.Desc, &g.OriginalPriceCents, &paramsRaw,
		&g.RequiredCount, &g.Joined, &g.Status, &g.StartAt, &g.EndAt,
		&g.StrategyID, &g.CreatedAt, &g.UpdatedAt,
	); err != nil {
		return nil, err
	}
	g.ProductID = productID.Int64
	if len(paramsRaw) > 0 {
		_ = json.Unmarshal(paramsRaw, &g.Params)
	}
	return &g, nil
}

// scanParticipant decodes one groupbuy_participants row.
func scanParticipant(s rowScanner) (*Participant, error) {
	var p Participant
	if err := s.Scan(
		&p.ID, &p.TenantID, &p.GroupbuyID, &p.MemberID, &p.Status, &p.JoinedAt,
	); err != nil {
		return nil, err
	}
	return &p, nil
}

// isUniqueViolation reports whether err is a Postgres unique_violation (SQLSTATE
// 23505) — the race-safety net behind JoinAtomic's participant INSERT.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ===== PGGroupbuyRepo =====

// PGGroupbuyRepo is the pgx-backed GroupbuyRepo. A nil pool makes single reads
// return ErrNotFound, writes return ErrNotFound, and lists return an empty
// slice — mirroring the payment样板 so a misconfigured main thread degrades
// gracefully.
type PGGroupbuyRepo struct{ pool PGPool }

// NewPGGroupbuyRepo constructs a PGGroupbuyRepo over pool.
func NewPGGroupbuyRepo(pool PGPool) *PGGroupbuyRepo { return &PGGroupbuyRepo{pool: pool} }

// Create persists a new squad. product_id is stored as NULL when 0; params is
// marshalled to JSONB. Zero CreatedAt defaults to now and UpdatedAt is stamped
// now, matching the memory repo. The allocated id is read back via RETURNING.
func (r *PGGroupbuyRepo) Create(ctx context.Context, g *Groupbuy) (*Groupbuy, error) {
	if r == nil || r.pool == nil || g == nil {
		return nil, ErrNotFound
	}
	now := time.Now()
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	if g.UpdatedAt.IsZero() {
		g.UpdatedAt = now
	}
	params, err := json.Marshal(g.Params)
	if err != nil {
		return nil, fmt.Errorf("groupbuy: pg create marshal params: %w", err)
	}
	var productID any = g.ProductID
	if g.ProductID == 0 {
		productID = nil // nullable column
	}
	if err := r.pool.QueryRow(ctx, sqlGBCreate,
		g.TenantID, g.StoreID, productID, g.Title, g.Desc, g.OriginalPriceCents, params, g.RequiredCount, g.Joined,
		g.Status, g.StartAt, g.EndAt, g.StrategyID, g.CreatedAt, g.UpdatedAt,
	).Scan(&g.ID); err != nil {
		return nil, fmt.Errorf("groupbuy: pg create: %w", err)
	}
	return cloneGroupbuy(g), nil
}

// GetByID loads a squad by id within tenantID; ErrNotFound when absent or owned
// by another tenant.
func (r *PGGroupbuyRepo) GetByID(ctx context.Context, tenantID, id int64) (*Groupbuy, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	g, err := scanGroupbuy(r.pool.QueryRow(ctx, sqlGBGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("groupbuy: pg get by id: %w", err)
	}
	return g, nil
}

// ListActive returns status=active squads for tenantID. storeID > 0 narrows by
// store; storeID <= 0 means all stores. Ordered by id ascending. A pool lacking
// Query yields an empty slice.
func (r *PGGroupbuyRepo) ListActive(ctx context.Context, tenantID, storeID int64) ([]*Groupbuy, error) {
	out := make([]*Groupbuy, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	var rows pgx.Rows
	var err error
	if storeID > 0 {
		rows, err = q.Query(ctx, sqlGBListActiveStore, tenantID, storeID)
	} else {
		rows, err = q.Query(ctx, sqlGBListActiveAll, tenantID)
	}
	if err != nil {
		return nil, fmt.Errorf("groupbuy: pg list active: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		g, err := scanGroupbuy(rows)
		if err != nil {
			return nil, fmt.Errorf("groupbuy: pg list active scan: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("groupbuy: pg list active rows: %w", err)
	}
	return out, nil
}

// ListByStrategy returns every squad (all statuses) attributed to strategyID
// within tenantID. strategyID <= 0 yields an empty (non-nil) slice, matching the
// memory repo (non-AI source). Ordered by id ascending.
func (r *PGGroupbuyRepo) ListByStrategy(ctx context.Context, tenantID, strategyID int64) ([]*Groupbuy, error) {
	out := make([]*Groupbuy, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	if strategyID <= 0 {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlGBListByStrategy, tenantID, strategyID)
	if err != nil {
		return nil, fmt.Errorf("groupbuy: pg list by strategy: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		g, err := scanGroupbuy(rows)
		if err != nil {
			return nil, fmt.Errorf("groupbuy: pg list by strategy scan: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("groupbuy: pg list by strategy rows: %w", err)
	}
	return out, nil
}

// UpdateStatus transitions a squad's status. The transition is re-validated in
// Go (isAllowedGroupbuyTransition, shared with the memory repo) so a terminal
// squad cannot be corrupted: an illegal transition yields ErrGroupbuyClosed;
// absent/foreign yields ErrNotFound. The UPDATE then RETURNING * the new row.
//
// The UPDATE carries a DB-side `AND status = 'active'` guard closing the TOCTOU
// window between the GetByID pre-read and the write: if a concurrent finaliser/
// cancel flipped the squad to a terminal status in that gap, the UPDATE matches
// no rows and we return ErrGroupbuyClosed (NOT ErrNotFound) so FinalizeExpired/
// Cancel silently skip an already-settled squad — matching the memory repo's
// under-lock re-validation.
func (r *PGGroupbuyRepo) UpdateStatus(ctx context.Context, tenantID, id int64, status string, now time.Time) (*Groupbuy, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	current, err := scanGroupbuy(r.pool.QueryRow(ctx, sqlGBGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("groupbuy: pg update status load: %w", err)
	}
	if !isAllowedGroupbuyTransition(current.Status, status) {
		return nil, ErrGroupbuyClosed
	}
	g, err := scanGroupbuy(r.pool.QueryRow(ctx, sqlGBUpdateStatus, status, now, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The pre-read saw an active squad and the Go-side check passed, so a
			// no-row guarded UPDATE means a concurrent writer settled the squad in
			// the gap. Report closed (silent-skip semantics), not not-found.
			return nil, ErrGroupbuyClosed
		}
		return nil, fmt.Errorf("groupbuy: pg update status: %w", err)
	}
	return g, nil
}

// JoinAtomic is the atomic critical section (CONTRACT §11.5.4). Sentinel
// precedence matches the memory repo exactly — already-joined, then full, then
// closed — via a read of the squad + a read of any existing participant. The
// two writes (INSERT participant, then the conditional joined++/success UPDATE)
// run inside a single transaction when the pool supports BeginTx (both
// *pgxpool.Pool and pgxmock do), so they are all-or-nothing: if the guarded
// UPDATE returns no rows (a concurrent join filled the squad past the read) the
// transaction rolls back and the just-inserted participant row is rolled back
// too — no orphan participant, no miscounted joined, no permanently-locked
// member — and the call maps to ErrGroupbuyFull. A pool lacking BeginTx degrades
// to the legacy two-step autocommit path (UNIQUE constraint + guarded UPDATE
// remain the race-safety nets there).
func (r *PGGroupbuyRepo) JoinAtomic(ctx context.Context, tenantID, groupbuyID, memberID int64, now time.Time) (*Participant, *Groupbuy, error) {
	if r == nil || r.pool == nil {
		return nil, nil, ErrNotFound
	}
	// 1. Load the squad (cross-tenant / absent → ErrNotFound).
	current, err := scanGroupbuy(r.pool.QueryRow(ctx, sqlGBGetByID, tenantID, groupbuyID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("groupbuy: pg join load squad: %w", err)
	}
	// 2. Already-joined check (highest precedence, as in memory).
	if _, err := scanParticipant(r.pool.QueryRow(ctx, sqlPartGetByMember, tenantID, groupbuyID, memberID)); err == nil {
		return nil, nil, ErrAlreadyJoined
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("groupbuy: pg join member check: %w", err)
	}
	// 3. Full check — also catches a succeeded squad (Joined == RequiredCount).
	if current.Joined >= current.RequiredCount {
		return nil, nil, ErrGroupbuyFull
	}
	// 4. Closed check — a failed/cancelled squad has, by construction, Joined <
	// RequiredCount, so it falls through to here.
	if current.Status != StatusActive {
		return nil, nil, ErrGroupbuyClosed
	}
	// 5+6. The two writes. Prefer a real transaction so the participant INSERT
	// and the joined++ UPDATE commit or roll back together.
	if beginner, ok := r.pool.(txBeginner); ok {
		return r.joinAtomicTx(ctx, beginner, tenantID, groupbuyID, memberID, now)
	}
	// Degrade: pool lacks BeginTx → legacy two-step autocommit.
	return r.joinAtomicTwoStep(ctx, tenantID, groupbuyID, memberID, now)
}

// joinAtomicTx runs JoinAtomic steps 5 (INSERT participant) and 6 (conditional
// joined++/success UPDATE) inside one transaction. A UNIQUE violation on the
// INSERT (concurrent join raced step 2) → ErrAlreadyJoined; a no-row guarded
// UPDATE (concurrent join filled the squad past the step-1 read) →
// ErrGroupbuyFull with the deferred Rollback rolling back the inserted
// participant so no orphan row survives. On success the transaction commits and
// both the participant and the updated squad are returned.
func (r *PGGroupbuyRepo) joinAtomicTx(ctx context.Context, beginner txBeginner, tenantID, groupbuyID, memberID int64, now time.Time) (*Participant, *Groupbuy, error) {
	tx, err := beginner.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("groupbuy: pg join begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// 5. Insert the participant (UNIQUE(tenant_id, groupbuy_id, member_id) →
	// ErrAlreadyJoined if a concurrent join raced step 2).
	p, err := scanParticipant(tx.QueryRow(ctx, sqlPartInsert, tenantID, groupbuyID, memberID, PStatusJoined, now))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, nil, ErrAlreadyJoined
		}
		return nil, nil, fmt.Errorf("groupbuy: pg join insert participant: %w", err)
	}
	// 6. Atomic joined++ and maybe success. The WHERE guard makes a race that
	// filled the squad between the read and here return no rows → ErrGroupbuyFull;
	// the deferred Rollback rolls back the participant inserted in step 5 (no orphan).
	g, err := scanGroupbuy(tx.QueryRow(ctx, sqlGBJoinIncr, now, tenantID, groupbuyID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrGroupbuyFull
		}
		return nil, nil, fmt.Errorf("groupbuy: pg join incr: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("groupbuy: pg join commit: %w", err)
	}
	committed = true
	return p, g, nil
}

// joinAtomicTwoStep is the legacy fallback for a pool that lacks BeginTx: the
// participant INSERT and the joined++ UPDATE run as two autocommit statements.
// The UNIQUE constraint and the guarded UPDATE remain the race-safety nets, but
// a no-row UPDATE here can leave an orphan participant (the very bug the
// transaction path fixes) — hence this path is only taken when no transaction
// capability is available.
func (r *PGGroupbuyRepo) joinAtomicTwoStep(ctx context.Context, tenantID, groupbuyID, memberID int64, now time.Time) (*Participant, *Groupbuy, error) {
	p, err := scanParticipant(r.pool.QueryRow(ctx, sqlPartInsert, tenantID, groupbuyID, memberID, PStatusJoined, now))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, nil, ErrAlreadyJoined
		}
		return nil, nil, fmt.Errorf("groupbuy: pg join insert participant: %w", err)
	}
	g, err := scanGroupbuy(r.pool.QueryRow(ctx, sqlGBJoinIncr, now, tenantID, groupbuyID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrGroupbuyFull
		}
		return nil, nil, fmt.Errorf("groupbuy: pg join incr: %w", err)
	}
	return p, g, nil
}

// GetParticipant loads a participant by (groupbuyID, memberID) within tenantID.
// ErrNotFound when the member never joined (or the squad is foreign — no
// participant row carries another tenant's id).
func (r *PGGroupbuyRepo) GetParticipant(ctx context.Context, tenantID, groupbuyID, memberID int64) (*Participant, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	p, err := scanParticipant(r.pool.QueryRow(ctx, sqlPartGetByMember, tenantID, groupbuyID, memberID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("groupbuy: pg get participant: %w", err)
	}
	return p, nil
}

// ListParticipants returns every participant of a squad (matched by id +
// tenant), ordered by participant id ascending. Matches the memory repo: a
// foreign / absent squad yields ErrNotFound rather than an empty list.
func (r *PGGroupbuyRepo) ListParticipants(ctx context.Context, tenantID, groupbuyID int64) ([]*Participant, error) {
	out := make([]*Participant, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	// Foreign / absent squad → ErrNotFound (not an empty list).
	if _, err := scanGroupbuy(r.pool.QueryRow(ctx, sqlGBGetByID, tenantID, groupbuyID)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("groupbuy: pg list participants load squad: %w", err)
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlPartListByGB, tenantID, groupbuyID)
	if err != nil {
		return nil, fmt.Errorf("groupbuy: pg list participants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanParticipant(rows)
		if err != nil {
			return nil, fmt.Errorf("groupbuy: pg list participants scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("groupbuy: pg list participants rows: %w", err)
	}
	return out, nil
}

// MarkPaid transitions a participant joined → paid (payment closed-loop deferred
// to M4; M2-S2 only persists the state flag). Idempotent on already-paid /
// refunded (returns the read row unchanged). ErrNotFound when the participant
// is absent/foreign. The conditional UPDATE (status = 'joined' guard) is the
// race-safety net; a guard failure returns the read row as-is.
func (r *PGGroupbuyRepo) MarkPaid(ctx context.Context, tenantID, groupbuyID, memberID int64, _ time.Time) (*Participant, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	p, err := scanParticipant(r.pool.QueryRow(ctx, sqlPartGetByMember, tenantID, groupbuyID, memberID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("groupbuy: pg mark paid load: %w", err)
	}
	if p.Status != PStatusJoined {
		return p, nil // idempotent: already paid / refunded
	}
	updated, err := scanParticipant(r.pool.QueryRow(ctx, sqlPartMarkPaid, tenantID, groupbuyID, memberID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return p, nil // raced out of joined; return the read copy
		}
		return nil, fmt.Errorf("groupbuy: pg mark paid: %w", err)
	}
	return updated, nil
}
