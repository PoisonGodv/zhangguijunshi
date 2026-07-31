package tenant

// pg_repo_test.go exercises PGRepo against pgxmock (CONTRACT §17.3 style),
// mirroring internal/store/pg_repo_test.go. The pool uses QueryMatcherEqual
// because the SQL text contains regex metacharacters; tests are same-package so
// they reference the unexported sqlTenantList / sqlTenantCount constants. The
// listColumns() helper order matches the Scan order in ListAllTenants exactly —
// that alignment is what makes pgxmock row mapping work.
//
// Every test drives the repo with a platform/system ctx (platformCtx) because
// ListAllTenants refuses non-platform callers; the gate itself is covered in
// service_test.go (TestRepo_PlatformGate) and re-asserted here for the PG path.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// Compile-time assertion that PGRepo satisfies Repo.
var _ Repo = (*PGRepo)(nil)

// tenantFixedTime is the deterministic timestamp used in seeded rows.
var tenantFixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// listColumns mirrors the Scan order in ListAllTenants exactly.
func listColumns() []string {
	return []string{
		"id", "name", "status", "plan", "created_at",
		"product_count", "member_count", "order_count", "store_count",
	}
}

func TestPGRepo_ListAllTenants(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)

	// 1) COUNT — total matching (status='', namePattern='' → all).
	mock.ExpectQuery(sqlTenantCount).
		WithArgs("", "").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))
	// 2) PAGED aggregate select (limit default 20, offset 0).
	mock.ExpectQuery(sqlTenantList).
		WithArgs("", "", 20, 0).
		WillReturnRows(pgxmock.NewRows(listColumns()).
			AddRow(int64(1), "阳光水果行", "active", "pro", tenantFixedTime, int64(120), int64(3400), int64(15800), int64(2)).
			AddRow(int64(2), "好孕母婴生活馆", "active", "free", tenantFixedTime, int64(60), int64(900), int64(4100), int64(1)))

	res, err := repo.ListAllTenants(platformCtx(), Filter{})
	require.NoError(t, err)
	require.Len(t, res.Items, 2)
	require.Equal(t, int64(2), res.Total)
	assert := require.New(t)
	assert.Equal(int64(1), res.Items[0].ID)
	assert.Equal("阳光水果行", res.Items[0].Name)
	assert.Equal(120, res.Items[0].ProductCount)
	assert.Equal(3400, res.Items[0].MemberCount)
	assert.Equal(15800, res.Items[0].OrderCount)
	assert.Equal(2, res.Items[0].StoreCount)
	assert.Equal(int64(2), res.Items[1].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGRepo_ListAllTenants_FiltersAndPaging proves the status/name filters and
// explicit limit/offset are forwarded as the SQL args ($1..$4).
func TestPGRepo_ListAllTenants_FiltersAndPaging(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)

	// status='suspended', namePattern='%母婴%', limit=5, offset=10.
	mock.ExpectQuery(sqlTenantCount).
		WithArgs("suspended", "%母婴%").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectQuery(sqlTenantList).
		WithArgs("suspended", "%母婴%", 5, 10).
		WillReturnRows(pgxmock.NewRows(listColumns()).
			AddRow(int64(7), "暂停母婴店", "suspended", "free", tenantFixedTime, int64(0), int64(0), int64(0), int64(0)))

	res, err := repo.ListAllTenants(platformCtx(), Filter{Status: "suspended", Query: "母婴", Limit: 5, Offset: 10})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	require.Equal(t, int64(1), res.Total)
	require.Equal(t, "暂停母婴店", res.Items[0].Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGRepo_ListAllTenants_ZeroTotalShortCircuits proves a zero count returns
// an empty page WITHOUT issuing the paged select (only the count expectation is
// set; ExpectationsWereMet would fail if the list query ran).
func TestPGRepo_ListAllTenants_ZeroTotalShortCircuits(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlTenantCount).
		WithArgs("", "").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))

	res, err := repo.ListAllTenants(platformCtx(), Filter{})
	require.NoError(t, err)
	require.Empty(t, res.Items)
	require.Equal(t, int64(0), res.Total)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGRepo_PlatformGate proves the PG repo refuses a non-platform ctx (empty
// page, and NO SQL is issued — no expectations set, ExpectationsWereMet passes).
func TestPGRepo_PlatformGate(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)

	// A tenant-scoped ctx → refused before any query.
	res, err := repo.ListAllTenants(tenantCtx(1), Filter{})
	require.NoError(t, err)
	require.Empty(t, res.Items)
	require.Equal(t, int64(0), res.Total)

	// No identity → refused too.
	res, err = repo.ListAllTenants(context.Background(), Filter{})
	require.NoError(t, err)
	require.Empty(t, res.Items)

	require.NoError(t, mock.ExpectationsWereMet(), "no SQL must run for non-platform callers")
}

// TestPGRepo_NilPoolIsSafe proves nil receiver / nil pool degrade to an empty
// page (never a panic) for an authorized platform caller.
func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	require.NotPanics(t, func() {
		var repo *PGRepo // nil receiver
		res, err := repo.ListAllTenants(platformCtx(), Filter{})
		require.NoError(t, err)
		require.Empty(t, res.Items)
	})
	repo := NewPGRepo(nil) // nil pool
	res, err := repo.ListAllTenants(platformCtx(), Filter{})
	require.NoError(t, err)
	require.Empty(t, res.Items)
}

// TestPGRepo_QueryError proves a pool error propagates wrapped (count path).
func TestPGRepo_QueryError(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlTenantCount).
		WithArgs("", "").
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.ListAllTenants(platformCtx(), Filter{})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
