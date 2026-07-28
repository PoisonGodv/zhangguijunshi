package bandit

// bandit_test.go exercises the epsilon-greedy learner, both Repo
// implementations, and the anti-corruption ports (CONTRACT §14.6 A3 DoD).
// Determinism: Select uses WithRand with a fixed-sequence source; Update and
// CreditItem take explicit rewards (no time.Now). MemoryRepo is mutex-guarded
// for -race safety; PGRepo is exercised through pgxmock (no live DB).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Config / clip helpers ----

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.InDelta(t, 0.1, cfg.Epsilon, 1e-9)
	assert.InDelta(t, 0.1, cfg.LR, 1e-9)
}

func TestClipReward(t *testing.T) {
	cases := map[float64]float64{
		0.0:   0.0,
		0.5:   0.5,
		1.0:   1.0,
		-0.5:  0.0, // negative clamped to 0
		1.5:   1.0, // >1 clamped to 1
		2.0:   1.0,
		-0.01: 0.0,
	}
	for in, want := range cases {
		got := clipReward(in)
		assert.InDelta(t, want, got, 1e-9, "clipReward(%v)", in)
	}
	// NaN must collapse to 0 (not poison RewardSum).
	assert.InDelta(t, 0, clipReward(nan()), 1e-9)
}

func nan() float64 {
	// zero / zero yields NaN without importing math in the test for a 0/0.
	var z float64
	return z / z
}

// ---- MemoryRepo ----

func TestMemoryRepo_Get_UnseenReturnsNeutralPrior(t *testing.T) {
	r := NewMemoryRepo()
	a, err := r.Get(context.Background(), 1, "rain_delivery")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "rain_delivery", a.RuleID)
	assert.InDelta(t, 0.5, a.Weight, 1e-9)
	assert.Zero(t, a.Trials)
	assert.Zero(t, a.RewardSum)
}

func TestMemoryRepo_Get_CrossTenantIsNeutralPrior(t *testing.T) {
	r := NewMemoryRepo()
	require.NoError(t, r.Put(context.Background(), 1, &Arm{RuleID: "x", Weight: 0.9, Trials: 5, RewardSum: 4}))
	// Tenant 2 must NOT see tenant 1's arm.
	a, err := r.Get(context.Background(), 2, "x")
	require.NoError(t, err)
	assert.InDelta(t, 0.5, a.Weight, 1e-9)
	assert.Zero(t, a.Trials)
	assert.NotEqual(t, 0.9, a.Weight, "cross-tenant weight must not leak")
}

func TestMemoryRepo_Put_Upsert(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	require.NoError(t, r.Put(ctx, 1, &Arm{RuleID: "x", Weight: 0.6, Trials: 1, RewardSum: 0.6}))
	// Second Put on the same (tenant,rule) overwrites.
	require.NoError(t, r.Put(ctx, 1, &Arm{RuleID: "x", Weight: 0.7, Trials: 2, RewardSum: 1.2}))
	a, err := r.Get(ctx, 1, "x")
	require.NoError(t, err)
	assert.InDelta(t, 0.7, a.Weight, 1e-9)
	assert.Equal(t, 2, a.Trials)
	assert.InDelta(t, 1.2, a.RewardSum, 1e-9)
}

func TestMemoryRepo_Put_NilArmNoOp(t *testing.T) {
	r := NewMemoryRepo()
	assert.NoError(t, r.Put(context.Background(), 1, nil))
}

func TestMemoryRepo_All_OnlyOwnTenantSorted(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	require.NoError(t, r.Put(ctx, 1, &Arm{RuleID: "wake_dormant", Weight: 0.3}))
	require.NoError(t, r.Put(ctx, 1, &Arm{RuleID: "clearance_coupon", Weight: 0.8}))
	require.NoError(t, r.Put(ctx, 2, &Arm{RuleID: "rain_delivery", Weight: 0.99})) // other tenant

	got, err := r.All(ctx, 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Sorted by RuleID: clearance_coupon < wake_dormant.
	assert.Equal(t, "clearance_coupon", got[0].RuleID)
	assert.Equal(t, "wake_dormant", got[1].RuleID)
	// Defensive copy: mutating the returned slice must not touch the store.
	got[0].Weight = -1
	again, _ := r.All(ctx, 1)
	assert.InDelta(t, 0.8, again[0].Weight, 1e-9, "All must return copies")
}

func TestMemoryRepo_All_EmptyAndCrossTenant(t *testing.T) {
	r := NewMemoryRepo()
	require.NoError(t, r.Put(context.Background(), 1, &Arm{RuleID: "x", Weight: 0.5}))
	got, err := r.All(context.Background(), 99)
	require.NoError(t, err)
	assert.Empty(t, got, "cross-tenant All must be empty")
}

func TestMemoryRepo_Concurrent_NoRace(t *testing.T) {
	// -race: hammer Put/Get/All from many goroutines; if the mutex is wrong
	// the race detector fires.
	r := NewMemoryRepo()
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = r.Put(ctx, 1, &Arm{RuleID: "rain_delivery", Weight: 0.5})
				_, _ = r.Get(ctx, 1, "rain_delivery")
				_, _ = r.All(ctx, 1)
			}
		}()
	}
	wg.Wait()
}

// ---- Select ----

func TestSelect_EmptyReturnsNilNil(t *testing.T) {
	l := NewLearner(NewMemoryRepo(), DefaultConfig())
	a, err := l.Select(context.Background(), 1, nil)
	assert.NoError(t, err)
	assert.Nil(t, a)

	a2, err := l.Select(context.Background(), 1, []Arm{})
	assert.NoError(t, err)
	assert.Nil(t, a2)
}

func TestSelect_Exploit_HighestWeight(t *testing.T) {
	// WithRand returns 0.9 (>= epsilon 0.1) → exploit → highest Weight.
	seq := seqRand(0.9)
	l := NewLearner(NewMemoryRepo(), DefaultConfig(), WithRand(seq.next))
	arms := []Arm{
		{RuleID: "a", Weight: 0.2},
		{RuleID: "b", Weight: 0.8},
		{RuleID: "c", Weight: 0.5},
	}
	a, err := l.Select(context.Background(), 1, arms)
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "b", a.RuleID)
}

func TestSelect_Exploit_TieFirstWins(t *testing.T) {
	seq := seqRand(0.9) // exploit
	l := NewLearner(NewMemoryRepo(), DefaultConfig(), WithRand(seq.next))
	arms := []Arm{
		{RuleID: "first", Weight: 0.8},
		{RuleID: "second", Weight: 0.8},
	}
	a, err := l.Select(context.Background(), 1, arms)
	require.NoError(t, err)
	assert.Equal(t, "first", a.RuleID, "ties broken by first occurrence")
}

func TestSelect_Explore_WithRand(t *testing.T) {
	// First draw 0.05 < epsilon 0.1 → explore. Second draw 0.5 → idx for
	// len=3 is int(0.5*3)=1 → arm index 1 (not the argmax, which is index 2).
	seq := seqRand(0.05, 0.5)
	l := NewLearner(NewMemoryRepo(), DefaultConfig(), WithRand(seq.next))
	arms := []Arm{
		{RuleID: "a", Weight: 0.1},
		{RuleID: "b", Weight: 0.2}, // explore lands here
		{RuleID: "c", Weight: 0.9}, // argmax (would be chosen on exploit)
	}
	a, err := l.Select(context.Background(), 1, arms)
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "b", a.RuleID, "explore must pick the random index, not the argmax")
}

func TestSelect_Explore_ClampTopEdge(t *testing.T) {
	// First draw 0.0 < epsilon → explore. Second draw 1.0 (the [0,1) top
	// edge) → idx int(1.0*2)=2 → clamped to 1.
	seq := seqRand(0.0, 1.0)
	l := NewLearner(NewMemoryRepo(), DefaultConfig(), WithRand(seq.next))
	arms := []Arm{
		{RuleID: "a", Weight: 0.9},
		{RuleID: "b", Weight: 0.1},
	}
	a, err := l.Select(context.Background(), 1, arms)
	require.NoError(t, err)
	assert.Equal(t, "b", a.RuleID)
}

// fixedSeq yields a fixed float64 sequence; .next is the func() float64.
type fixedSeq struct {
	vals []float64
	i    int
}

func seqRand(vs ...float64) *fixedSeq { return &fixedSeq{vals: vs} }
func (s *fixedSeq) next() float64 {
	if s.i >= len(s.vals) {
		return s.vals[len(s.vals)-1]
	}
	v := s.vals[s.i]
	s.i++
	return v
}

// ---- Update ----

func TestUpdate_SoftUpdateAndIncrementalMean(t *testing.T) {
	// cfg LR=0.5 for arithmetic clarity. Seed arm Weight=0.5 (unseen).
	// reward=1.0 → Trials=1, RewardSum=1.0, q=1.0, Weight=0.5*0.5 + 1.0*0.5 = 0.75.
	cfg := Config{Epsilon: 0.1, LR: 0.5}
	l := NewLearner(NewMemoryRepo(), cfg)
	a, err := l.Update(context.Background(), 1, "rain_delivery", 1.0)
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "rain_delivery", a.RuleID)
	assert.Equal(t, 1, a.Trials)
	assert.InDelta(t, 1.0, a.RewardSum, 1e-9)
	assert.InDelta(t, 0.75, a.Weight, 1e-9)

	// Second reward 0.0: Trials=2, RewardSum=1.0, q=0.5,
	// Weight = 0.75*0.5 + 0.5*0.5 = 0.625.
	a2, err := l.Update(context.Background(), 1, "rain_delivery", 0.0)
	require.NoError(t, err)
	assert.Equal(t, 2, a2.Trials)
	assert.InDelta(t, 1.0, a2.RewardSum, 1e-9)
	assert.InDelta(t, 0.625, a2.Weight, 1e-9)
}

func TestUpdate_UnseenSeedsNeutralPrior(t *testing.T) {
	l := NewLearner(NewMemoryRepo(), Config{Epsilon: 0.1, LR: 0.1})
	a, err := l.Update(context.Background(), 1, "fresh_rule", 0.5)
	require.NoError(t, err)
	assert.InDelta(t, 0.5, a.RewardSum/float64(a.Trials), 1e-9) // q=0.5
	// Weight = 0.5*(0.9) + 0.5*0.1 = 0.5 (seed 0.5, q 0.5, LR 0.1).
	assert.InDelta(t, 0.5, a.Weight, 1e-9)
}

func TestUpdate_ClipReward(t *testing.T) {
	l := NewLearner(NewMemoryRepo(), Config{Epsilon: 0.1, LR: 1.0}) // LR=1 → Weight=q
	// reward 2.0 clipped to 1.0 → q=1.0 → Weight=1.0 (ignoring seed).
	a, err := l.Update(context.Background(), 1, "x", 2.0)
	require.NoError(t, err)
	assert.InDelta(t, 1.0, a.RewardSum, 1e-9)
	assert.InDelta(t, 1.0, a.Weight, 1e-9)

	// Negative clipped to 0.
	a2, err := l.Update(context.Background(), 1, "y", -3.0)
	require.NoError(t, err)
	assert.InDelta(t, 0.0, a2.RewardSum, 1e-9)
	assert.InDelta(t, 0.0, a2.Weight, 1e-9)
}

func TestUpdate_PersistsAcrossCalls(t *testing.T) {
	repo := NewMemoryRepo()
	l := NewLearner(repo, Config{Epsilon: 0.1, LR: 0.1})
	_, err := l.Update(context.Background(), 1, "z", 0.9)
	require.NoError(t, err)
	// A fresh learner over the same repo must see the persisted Trials=1.
	l2 := NewLearner(repo, Config{Epsilon: 0.1, LR: 0.1})
	a, err := l2.Update(context.Background(), 1, "z", 0.1)
	require.NoError(t, err)
	assert.Equal(t, 2, a.Trials, "Update must persist via Put")
}

func TestUpdate_EmptyRuleID_ErrParam(t *testing.T) {
	l := NewLearner(NewMemoryRepo(), DefaultConfig())
	_, err := l.Update(context.Background(), 1, "", 0.5)
	assert.ErrorIs(t, err, ErrParam)
}

func TestUpdate_NilRepo_ErrParam(t *testing.T) {
	l := NewLearner(nil, DefaultConfig())
	_, err := l.Update(context.Background(), 1, "x", 0.5)
	assert.ErrorIs(t, err, ErrParam)
}

// ---- CreditItem ----

type fakeResolver struct {
	ruleID     string
	strategyID int64
	storeID    int64
	err        error
}

func (f *fakeResolver) Resolve(_ context.Context, _ int64, _ int64) (string, int64, int64, error) {
	return f.ruleID, f.strategyID, f.storeID, f.err
}

type fakeRewardSource struct {
	reward float64
	err    error
	called bool
}

func (f *fakeRewardSource) Reward(_ context.Context, _, _, _, _ int64) (float64, error) {
	f.called = true
	return f.reward, f.err
}

func TestCreditItem_NilPortsReturnsNilNil(t *testing.T) {
	// No ports wired → safe no-op (dev without attribution data).
	l := NewLearner(NewMemoryRepo(), DefaultConfig())
	a, err := l.CreditItem(context.Background(), 1, 42)
	assert.NoError(t, err)
	assert.Nil(t, a)
}

func TestCreditItem_ResolveRewardUpdate(t *testing.T) {
	rs := &fakeRewardSource{reward: 1.0}
	rv := &fakeResolver{ruleID: "high_value_push", strategyID: 7, storeID: 3}
	l := NewLearner(NewMemoryRepo(), Config{Epsilon: 0.1, LR: 1.0}, WithRewardSource(rs), WithItemResolver(rv))

	a, err := l.CreditItem(context.Background(), 1, 42)
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "high_value_push", a.RuleID)
	assert.Equal(t, 1, a.Trials)
	assert.True(t, rs.called, "RewardSource.Reward must be called")
	// reward 1.0, LR 1.0 → Weight = 0.5*0 + 1.0*1 = 1.0.
	assert.InDelta(t, 1.0, a.Weight, 1e-9)
}

func TestCreditItem_ResolveError(t *testing.T) {
	rv := &fakeResolver{err: errors.New("no such item")}
	rs := &fakeRewardSource{reward: 1.0}
	l := NewLearner(NewMemoryRepo(), DefaultConfig(), WithRewardSource(rs), WithItemResolver(rv))
	a, err := l.CreditItem(context.Background(), 1, 42)
	assert.Nil(t, a)
	assert.ErrorContains(t, err, "no such item")
	assert.False(t, rs.called, "Reward must not be called when Resolve fails")
}

func TestCreditItem_RewardError(t *testing.T) {
	rv := &fakeResolver{ruleID: "x", strategyID: 1, storeID: 1}
	rs := &fakeRewardSource{err: errors.New("attribution unavailable")}
	l := NewLearner(NewMemoryRepo(), DefaultConfig(), WithRewardSource(rs), WithItemResolver(rv))
	a, err := l.CreditItem(context.Background(), 1, 42)
	assert.Nil(t, a)
	assert.ErrorContains(t, err, "attribution unavailable")
}

func TestCreditItem_RewardClip(t *testing.T) {
	rv := &fakeResolver{ruleID: "x", strategyID: 1, storeID: 1}
	rs := &fakeRewardSource{reward: 5.0} // >1, must be clipped to 1.0
	l := NewLearner(NewMemoryRepo(), Config{Epsilon: 0.1, LR: 1.0}, WithRewardSource(rs), WithItemResolver(rv))
	a, err := l.CreditItem(context.Background(), 1, 42)
	require.NoError(t, err)
	assert.InDelta(t, 1.0, a.RewardSum, 1e-9, "CreditItem reward must be clipped")
}

// ---- PGRepo (pgxmock) ----

func newMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	m, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	return m
}

func TestPGRepo_Get_Hit(t *testing.T) {
	m := newMock(t)
	defer m.Close()
	m.ExpectQuery(sqlGet).
		WithArgs(int64(1), "rain_delivery").
		WillReturnRows(pgxmock.NewRows([]string{"rule_id", "weight", "trials", "reward_sum"}).
			AddRow("rain_delivery", 0.83, 10, 7.0))

	r := NewPGRepo(m)
	a, err := r.Get(context.Background(), 1, "rain_delivery")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "rain_delivery", a.RuleID)
	assert.InDelta(t, 0.83, a.Weight, 1e-9)
	assert.Equal(t, 10, a.Trials)
	assert.NoError(t, m.ExpectationsWereMet())
}

func TestPGRepo_Get_UnseenErrNoRowsNeutralPrior(t *testing.T) {
	m := newMock(t)
	defer m.Close()
	m.ExpectQuery(sqlGet).
		WithArgs(int64(1), "fresh").
		WillReturnRows(pgxmock.NewRows([]string{"rule_id", "weight", "trials", "reward_sum"})) // no rows

	r := NewPGRepo(m)
	a, err := r.Get(context.Background(), 1, "fresh")
	require.NoError(t, err, "ErrNoRows must map to neutral prior, not an error")
	assert.InDelta(t, 0.5, a.Weight, 1e-9)
	assert.Zero(t, a.Trials)
	assert.NoError(t, m.ExpectationsWereMet())
}

func TestPGRepo_Get_CrossTenantNeutralPrior(t *testing.T) {
	// A cross-tenant read is just a miss (tenant_id=2 row absent under the
	// WHERE tenant_id=1 scope), so it returns the neutral prior — no leak.
	m := newMock(t)
	defer m.Close()
	m.ExpectQuery(sqlGet).
		WithArgs(int64(1), "x").
		WillReturnRows(pgxmock.NewRows([]string{"rule_id", "weight", "trials", "reward_sum"}))

	r := NewPGRepo(m)
	a, err := r.Get(context.Background(), 1, "x")
	require.NoError(t, err)
	assert.InDelta(t, 0.5, a.Weight, 1e-9)
	assert.NoError(t, m.ExpectationsWereMet())
}

func TestPGRepo_Put_Upsert(t *testing.T) {
	m := newMock(t)
	defer m.Close()
	m.ExpectExec(sqlPut).
		WithArgs(int64(1), "rain_delivery", 0.83, 10, 7.0).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	r := NewPGRepo(m)
	err := r.Put(context.Background(), 1, &Arm{RuleID: "rain_delivery", Weight: 0.83, Trials: 10, RewardSum: 7.0})
	assert.NoError(t, err)
	assert.NoError(t, m.ExpectationsWereMet())
}

func TestPGRepo_Put_NilArmNoOp(t *testing.T) {
	m := newMock(t)
	defer m.Close()
	r := NewPGRepo(m)
	assert.NoError(t, r.Put(context.Background(), 1, nil))
	// No expectations set → a call to Exec would fail expectations; the no-op
	// must NOT touch the pool.
	assert.NoError(t, m.ExpectationsWereMet())
}

func TestPGRepo_All_MultiRow(t *testing.T) {
	m := newMock(t)
	defer m.Close()
	m.ExpectQuery(sqlAll).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"rule_id", "weight", "trials", "reward_sum"}).
			AddRow("clearance_coupon", 0.8, 3, 2.0).
			AddRow("wake_dormant", 0.4, 1, 0.5))

	r := NewPGRepo(m)
	got, err := r.All(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "clearance_coupon", got[0].RuleID)
	assert.Equal(t, "wake_dormant", got[1].RuleID)
	assert.NoError(t, m.ExpectationsWereMet())
}

func TestPGRepo_All_Empty(t *testing.T) {
	m := newMock(t)
	defer m.Close()
	m.ExpectQuery(sqlAll).
		WithArgs(int64(7)).
		WillReturnRows(pgxmock.NewRows([]string{"rule_id", "weight", "trials", "reward_sum"}))

	r := NewPGRepo(m)
	got, err := r.All(context.Background(), 7)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NoError(t, m.ExpectationsWereMet())
}

func TestPGRepo_NilPoolSafe(t *testing.T) {
	r := NewPGRepo(nil)
	a, err := r.Get(context.Background(), 1, "x")
	require.NoError(t, err)
	assert.InDelta(t, 0.5, a.Weight, 1e-9)
	assert.NoError(t, r.Put(context.Background(), 1, &Arm{RuleID: "x", Weight: 0.9}))
	got, err := r.All(context.Background(), 1)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// satisfy assertions: ensure the fake ports implement the interfaces.
var _ RewardSource = (*fakeRewardSource)(nil)
var _ ItemResolver = (*fakeResolver)(nil)

// pgxmock.PgxPoolIface must satisfy our optional queryer (it has Query).
var _ queryer = (pgxmock.PgxPoolIface)(nil)

// ensure pgx.Rows / pgconn are referenced (compile-time interface alignment).
var _ = pgx.ErrNoRows
