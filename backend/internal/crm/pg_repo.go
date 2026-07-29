package crm

// pg_repo.go is the pgx-backed Repo (CONTRACT §17 / §11.3.5) over migration
// 000006's community_groups + wework_living_codes tables. It mirrors the
// authoritative payment/pg_repo.go idiom verbatim: a narrow PGPool surface
// (QueryRow / Exec), multi-row reads type-asserting the concrete pool to
// queryer, per-statement tenant_id = $N row isolation, scan helpers that
// decode nullable / JSONB columns, and a nil pool that degrades safely to
// ErrNotFound / empty rather than panicking.
//
// The two mutating critical sections are single atomic statements (matching
// the memory repo's locked critical sections): UpdateGroupSOP is a conditional
// UPDATE ... RETURNING *, and AddGroupMembers is a signed-delta UPDATE clamped
// at 0 via GREATEST(member_count + $1, 0). Cross-tenant access is reported as
// ErrNotFound (absent and foreign are indistinguishable by design — ARCH-01).

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

// SQL for community_groups (migration 000006). sop_stage is nullable; an empty
// stage is stored as NULL (the column CHECK only admits the four funnel stages
// or NULL), and CreateGroup seeds an empty stage to recruit. Every read/update
// is scoped by tenant_id.
const (
	sqlGroupCreate = `INSERT INTO community_groups (tenant_id, store_id, wework_chat_id, name, member_count, sop_stage, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id`
	sqlGroupGetByID = `SELECT id, tenant_id, store_id, wework_chat_id, name, member_count, sop_stage, created_at, updated_at
FROM community_groups
WHERE tenant_id = $1 AND id = $2`
	sqlGroupListAll    = `SELECT id, tenant_id, store_id, wework_chat_id, name, member_count, sop_stage, created_at, updated_at FROM community_groups WHERE tenant_id = $1 ORDER BY id`
	sqlGroupListStore  = `SELECT id, tenant_id, store_id, wework_chat_id, name, member_count, sop_stage, created_at, updated_at FROM community_groups WHERE tenant_id = $1 AND store_id = $2 ORDER BY id`
	sqlGroupUpdateSOP  = `UPDATE community_groups SET sop_stage = $1, updated_at = $2 WHERE tenant_id = $3 AND id = $4 RETURNING id, tenant_id, store_id, wework_chat_id, name, member_count, sop_stage, created_at, updated_at`
	sqlGroupAddMembers = `UPDATE community_groups SET member_count = GREATEST(member_count + $1, 0), updated_at = $2 WHERE tenant_id = $3 AND id = $4 RETURNING id, tenant_id, store_id, wework_chat_id, name, member_count, sop_stage, created_at, updated_at`
)

// SQL for wework_living_codes (migration 000006). store_id is nullable (a code
// can fan across stores); config is JSONB ({auto_assign_rule, welcome_msg}).
// UNIQUE(tenant_id, code) makes GetCode-by-slug a tenant-scoped point lookup.
const (
	sqlCodeCreate = `INSERT INTO wework_living_codes (tenant_id, store_id, code, scene, config, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id`
	sqlCodeGetByCode = `SELECT id, tenant_id, store_id, code, scene, config, created_at FROM wework_living_codes WHERE tenant_id = $1 AND code = $2`
	sqlCodeListAll   = `SELECT id, tenant_id, store_id, code, scene, config, created_at FROM wework_living_codes WHERE tenant_id = $1 ORDER BY id`
	sqlCodeListStore = `SELECT id, tenant_id, store_id, code, scene, config, created_at FROM wework_living_codes WHERE tenant_id = $1 AND store_id = $2 ORDER BY id`
)

// ===== scan helpers =====

// scanCommunityGroup decodes one community_groups row. wework_chat_id and
// sop_stage are nullable (NULL → empty string, matching the memory repo's
// zero-value semantics).
func scanCommunityGroup(s rowScanner) (*CommunityGroup, error) {
	var g CommunityGroup
	var weworkChatID sql.NullString
	var sopStage sql.NullString
	if err := s.Scan(
		&g.ID, &g.TenantID, &g.StoreID, &weworkChatID, &g.Name, &g.MemberCount,
		&sopStage, &g.CreatedAt, &g.UpdatedAt,
	); err != nil {
		return nil, err
	}
	g.WeworkChatID = weworkChatID.String
	g.SOPStage = sopStage.String
	return &g, nil
}

// scanLivingCode decodes one wework_living_codes row. store_id is nullable
// (NULL → 0); config is JSONB decoded into LivingCodeConfig.
func scanLivingCode(s rowScanner) (*LivingCode, error) {
	var lc LivingCode
	var storeID sql.NullInt64
	var configRaw []byte
	if err := s.Scan(
		&lc.ID, &lc.TenantID, &storeID, &lc.Code, &lc.Scene, &configRaw, &lc.CreatedAt,
	); err != nil {
		return nil, err
	}
	lc.StoreID = storeID.Int64
	if len(configRaw) > 0 {
		_ = json.Unmarshal(configRaw, &lc.Config)
	}
	return &lc, nil
}

// nullInt64 converts an int64 store id to sql.NullInt64 (0 → NULL), used for
// wework_living_codes.store_id which is nullable.
func nullInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

// nullString converts a string to sql.NullString (empty → NULL), used for the
// nullable wework_chat_id and for sop_stage (whose CHECK only admits the four
// funnel stages or NULL).
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// ===== PGRepo =====

// PGRepo is the pgx-backed Repo. A nil pool makes single reads return
// ErrNotFound, writes return ErrNotFound, and lists return an empty slice —
// mirroring the payment样板 so a misconfigured main thread degrades gracefully.
type PGRepo struct{ pool PGPool }

// NewPGRepo constructs a PGRepo over pool.
func NewPGRepo(pool PGPool) *PGRepo { return &PGRepo{pool: pool} }

// CreateGroup persists a new community group. An empty SOPStage is seeded to
// recruit (so the funnel always has a start) and zero timestamps default to now,
// matching the memory repo. The allocated id is read back via RETURNING.
func (r *PGRepo) CreateGroup(ctx context.Context, g *CommunityGroup) (*CommunityGroup, error) {
	if r == nil || r.pool == nil || g == nil {
		return nil, ErrNotFound
	}
	if g.SOPStage == "" {
		g.SOPStage = SOPRecruit
	}
	now := time.Now()
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	if g.UpdatedAt.IsZero() {
		g.UpdatedAt = now
	}
	if err := r.pool.QueryRow(ctx, sqlGroupCreate,
		g.TenantID, g.StoreID, nullString(g.WeworkChatID), g.Name, g.MemberCount,
		nullString(g.SOPStage), g.CreatedAt, g.UpdatedAt,
	).Scan(&g.ID); err != nil {
		return nil, fmt.Errorf("crm: pg create group: %w", err)
	}
	return cloneGroup(g), nil
}

// GetGroup loads a community group by id within tenantID; ErrNotFound when
// absent or owned by another tenant.
func (r *PGRepo) GetGroup(ctx context.Context, tenantID, id int64) (*CommunityGroup, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	g, err := scanCommunityGroup(r.pool.QueryRow(ctx, sqlGroupGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("crm: pg get group: %w", err)
	}
	return g, nil
}

// ListGroups returns the tenant's groups, optionally narrowed by store
// (storeID <= 0 = all stores), ordered by id ascending. A pool lacking Query
// (bare PGPool) yields an empty slice.
func (r *PGRepo) ListGroups(ctx context.Context, tenantID, storeID int64) ([]*CommunityGroup, error) {
	out := make([]*CommunityGroup, 0)
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
		rows, err = q.Query(ctx, sqlGroupListStore, tenantID, storeID)
	} else {
		rows, err = q.Query(ctx, sqlGroupListAll, tenantID)
	}
	if err != nil {
		return nil, fmt.Errorf("crm: pg list groups: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		g, err := scanCommunityGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("crm: pg list groups scan: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("crm: pg list groups rows: %w", err)
	}
	return out, nil
}

// UpdateGroupSOP rewrites a group's SOP stage and stamps updated_at in a single
// conditional UPDATE ... RETURNING. ErrNotFound when the group is absent or
// owned by another tenant (RETURNING yields no rows). An empty stage is stored
// as NULL to honour the column CHECK.
func (r *PGRepo) UpdateGroupSOP(ctx context.Context, tenantID, id int64, stage string, now time.Time) (*CommunityGroup, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	g, err := scanCommunityGroup(r.pool.QueryRow(ctx, sqlGroupUpdateSOP, nullString(stage), now, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("crm: pg update group sop: %w", err)
	}
	return g, nil
}

// AddGroupMembers applies a signed delta to a group's MemberCount in a single
// atomic UPDATE, clamped at 0 via GREATEST(member_count + $1, 0) (delta may be
// negative — members leaving). ErrNotFound when the group is absent or foreign.
func (r *PGRepo) AddGroupMembers(ctx context.Context, tenantID, id int64, delta int, now time.Time) (*CommunityGroup, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	g, err := scanCommunityGroup(r.pool.QueryRow(ctx, sqlGroupAddMembers, delta, now, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("crm: pg add group members: %w", err)
	}
	return g, nil
}

// CreateCode persists a new living code. A zero StoreID is stored as NULL (the
// code can fan across stores); config is marshalled to JSONB. Zero CreatedAt
// defaults to now. The allocated id is read back via RETURNING.
func (r *PGRepo) CreateCode(ctx context.Context, lc *LivingCode) (*LivingCode, error) {
	if r == nil || r.pool == nil || lc == nil {
		return nil, ErrNotFound
	}
	now := time.Now()
	if lc.CreatedAt.IsZero() {
		lc.CreatedAt = now
	}
	cfg, err := json.Marshal(lc.Config)
	if err != nil {
		return nil, fmt.Errorf("crm: pg create code marshal config: %w", err)
	}
	if err := r.pool.QueryRow(ctx, sqlCodeCreate,
		lc.TenantID, nullInt64(lc.StoreID), lc.Code, lc.Scene, cfg, lc.CreatedAt,
	).Scan(&lc.ID); err != nil {
		return nil, fmt.Errorf("crm: pg create code: %w", err)
	}
	return cloneCode(lc), nil
}

// GetCode loads a living code by its slug within tenantID; ErrNotFound when
// absent or owned by another tenant.
func (r *PGRepo) GetCode(ctx context.Context, tenantID int64, code string) (*LivingCode, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	lc, err := scanLivingCode(r.pool.QueryRow(ctx, sqlCodeGetByCode, tenantID, code))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("crm: pg get code: %w", err)
	}
	return lc, nil
}

// ListCodes returns the tenant's living codes, optionally narrowed by store
// (storeID <= 0 = all stores), ordered by id ascending. A pool lacking Query
// yields an empty slice.
func (r *PGRepo) ListCodes(ctx context.Context, tenantID, storeID int64) ([]*LivingCode, error) {
	out := make([]*LivingCode, 0)
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
		rows, err = q.Query(ctx, sqlCodeListStore, tenantID, storeID)
	} else {
		rows, err = q.Query(ctx, sqlCodeListAll, tenantID)
	}
	if err != nil {
		return nil, fmt.Errorf("crm: pg list codes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		lc, err := scanLivingCode(rows)
		if err != nil {
			return nil, fmt.Errorf("crm: pg list codes scan: %w", err)
		}
		out = append(out, lc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("crm: pg list codes rows: %w", err)
	}
	return out, nil
}
