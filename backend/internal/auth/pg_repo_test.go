package auth

// pg_repo_test.go exercises the pgx-backed ConsumerRepo against pgxmock
// (CONTRACT §17.3 / §17.7 A1: "INSERT ... ON CONFLICT upsert + SELECT"). The
// pool uses QueryMatcherEqual because the SQL text contains regex
// metacharacters; the test is same-package so it references the unexported
// sqlConsumer* constants. The upsert's insert-vs-on-conflict semantics are
// the load-bearing assertion: a refresh preserves member_id / version while
// overwriting session_key / unionid.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// fixedTime is the deterministic clock used everywhere a test needs a stable
// timestamp (mirrors payment/helpers_test.go).
var fixedTime = time.Date(2026, time.July, 29, 10, 0, 0, 0, time.UTC)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// consumerColumns returns the wx_sessions column list in scanConsumer order.
func consumerColumns() []string {
	return []string{"id", "tenant_id", "openid", "unionid", "member_id", "session_key", "version", "created_at", "updated_at"}
}

func TestPGConsumerRepo_UpsertByOpenIDInsert(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGConsumerRepo(mock)
	// New row: DB allocates id, member_id / version take defaults (0).
	mock.ExpectQuery(sqlConsumerUpsert).
		WithArgs(int64(1), "wx-openid-1", "wx-unionid-1", "sk-1").
		WillReturnRows(pgxmock.NewRows(consumerColumns()).AddRow(
			int64(1001), int64(1), "wx-openid-1", "wx-unionid-1", int64(0), "sk-1", int64(0), fixedTime, fixedTime,
		))
	c, err := repo.UpsertByOpenID(context.Background(), 1, "wx-openid-1", "wx-unionid-1", "sk-1")
	require.NoError(t, err)
	require.Equal(t, int64(1001), c.ID)
	require.Equal(t, "sk-1", c.SessionKey)
	require.Equal(t, "wx-unionid-1", c.UnionID)
	require.Equal(t, int64(0), c.MemberID) // default, no cross-domain backfill (§17.8)
	require.Equal(t, int64(0), c.Version)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGConsumerRepo_UpsertByOpenIDOnConflictUpdate(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGConsumerRepo(mock)
	// Existing row: session_key/unionid refreshed, member_id/version preserved.
	mock.ExpectQuery(sqlConsumerUpsert).
		WithArgs(int64(1), "wx-openid-1", "wx-unionid-2", "sk-2").
		WillReturnRows(pgxmock.NewRows(consumerColumns()).AddRow(
			int64(1001), int64(1), "wx-openid-1", "wx-unionid-2", int64(42), "sk-2", int64(3), fixedTime, fixedTime,
		))
	c, err := repo.UpsertByOpenID(context.Background(), 1, "wx-openid-1", "wx-unionid-2", "sk-2")
	require.NoError(t, err)
	require.Equal(t, int64(1001), c.ID)
	require.Equal(t, "sk-2", c.SessionKey)      // refreshed
	require.Equal(t, "wx-unionid-2", c.UnionID) // refreshed
	require.Equal(t, int64(42), c.MemberID)     // preserved
	require.Equal(t, int64(3), c.Version)       // preserved
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGConsumerRepo_GetByID(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGConsumerRepo(mock)
	mock.ExpectQuery(sqlConsumerGetByID).
		WithArgs(int64(1), int64(1001)).
		WillReturnRows(pgxmock.NewRows(consumerColumns()).AddRow(
			int64(1001), int64(1), "wx-openid-1", "wx-unionid-1", int64(42), "sk-1", int64(3), fixedTime, fixedTime,
		))
	c, err := repo.GetByID(context.Background(), 1, 1001)
	require.NoError(t, err)
	require.Equal(t, int64(1001), c.ID)
	require.Equal(t, "wx-openid-1", c.OpenID)
	require.Equal(t, "sk-1", c.SessionKey)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGConsumerRepo_GetByIDNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGConsumerRepo(mock)
	mock.ExpectQuery(sqlConsumerGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetByID(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	// Nil pools degrade to ErrNotFound rather than panicking.
	repo := NewPGConsumerRepo(nil)
	_, err := repo.UpsertByOpenID(context.Background(), 1, "wx-openid-1", "wx-unionid-1", "sk-1")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.GetByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	// Nil repo pointer is also safe (method on nil *PGConsumerRepo).
	var nilRepo *PGConsumerRepo
	_, err = nilRepo.GetByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
}
