package strategy

// pg_repo_test.go exercises PGStrategyRepo against pgxmock (CONTRACT §17.3 /
// §12.3.1), mirroring internal/payment/pg_repo_test.go. Same-package so the
// unexported sql* constants are referenced; QueryMatcherEqual because the SQL
// text contains regex metacharacters. Column helpers match the scanXxx Scan
// order exactly. SaveAdvice's transactional upsert+items-rebuild chain
// (BEGIN → ON CONFLICT upsert → delete feedback → delete items → insert items →
// COMMIT; mid-chain failure → ROLLBACK) and UpdateItemStatus's single
// conditional-UPDATE state machine (0 rows → sqlItemExists disambiguates
// ErrNotFound vs ErrParam) are the load-bearing cases.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// fixedTime is the deterministic clock used across these tests.
var fixedTime = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// strategyColumns mirrors the scanStrategy Scan order (11 columns; uuid skipped,
// for_date read as ::text).
func strategyColumns() []string {
	return []string{"id", "tenant_id", "store_id", "for_date", "signals", "data_quality_score", "generated_by", "llm_tier", "total_cost_microcents", "status", "created_at"}
}

// itemColumns mirrors the scanItem Scan order (15 columns).
func itemColumns() []string {
	return []string{"id", "tenant_id", "strategy_id", "rank", "action_type", "reason_type", "target", "product_id", "params", "copy", "reason", "expected_effect", "margin_ok", "execution_status", "executed_at"}
}

// feedbackColumns mirrors the scanFeedback Scan order (5 columns).
func feedbackColumns() []string {
	return []string{"id", "tenant_id", "strategy_item_id", "useful", "created_at"}
}

// mustJSON marshals v to deterministic JSON bytes (map keys sorted by encoding/json).
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// newSampleAdvice builds a one-item Strategy for SaveAdvice tests.
func newSampleAdvice() *Strategy {
	return &Strategy{
		TenantID: 1, StoreID: 10, ForDate: "2026-07-29",
		Signals:          json.RawMessage(`{"business":"fruit"}`),
		DataQualityScore: 80,
		GeneratedBy:      GenRuleOnly,
		Status:           StatusGenerated,
		Items: []StrategyItem{{
			Rank: 1, ActionType: "coupon_wake", ReasonType: "wake_dormant",
			Params: map[string]any{"off_cents": int64(500)},
			Copy:   "hi", Reason: "wake dormant member", MarginOK: true,
		}},
	}
}

func TestPGStrategyRepo_SaveAdvice(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	s := newSampleAdvice()
	itemParams := mustJSON(t, s.Items[0].Params)

	// Whole rebuild runs inside one transaction.
	mock.ExpectBegin()
	// (1) upsert strategy row; RETURNING id + preserved created_at.
	mock.ExpectQuery(sqlStrategyUpsert).
		WithArgs(int64(1), int64(10), "2026-07-29", []byte(s.Signals), 80, GenRuleOnly, nil, int64(0), StatusGenerated).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), fixedTime))
	// (2) discard old feedback for the strategy's items.
	mock.ExpectExec(sqlFeedbackDeleteByStrategy).
		WithArgs(int64(1), int64(1), int64(7)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// (3) discard old items.
	mock.ExpectExec(sqlItemsDeleteByStrategy).
		WithArgs(int64(1), int64(7)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// (4) insert fresh item; ExecutionStatus defaulted to pending, ExecutedAt nil.
	mock.ExpectQuery(sqlItemInsert).
		WithArgs(int64(1), int64(7), 1, "coupon_wake", "wake_dormant", nil, nil, itemParams, "hi", "wake dormant member", nil, true, ItemPending, (*time.Time)(nil)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(11)))
	mock.ExpectCommit()

	out, err := repo.SaveAdvice(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, int64(7), out.ID)         // backfilled from RETURNING
	require.Equal(t, fixedTime, out.CreatedAt) // preserved
	require.Len(t, out.Items, 1)
	require.Equal(t, int64(11), out.Items[0].ID) // item id backfilled
	require.Equal(t, int64(7), out.Items[0].StrategyID)
	require.Equal(t, ItemPending, out.Items[0].ExecutionStatus) // defaulted
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_SaveAdviceOverwrite(t *testing.T) {
	// Same-day regeneration runs the same chain; the ON CONFLICT DO UPDATE
	// preserves id + created_at while items are rebuilt (here with 2 fresh
	// items). The two item inserts each RETURNING their own id.
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	s := newSampleAdvice()
	s.Items = append(s.Items, StrategyItem{
		Rank: 2, ActionType: "group_seckill", ReasonType: "clearance_coupon",
		Params: map[string]any{"discount_permille": int64(700)}, MarginOK: true,
	})
	p1 := mustJSON(t, s.Items[0].Params)
	p2 := mustJSON(t, s.Items[1].Params)

	mock.ExpectBegin()
	mock.ExpectQuery(sqlStrategyUpsert).
		WithArgs(int64(1), int64(10), "2026-07-29", []byte(s.Signals), 80, GenRuleOnly, nil, int64(0), StatusGenerated).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), fixedTime))
	mock.ExpectExec(sqlFeedbackDeleteByStrategy).WithArgs(int64(1), int64(1), int64(7)).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(sqlItemsDeleteByStrategy).WithArgs(int64(1), int64(7)).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectQuery(sqlItemInsert).
		WithArgs(int64(1), int64(7), 1, "coupon_wake", "wake_dormant", nil, nil, p1, "hi", "wake dormant member", nil, true, ItemPending, (*time.Time)(nil)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(11)))
	mock.ExpectQuery(sqlItemInsert).
		WithArgs(int64(1), int64(7), 2, "group_seckill", "clearance_coupon", nil, nil, p2, "", "", nil, true, ItemPending, (*time.Time)(nil)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(12)))
	mock.ExpectCommit()

	out, err := repo.SaveAdvice(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, int64(7), out.ID)
	require.Equal(t, fixedTime, out.CreatedAt)
	require.Len(t, out.Items, 2)
	require.Equal(t, int64(11), out.Items[0].ID)
	require.Equal(t, int64(12), out.Items[1].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGStrategyRepo_SaveAdviceRollbackOnItemInsertFailure is the regression
// case for the pre-fix bug: a failure on the 2nd item INSERT must roll the whole
// rebuild back (no half-rebuilt strategy, no silently-lost old feedback).
func TestPGStrategyRepo_SaveAdviceRollbackOnItemInsertFailure(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	s := newSampleAdvice()
	s.Items = append(s.Items, StrategyItem{
		Rank: 2, ActionType: "group_seckill", ReasonType: "clearance_coupon",
		Params: map[string]any{"discount_permille": int64(700)}, MarginOK: true,
	})
	p1 := mustJSON(t, s.Items[0].Params)
	p2 := mustJSON(t, s.Items[1].Params)

	mock.ExpectBegin()
	mock.ExpectQuery(sqlStrategyUpsert).
		WithArgs(int64(1), int64(10), "2026-07-29", []byte(s.Signals), 80, GenRuleOnly, nil, int64(0), StatusGenerated).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), fixedTime))
	mock.ExpectExec(sqlFeedbackDeleteByStrategy).WithArgs(int64(1), int64(1), int64(7)).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(sqlItemsDeleteByStrategy).WithArgs(int64(1), int64(7)).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	// First item insert succeeds.
	mock.ExpectQuery(sqlItemInsert).
		WithArgs(int64(1), int64(7), 1, "coupon_wake", "wake_dormant", nil, nil, p1, "hi", "wake dormant member", nil, true, ItemPending, (*time.Time)(nil)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(11)))
	// Second item insert fails → the whole rebuild must roll back.
	mock.ExpectQuery(sqlItemInsert).
		WithArgs(int64(1), int64(7), 2, "group_seckill", "clearance_coupon", nil, nil, p2, "", "", nil, true, ItemPending, (*time.Time)(nil)).
		WillReturnError(errors.New("boom: item insert failed"))
	mock.ExpectRollback()

	_, err := repo.SaveAdvice(context.Background(), s)
	require.Error(t, err)
	require.Contains(t, err.Error(), "insert item")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_Get(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	mock.ExpectQuery(sqlStrategyGetByID).
		WithArgs(int64(1), int64(7)).
		WillReturnRows(pgxmock.NewRows(strategyColumns()).AddRow(
			int64(7), int64(1), int64(10), "2026-07-29", []byte(`{"business":"fruit"}`), 80, GenRuleOnly, nil, int64(0), StatusGenerated, fixedTime,
		))
	mock.ExpectQuery(sqlItemsByStrategy).
		WithArgs(int64(1), int64(7)).
		WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(
			int64(11), int64(1), int64(7), 1, "coupon_wake", "wake_dormant",
			nil, nil, []byte(`{"off_cents":500}`), "hi", "wake dormant member", nil, true, ItemPending, nil,
		))
	st, err := repo.Get(context.Background(), 1, 7)
	require.NoError(t, err)
	require.Equal(t, int64(7), st.ID)
	require.Equal(t, "2026-07-29", st.ForDate)
	require.Equal(t, json.RawMessage(`{"business":"fruit"}`), st.Signals)
	require.Len(t, st.Items, 1)
	require.Equal(t, "coupon_wake", st.Items[0].ActionType)
	// JSONB numeric round-trip: encoding/json decodes numbers into float64
	// (generic decode), so 500 comes back as float64, not int64. This is the
	// one inherent memory↔pg asymmetry (memory preserves the Go int64).
	require.Equal(t, float64(500), st.Items[0].Params["off_cents"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_GetNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	mock.ExpectQuery(sqlStrategyGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.Get(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_GetToday(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	mock.ExpectQuery(sqlStrategyGetToday).
		WithArgs(int64(1), int64(10), "2026-07-29").
		WillReturnRows(pgxmock.NewRows(strategyColumns()).AddRow(
			int64(7), int64(1), int64(10), "2026-07-29", []byte(`{}`), 80, GenRuleOnly, nil, int64(0), StatusGenerated, fixedTime,
		))
	mock.ExpectQuery(sqlItemsByStrategy).
		WithArgs(int64(1), int64(7)).
		WillReturnRows(pgxmock.NewRows(itemColumns()))
	st, err := repo.GetToday(context.Background(), 1, 10, "2026-07-29")
	require.NoError(t, err)
	require.Equal(t, int64(7), st.ID)
	require.Empty(t, st.Items)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_GetTodayNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	mock.ExpectQuery(sqlStrategyGetToday).
		WithArgs(int64(1), int64(10), "2026-07-29").
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetToday(context.Background(), 1, 10, "2026-07-29")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_List(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	// List query (default limit 50, offset 0, no filters → storeID=0, date="", status="").
	mock.ExpectQuery(sqlStrategyList).
		WithArgs(int64(1), int64(0), "", "", 50, 0).
		WillReturnRows(pgxmock.NewRows(strategyColumns()).AddRow(
			int64(7), int64(1), int64(10), "2026-07-29", []byte(`{}`), 80, GenRuleOnly, nil, int64(0), StatusGenerated, fixedTime,
		))
	// Per-strategy items load.
	mock.ExpectQuery(sqlItemsByStrategy).
		WithArgs(int64(1), int64(7)).
		WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(
			int64(11), int64(1), int64(7), 1, "coupon_wake", "wake_dormant", nil, nil, []byte(`{}`), "", "", nil, true, ItemPending, nil,
		))
	out, err := repo.List(context.Background(), 1, StrategyFilter{})
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, int64(7), out[0].ID)
	require.Len(t, out[0].Items, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_GetItem(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	mock.ExpectQuery(sqlItemGetByID).
		WithArgs(int64(1), int64(11)).
		WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(
			int64(11), int64(1), int64(7), 1, "coupon_wake", "wake_dormant",
			[]byte(`{"seg":"sleep_30"}`), nil, []byte(`{"off_cents":500}`), "hi", "wake", nil, true, ItemPending, nil,
		))
	it, err := repo.GetItem(context.Background(), 1, 11)
	require.NoError(t, err)
	require.Equal(t, int64(11), it.ID)
	require.Equal(t, "sleep_30", it.Target["seg"])         // JSONB target round-trip
	require.Equal(t, float64(500), it.Params["off_cents"]) // number decoded as float64
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_GetItemNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	mock.ExpectQuery(sqlItemGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetItem(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_UpdateItemStatus(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	// Legal transition pending→executed: the single conditional UPDATE matches
	// (WHERE execution_status='pending' AND $1<>'pending') and RETURNs the row.
	executedAt := fixedTime
	mock.ExpectQuery(sqlItemUpdateStatus).
		WithArgs(ItemExecuted, &executedAt, int64(1), int64(11)).
		WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(
			int64(11), int64(1), int64(7), 1, "coupon_wake", "wake_dormant", nil, nil, []byte(`{}`), "", "", nil, true, ItemExecuted, executedAt,
		))
	it, err := repo.UpdateItemStatus(context.Background(), 1, 11, ItemExecuted, &executedAt, fixedTime)
	require.NoError(t, err)
	require.Equal(t, ItemExecuted, it.ExecutionStatus)
	require.NotNil(t, it.ExecutedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_UpdateItemStatusIllegal(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	// executed→pending is illegal: the conditional UPDATE's WHERE clause matches
	// nothing (row is 'executed', $1='pending' so the pending-branch is false and
	// execution_status=$1 is false) → ErrNoRows. sqlItemExists then confirms the
	// item exists, disambiguating to ErrParam (not ErrNotFound).
	mock.ExpectQuery(sqlItemUpdateStatus).
		WithArgs(ItemPending, (*time.Time)(nil), int64(1), int64(11)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(sqlItemExists).
		WithArgs(int64(1), int64(11)).
		WillReturnRows(pgxmock.NewRows([]string{"?column?"}).AddRow(1))
	_, err := repo.UpdateItemStatus(context.Background(), 1, 11, ItemPending, nil, fixedTime)
	require.ErrorIs(t, err, ErrParam)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_UpdateItemStatusNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	// Conditional UPDATE matches nothing and sqlItemExists also misses
	// (absent/cross-tenant) → ErrNotFound.
	mock.ExpectQuery(sqlItemUpdateStatus).
		WithArgs(ItemExecuted, &fixedTime, int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(sqlItemExists).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.UpdateItemStatus(context.Background(), 1, 99, ItemExecuted, &fixedTime, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_RecordFeedback(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	// (1) verify the item exists within tenant.
	mock.ExpectQuery(sqlItemExists).
		WithArgs(int64(1), int64(11)).
		WillReturnRows(pgxmock.NewRows([]string{"?column?"}).AddRow(1))
	// (2) UNIQUE(tenant, item) upsert returns the feedback row.
	mock.ExpectQuery(sqlFeedbackUpsert).
		WithArgs(int64(1), int64(11), true, fixedTime).
		WillReturnRows(pgxmock.NewRows(feedbackColumns()).AddRow(
			int64(21), int64(1), int64(11), true, fixedTime,
		))
	fb, err := repo.RecordFeedback(context.Background(), 1, 11, true, fixedTime)
	require.NoError(t, err)
	require.Equal(t, int64(21), fb.ID)
	require.Equal(t, int64(11), fb.StrategyItemID)
	require.True(t, fb.Useful)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStrategyRepo_RecordFeedbackItemNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGStrategyRepo(mock)
	// Item exists check misses (cross-tenant or absent) → ErrNotFound, no upsert.
	mock.ExpectQuery(sqlItemExists).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.RecordFeedback(context.Background(), 1, 99, true, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	// Nil pool / nil repo degrade gracefully (no panic): reads → ErrNotFound,
	// list → empty.
	repo := NewPGStrategyRepo(nil)

	_, err := repo.SaveAdvice(context.Background(), newSampleAdvice())
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.Get(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.GetToday(context.Background(), 1, 1, "2026-07-29")
	require.ErrorIs(t, err, ErrNotFound)

	out, err := repo.List(context.Background(), 1, StrategyFilter{})
	require.NoError(t, err)
	require.Empty(t, out)

	_, err = repo.GetItem(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.UpdateItemStatus(context.Background(), 1, 1, ItemExecuted, &fixedTime, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.RecordFeedback(context.Background(), 1, 1, true, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)

	// A nil receiver is equally safe.
	var nilRepo *PGStrategyRepo
	_, err = nilRepo.Get(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
}
