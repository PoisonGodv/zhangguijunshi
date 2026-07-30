package auth

// account_pg_repo.go implements AccountRepo over pgx (CONTRACT §19 / BE-A-01)
// reading the users table (migration 000001 + 000017 password_hash + 000018
// version/username). It reuses the PGPool surface and scan helpers defined in
// pg_repo.go. A nil pool degrades every read to ErrAccountNotFound so a
// misconfigured main thread cannot panic (matches PGConsumerRepo's nil-safety
// contract).
//
// version: the users table has a real version BIGINT NOT NULL DEFAULT 0 column
// (migration 000018); the repo selects it directly and BumpVersion increments it
// (revoke-on-password-change hook). TouchLogin does not bump version.
//
// phone: users stores the phone encrypted (phone_enc BYTEA) plus phone_hash for
// equality lookup; FindByPhone hashes the input with PhoneHash and matches
// phone_hash. The plaintext phone is NOT decrypted (no KMS in this path), so
// Account.Phone is empty on the PG path.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SQL for users (migration 000001 + 000017 password_hash + 000018
// version/username). Every read carries
// WHERE tenant_id = $N AND deleted_at IS NULL AND status = 'active' so a
// soft-deleted or disabled account behaves as absent (matches the memory repo).
// version is the real version column (000018). FindByUsername matches the
// dedicated username column (per-tenant unique), NOT the display name column.
const (
	sqlAccountByPhone = `SELECT id, tenant_id, store_id, name, role, status, password_hash, version, last_login_at
FROM users
WHERE tenant_id = $1 AND phone_hash = $2 AND deleted_at IS NULL AND status = 'active'
LIMIT 1`
	sqlAccountByUsername = `SELECT id, tenant_id, store_id, name, role, status, password_hash, version, last_login_at
FROM users
WHERE tenant_id = $1 AND username = $2 AND deleted_at IS NULL AND status = 'active'
LIMIT 1`
	sqlAccountTouchLogin  = `UPDATE users SET last_login_at = now() WHERE id = $1`
	sqlAccountBumpVersion = `UPDATE users SET version = version + 1 WHERE id = $1 RETURNING version`
)

// scanAccount decodes one users row into an Account. store_id, password_hash
// and last_login_at are nullable and scan into sql.Null* (matches the membership
// pg_repo idiom); version is a non-null BIGINT (migration 000018).
func scanAccount(s rowScanner) (*Account, error) {
	var (
		a       Account
		storeID sql.NullInt64
		pwdHash sql.NullString
		last    sql.NullTime
	)
	if err := s.Scan(&a.ID, &a.TenantID, &storeID, &a.Name, &a.Role, &a.Status, &pwdHash, &a.Version, &last); err != nil {
		return nil, err
	}
	if storeID.Valid {
		a.StoreID = storeID.Int64
	}
	a.PasswordHash = pwdHash.String // "" when NULL (login flow rejects empty hash)
	if last.Valid {
		a.LastLoginAt = last.Time
	}
	return &a, nil
}

// PGAccountRepo is the pgx-backed AccountRepo. A nil pool makes reads return
// ErrAccountNotFound and TouchLogin a no-op error (nil pool is treated as
// "no row" so the login flow degrades cleanly to ErrLoginFailed).
type PGAccountRepo struct{ pool PGPool }

// NewPGAccountRepo constructs a PGAccountRepo over pool.
func NewPGAccountRepo(pool PGPool) *PGAccountRepo { return &PGAccountRepo{pool: pool} }

// FindByPhone implements AccountRepo.
func (r *PGAccountRepo) FindByPhone(ctx context.Context, tenantID int64, phone string) (*Account, error) {
	if r == nil || r.pool == nil {
		return nil, ErrAccountNotFound
	}
	a, err := scanAccount(r.pool.QueryRow(ctx, sqlAccountByPhone, tenantID, PhoneHash(phone)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("auth: pg account find by phone: %w", err)
	}
	return a, nil
}

// FindByUsername implements AccountRepo. It matches the dedicated username
// column (migration 000018), not the display name column.
func (r *PGAccountRepo) FindByUsername(ctx context.Context, tenantID int64, username string) (*Account, error) {
	if r == nil || r.pool == nil {
		return nil, ErrAccountNotFound
	}
	a, err := scanAccount(r.pool.QueryRow(ctx, sqlAccountByUsername, tenantID, username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("auth: pg account find by username: %w", err)
	}
	return a, nil
}

// TouchLogin implements AccountRepo.
func (r *PGAccountRepo) TouchLogin(ctx context.Context, id int64) error {
	if r == nil || r.pool == nil {
		return nil // best-effort: nil pool = no-op, login flow ignores the error anyway
	}
	if _, err := r.pool.Exec(ctx, sqlAccountTouchLogin, id); err != nil {
		return fmt.Errorf("auth: pg account touch login: %w", err)
	}
	return nil
}

// BumpVersion implements AccountRepo: UPDATE ... SET version = version + 1
// RETURNING version. No updated row (unknown id) degrades to ErrAccountNotFound.
func (r *PGAccountRepo) BumpVersion(ctx context.Context, id int64) (int64, error) {
	if r == nil || r.pool == nil {
		return 0, ErrAccountNotFound
	}
	var v int64
	if err := r.pool.QueryRow(ctx, sqlAccountBumpVersion, id).Scan(&v); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrAccountNotFound
		}
		return 0, fmt.Errorf("auth: pg account bump version: %w", err)
	}
	return v, nil
}
