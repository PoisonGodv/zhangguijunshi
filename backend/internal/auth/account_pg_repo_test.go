package auth

// account_pg_repo_test.go exercises the pgx-backed AccountRepo against pgxmock
// (CONTRACT §19 / BE-A-01). The pool uses QueryMatcherEqual because the SQL
// text contains regex metacharacters; same-package so it references the
// unexported sqlAccount* constants. Reuses newMockPool / fixedTime from
// pg_repo_test.go. store_id / password_hash / last_login_at are nullable and
// scanned into sql.Null* (mirrors the membership pg_repo idiom). version is the
// real users.version column (migration 000018).

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/pashagolub/pgxmock/v4"
)

// accountColumns returns the users column list in scanAccount order. version is
// the real users.version column (migration 000018); it is always non-null, so
// AddRow supplies a concrete int64.
func accountColumns() []string {
	return []string{"id", "tenant_id", "store_id", "name", "role", "status", "password_hash", "version", "last_login_at"}
}

func TestPGAccountRepo_FindByPhone(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	hash, err := HashPassword("boss12345")
	require.NoError(t, err)
	mock.ExpectQuery(sqlAccountByPhone).
		WithArgs(int64(1), PhoneHash("13800000001")).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(2001), int64(1), int64(1), "demo owner", "owner", "active", hash, int64(0), fixedTime,
		))
	acc, err := repo.FindByPhone(context.Background(), 1, "13800000001")
	require.NoError(t, err)
	require.Equal(t, int64(2001), acc.ID)
	require.Equal(t, int64(1), acc.TenantID)
	require.Equal(t, int64(1), acc.StoreID)
	require.Equal(t, "owner", acc.Role)
	require.Equal(t, "active", acc.Status)
	require.Equal(t, hash, acc.PasswordHash)
	require.Equal(t, int64(0), acc.Version) // real version column (000018); row seeded at 0
	require.Equal(t, fixedTime, acc.LastLoginAt)
	require.Empty(t, acc.Phone, "PG path does not decrypt phone_enc")
	// Crucially, the lookup hashes the phone the same way the seed would.
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_FindByPhone_NullStoreAndHash(t *testing.T) {
	// Platform-style row: store_id NULL, password_hash NULL, last_login_at NULL.
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	mock.ExpectQuery(sqlAccountByPhone).
		WithArgs(int64(1), PhoneHash("10000")).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(2010), int64(1), nil, "ops", "platform_ops", "active", nil, int64(0), nil,
		))
	acc, err := repo.FindByPhone(context.Background(), 1, "10000")
	require.NoError(t, err)
	require.Equal(t, int64(0), acc.StoreID)   // NULL -> 0
	require.Empty(t, acc.PasswordHash)        // NULL -> "" (login flow rejects empty)
	require.True(t, acc.LastLoginAt.IsZero()) // NULL -> zero
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_FindByPhone_NotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	mock.ExpectQuery(sqlAccountByPhone).
		WithArgs(int64(1), PhoneHash("13900000000")).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.FindByPhone(context.Background(), 1, "13900000000")
	require.ErrorIs(t, err, ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_FindByUsername(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	hash, _ := HashPassword("ops12345")
	// FindByUsername matches the username column (000018) and returns the real
	// version. We return a non-zero version to prove it is not a constant 0.
	mock.ExpectQuery(sqlAccountByUsername).
		WithArgs(int64(1), "ops").
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(2011), int64(1), nil, "ops", "platform_ops", "active", hash, int64(3), fixedTime,
		))
	acc, err := repo.FindByUsername(context.Background(), 1, "ops")
	require.NoError(t, err)
	require.Equal(t, "platform_ops", acc.Role)
	require.Equal(t, hash, acc.PasswordHash)
	require.Equal(t, int64(3), acc.Version) // real version column value, not a constant 0
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_FindByUsername_CrossTenantNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	// username exists in tenant 1 but the query scopes tenant 2 -> no rows.
	mock.ExpectQuery(sqlAccountByUsername).
		WithArgs(int64(2), "ops").
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.FindByUsername(context.Background(), 2, "ops")
	require.ErrorIs(t, err, ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_BumpVersion(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	mock.ExpectQuery(sqlAccountBumpVersion).
		WithArgs(int64(2001)).
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(4)))
	v, err := repo.BumpVersion(context.Background(), 2001)
	require.NoError(t, err)
	require.Equal(t, int64(4), v)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_BumpVersion_NotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	// No row updated (unknown id) -> ErrAccountNotFound.
	mock.ExpectQuery(sqlAccountBumpVersion).
		WithArgs(int64(9999)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.BumpVersion(context.Background(), 9999)
	require.ErrorIs(t, err, ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_BumpVersion_DBError(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	mock.ExpectQuery(sqlAccountBumpVersion).
		WithArgs(int64(2001)).
		WillReturnError(errors.New("boom")) // non-NoRows DB error is wrapped
	_, err := repo.BumpVersion(context.Background(), 2001)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_FindByID(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	hash, _ := HashPassword("boss12345")
	// A non-zero version proves FindByID surfaces the live version column.
	mock.ExpectQuery(sqlAccountByID).
		WithArgs(int64(1), int64(2001)).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(2001), int64(1), int64(1), "demo owner", "owner", "active", hash, int64(5), fixedTime,
		))
	acc, err := repo.FindByID(context.Background(), 1, 2001)
	require.NoError(t, err)
	require.Equal(t, int64(2001), acc.ID)
	require.Equal(t, int64(1), acc.TenantID)
	require.Equal(t, hash, acc.PasswordHash)
	require.Equal(t, int64(5), acc.Version) // live version column, not a constant
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_FindByID_NotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	// Either a cross-tenant id or a genuinely unknown id resolves to no row.
	mock.ExpectQuery(sqlAccountByID).
		WithArgs(int64(1), int64(9999)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.FindByID(context.Background(), 1, 9999)
	require.ErrorIs(t, err, ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_UpdatePassword(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	newHash, _ := HashPassword("newpw12345")
	// UPDATE affects exactly one row -> success, no error.
	mock.ExpectExec(sqlAccountUpdatePassword).
		WithArgs(int64(2001), newHash).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, repo.UpdatePassword(context.Background(), 2001, newHash))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_UpdatePassword_NotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	// Zero rows affected (unknown / soft-deleted id) -> ErrAccountNotFound.
	mock.ExpectExec(sqlAccountUpdatePassword).
		WithArgs(int64(9999), "irrelevant").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	err := repo.UpdatePassword(context.Background(), 9999, "irrelevant")
	require.ErrorIs(t, err, ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_UpdatePassword_DBError(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	mock.ExpectExec(sqlAccountUpdatePassword).
		WithArgs(int64(2001), "x").
		WillReturnError(errors.New("boom")) // non-NoRows DB error is wrapped
	err := repo.UpdatePassword(context.Background(), 2001, "x")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_TouchLogin(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	mock.ExpectExec(sqlAccountTouchLogin).
		WithArgs(int64(2001)).
		WillReturnResult(pgxmock.NewResult("UPDATE 1", 1))
	require.NoError(t, repo.TouchLogin(context.Background(), 2001))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_TouchLogin_DBError(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	mock.ExpectExec(sqlAccountTouchLogin).
		WithArgs(int64(2001)).
		WillReturnError(pgx.ErrNoRows) // any DB error is wrapped
	err := repo.TouchLogin(context.Background(), 2001)
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGAccountRepo_NilPoolIsSafe(t *testing.T) {
	repo := NewPGAccountRepo(nil)
	_, err := repo.FindByPhone(context.Background(), 1, "13800000001")
	require.ErrorIs(t, err, ErrAccountNotFound)
	_, err = repo.FindByUsername(context.Background(), 1, "ops")
	require.ErrorIs(t, err, ErrAccountNotFound)
	// TouchLogin on a nil pool is a no-op (best-effort; login flow ignores err).
	require.NoError(t, repo.TouchLogin(context.Background(), 1))
	// New id/write paths also degrade to ErrAccountNotFound on a nil pool.
	_, err = repo.FindByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrAccountNotFound)
	require.ErrorIs(t, repo.UpdatePassword(context.Background(), 1, "h"), ErrAccountNotFound)

	// Nil repo pointer is also safe (methods on nil *PGAccountRepo).
	var nilRepo *PGAccountRepo
	_, err = nilRepo.FindByPhone(context.Background(), 1, "13800000001")
	require.ErrorIs(t, err, ErrAccountNotFound)
	_, err = nilRepo.FindByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrAccountNotFound)
	require.ErrorIs(t, nilRepo.UpdatePassword(context.Background(), 1, "h"), ErrAccountNotFound)
}

// TestPGAccountRepo_ScanNullableTypes is a focused check that scanAccount
// tolerates NULL store_id / password_hash / last_login_at through sql.Null*
// (passing nil directly, as a NULL column arrives from the driver).
func TestPGAccountRepo_ScanNullableTypes(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGAccountRepo(mock)
	rows := pgxmock.NewRows(accountColumns()).AddRow(
		int64(2020), int64(1), nil, "clerk", "clerk", "active", nil, int64(0), nil,
	)
	mock.ExpectQuery(sqlAccountByPhone).
		WithArgs(int64(1), PhoneHash("x")).
		WillReturnRows(rows)
	acc, err := repo.FindByPhone(context.Background(), 1, "x")
	require.NoError(t, err)
	require.Equal(t, int64(0), acc.StoreID)
	require.Empty(t, acc.PasswordHash)
	require.True(t, acc.LastLoginAt.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}
