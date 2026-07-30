package auth

// account_pg_repo.go implements AccountRepo over pgx (CONTRACT §19 / BE-A-01)
// reading the users table (migration 000001 + 000017 password_hash). It reuses
// the PGPool surface and scan helpers defined in pg_repo.go. A nil pool
// degrades every read to ErrAccountNotFound so a misconfigured main thread
// cannot panic (matches PGConsumerRepo's nil-safety contract).
//
// version: the users table currently has NO version column; this repo selects a
// constant 0 (AS version) and TouchLogin does not bump any version. A future
// migration adds users.version for revoke-on-password-change; until then
// Version is always 0 and the JWT ver check is a no-op.
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

// SQL for users (migration 000001 + 000017 password_hash). Every read carries
// WHERE tenant_id = $N AND deleted_at IS NULL AND status = 'active' so a
// soft-deleted or disabled account behaves as absent (matches the memory repo).
// version is a literal 0 because the column does not exist yet.
const (
	sqlAccountByPhone = `SELECT id, tenant_id, store_id, name, role, status, password_hash, 0 AS version, last_login_at
FROM users
WHERE tenant_id = $1 AND phone_hash = $2 AND deleted_at IS NULL AND status = 'active'
LIMIT 1`
	sqlAccountByName = `SELECT id, tenant_id, store_id, name, role, status, password_hash, 0 AS version, last_login_at
FROM users
WHERE tenant_id = $1 AND name = $2 AND deleted_at IS NULL AND status = 'active'
LIMIT 1`
	sqlAccountTouchLogin = `UPDATE users SET last_login_at = now() WHERE id = $1`
)

// scanAccount decodes one users row into an Account. store_id, password_hash
// and last_login_at are nullable and scan into sql.Null* (matches the membership
// pg_repo idiom); version is a non-null literal 0.
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

// FindByUsername implements AccountRepo.
func (r *PGAccountRepo) FindByUsername(ctx context.Context, tenantID int64, username string) (*Account, error) {
	if r == nil || r.pool == nil {
		return nil, ErrAccountNotFound
	}
	a, err := scanAccount(r.pool.QueryRow(ctx, sqlAccountByName, tenantID, username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("auth: pg account find by name: %w", err)
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
