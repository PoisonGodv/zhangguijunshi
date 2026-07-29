package membership

// pg_repo.go implements the pgx-backed MemberRepo (CONTRACT §17.4 / §17.2),
// following internal/payment/pg_repo.go as the one authoritative template. The
// pool surface is the narrow PGPool (QueryRow / Exec); multi-row reads
// type-assert the concrete pool to queryer (both *pgxpool.Pool and
// pgxmock.PgxPoolIface satisfy it) so the frozen PGPool interface stays minimal.
// A nil pool is a safe no-op so a misconfigured main thread degrades to
// ErrNotFound / empty / nil-audit rather than panicking.
//
// Tenant isolation lives in every statement's WHERE tenant_id = $N (ARCH-01);
// cross-tenant access is indistinguishable from absence (both -> ErrNotFound).
// Nullable columns (phone_hash / wx_openid / last_visit_at / birthday /
// region_cap_cents / ref_order_id) are scanned via database/sql Null types.
//
// redeemAtomic / rechargeAtomic are the two multi-table critical sections
// (account, balance_log, member, points_log). The domain contract
// (membership.go / service.go doc comments: "the production pgx implementation
// will use a real transaction") plus the audit-log strong-audit invariant
// require these to be all-or-nothing, so they run inside a real pgx transaction
// (mirroring pkg/db/tx.go's txStarter pattern) rather than as loose autocommit
// statements: a mid-failure between the balance UPDATE and the audit INSERT
// would otherwise leave un-audited state. Balance overdraft is detected with a
// conditional UPDATE (balance_cents >= $1) whose 0-rows-affected outcome maps
// to ErrInsufficientBalance, the conditional-UPDATE row-affected idiom.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zhangguijunshi/backend/pkg/errcode"
)

// PGPool is the narrow database surface the PG repo needs (CONTRACT §17.2),
// structurally identical to payment.PGPool / attribution.PGPool so the main
// thread can share one *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, query string, args ...any) pgx.Row
	Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool
// and pgxmock expose on top of PGPool. The PGPool interface signature is frozen
// (no Query), so ListMembers type-asserts the concrete pool to queryer; a pool
// lacking Query returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, query string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// txStarter is the optional transaction surface (*pgxpool.Pool and pgxmock
// both satisfy it). redeemAtomic / rechargeAtomic type-assert the pool to this
// to run their multi-table critical sections inside a real transaction; a pool
// lacking BeginTx (including the nil-pool fast path) degrades to ErrNotFound.
// Mirrors pkg/db/tx.go's txStarter so it stays unit-testable with pgxmock.
type txStarter interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// SQL for members (migration 000004 + 000009 birthday). Every read/write
// statement carries WHERE tenant_id = $N. Empty identity strings are stored as
// NULL so the partial UNIQUE (tenant_id, phone_hash) / (tenant_id, wx_openid)
// indexes only apply to real identities (an empty phone never collides).
// Reads filter deleted_at IS NULL so a future admin soft-delete behaves as
// absent, matching the memory repo's "no delete path" semantics.
const (
	sqlMemberCreate = `INSERT INTO members (tenant_id, store_id, phone_hash, wx_openid, name, level, points, total_spent_cents, last_visit_at, birthday, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING id`
	sqlMemberGetByID = `SELECT id, tenant_id, store_id, phone_hash, wx_openid, name, level, points, total_spent_cents, last_visit_at, birthday, status, created_at, updated_at
FROM members
WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	sqlMemberUpdate = `UPDATE members
SET phone_hash = $1, wx_openid = $2, name = $3, level = $4, points = $5, total_spent_cents = $6, last_visit_at = $7, birthday = $8, status = $9
WHERE tenant_id = $10 AND id = $11 AND deleted_at IS NULL
RETURNING id, tenant_id, store_id, phone_hash, wx_openid, name, level, points, total_spent_cents, last_visit_at, birthday, status, created_at, updated_at`
	sqlMemberIDByPhone  = `SELECT id FROM members WHERE tenant_id = $1 AND phone_hash = $2 AND deleted_at IS NULL LIMIT 1`
	sqlMemberIDByOpenID = `SELECT id FROM members WHERE tenant_id = $1 AND wx_openid = $2 AND deleted_at IS NULL LIMIT 1`
	// sqlMemberRedeemTouch always advances last_visit_at; points += $1 is a
	// no-op when earnPoints == 0. RETURNING yields the post-update member so the
	// points_log pointsAfter is accurate without a racy re-read.
	sqlMemberRedeemTouch = `UPDATE members SET points = points + $1, last_visit_at = $2 WHERE tenant_id = $3 AND id = $4 AND deleted_at IS NULL RETURNING id, tenant_id, store_id, phone_hash, wx_openid, name, level, points, total_spent_cents, last_visit_at, birthday, status, created_at, updated_at`
	sqlMemberListPrefix  = `SELECT id, tenant_id, store_id, phone_hash, wx_openid, name, level, points, total_spent_cents, last_visit_at, birthday, status, created_at, updated_at FROM members WHERE tenant_id = $1 AND deleted_at IS NULL`
)

// SQL for member_balance_accounts (migration 000004). UpsertAccount is an
// INSERT ... ON CONFLICT (tenant_id, member_id) DO UPDATE that replaces
// balance/gift/cap in place (id preserved on conflict, matching memory's
// "replace in place" semantics). region_cap_cents is nullable; 0 (uncapped) is
// stored as NULL via nullInt64.
const (
	sqlAccountUpsert = `INSERT INTO member_balance_accounts (tenant_id, member_id, balance_cents, gift_cents, region_cap_cents, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, member_id) DO UPDATE SET balance_cents = EXCLUDED.balance_cents, gift_cents = EXCLUDED.gift_cents, region_cap_cents = EXCLUDED.region_cap_cents
RETURNING id, tenant_id, member_id, balance_cents, gift_cents, region_cap_cents, created_at, updated_at`
	sqlAccountGet = `SELECT id, tenant_id, member_id, balance_cents, gift_cents, region_cap_cents, created_at, updated_at FROM member_balance_accounts WHERE tenant_id = $1 AND member_id = $2`
	// sqlAccountDeductBalance is the conditional overdraft guard: the WHERE
	// balance_cents >= $1 clause means 0 rows affected is equivalent to
	// insufficient balance (or no account); redeemAtomic disambiguates the two
	// via sqlAccountExists.
	sqlAccountDeductBalance = `UPDATE member_balance_accounts SET balance_cents = balance_cents - $1 WHERE tenant_id = $2 AND member_id = $3 AND balance_cents >= $1 RETURNING id, tenant_id, member_id, balance_cents, gift_cents, region_cap_cents, created_at, updated_at`
	sqlAccountExists        = `SELECT 1 FROM member_balance_accounts WHERE tenant_id = $1 AND member_id = $2`
	sqlAccountCredit        = `UPDATE member_balance_accounts SET balance_cents = balance_cents + $1, gift_cents = gift_cents + $2 WHERE tenant_id = $3 AND member_id = $4 RETURNING id, tenant_id, member_id, balance_cents, gift_cents, region_cap_cents, created_at, updated_at`
	sqlAccountSetRegionCap  = `UPDATE member_balance_accounts SET region_cap_cents = $1 WHERE tenant_id = $2 AND member_id = $3`
)

// SQL for the audit logs (migration 000004). Both are append-only, no
// updated_at, no soft delete. ref_order_id is nullable (0 -> NULL). The balance
// log has an operator_id column but AppendBalanceLog's signature carries no
// operator, so it stays NULL (matching the memory repo's balanceFlow struct).
const (
	sqlBalanceLogAppend = `INSERT INTO member_balance_log (tenant_id, member_id, account_id, type, amount_cents, balance_after, ref_order_id, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	sqlPointsLogAppend  = `INSERT INTO member_points_log (tenant_id, member_id, type, delta, points_after, ref_order_id, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`
	// sqlPointsLogList returns the points-flow rows for (tenantID, memberID),
	// newest first (FE-MP-02 CONTRACT §16.3(8)). Every read carries
	// WHERE tenant_id = $N (ARCH-01). ref_order_id is nullable (NULL → 0).
	sqlPointsLogList = `SELECT id, tenant_id, member_id, type, delta, points_after, COALESCE(ref_order_id,0), created_at
FROM member_points_log
WHERE tenant_id = $1 AND member_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3`
)

// sqlMemberListCountPrefix is the COUNT(*) prefix for the ListMembersPage total
// (FE-MP-02 / FE-ADMIN boss list, CONTRACT §17.11). It shares the filter
// dimensions with sqlMemberListPrefix.
const sqlMemberListCountPrefix = `SELECT COUNT(*) FROM members WHERE tenant_id = $1 AND deleted_at IS NULL`

// scanPointsLog decodes one member_points_log row. ref_order_id is COALESCE'd
// to 0 in the SELECT, so it scans into a plain int64 (no NULL handling here).
func scanPointsLog(s rowScanner) (*PointsLog, error) {
	var p PointsLog
	if err := s.Scan(
		&p.ID, &p.TenantID, &p.MemberID, &p.Type, &p.Delta, &p.PointsAfter,
		&p.RefOrderID, &p.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &p, nil
}

// ===== scan helpers =====

// scanMember decodes one members row. phone_hash / wx_openid are nullable (a
// member needs only one identity); last_visit_at and birthday (DATE, migration
// 000009) are nullable. Birthday scans into sql.NullTime and re-points at a
// fresh time value so the returned struct owns no aliased state.
func scanMember(s rowScanner) (*Member, error) {
	var m Member
	var phoneHash, wxOpenID sql.NullString
	var lastVisit, birthday sql.NullTime
	if err := s.Scan(
		&m.ID, &m.TenantID, &m.StoreID, &phoneHash, &wxOpenID, &m.Name, &m.Level,
		&m.Points, &m.TotalSpentCents, &lastVisit, &birthday, &m.Status,
		&m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	m.PhoneHash = phoneHash.String
	m.WxOpenID = wxOpenID.String
	if lastVisit.Valid {
		t := lastVisit.Time
		m.LastVisitAt = &t
	}
	if birthday.Valid {
		t := birthday.Time
		m.Birthday = &t
	}
	return &m, nil
}

// scanAccount decodes one member_balance_accounts row. region_cap_cents is
// nullable (NULL / 0 means uncapped).
func scanAccount(s rowScanner) (*BalanceAccount, error) {
	var a BalanceAccount
	var regionCap sql.NullInt64
	if err := s.Scan(
		&a.ID, &a.TenantID, &a.MemberID, &a.BalanceCents, &a.GiftCents, &regionCap,
		&a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	a.RegionCapCents = regionCap.Int64
	return &a, nil
}

// nullStr converts a string to sql.NullString (empty -> NULL). Used for the
// nullable identity columns so empty values do not pollute the partial UNIQUE
// indexes.
func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// nullTime converts a time.Time to sql.NullTime (zero -> NULL). Mirrors the
// payment helper.
func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// nullTimePtr converts a *time.Time to sql.NullTime (nil / zero -> NULL). Used
// for last_visit_at and birthday.
func nullTimePtr(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return nullTime(*t)
}

// nullInt64 converts an int64 to sql.NullInt64 (0 -> NULL). Used for the
// nullable region_cap_cents (0 = uncapped) and ref_order_id (0 = none)
// columns.
func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: v != 0}
}

// ===== PGMemberRepo =====

// PGMemberRepo is the pgx-backed MemberRepo. A nil pool makes reads return
// ErrNotFound, audit writes return nil (no-op, matching the memory repo's
// cannot-fail audit semantics), and atomic critical sections return ErrNotFound.
type PGMemberRepo struct{ pool PGPool }

// NewPGMemberRepo constructs a PGMemberRepo over pool.
func NewPGMemberRepo(pool PGPool) *PGMemberRepo {
	return &PGMemberRepo{pool: pool}
}

// CreateMember inserts m and returns it with the allocated id. Empty identities
// are stored as NULL so they never collide on the partial UNIQUE indexes.
func (r *PGMemberRepo) CreateMember(ctx context.Context, m *Member) (*Member, error) {
	if r == nil || r.pool == nil || m == nil {
		return nil, ErrNotFound
	}
	if err := r.pool.QueryRow(ctx, sqlMemberCreate,
		m.TenantID, m.StoreID, nullStr(m.PhoneHash), nullStr(m.WxOpenID), m.Name, m.Level,
		m.Points, m.TotalSpentCents, nullTimePtr(m.LastVisitAt), nullTimePtr(m.Birthday),
		m.Status, m.CreatedAt, m.UpdatedAt,
	).Scan(&m.ID); err != nil {
		return nil, fmt.Errorf("membership: pg member create: %w", err)
	}
	return m, nil
}

// GetMember loads the member for (tenantID, id); ErrNotFound when absent,
// soft-deleted, or owned by another tenant.
func (r *PGMemberRepo) GetMember(ctx context.Context, tenantID, id int64) (*Member, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	m, err := scanMember(r.pool.QueryRow(ctx, sqlMemberGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("membership: pg member get: %w", err)
	}
	return m, nil
}

// UpdateMember rewrites the member (matched by ID + tenant) and returns the
// post-update row. ErrNotFound when absent, soft-deleted, or cross-tenant.
func (r *PGMemberRepo) UpdateMember(ctx context.Context, m *Member) (*Member, error) {
	if r == nil || r.pool == nil || m == nil {
		return nil, ErrNotFound
	}
	updated, err := scanMember(r.pool.QueryRow(ctx, sqlMemberUpdate,
		nullStr(m.PhoneHash), nullStr(m.WxOpenID), m.Name, m.Level, m.Points, m.TotalSpentCents,
		nullTimePtr(m.LastVisitAt), nullTimePtr(m.Birthday), m.Status, m.TenantID, m.ID,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("membership: pg member update: %w", err)
	}
	return updated, nil
}

// UpsertAccount creates-or-replaces the stored-value account for
// (a.TenantID, a.MemberID). On conflict the id is preserved and balance/gift/cap
// are replaced (matching memory's "replace in place"). Returns the stored row.
func (r *PGMemberRepo) UpsertAccount(ctx context.Context, a *BalanceAccount) (*BalanceAccount, error) {
	if r == nil || r.pool == nil || a == nil {
		return nil, ErrNotFound
	}
	stored, err := scanAccount(r.pool.QueryRow(ctx, sqlAccountUpsert,
		a.TenantID, a.MemberID, a.BalanceCents, a.GiftCents, nullInt64(a.RegionCapCents),
		a.CreatedAt, a.UpdatedAt,
	))
	if err != nil {
		return nil, fmt.Errorf("membership: pg account upsert: %w", err)
	}
	return stored, nil
}

// GetAccount loads the account for (tenantID, memberID); ErrNotFound when no
// account exists or it belongs to another tenant.
func (r *PGMemberRepo) GetAccount(ctx context.Context, tenantID, memberID int64) (*BalanceAccount, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	a, err := scanAccount(r.pool.QueryRow(ctx, sqlAccountGet, tenantID, memberID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("membership: pg account get: %w", err)
	}
	return a, nil
}

// AppendPointsLog appends one immutable points-flow row. A nil pool is a no-op
// success, matching the memory repo's cannot-fail audit semantics.
func (r *PGMemberRepo) AppendPointsLog(ctx context.Context, tenantID, memberID int64, typ string, delta, pointsAfter, refOrderID int64) error {
	if r == nil || r.pool == nil {
		return nil
	}
	if _, err := r.pool.Exec(ctx, sqlPointsLogAppend,
		tenantID, memberID, typ, delta, pointsAfter, nullInt64(refOrderID), time.Now(),
	); err != nil {
		return fmt.Errorf("membership: pg points log append: %w", err)
	}
	return nil
}

// AppendBalanceLog appends one immutable balance-flow row. A nil pool is a
// no-op success, matching the memory repo's cannot-fail audit semantics.
func (r *PGMemberRepo) AppendBalanceLog(ctx context.Context, tenantID, memberID, accountID int64, typ string, amountCents, balanceAfter, refOrderID int64) error {
	if r == nil || r.pool == nil {
		return nil
	}
	if _, err := r.pool.Exec(ctx, sqlBalanceLogAppend,
		tenantID, memberID, accountID, typ, amountCents, balanceAfter, nullInt64(refOrderID), time.Now(),
	); err != nil {
		return fmt.Errorf("membership: pg balance log append: %w", err)
	}
	return nil
}

// ListMembers enumerates tenantID's members matching f (M3-S2 signal
// enumeration + FE-MP-02 boss list). Filter dimensions map onto SQL:
// SleepBefore (members who never visited count as sleeping: last_visit_at IS
// NULL matches), MinPoints, BirthdayOn (month+day via TO_CHAR on the birthday
// DATE column, migration 000009), Level (equality), Query (ILIKE on name OR
// phone_hash). Limit<=0 defaults to 500. Cross-tenant members are never
// visible. A pool lacking Query (or a nil pool) returns an empty slice.
func (r *PGMemberRepo) ListMembers(ctx context.Context, tenantID int64, f MemberFilter) ([]*Member, error) {
	out := make([]*Member, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}

	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(sqlMemberListPrefix)
	args = append(args, tenantID)
	ph := 2 // $1 is tenant_id
	if f.SleepBefore != nil {
		sb.WriteString(fmt.Sprintf(` AND (last_visit_at IS NULL OR last_visit_at < $%d)`, ph))
		args = append(args, *f.SleepBefore)
		ph++
	}
	if f.MinPoints > 0 {
		sb.WriteString(fmt.Sprintf(` AND points >= $%d`, ph))
		args = append(args, f.MinPoints)
		ph++
	}
	if f.BirthdayOn != nil {
		md := fmt.Sprintf("%02d-%02d", int(f.BirthdayOn.Month()), f.BirthdayOn.Day())
		sb.WriteString(fmt.Sprintf(` AND birthday IS NOT NULL AND TO_CHAR(birthday, 'MM-DD') = $%d`, ph))
		args = append(args, md)
		ph++
	}
	if f.Level != "" {
		sb.WriteString(fmt.Sprintf(` AND level = $%d`, ph))
		args = append(args, f.Level)
		ph++
	}
	if f.Query != "" {
		// Loose substring match on name OR phone_hash (FE-MP-02 boss ?q=).
		// ILIKE is case-insensitive substring; phone_hash is opaque so a
		// human query rarely matches it, but name match is the practical path.
		sb.WriteString(fmt.Sprintf(` AND (name ILIKE $%d OR phone_hash ILIKE $%d)`, ph, ph))
		args = append(args, "%"+f.Query+"%")
		ph++
	}
	sb.WriteString(` ORDER BY id`)
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListMembersLimit
	}
	sb.WriteString(fmt.Sprintf(` LIMIT $%d`, ph))
	args = append(args, limit)

	rows, err := q.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("membership: pg member list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, fmt.Errorf("membership: pg member list scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("membership: pg member list rows: %w", err)
	}
	return out, nil
}

// ListMembersPage returns one page of tenantID's members matching f plus the
// total matching count (FE-MP-02 / FE-ADMIN boss list, CONTRACT §17.11). It
// runs two queries: a COUNT (sans Offset/Limit) for the total, then the paged
// SELECT with OFFSET/LIMIT. Filter dimensions map onto SQL identically to
// ListMembers (SleepBefore / MinPoints / BirthdayOn / Level / Query). A nil
// pool / a pool lacking Query returns an empty page with total 0.
func (r *PGMemberRepo) ListMembersPage(ctx context.Context, tenantID int64, f MemberFilter) (ListMembersResult, error) {
	empty := ListMembersResult{Items: []*Member{}, Total: 0}
	if r == nil || r.pool == nil {
		return empty, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return empty, nil
	}

	// Build the shared filter suffix once (sans ORDER BY / LIMIT / OFFSET).
	var (
		fsb  strings.Builder
		farg []any
	)
	fsb.WriteString(sqlMemberListPrefix)
	farg = append(farg, tenantID)
	ph := 2 // $1 is tenant_id
	if f.SleepBefore != nil {
		fsb.WriteString(fmt.Sprintf(` AND (last_visit_at IS NULL OR last_visit_at < $%d)`, ph))
		farg = append(farg, *f.SleepBefore)
		ph++
	}
	if f.MinPoints > 0 {
		fsb.WriteString(fmt.Sprintf(` AND points >= $%d`, ph))
		farg = append(farg, f.MinPoints)
		ph++
	}
	if f.BirthdayOn != nil {
		md := fmt.Sprintf("%02d-%02d", int(f.BirthdayOn.Month()), f.BirthdayOn.Day())
		fsb.WriteString(fmt.Sprintf(` AND birthday IS NOT NULL AND TO_CHAR(birthday, 'MM-DD') = $%d`, ph))
		farg = append(farg, md)
		ph++
	}
	if f.Level != "" {
		fsb.WriteString(fmt.Sprintf(` AND level = $%d`, ph))
		farg = append(farg, f.Level)
		ph++
	}
	if f.Query != "" {
		fsb.WriteString(fmt.Sprintf(` AND (name ILIKE $%d OR phone_hash ILIKE $%d)`, ph, ph))
		farg = append(farg, "%"+f.Query+"%")
		ph++
	}
	filterSQL := fsb.String()

	// 1) COUNT — total matching sans Offset/Limit. Replace the SELECT list with
	// COUNT(*) and drop the ORDER BY.
	var total int64
	countSQL := strings.Replace(filterSQL, `SELECT id, tenant_id, store_id, phone_hash, wx_openid, name, level, points, total_spent_cents, last_visit_at, birthday, status, created_at, updated_at FROM members`, `SELECT COUNT(*) FROM members`, 1)
	if err := r.pool.QueryRow(ctx, countSQL, farg...).Scan(&total); err != nil {
		return empty, fmt.Errorf("membership: pg member list count: %w", err)
	}
	if total == 0 {
		return empty, nil
	}

	// 2) PAGED SELECT — append ORDER BY + OFFSET/LIMIT.
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListMembersPageLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	pageSQL := filterSQL + fmt.Sprintf(` ORDER BY id LIMIT $%d OFFSET $%d`, ph, ph+1)
	pageArgs := append(append([]any{}, farg...), limit, offset)

	rows, err := q.Query(ctx, pageSQL, pageArgs...)
	if err != nil {
		return empty, fmt.Errorf("membership: pg member list page: %w", err)
	}
	defer rows.Close()
	out := make([]*Member, 0, limit)
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return empty, fmt.Errorf("membership: pg member list page scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return empty, fmt.Errorf("membership: pg member list page rows: %w", err)
	}
	return ListMembersResult{Items: out, Total: total}, nil
}

// ListPointsLog returns the points-flow rows for (tenantID, memberID), newest
// first (FE-MP-02 CONTRACT §16.3(8)). limit<=0 defaults to 50. Cross-tenant
// rows are never visible (filtered by tenant_id). A nil pool / a pool lacking
// Query returns an empty slice.
func (r *PGMemberRepo) ListPointsLog(ctx context.Context, tenantID, memberID int64, limit int) ([]PointsLog, error) {
	out := make([]PointsLog, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	if limit <= 0 {
		limit = defaultPointsLogLimit
	}
	rows, err := q.Query(ctx, sqlPointsLogList, tenantID, memberID, limit)
	if err != nil {
		return nil, fmt.Errorf("membership: pg points log list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPointsLog(rows)
		if err != nil {
			return nil, fmt.Errorf("membership: pg points log list scan: %w", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("membership: pg points log list rows: %w", err)
	}
	return out, nil
}

// findMemberIDByPhone returns the id of the member registered under
// (tenantID, phoneHash); ok=false when none. Drives Join idempotency. A nil pool
// yields (0, false).
func (r *PGMemberRepo) findMemberIDByPhone(tenantID int64, phoneHash string) (int64, bool) {
	if r == nil || r.pool == nil {
		return 0, false
	}
	var id int64
	if err := r.pool.QueryRow(context.Background(), sqlMemberIDByPhone, tenantID, phoneHash).Scan(&id); err != nil {
		return 0, false
	}
	return id, true
}

// findMemberIDByOpenID returns the id of the member registered under
// (tenantID, openID); ok=false when none. Drives Join idempotency. A nil pool
// yields (0, false).
func (r *PGMemberRepo) findMemberIDByOpenID(tenantID int64, openID string) (int64, bool) {
	if r == nil || r.pool == nil {
		return 0, false
	}
	var id int64
	if err := r.pool.QueryRow(context.Background(), sqlMemberIDByOpenID, tenantID, openID).Scan(&id); err != nil {
		return 0, false
	}
	return id, true
}

// redeemAtomic implements MemberRepo. It runs the whole Redeem critical section
// (conditional balance deduction + consume flow, points accrual + earn flow,
// last-visit touch) inside one pgx transaction so the audit logs and the
// balance/points they describe can never diverge. Balance overdraft is detected
// by the conditional UPDATE (balance_cents >= $1; 0 rows -> rejected) and
// disambiguated from a missing account via sqlAccountExists, mirroring the
// memory repo's distinct ErrNotFound / ErrInsufficientBalance outcomes. NO state
// is committed when the balance would overdraw (the transaction rolls back).
func (r *PGMemberRepo) redeemAtomic(tenantID, memberID int64, now time.Time, consumeCents, earnPoints, refOrderID int64) (*redeemOutcome, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	starter, ok := r.pool.(txStarter)
	if !ok {
		return nil, ErrNotFound
	}
	ctx := context.Background()
	tx, err := starter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("membership: pg redeem begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	var acc *BalanceAccount
	if consumeCents > 0 {
		// Conditional deduct: WHERE balance_cents >= $1 makes 0 rows equivalent
		// to overdraft OR no account. RETURNING gives the post-deduct balance.
		a, err := scanAccount(tx.QueryRow(ctx, sqlAccountDeductBalance, consumeCents, tenantID, memberID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Disambiguate: a missing account is ErrNotFound (matches memory);
				// an existing-but-insufficient account is ErrInsufficientBalance.
				var one int
				if err2 := tx.QueryRow(ctx, sqlAccountExists, tenantID, memberID).Scan(&one); err2 != nil {
					if errors.Is(err2, pgx.ErrNoRows) {
						return nil, ErrNotFound
					}
					return nil, fmt.Errorf("membership: pg redeem account exists: %w", err2)
				}
				return nil, ErrInsufficientBalance
			}
			return nil, fmt.Errorf("membership: pg redeem deduct: %w", err)
		}
		acc = a
		if _, err := tx.Exec(ctx, sqlBalanceLogAppend,
			tenantID, memberID, acc.ID, "consume", -consumeCents, acc.BalanceCents, nullInt64(refOrderID), now,
		); err != nil {
			return nil, fmt.Errorf("membership: pg redeem balance log: %w", err)
		}
	}

	// Always touch last_visit_at; points += earnPoints (no-op when 0). RETURNING
	// yields the post-update Points for the earn-log pointsAfter.
	m, err := scanMember(tx.QueryRow(ctx, sqlMemberRedeemTouch, earnPoints, now, tenantID, memberID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("membership: pg redeem member touch: %w", err)
	}

	if earnPoints > 0 {
		if _, err := tx.Exec(ctx, sqlPointsLogAppend,
			tenantID, memberID, "earn", earnPoints, m.Points, nullInt64(refOrderID), now,
		); err != nil {
			return nil, fmt.Errorf("membership: pg redeem points log: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("membership: pg redeem commit: %w", err)
	}
	committed = true

	out := &redeemOutcome{member: m}
	if acc != nil {
		out.account = acc
	}
	return out, nil
}

// rechargeAtomic implements MemberRepo (M4-S1 BE-B-12). It runs the whole
// Recharge critical section inside one pgx transaction: load the account (absent
// -> ErrNotFound), enforce the regional per-card cap (breach ->
// errcode.ErrBalanceOver with NO state changed; the transaction rolls back),
// credit balance+gift, and append the recharge balance-flow. operatorID is
// accepted for the audit contract but not yet persisted (the M1-S2 balance-log
// row has no operator column), matching the memory repo.
func (r *PGMemberRepo) rechargeAtomic(tenantID, memberID int64, now time.Time, amountCents, giftCents, refPaymentID, operatorID int64) (*BalanceAccount, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	starter, ok := r.pool.(txStarter)
	if !ok {
		return nil, ErrNotFound
	}
	ctx := context.Background()
	tx, err := starter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("membership: pg recharge begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// Load the account (existence + current balance/cap). ErrNotFound on absent
	// or cross-tenant, matching memory.
	acc, err := scanAccount(tx.QueryRow(ctx, sqlAccountGet, tenantID, memberID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("membership: pg recharge load: %w", err)
	}

	// Regional per-card cap (0 = uncapped). A breach leaves ALL state untouched
	// (rollback): no credit, no audit row.
	if acc.RegionCapCents > 0 && acc.BalanceCents+amountCents+giftCents > acc.RegionCapCents {
		return nil, errcode.ErrBalanceOver
	}

	// Credit balance + gift; RETURNING gives the post-credit totals for the log.
	credited, err := scanAccount(tx.QueryRow(ctx, sqlAccountCredit, amountCents, giftCents, tenantID, memberID))
	if err != nil {
		return nil, fmt.Errorf("membership: pg recharge credit: %w", err)
	}
	if _, err := tx.Exec(ctx, sqlBalanceLogAppend,
		tenantID, memberID, credited.ID, "recharge", amountCents, credited.BalanceCents, nullInt64(refPaymentID), now,
	); err != nil {
		return nil, fmt.Errorf("membership: pg recharge balance log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("membership: pg recharge commit: %w", err)
	}
	committed = true
	_ = operatorID // reserved for a future operator-audit column
	return credited, nil
}

// UpdateAccountRegionCap implements MemberRepo (M4-S1 BE-B-12). It sets
// region_cap_cents on the (tenantID, memberID) account; 0 (uncapped) is stored
// as NULL. Cross-tenant or absent account -> ErrNotFound (detected via
// RowsAffected so an UPDATE that matched no row is reported as absence).
func (r *PGMemberRepo) UpdateAccountRegionCap(ctx context.Context, tenantID, memberID, regionCapCents int64) error {
	if r == nil || r.pool == nil {
		return ErrNotFound
	}
	tag, err := r.pool.Exec(ctx, sqlAccountSetRegionCap, nullInt64(regionCapCents), tenantID, memberID)
	if err != nil {
		return fmt.Errorf("membership: pg set region cap: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
