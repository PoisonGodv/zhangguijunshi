package groupbuy

// pg_repo_test.go exercises the pgx-backed GroupbuyRepo against pgxmock
// (CONTRACT §17.3, mirroring payment/pg_repo_test.go). The pool uses
// QueryMatcherEqual because the SQL text contains regex metacharacters; tests
// are same-package so they reference the unexported sql* constants and the
// isUniqueViolation helper. JoinAtomic's 4-step read-then-mutate flow (load
// squad → member-not-joined → insert participant → conditional joined++/success)
// and its sentinel precedence (already-joined → full → closed) are the
// load-bearing assertions mirrored from the memory repo.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// Compile-time interface satisfaction.
var _ GroupbuyRepo = (*PGGroupbuyRepo)(nil)

// fixedTime is the deterministic clock used across every pgxmock row.
var fixedTime = time.Date(2026, time.July, 29, 9, 30, 0, 0, time.UTC)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// gbColumns returns the groupbuys column set in scanGroupbuy order (the row
// mapping pgxmock relies on). description/original_price_cents were added by
// migration 000024 (CONTRACT §24.6).
func gbColumns() []string {
	return []string{"id", "tenant_id", "store_id", "product_id", "title", "description", "original_price_cents", "params", "required_count", "joined", "status", "start_at", "end_at", "strategy_id", "created_at", "updated_at"}
}

// partColumns returns the groupbuy_participants column set in scanParticipant
// order.
func partColumns() []string {
	return []string{"id", "tenant_id", "groupbuy_id", "member_id", "status", "joined_at"}
}

// sampleGroupbuy is a canonical active 3-person squad used across join tests.
func sampleGroupbuy(joined int) *Groupbuy {
	return &Groupbuy{
		ID: 42, TenantID: 1, StoreID: 10, ProductID: 7, Title: "荔枝拼团",
		Desc:               "现摘现发，拼团更划算",
		OriginalPriceCents: 2990,
		Params:             GroupbuyParams{GroupSize: 3, PriceCents: 1990, ValidHours: 24},
		RequiredCount:      3, Joined: joined, Status: StatusActive,
		StartAt: fixedTime, EndAt: fixedTime.Add(24 * time.Hour), StrategyID: 9001,
		CreatedAt: fixedTime, UpdatedAt: fixedTime,
	}
}

func TestPGGroupbuyRepo_Create(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	paramsBytes, _ := json.Marshal(GroupbuyParams{GroupSize: 3, PriceCents: 1990, ValidHours: 24})
	g := &Groupbuy{
		TenantID: 1, StoreID: 10, ProductID: 7, Title: "荔枝拼团",
		Desc:               "现摘现发，拼团更划算",
		OriginalPriceCents: 2990,
		Params:             GroupbuyParams{GroupSize: 3, PriceCents: 1990, ValidHours: 24},
		RequiredCount:      3, Joined: 0, Status: StatusActive,
		StartAt: fixedTime, EndAt: fixedTime.Add(24 * time.Hour), StrategyID: 9001,
		CreatedAt: fixedTime, UpdatedAt: fixedTime,
	}
	mock.ExpectQuery(sqlGBCreate).
		WithArgs(int64(1), int64(10), int64(7), "荔枝拼团", "现摘现发，拼团更划算", int64(2990), paramsBytes, 3, 0, StatusActive,
			fixedTime, fixedTime.Add(24*time.Hour), int64(9001), fixedTime, fixedTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(42)))
	got, err := repo.Create(context.Background(), g)
	require.NoError(t, err)
	require.Equal(t, int64(42), got.ID)
	require.Equal(t, "现摘现发，拼团更划算", got.Desc)
	require.Equal(t, int64(2990), got.OriginalPriceCents)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_GetByID(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	paramsBytes, _ := json.Marshal(GroupbuyParams{PriceCents: 1990})
	mock.ExpectQuery(sqlGBGetByID).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			int64(42), int64(1), int64(10), int64(7), "荔枝拼团", "现摘现发", int64(2990), paramsBytes, 3, 1, StatusActive,
			fixedTime, fixedTime.Add(24*time.Hour), int64(9001), fixedTime, fixedTime,
		))
	g, err := repo.GetByID(context.Background(), 1, 42)
	require.NoError(t, err)
	require.Equal(t, "荔枝拼团", g.Title)
	require.Equal(t, "现摘现发", g.Desc)
	require.Equal(t, int64(2990), g.OriginalPriceCents)
	require.Equal(t, 3, g.RequiredCount)
	require.Equal(t, int64(7), g.ProductID)
	require.Equal(t, int64(9001), g.StrategyID)
	require.Equal(t, int64(1990), g.Params.PriceCents)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_GetByIDNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	mock.ExpectQuery(sqlGBGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetByID(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_ListActive(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	mock.ExpectQuery(sqlGBListActiveAll).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).
			AddRow(int64(1), int64(1), int64(10), int64(7), "A", "", int64(0), []byte(`{}`), 3, 0, StatusActive, fixedTime, fixedTime, int64(0), fixedTime, fixedTime).
			AddRow(int64(2), int64(1), int64(11), nil, "B", "描述B", int64(5000), []byte(`{}`), 3, 1, StatusActive, fixedTime, fixedTime, int64(0), fixedTime, fixedTime))
	got, err := repo.ListActive(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(7), got[0].ProductID)
	require.Equal(t, int64(0), got[1].ProductID) // NULL product_id → 0
	require.Equal(t, "描述B", got[1].Desc)
	require.Equal(t, int64(5000), got[1].OriginalPriceCents)
	require.Equal(t, "", got[0].Desc) // DEFAULT '' (old rows degrade to title client-side)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_ListActiveByStore(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	mock.ExpectQuery(sqlGBListActiveStore).
		WithArgs(int64(1), int64(10)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).
			AddRow(int64(1), int64(1), int64(10), int64(7), "A", "", int64(0), []byte(`{}`), 3, 0, StatusActive, fixedTime, fixedTime, int64(0), fixedTime, fixedTime))
	got, err := repo.ListActive(context.Background(), 1, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(10), got[0].StoreID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_ListByStrategy(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	mock.ExpectQuery(sqlGBListByStrategy).
		WithArgs(int64(1), int64(9001)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).
			AddRow(int64(1), int64(1), int64(10), int64(7), "A", "", int64(0), []byte(`{}`), 3, 3, StatusSuccess, fixedTime, fixedTime, int64(9001), fixedTime, fixedTime))
	got, err := repo.ListByStrategy(context.Background(), 1, 9001)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(9001), got[0].StrategyID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_ListByStrategyNonAI(t *testing.T) {
	// strategyID <= 0 → empty slice, no DB hit.
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	got, err := repo.ListByStrategy(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_UpdateStatus(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	// Step 1: load current (active).
	mock.ExpectQuery(sqlGBGetByID).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			int64(42), int64(1), int64(10), int64(7), "A", "", int64(0), []byte(`{}`), 3, 0, StatusActive, fixedTime, fixedTime, int64(0), fixedTime, fixedTime,
		))
	// Step 2: conditional UPDATE returns the failed row.
	mock.ExpectQuery(sqlGBUpdateStatus).
		WithArgs(StatusFailed, fixedTime, int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			int64(42), int64(1), int64(10), int64(7), "A", "", int64(0), []byte(`{}`), 3, 0, StatusFailed, fixedTime, fixedTime, int64(0), fixedTime, fixedTime,
		))
	g, err := repo.UpdateStatus(context.Background(), 1, 42, StatusFailed, fixedTime)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, g.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_UpdateStatusIllegalTransition(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	// Load returns a terminal success squad → re-validate rejects (no UPDATE).
	mock.ExpectQuery(sqlGBGetByID).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			int64(42), int64(1), int64(10), int64(7), "A", "", int64(0), []byte(`{}`), 3, 3, StatusSuccess, fixedTime, fixedTime, int64(0), fixedTime, fixedTime,
		))
	_, err := repo.UpdateStatus(context.Background(), 1, 42, StatusFailed, fixedTime)
	require.ErrorIs(t, err, ErrGroupbuyClosed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_UpdateStatusNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	mock.ExpectQuery(sqlGBGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.UpdateStatus(context.Background(), 1, 99, StatusFailed, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGGroupbuyRepo_UpdateStatusGuardMiss reproduces the TOCTOU the DB-side
// `AND status = 'active'` guard closes: the GetByID pre-read returns an active
// squad (so the Go-side transition check passes), but a concurrent finaliser
// flips the squad to a terminal status before the UPDATE, so the guarded UPDATE
// matches no rows. The repo must report ErrGroupbuyClosed (silent-skip for
// FinalizeExpired/Cancel), NOT ErrNotFound.
func TestPGGroupbuyRepo_UpdateStatusGuardMiss(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	// Pre-read: still active → Go-side check passes.
	mock.ExpectQuery(sqlGBGetByID).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			int64(42), int64(1), int64(10), int64(7), "A", "", int64(0), []byte(`{}`), 3, 3, StatusActive, fixedTime, fixedTime, int64(0), fixedTime, fixedTime,
		))
	// Guarded UPDATE (AND status='active') matches no rows: a concurrent writer
	// settled the squad in the gap.
	mock.ExpectQuery(sqlGBUpdateStatus).
		WithArgs(StatusFailed, fixedTime, int64(1), int64(42)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.UpdateStatus(context.Background(), 1, 42, StatusFailed, fixedTime)
	require.ErrorIs(t, err, ErrGroupbuyClosed)
	require.NotErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_JoinAtomic(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	cur := sampleGroupbuy(0) // joined=0, required=3
	paramsBytes, _ := json.Marshal(cur.Params)
	// 1. Load squad.
	mock.ExpectQuery(sqlGBGetByID).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			cur.ID, cur.TenantID, cur.StoreID, cur.ProductID, cur.Title, cur.Desc, cur.OriginalPriceCents, paramsBytes, cur.RequiredCount, cur.Joined, cur.Status, cur.StartAt, cur.EndAt, cur.StrategyID, cur.CreatedAt, cur.UpdatedAt,
		))
	// 2. Already-joined check → not joined.
	mock.ExpectQuery(sqlPartGetByMember).
		WithArgs(int64(1), int64(42), int64(100)).
		WillReturnError(pgx.ErrNoRows)
	// Steps 5+6 run inside one transaction (pgxmock satisfies txBeginner).
	mock.ExpectBegin()
	// 3. Insert participant.
	mock.ExpectQuery(sqlPartInsert).
		WithArgs(int64(1), int64(42), int64(100), PStatusJoined, fixedTime).
		WillReturnRows(pgxmock.NewRows(partColumns()).AddRow(
			int64(500), int64(1), int64(42), int64(100), PStatusJoined, fixedTime,
		))
	// 4. Conditional joined++ (1 < 3, stays active).
	mock.ExpectQuery(sqlGBJoinIncr).
		WithArgs(fixedTime, int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			int64(42), int64(1), int64(10), int64(7), "荔枝拼团", "现摘现发", int64(2990), paramsBytes, 3, 1, StatusActive, fixedTime, fixedTime.Add(24*time.Hour), int64(9001), fixedTime, fixedTime,
		))
	mock.ExpectCommit()
	p, g, err := repo.JoinAtomic(context.Background(), 1, 42, 100, fixedTime)
	require.NoError(t, err)
	require.Equal(t, int64(500), p.ID)
	require.Equal(t, PStatusJoined, p.Status)
	require.Equal(t, 1, g.Joined)
	require.Equal(t, StatusActive, g.Status) // not yet full
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_JoinAtomicReachesSuccess(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	cur := sampleGroupbuy(2) // joined=2, required=3 → this join fills it
	paramsBytes, _ := json.Marshal(cur.Params)
	mock.ExpectQuery(sqlGBGetByID).WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			cur.ID, cur.TenantID, cur.StoreID, cur.ProductID, cur.Title, cur.Desc, cur.OriginalPriceCents, paramsBytes, cur.RequiredCount, cur.Joined, cur.Status, cur.StartAt, cur.EndAt, cur.StrategyID, cur.CreatedAt, cur.UpdatedAt,
		))
	mock.ExpectQuery(sqlPartGetByMember).WithArgs(int64(1), int64(42), int64(101)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(sqlPartInsert).WithArgs(int64(1), int64(42), int64(101), PStatusJoined, fixedTime).
		WillReturnRows(pgxmock.NewRows(partColumns()).AddRow(int64(501), int64(1), int64(42), int64(101), PStatusJoined, fixedTime))
	// joined 2→3 reaches required_count → status flips to success.
	mock.ExpectQuery(sqlGBJoinIncr).WithArgs(fixedTime, int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			int64(42), int64(1), int64(10), int64(7), "荔枝拼团", "现摘现发", int64(2990), paramsBytes, 3, 3, StatusSuccess, fixedTime, fixedTime.Add(24*time.Hour), int64(9001), fixedTime, fixedTime,
		))
	mock.ExpectCommit()
	p, g, err := repo.JoinAtomic(context.Background(), 1, 42, 101, fixedTime)
	require.NoError(t, err)
	require.Equal(t, int64(501), p.ID)
	require.Equal(t, 3, g.Joined)
	require.Equal(t, StatusSuccess, g.Status) // filled → success
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_JoinAtomicNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	mock.ExpectQuery(sqlGBGetByID).WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, _, err := repo.JoinAtomic(context.Background(), 1, 99, 100, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_JoinAtomicAlreadyJoined(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	cur := sampleGroupbuy(0)
	paramsBytes, _ := json.Marshal(cur.Params)
	mock.ExpectQuery(sqlGBGetByID).WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			cur.ID, cur.TenantID, cur.StoreID, cur.ProductID, cur.Title, cur.Desc, cur.OriginalPriceCents, paramsBytes, cur.RequiredCount, cur.Joined, cur.Status, cur.StartAt, cur.EndAt, cur.StrategyID, cur.CreatedAt, cur.UpdatedAt,
		))
	// Member already has a participant row → ErrAlreadyJoined (no INSERT).
	mock.ExpectQuery(sqlPartGetByMember).WithArgs(int64(1), int64(42), int64(100)).
		WillReturnRows(pgxmock.NewRows(partColumns()).AddRow(int64(500), int64(1), int64(42), int64(100), PStatusJoined, fixedTime))
	_, _, err := repo.JoinAtomic(context.Background(), 1, 42, 100, fixedTime)
	require.ErrorIs(t, err, ErrAlreadyJoined)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_JoinAtomicRaceUniqueViolation(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	cur := sampleGroupbuy(0)
	paramsBytes, _ := json.Marshal(cur.Params)
	mock.ExpectQuery(sqlGBGetByID).WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			cur.ID, cur.TenantID, cur.StoreID, cur.ProductID, cur.Title, cur.Desc, cur.OriginalPriceCents, paramsBytes, cur.RequiredCount, cur.Joined, cur.Status, cur.StartAt, cur.EndAt, cur.StrategyID, cur.CreatedAt, cur.UpdatedAt,
		))
	mock.ExpectQuery(sqlPartGetByMember).WithArgs(int64(1), int64(42), int64(100)).
		WillReturnError(pgx.ErrNoRows)
	// A concurrent insert raced past step 2 → UNIQUE(tenant,groupbuy,member)
	// violation is mapped to ErrAlreadyJoined; the tx rolls back (no orphan).
	mock.ExpectBegin()
	mock.ExpectQuery(sqlPartInsert).WithArgs(int64(1), int64(42), int64(100), PStatusJoined, fixedTime).
		WillReturnError(&pgconn.PgError{Code: "23505"})
	mock.ExpectRollback()
	_, _, err := repo.JoinAtomic(context.Background(), 1, 42, 100, fixedTime)
	require.ErrorIs(t, err, ErrAlreadyJoined)
	// Sanity: the helper classifies SQLSTATE 23505.
	require.True(t, isUniqueViolation(&pgconn.PgError{Code: "23505"}))
	require.False(t, isUniqueViolation(errors.New("other")))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGGroupbuyRepo_JoinAtomicIncrRaceRollback reproduces the orphan-row bug
// the transaction fix closes: the participant INSERT (step 5) succeeds, but the
// conditional joined++ UPDATE (step 6) returns no rows because a concurrent join
// filled the squad between the step-1 read and here. The transaction must roll
// back (rolling back the inserted participant — no orphan row, no miscounted joined,
// member not permanently locked) and map to ErrGroupbuyFull.
func TestPGGroupbuyRepo_JoinAtomicIncrRaceRollback(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	cur := sampleGroupbuy(2) // read saw joined=2/3 active; a concurrent join fills it
	paramsBytes, _ := json.Marshal(cur.Params)
	mock.ExpectQuery(sqlGBGetByID).WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			cur.ID, cur.TenantID, cur.StoreID, cur.ProductID, cur.Title, cur.Desc, cur.OriginalPriceCents, paramsBytes, cur.RequiredCount, cur.Joined, cur.Status, cur.StartAt, cur.EndAt, cur.StrategyID, cur.CreatedAt, cur.UpdatedAt,
		))
	mock.ExpectQuery(sqlPartGetByMember).WithArgs(int64(1), int64(42), int64(100)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectBegin()
	// Step 5 INSERT succeeds...
	mock.ExpectQuery(sqlPartInsert).WithArgs(int64(1), int64(42), int64(100), PStatusJoined, fixedTime).
		WillReturnRows(pgxmock.NewRows(partColumns()).AddRow(int64(500), int64(1), int64(42), int64(100), PStatusJoined, fixedTime))
	// ...but step 6 guarded UPDATE matches no rows (squad raced to full/success).
	mock.ExpectQuery(sqlGBJoinIncr).WithArgs(fixedTime, int64(1), int64(42)).
		WillReturnError(pgx.ErrNoRows)
	// The deferred Rollback rolls back the inserted participant → no orphan row.
	mock.ExpectRollback()
	p, g, err := repo.JoinAtomic(context.Background(), 1, 42, 100, fixedTime)
	require.Nil(t, p)
	require.Nil(t, g)
	require.ErrorIs(t, err, ErrGroupbuyFull)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_JoinAtomicFull(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	// joined == required (also covers a succeeded squad): full check fires before
	// the INSERT, so no participant is orphaned.
	cur := sampleGroupbuy(3)
	paramsBytes, _ := json.Marshal(cur.Params)
	mock.ExpectQuery(sqlGBGetByID).WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			cur.ID, cur.TenantID, cur.StoreID, cur.ProductID, cur.Title, cur.Desc, cur.OriginalPriceCents, paramsBytes, cur.RequiredCount, cur.Joined, cur.Status, cur.StartAt, cur.EndAt, cur.StrategyID, cur.CreatedAt, cur.UpdatedAt,
		))
	mock.ExpectQuery(sqlPartGetByMember).WithArgs(int64(1), int64(42), int64(100)).
		WillReturnError(pgx.ErrNoRows)
	_, _, err := repo.JoinAtomic(context.Background(), 1, 42, 100, fixedTime)
	require.ErrorIs(t, err, ErrGroupbuyFull)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_JoinAtomicClosed(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	// failed squad with Joined<RequiredCount → closed check fires (not full).
	cur := sampleGroupbuy(1)
	cur.Status = StatusFailed
	paramsBytes, _ := json.Marshal(cur.Params)
	mock.ExpectQuery(sqlGBGetByID).WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			cur.ID, cur.TenantID, cur.StoreID, cur.ProductID, cur.Title, cur.Desc, cur.OriginalPriceCents, paramsBytes, cur.RequiredCount, cur.Joined, cur.Status, cur.StartAt, cur.EndAt, cur.StrategyID, cur.CreatedAt, cur.UpdatedAt,
		))
	mock.ExpectQuery(sqlPartGetByMember).WithArgs(int64(1), int64(42), int64(100)).
		WillReturnError(pgx.ErrNoRows)
	_, _, err := repo.JoinAtomic(context.Background(), 1, 42, 100, fixedTime)
	require.ErrorIs(t, err, ErrGroupbuyClosed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_GetParticipant(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	mock.ExpectQuery(sqlPartGetByMember).
		WithArgs(int64(1), int64(42), int64(100)).
		WillReturnRows(pgxmock.NewRows(partColumns()).AddRow(
			int64(500), int64(1), int64(42), int64(100), PStatusJoined, fixedTime,
		))
	p, err := repo.GetParticipant(context.Background(), 1, 42, 100)
	require.NoError(t, err)
	require.Equal(t, int64(500), p.ID)
	require.Equal(t, PStatusJoined, p.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_GetParticipantNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	mock.ExpectQuery(sqlPartGetByMember).
		WithArgs(int64(1), int64(42), int64(999)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetParticipant(context.Background(), 1, 42, 999)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_ListParticipants(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	// Step 1: squad exists for this tenant.
	mock.ExpectQuery(sqlGBGetByID).WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(gbColumns()).AddRow(
			int64(42), int64(1), int64(10), int64(7), "A", "", int64(0), []byte(`{}`), 3, 2, StatusActive, fixedTime, fixedTime, int64(0), fixedTime, fixedTime,
		))
	// Step 2: list participants ordered by id.
	mock.ExpectQuery(sqlPartListByGB).WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(partColumns()).
			AddRow(int64(500), int64(1), int64(42), int64(100), PStatusJoined, fixedTime).
			AddRow(int64(501), int64(1), int64(42), int64(101), PStatusPaid, fixedTime))
	got, err := repo.ListParticipants(context.Background(), 1, 42)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(500), got[0].ID)
	require.Equal(t, PStatusPaid, got[1].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_ListParticipantsForeignSquad(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	// Foreign / absent squad → ErrNotFound (not an empty list), matching memory.
	mock.ExpectQuery(sqlGBGetByID).WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.ListParticipants(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_MarkPaid(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	// Step 1: load participant (joined).
	mock.ExpectQuery(sqlPartGetByMember).WithArgs(int64(1), int64(42), int64(100)).
		WillReturnRows(pgxmock.NewRows(partColumns()).AddRow(
			int64(500), int64(1), int64(42), int64(100), PStatusJoined, fixedTime,
		))
	// Step 2: conditional joined→paid.
	mock.ExpectQuery(sqlPartMarkPaid).WithArgs(int64(1), int64(42), int64(100)).
		WillReturnRows(pgxmock.NewRows(partColumns()).AddRow(
			int64(500), int64(1), int64(42), int64(100), PStatusPaid, fixedTime,
		))
	p, err := repo.MarkPaid(context.Background(), 1, 42, 100, fixedTime)
	require.NoError(t, err)
	require.Equal(t, PStatusPaid, p.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_MarkPaidIdempotent(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	// Already paid → return as-is, no UPDATE issued.
	mock.ExpectQuery(sqlPartGetByMember).WithArgs(int64(1), int64(42), int64(100)).
		WillReturnRows(pgxmock.NewRows(partColumns()).AddRow(
			int64(500), int64(1), int64(42), int64(100), PStatusPaid, fixedTime,
		))
	p, err := repo.MarkPaid(context.Background(), 1, 42, 100, fixedTime)
	require.NoError(t, err)
	require.Equal(t, PStatusPaid, p.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_MarkPaidNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGGroupbuyRepo(mock)
	mock.ExpectQuery(sqlPartGetByMember).WithArgs(int64(1), int64(42), int64(999)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.MarkPaid(context.Background(), 1, 42, 999, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGGroupbuyRepo_NilPoolIsSafe(t *testing.T) {
	// Nil pool degrades: single reads / writes → ErrNotFound; lists → empty.
	r1 := NewPGGroupbuyRepo(nil)
	_, err := r1.GetByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r1.GetParticipant(context.Background(), 1, 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r1.UpdateStatus(context.Background(), 1, 1, StatusFailed, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = r1.JoinAtomic(context.Background(), 1, 1, 1, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r1.MarkPaid(context.Background(), 1, 1, 1, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r1.Create(context.Background(), &Groupbuy{Title: "x"})
	require.ErrorIs(t, err, ErrNotFound)

	got, err := r1.ListActive(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Empty(t, got)
	got2, err := r1.ListByStrategy(context.Background(), 1, 9001)
	require.NoError(t, err)
	require.Empty(t, got2)
	got3, err := r1.ListParticipants(context.Background(), 1, 1)
	require.NoError(t, err)
	require.Empty(t, got3)

	// Nil receiver is safe too.
	var r2 *PGGroupbuyRepo
	_, err = r2.GetByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
}
