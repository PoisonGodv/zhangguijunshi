package attribution

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// newMockPool builds a pgxmock pool whose SQL matcher is plain equality (the
// attribution SQL contains regex metacharacters like ( ) $ that would break
// the default regexp matcher). Tests are same-package so they can reference the
// unexported sqlRecord / sqlList / sqlLatest constants.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

func TestPGRepo_RecordUpsertReturnsID(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)

	mock.ExpectQuery(sqlRecord).
		WithArgs(int64(1), int64(10), MetricCouponRedeem, 0.5, fixedMeasure).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(42)))

	m, err := repo.Record(context.Background(), 1, 10, MetricCouponRedeem, 0.5, fixedMeasure)
	require.NoError(t, err)
	require.Equal(t, int64(42), m.ID)
	require.Equal(t, int64(1), m.TenantID)
	require.Equal(t, int64(10), m.StrategyItemID)
	require.InDelta(t, 0.5, m.Value, 1e-9)
	require.True(t, m.MeasuredAt.Equal(fixedMeasure))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_RecordUnknownMetricErrParam(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	// No ExpectQuery: ErrParam is returned before any DB round-trip.
	_, err := repo.Record(context.Background(), 1, 10, "no_such_metric", 0.5, fixedMeasure)
	require.ErrorIs(t, err, ErrParam)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_RecordQueryErrorWrapped(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlRecord).
		WithArgs(int64(1), int64(10), MetricCouponRedeem, 0.5, fixedMeasure).
		WillReturnError(pgx.ErrNoRows) // RETURNING nothing → surfaced as error
	_, err := repo.Record(context.Background(), 1, 10, MetricCouponRedeem, 0.5, fixedMeasure)
	require.Error(t, err)
	require.ErrorIs(t, err, pgx.ErrNoRows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_LatestPerMetric(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)

	mock.ExpectQuery(sqlLatest).
		WithArgs(int64(1), int64(10)).
		WillReturnRows(pgxmock.NewRows([]string{"metric", "value"}).
			AddRow(MetricCouponRedeem, 0.9).
			AddRow(MetricVisitUplift, 2.0))

	got, err := repo.LatestPerMetric(context.Background(), 1, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.InDelta(t, 0.9, got[MetricCouponRedeem], 1e-9)
	require.InDelta(t, 2.0, got[MetricVisitUplift], 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_LatestPerMetricEmpty(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlLatest).
		WithArgs(int64(1), int64(10)).
		WillReturnRows(pgxmock.NewRows([]string{"metric", "value"}))
	got, err := repo.LatestPerMetric(context.Background(), 1, 10)
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_List(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlList).
		WithArgs(int64(1), []int64{10, 11}, []string{MetricCouponRedeem}, 50).
		WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "strategy_item_id", "metric", "value", "measured_at"}).
			AddRow(int64(1), int64(1), int64(10), MetricCouponRedeem, 0.4, fixedMeasure).
			AddRow(int64(2), int64(1), int64(11), MetricCouponRedeem, 0.6, fixedMeasure))

	got, err := repo.List(context.Background(), 1, MeasurementFilter{
		ItemIDs: []int64{10, 11},
		Metrics: []string{MetricCouponRedeem},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(10), got[0].StrategyItemID)
	require.Equal(t, int64(11), got[1].StrategyItemID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_ListNoFilterPassesNullSlices(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlList).
		WithArgs(int64(1), []int64(nil), []string(nil), 50).
		WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "strategy_item_id", "metric", "value", "measured_at"}).
			AddRow(int64(1), int64(1), int64(10), MetricCouponRedeem, 0.4, fixedMeasure))
	got, err := repo.List(context.Background(), 1, MeasurementFilter{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_NilPoolIsNoOp(t *testing.T) {
	repo := NewPGRepo(nil) // nil pool
	_, err := repo.Record(context.Background(), 1, 10, MetricCouponRedeem, 0.5, fixedMeasure)
	require.ErrorIs(t, err, ErrNotFound)
	got, err := repo.List(context.Background(), 1, MeasurementFilter{})
	require.NoError(t, err)
	require.Empty(t, got)
	latest, err := repo.LatestPerMetric(context.Background(), 1, 10)
	require.NoError(t, err)
	require.Empty(t, latest)
}

func TestPGRepo_RecordUsesOnConflictSQL(t *testing.T) {
	// Guards the DoD requirement that the upsert uses
	// INSERT ... ON CONFLICT (tenant_id, strategy_item_id, metric, measured_at)
	// DO UPDATE (the UNIQUE-key upsert). QueryMatcherEqual verifies the exact
	// SQL text is sent.
	require.Contains(t, sqlRecord, "ON CONFLICT (tenant_id, strategy_item_id, metric, measured_at)")
	require.Contains(t, sqlRecord, "DO UPDATE")
	require.Contains(t, sqlRecord, "INSERT INTO strategy_attribution")
}
