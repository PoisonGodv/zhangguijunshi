package auth

// pg_repo.go implements the pgx-backed ConsumerRepo (CONTRACT §17.2 / §17.7)
// over the narrow PGPool surface (QueryRow), the same idiom as payment.PGPool.
// A nil pool is a safe no-op so a misconfigured main thread degrades to
// ErrNotFound rather than panicking.
//
// Tenant isolation lives in every statement's WHERE tenant_id = $1. The upsert
// is INSERT ... ON CONFLICT (tenant_id, openid) DO UPDATE, equivalent to the
// memory repo's "exists → refresh session_key/unionid, else insert" semantics.
// member_id / version take DB defaults (0) on INSERT — cross-domain member_id
// backfill is deferred (§17.8).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface the PG repos need (CONTRACT §17.2).
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

// SQL for wx_sessions (migration 000014). UpsertByOpenID is an
// INSERT ... ON CONFLICT DO UPDATE that refreshes session_key/unionid on an
// existing (tenant_id, openid) row, returning the full record. member_id and
// version are not in the INSERT column list so they take DB defaults on insert
// and are preserved on conflict (matching the memory repo's update semantics).
const (
	sqlConsumerUpsert = `INSERT INTO wx_sessions (tenant_id, openid, unionid, session_key)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, openid) DO UPDATE SET session_key = EXCLUDED.session_key, unionid = EXCLUDED.unionid, updated_at = now()
RETURNING id, tenant_id, openid, unionid, member_id, session_key, version, created_at, updated_at`
	sqlConsumerGetByID = `SELECT id, tenant_id, openid, unionid, member_id, session_key, version, created_at, updated_at
FROM wx_sessions
WHERE tenant_id = $1 AND id = $2`
)

// ===== scan helpers =====

// scanConsumer decodes one wx_sessions row.
func scanConsumer(s rowScanner) (*Consumer, error) {
	var c Consumer
	if err := s.Scan(
		&c.ID, &c.TenantID, &c.OpenID, &c.UnionID, &c.MemberID,
		&c.SessionKey, &c.Version, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

// ===== Consumer =====

// PGConsumerRepo is the pgx-backed ConsumerRepo. A nil pool makes reads return
// ErrNotFound and writes return ErrNotFound.
type PGConsumerRepo struct{ pool PGPool }

// NewPGConsumerRepo constructs a PGConsumerRepo over pool.
func NewPGConsumerRepo(pool PGPool) *PGConsumerRepo { return &PGConsumerRepo{pool: pool} }

// UpsertByOpenID inserts a new (tenantID, openid) session or refreshes
// session_key/unionid on the existing row, returning the persisted record. A
// nil pool degrades to ErrNotFound (write cannot persist).
func (r *PGConsumerRepo) UpsertByOpenID(ctx context.Context, tenantID int64, openid, unionid, sessionKey string) (*Consumer, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	c, err := scanConsumer(r.pool.QueryRow(ctx, sqlConsumerUpsert, tenantID, openid, unionid, sessionKey))
	if err != nil {
		return nil, fmt.Errorf("auth: pg consumer upsert: %w", err)
	}
	return c, nil
}

// GetByID loads the consumer for (tenantID, id); ErrNotFound when absent or
// cross-tenant.
func (r *PGConsumerRepo) GetByID(ctx context.Context, tenantID, id int64) (*Consumer, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	c, err := scanConsumer(r.pool.QueryRow(ctx, sqlConsumerGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("auth: pg consumer get: %w", err)
	}
	return c, nil
}
