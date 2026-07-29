package store

// pg_repo.go implements the pgx-backed StoreRepo (FE-MP-02 / FE-ADMIN, CONTRACT
// §16.11). It reads the stores table created by migration 000001, plus the
// phone / group_qr columns added by 000016. The pool surface is the narrow
// PGPool (QueryRow / Exec); multi-row reads type-assert the concrete pool to
// queryer (both *pgxpool.Pool and pgxmock.PgxPoolIface satisfy it) so the
// frozen PGPool interface stays minimal. A nil pool is a safe no-op so a
// misconfigured main thread degrades to an empty list rather than panicking.
//
// Tenant isolation lives in the statement's WHERE tenant_id = $1 (ARCH-01);
// cross-tenant access is indistinguishable from absence (empty slice). The
// status column ('active' | 'closed') maps to the Store.Active bool. Nullable
// text columns (address / phone / group_qr) scan via database/sql NullString
// so a missing value reads as "".

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface the PG store repo needs, structurally
// identical to membership.PGPool / marketing.PGPool so the main thread can
// share one *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, query string, args ...any) pgx.Row
	Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool
// and pgxmock expose on top of PGPool. The PGPool interface signature is frozen
// (no Query), so ListStores type-asserts the concrete pool to queryer; a pool
// lacking Query returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, query string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// sqlStoreList selects the C-side subset of a tenant's active stores. Every
// read carries WHERE tenant_id = $1 (ARCH-01). address / phone / group_qr are
// nullable (NULL → ""). status='active' maps to Active=true. deleted_at IS
// NULL skips soft-deleted stores. Ordered by id for deterministic output.
const sqlStoreList = `SELECT id, tenant_id, COALESCE(address,''), COALESCE(phone,''), COALESCE(group_qr,''), status = 'active', created_at
FROM stores
WHERE tenant_id = $1 AND deleted_at IS NULL
ORDER BY id`

// ===== scan helpers =====

// scanStore decodes one stores row. address / phone / group_qr are nullable
// (a store may omit any of them); they scan into sql.NullString and flatten to
// "". The status='active' boolean expression scans directly into Store.Active.
func scanStore(s rowScanner) (*Store, error) {
	var st Store
	var address, phone, groupQR sql.NullString
	if err := s.Scan(
		&st.ID, &st.TenantID, &address, &phone, &groupQR, &st.Active, &st.CreatedAt,
	); err != nil {
		return nil, err
	}
	st.Address = address.String
	st.Phone = phone.String
	st.GroupQR = groupQR.String
	return &st, nil
}

// ===== PGStoreRepo =====

// PGStoreRepo is the pgx-backed StoreRepo. A nil pool makes ListStores return
// an empty slice (no panic) so a misconfigured main thread degrades cleanly.
type PGStoreRepo struct{ pool PGPool }

// NewPGStoreRepo constructs a PGStoreRepo over pool.
func NewPGStoreRepo(pool PGPool) *PGStoreRepo {
	return &PGStoreRepo{pool: pool}
}

// ListStores returns the active stores of tenantID, ordered by id. Cross-tenant
// stores are never visible (filtered by tenant_id); soft-deleted and inactive
// stores are excluded. A nil pool / a pool lacking Query returns an empty slice.
func (r *PGStoreRepo) ListStores(ctx context.Context, tenantID int64) ([]*Store, error) {
	out := make([]*Store, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlStoreList, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: pg list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		s, err := scanStore(rows)
		if err != nil {
			return nil, fmt.Errorf("store: pg list scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pg list rows: %w", err)
	}
	return out, nil
}

// Compile-time assertion that PGStoreRepo satisfies StoreRepo.
var _ StoreRepo = (*PGStoreRepo)(nil)
