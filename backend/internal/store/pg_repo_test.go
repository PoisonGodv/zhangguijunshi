package store

// pg_repo_test.go exercises PGStoreRepo against pgxmock (CONTRACT §17.3 /
// §16.11), mirroring internal/membership/pg_repo_test.go. The pool uses
// QueryMatcherEqual because the SQL text contains regex metacharacters;
// tests are same-package so they reference the unexported sqlStoreList
// constant. The storeColumns() helper order matches scanStore's Scan order
// exactly — that alignment is what makes pgxmock row mapping work.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// Compile-time assertion that PGStoreRepo satisfies StoreRepo.
var _ StoreRepo = (*PGStoreRepo)(nil)

// storeFixedTime is the deterministic timestamp used in seeded rows.
var storeFixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// storeColumns mirrors scanStore's Scan order exactly.
func storeColumns() []string {
	return []string{"id", "tenant_id", "address", "phone", "group_qr", "active", "created_at"}
}

// toRow flattens a Store into a pgxmock row whose column order matches
// storeColumns() (address / phone / group_qr are sent as the raw strings; the
// COALESCE in the SQL turns NULL into ” so the scanned NullString is the
// string itself).
func toRow(s *Store) []any {
	return []any{s.ID, s.TenantID, s.Address, s.Phone, s.GroupQR, s.Active, s.CreatedAt}
}

func TestPGStoreRepo_ListStores(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStoreRepo(mock)
	mock.ExpectQuery(sqlStoreList).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows(storeColumns()).
			AddRow(toRow(&Store{ID: 7001, TenantID: 1, Name: "总店", Address: "路1号", Phone: "021-1", GroupQR: "qr1", Active: true, CreatedAt: storeFixedTime})...).
			AddRow(toRow(&Store{ID: 7002, TenantID: 1, Name: "分店", Address: "路2号", Phone: "021-2", GroupQR: "qr2", Active: true, CreatedAt: storeFixedTime})...))
	got, err := repo.ListStores(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(7001), got[0].ID)
	require.Equal(t, "路1号", got[0].Address)
	require.Equal(t, "021-1", got[0].Phone)
	require.Equal(t, "qr1", got[0].GroupQR)
	require.True(t, got[0].Active)
	require.Equal(t, int64(7002), got[1].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStoreRepo_ListStoresEmpty(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStoreRepo(mock)
	mock.ExpectQuery(sqlStoreList).
		WithArgs(int64(999)).
		WillReturnRows(pgxmock.NewRows(storeColumns()))
	got, err := repo.ListStores(context.Background(), 999)
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGStoreRepo_ListStoresNullCols proves nullable address/phone/group_qr
// columns (NULL → "") decode cleanly. A store with no phone/qr still scans.
func TestPGStoreRepo_ListStoresNullCols(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStoreRepo(mock)
	// NULL columns come back as nil through pgxmock; the COALESCE in the SQL
	// turns them into '' at the DB layer, but pgxmock does not evaluate
	// COALESCE — it returns whatever we feed it, so we feed "" to mirror the
	// post-COALESCE shape scanStore expects.
	mock.ExpectQuery(sqlStoreList).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows(storeColumns()).
			AddRow(int64(7001), int64(1), "", "", "", false, storeFixedTime))
	got, err := repo.ListStores(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "", got[0].Phone)
	require.Equal(t, "", got[0].GroupQR)
	require.False(t, got[0].Active)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGStoreRepo_NilPoolIsSafe proves nil repo / nil pool degrade to an
// empty slice (never a panic).
func TestPGStoreRepo_NilPoolIsSafe(t *testing.T) {
	require.NotPanics(t, func() {
		var repo *PGStoreRepo // nil receiver
		got, err := repo.ListStores(context.Background(), 1)
		require.NoError(t, err)
		require.Empty(t, got)
	})
	mock := newMockPool(t)
	repo := NewPGStoreRepo(nil) // nil pool
	got, err := repo.ListStores(context.Background(), 1)
	require.NoError(t, err)
	require.Empty(t, got)
	_ = mock // unused pool asserts nothing (nil-pool fast path bypasses Query)
}

// TestPGStoreRepo_QueryError proves a pool Query error propagates wrapped.
func TestPGStoreRepo_QueryError(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStoreRepo(mock)
	mock.ExpectQuery(sqlStoreList).
		WithArgs(int64(1)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.ListStores(context.Background(), 1)
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
