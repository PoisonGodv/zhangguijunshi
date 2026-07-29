package reach

// pg_repo_test.go exercises PGReachRepo against pgxmock (CONTRACT §17.3 /
// §11.3.2), mirroring internal/payment/pg_repo_test.go. Same-package so the
// unexported sql* constants are referenced; QueryMatcherEqual because the SQL
// text contains regex metacharacters. Column helpers match the scanXxx Scan
// order exactly — that ordering is what makes pgxmock row mapping succeed.
// ConsumeSubscribe's "no row → ErrSubscribeExhausted" and GrantSubscribe's
// read-then-write (no UNIQUE on the lookup tuple) are the load-bearing cases.

import (
	"context"
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

// taskColumns mirrors the scanReachTask Scan order (12 columns).
func taskColumns() []string {
	return []string{"id", "tenant_id", "store_id", "channel", "template_code", "target_count", "status", "blocked_reason", "strategy_id", "scheduled_at", "sent_at", "created_at"}
}

// subColumns mirrors the scanSubscribeAuth Scan order (7 columns).
func subColumns() []string {
	return []string{"id", "tenant_id", "member_id", "template_id", "granted_quota", "used_quota", "granted_at"}
}

// unsubColumns mirrors the scanUnsubscribe Scan order (6 columns).
func unsubColumns() []string {
	return []string{"id", "tenant_id", "member_id", "channel", "reason", "unsubscribed_at"}
}

func TestPGReachRepo_CreateAndGet(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)

	// CreateTask: non-AI attribution (StrategyID 0 → NULL arg), zero SentAt →
	// NULL arg, blocked task. Args order matches sqlReachTaskCreate exactly.
	mock.ExpectQuery(sqlReachTaskCreate).
		WithArgs(int64(1), int64(10), ChannelWeworkOneV, TplRecall30, 1, StatusBlocked, "freq_circuit", nil, fixedTime, nil, fixedTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(5)))
	task, err := repo.CreateTask(context.Background(), &ReachTask{
		TenantID: 1, StoreID: 10, Channel: ChannelWeworkOneV, TemplateCode: TplRecall30,
		TargetCount: 1, Status: StatusBlocked, BlockedReason: BlockFreqCircuit,
		ScheduledAt: fixedTime, CreatedAt: fixedTime,
	})
	require.NoError(t, err)
	require.Equal(t, int64(5), task.ID)

	// GetTask reads back a sent task with AI attribution + sent_at populated.
	mock.ExpectQuery(sqlReachTaskGetByID).
		WithArgs(int64(1), int64(5)).
		WillReturnRows(pgxmock.NewRows(taskColumns()).AddRow(
			int64(5), int64(1), int64(10), ChannelWeworkOneV, TplRecall30, 1, "sent",
			nil, int64(7), fixedTime, fixedTime, fixedTime,
		))
	got, err := repo.GetTask(context.Background(), 1, 5)
	require.NoError(t, err)
	require.Equal(t, StatusSent, got.Status)
	require.Equal(t, "", got.BlockedReason) // NULL → ""
	require.Equal(t, int64(7), got.StrategyID)
	require.Equal(t, fixedTime, got.SentAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_GetTaskNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectQuery(sqlReachTaskGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetTask(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_ListTasks(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectQuery(sqlReachTaskList).
		WithArgs(int64(1), int64(10), StatusSent).
		WillReturnRows(pgxmock.NewRows(taskColumns()).
			AddRow(int64(5), int64(1), int64(10), ChannelWeworkOneV, TplRecall30, 1, "sent", nil, int64(7), fixedTime, fixedTime, fixedTime))
	got, err := repo.ListTasks(context.Background(), 1, 10, StatusSent)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(5), got[0].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_UpdateTaskStatus(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	// status="sent", blockedReason="" (NULLIF → NULL), sentAt=fixedTime (non-zero
	// → overwrite). Args: status, blockedReason, sentAtArg, tenant, id.
	mock.ExpectQuery(sqlReachTaskUpdateStatus).
		WithArgs(StatusSent, "", fixedTime, int64(1), int64(5)).
		WillReturnRows(pgxmock.NewRows(taskColumns()).AddRow(
			int64(5), int64(1), int64(10), ChannelWeworkOneV, TplRecall30, 1, "sent", nil, int64(7), fixedTime, fixedTime, fixedTime,
		))
	t2, err := repo.UpdateTaskStatus(context.Background(), 1, 5, StatusSent, "", fixedTime)
	require.NoError(t, err)
	require.Equal(t, StatusSent, t2.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_UpdateTaskStatusNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectQuery(sqlReachTaskUpdateStatus).
		WithArgs(StatusSent, "", fixedTime, int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.UpdateTaskStatus(context.Background(), 1, 99, StatusSent, "", fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_GrantSubscribeNew(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	// Lookup misses (no existing auth) → INSERT new with GrantedQuota=quota.
	mock.ExpectQuery(sqlSubAuthGrantLookup).
		WithArgs(int64(1), int64(100), "tpl_subscribe").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(sqlSubAuthGrantInsert).
		WithArgs(int64(1), int64(100), "tpl_subscribe", 3, fixedTime).
		WillReturnRows(pgxmock.NewRows(subColumns()).AddRow(
			int64(9), int64(1), int64(100), "tpl_subscribe", 3, 0, fixedTime,
		))
	a, err := repo.GrantSubscribe(context.Background(), 1, 100, "tpl_subscribe", 3, fixedTime)
	require.NoError(t, err)
	require.Equal(t, int64(9), a.ID)
	require.Equal(t, 3, a.GrantedQuota)
	require.Equal(t, 0, a.UsedQuota)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_GrantSubscribeAccumulate(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	// Lookup hits (existing id=9) → UPDATE granted_quota += quota.
	mock.ExpectQuery(sqlSubAuthGrantLookup).
		WithArgs(int64(1), int64(100), "tpl_subscribe").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(9)))
	mock.ExpectQuery(sqlSubAuthGrantAdd).
		WithArgs(3, fixedTime, int64(1), int64(100), "tpl_subscribe").
		WillReturnRows(pgxmock.NewRows(subColumns()).AddRow(
			int64(9), int64(1), int64(100), "tpl_subscribe", 6, 1, fixedTime,
		))
	a, err := repo.GrantSubscribe(context.Background(), 1, 100, "tpl_subscribe", 3, fixedTime)
	require.NoError(t, err)
	require.Equal(t, 6, a.GrantedQuota) // 3 accumulated onto prior 3
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_ConsumeSubscribe(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectQuery(sqlSubAuthConsume).
		WithArgs(int64(1), int64(100), "tpl_subscribe").
		WillReturnRows(pgxmock.NewRows(subColumns()).AddRow(
			int64(9), int64(1), int64(100), "tpl_subscribe", 3, 1, fixedTime,
		))
	a, err := repo.ConsumeSubscribe(context.Background(), 1, 100, "tpl_subscribe", fixedTime)
	require.NoError(t, err)
	require.Equal(t, 1, a.UsedQuota)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_ConsumeSubscribeExhausted(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	// Conditional UPDATE matches no row (absent or used>=granted) → ErrNoRows.
	mock.ExpectQuery(sqlSubAuthConsume).
		WithArgs(int64(1), int64(100), "tpl_subscribe").
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.ConsumeSubscribe(context.Background(), 1, 100, "tpl_subscribe", fixedTime)
	require.ErrorIs(t, err, ErrSubscribeExhausted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_GetSubscribe(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectQuery(sqlSubAuthGet).
		WithArgs(int64(1), int64(100), "tpl_subscribe").
		WillReturnRows(pgxmock.NewRows(subColumns()).AddRow(
			int64(9), int64(1), int64(100), "tpl_subscribe", 3, 1, fixedTime,
		))
	a, err := repo.GetSubscribe(context.Background(), 1, 100, "tpl_subscribe")
	require.NoError(t, err)
	require.Equal(t, 3, a.GrantedQuota)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_GetSubscribeNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectQuery(sqlSubAuthGet).
		WithArgs(int64(1), int64(100), "tpl_subscribe").
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetSubscribe(context.Background(), 1, 100, "tpl_subscribe")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_Unsubscribe(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectQuery(sqlUnsubscribeUpsert).
		WithArgs(int64(1), int64(100), ChannelWeworkGroup, "too often", fixedTime).
		WillReturnRows(pgxmock.NewRows(unsubColumns()).AddRow(
			int64(1), int64(1), int64(100), ChannelWeworkGroup, "too often", fixedTime,
		))
	u, err := repo.Unsubscribe(context.Background(), 1, 100, ChannelWeworkGroup, "too often", fixedTime)
	require.NoError(t, err)
	require.Equal(t, "too often", u.Reason)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_IsUnsubscribed(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectQuery(sqlUnsubscribedExists).
		WithArgs(int64(1), int64(100), ChannelWeworkGroup).
		WillReturnRows(pgxmock.NewRows([]string{"?column?"}).AddRow(1))
	ok, err := repo.IsUnsubscribed(context.Background(), 1, 100, ChannelWeworkGroup)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_IsUnsubscribedFalse(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectQuery(sqlUnsubscribedExists).
		WithArgs(int64(1), int64(100), ChannelWeworkGroup).
		WillReturnError(pgx.ErrNoRows)
	ok, err := repo.IsUnsubscribed(context.Background(), 1, 100, ChannelWeworkGroup)
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGReachRepo_Resubscribe(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGReachRepo(mock)
	mock.ExpectExec(sqlResubscribe).
		WithArgs(int64(1), int64(100), ChannelWeworkGroup).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	require.NoError(t, repo.Resubscribe(context.Background(), 1, 100, ChannelWeworkGroup))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	// Nil pool / nil repo degrade gracefully (no panic): reads → ErrNotFound,
	// lists → empty, IsUnsubscribed → (false,nil), Consume → exhausted,
	// Resubscribe → nil.
	repo := NewPGReachRepo(nil)

	_, err := repo.GetTask(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	out, err := repo.ListTasks(context.Background(), 1, 0, "")
	require.NoError(t, err)
	require.Empty(t, out)

	_, err = repo.UpdateTaskStatus(context.Background(), 1, 1, StatusSent, "", fixedTime)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.GrantSubscribe(context.Background(), 1, 1, "t", 1, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.ConsumeSubscribe(context.Background(), 1, 1, "t", fixedTime)
	require.ErrorIs(t, err, ErrSubscribeExhausted)

	_, err = repo.GetSubscribe(context.Background(), 1, 1, "t")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.Unsubscribe(context.Background(), 1, 1, ChannelSMS, "", fixedTime)
	require.ErrorIs(t, err, ErrNotFound)

	ok, err := repo.IsUnsubscribed(context.Background(), 1, 1, ChannelSMS)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, repo.Resubscribe(context.Background(), 1, 1, ChannelSMS))

	// CreateTask on nil pool → ErrNotFound (write cannot persist).
	_, err = repo.CreateTask(context.Background(), &ReachTask{TenantID: 1})
	require.ErrorIs(t, err, ErrNotFound)

	// A nil receiver is equally safe.
	var nilRepo *PGReachRepo
	_, err = nilRepo.GetTask(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
}
