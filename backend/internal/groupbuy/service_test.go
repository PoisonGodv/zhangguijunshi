package groupbuy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

func ctxWithTenant(tid int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tid,
		Source:   multitenancy.SourceMerchant,
	})
}

// fixedNow is the service clock used across tests; squads are given a window
// around it unless a test needs to advance the clock.
var fixedNow = time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)

func newSvc(t *testing.T, opts ...Option) *Service {
	t.Helper()
	return New(NewMemoryGroupbuyRepo(), append([]Option{WithNowFunc(func() time.Time { return fixedNow })}, opts...)...)
}

func mustCreate(t *testing.T, s *Service, in CreateInput) *Groupbuy {
	t.Helper()
	if in.Title == "" {
		in.Title = "t"
	}
	if in.ProductID == 0 {
		in.ProductID = 1
	}
	g, err := s.Create(ctxWithTenant(50021), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return g
}

func TestCreate_DefaultsAndWindow(t *testing.T) {
	s := newSvc(t)
	g, err := s.Create(ctxWithTenant(50021), CreateInput{
		Title: "三人团", ProductID: 7, Params: GroupbuyParams{PriceCents: 9900},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if g.Status != StatusActive {
		t.Errorf("expect active, got %s", g.Status)
	}
	if g.RequiredCount != 3 {
		t.Errorf("expect default required=3, got %d", g.RequiredCount)
	}
	if g.Joined != 0 {
		t.Errorf("expect joined=0, got %d", g.Joined)
	}
	if !g.StartAt.Equal(fixedNow) {
		t.Errorf("expect start=now, got %v", g.StartAt)
	}
	wantEnd := fixedNow.Add(24 * time.Hour)
	if !g.EndAt.Equal(wantEnd) {
		t.Errorf("expect end=now+24h, got %v want %v", g.EndAt, wantEnd)
	}
}

func TestCreate_RequiredCountFallback(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	// explicit RequiredCount wins
	g1, _ := s.Create(ctx, CreateInput{Title: "a", ProductID: 1, RequiredCount: 4})
	if g1.RequiredCount != 4 {
		t.Fatalf("expect 4, got %d", g1.RequiredCount)
	}
	// RequiredCount=0 → Params.GroupSize
	g2, _ := s.Create(ctx, CreateInput{Title: "b", ProductID: 1, Params: GroupbuyParams{GroupSize: 5}})
	if g2.RequiredCount != 5 {
		t.Fatalf("expect GroupSize 5, got %d", g2.RequiredCount)
	}
	// both zero → 3
	g3, _ := s.Create(ctx, CreateInput{Title: "c", ProductID: 1})
	if g3.RequiredCount != 3 {
		t.Fatalf("expect default 3, got %d", g3.RequiredCount)
	}
}

func TestCreate_ValidHoursFallback(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	// ValidHours=0, Params.ValidHours=6 → 6h
	g1, _ := s.Create(ctx, CreateInput{Title: "a", ProductID: 1, Params: GroupbuyParams{ValidHours: 6}})
	want := fixedNow.Add(6 * time.Hour)
	if !g1.EndAt.Equal(want) {
		t.Fatalf("expect end=now+6h, got %v want %v", g1.EndAt, want)
	}
	// both zero → 24h
	g2, _ := s.Create(ctx, CreateInput{Title: "b", ProductID: 1})
	if !g2.EndAt.Equal(fixedNow.Add(24 * time.Hour)) {
		t.Fatalf("expect end=now+24h, got %v", g2.EndAt)
	}
}

func TestCreate_Validation(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	if _, err := s.Create(ctx, CreateInput{Title: "", ProductID: 1}); err == nil {
		t.Fatal("empty title should reject")
	}
	if _, err := s.Create(ctx, CreateInput{Title: "x", ProductID: 0}); err == nil {
		t.Fatal("zero product_id should reject")
	}
}

func TestJoin_JoinedIncrementAndSuccess(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	g := mustCreate(t, s, CreateInput{Title: "t", ProductID: 1, RequiredCount: 3})

	// 1st join
	p1, gb, err := s.Join(ctx, g.ID, 7001)
	if err != nil {
		t.Fatalf("join1: %v", err)
	}
	if p1.Status != PStatusJoined || p1.MemberID != 7001 {
		t.Errorf("participant wrong: %+v", p1)
	}
	if gb.Joined != 1 || gb.Status != StatusActive {
		t.Errorf("expect joined=1 active, got joined=%d status=%s", gb.Joined, gb.Status)
	}
	// 2nd join — still active
	_, gb, err = s.Join(ctx, g.ID, 7002)
	if err != nil {
		t.Fatalf("join2: %v", err)
	}
	if gb.Joined != 2 || gb.Status != StatusActive {
		t.Errorf("expect joined=2 active, got joined=%d status=%s", gb.Joined, gb.Status)
	}
	// 3rd join — flips to success
	_, gb, err = s.Join(ctx, g.ID, 7003)
	if err != nil {
		t.Fatalf("join3: %v", err)
	}
	if gb.Joined != 3 || gb.Status != StatusSuccess {
		t.Fatalf("expect joined=3 success, got joined=%d status=%s", gb.Joined, gb.Status)
	}
	// persisted state matches
	got, _ := s.Get(ctx, g.ID)
	if got.Joined != 3 || got.Status != StatusSuccess {
		t.Fatalf("persisted joined/status wrong: joined=%d status=%s", got.Joined, got.Status)
	}
}

func TestJoin_FullRejected(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	g := mustCreate(t, s, CreateInput{Title: "t", ProductID: 1, RequiredCount: 2})
	s.Join(ctx, g.ID, 7001)
	s.Join(ctx, g.ID, 7002) // → success
	// a 3rd distinct member is over-quota
	_, _, err := s.Join(ctx, g.ID, 7003)
	if !errors.Is(err, ErrGroupbuyFull) {
		t.Fatalf("over-quota join should ErrGroupbuyFull, got %v", err)
	}
	// Joined must not have grown past RequiredCount
	got, _ := s.Get(ctx, g.ID)
	if got.Joined != 2 {
		t.Fatalf("joined must stay 2, got %d", got.Joined)
	}
}

func TestJoin_AlreadyJoinedRejected(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	g := mustCreate(t, s, CreateInput{Title: "t", ProductID: 1, RequiredCount: 3})
	if _, _, err := s.Join(ctx, g.ID, 7001); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Join(ctx, g.ID, 7001)
	if !errors.Is(err, ErrAlreadyJoined) {
		t.Fatalf("re-join should ErrAlreadyJoined, got %v", err)
	}
	got, _ := s.Get(ctx, g.ID)
	if got.Joined != 1 {
		t.Fatalf("joined must stay 1 after duplicate, got %d", got.Joined)
	}
}

func TestJoin_NonActiveRejected(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	// cancelled squad
	g1 := mustCreate(t, s, CreateInput{Title: "c", ProductID: 1, RequiredCount: 3})
	if _, err := s.Cancel(ctx, g1.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Join(ctx, g1.ID, 7001); !errors.Is(err, ErrGroupbuyClosed) {
		t.Fatalf("join cancelled should ErrGroupbuyClosed, got %v", err)
	}
	// succeeded squad: already covered by FullRejected (over-quota fires first);
	// verify a *re-joining* member on a succeeded squad still gets AlreadyJoined.
	g2 := mustCreate(t, s, CreateInput{Title: "s", ProductID: 1, RequiredCount: 1})
	if _, _, err := s.Join(ctx, g2.ID, 7001); err != nil {
		t.Fatal(err)
	} // → success
	if _, _, err := s.Join(ctx, g2.ID, 7001); !errors.Is(err, ErrAlreadyJoined) {
		t.Fatalf("re-join succeeded should ErrAlreadyJoined, got %v", err)
	}
}

func TestCancel_ActiveToCancelled(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	g := mustCreate(t, s, CreateInput{Title: "t", ProductID: 1, RequiredCount: 3})
	out, err := s.Cancel(ctx, g.ID)
	if err != nil || out.Status != StatusCancelled {
		t.Fatalf("cancel failed: %v status=%s", err, out.Status)
	}
	// double cancel → not active
	if _, err := s.Cancel(ctx, g.ID); !errors.Is(err, ErrGroupbuyClosed) {
		t.Fatalf("cancel non-active should ErrGroupbuyClosed, got %v", err)
	}
}

func TestFinalizeExpired_ExpiredNotFullToFailed(t *testing.T) {
	clock := fixedNow
	s := newSvc(t, WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(50021)
	// 1h window, required 3, one member joins (not full)
	g, _ := s.Create(ctx, CreateInput{Title: "e", ProductID: 1, RequiredCount: 3, ValidHours: 1})
	s.Join(ctx, g.ID, 7001)

	// advance past EndAt
	clock = clock.Add(2 * time.Hour)
	n, err := s.FinalizeExpired(ctx, clock)
	if err != nil {
		t.Fatalf("FinalizeExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("expect 1 finalised, got %d", n)
	}
	got, _ := s.Get(ctx, g.ID)
	if got.Status != StatusFailed {
		t.Fatalf("expect failed, got %s", got.Status)
	}
	// joining a failed squad now → closed
	if _, _, err := s.Join(ctx, g.ID, 7002); !errors.Is(err, ErrGroupbuyClosed) {
		t.Fatalf("join failed squad should ErrGroupbuyClosed, got %v", err)
	}
}

func TestFinalizeExpired_SkipsNotExpiredAndFull(t *testing.T) {
	clock := fixedNow
	s := newSvc(t, WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(50021)
	// not-yet-expired squad
	live, _ := s.Create(ctx, CreateInput{Title: "live", ProductID: 1, RequiredCount: 3, ValidHours: 24})
	// succeeded (full) squad — must NOT be failed even though window is short
	full, _ := s.Create(ctx, CreateInput{Title: "full", ProductID: 1, RequiredCount: 1, ValidHours: 1})
	s.Join(ctx, full.ID, 7001) // → success

	clock = clock.Add(2 * time.Hour) // past the full squad's EndAt
	n, err := s.FinalizeExpired(ctx, clock)
	if err != nil {
		t.Fatalf("FinalizeExpired: %v", err)
	}
	if n != 0 {
		t.Fatalf("expect 0 finalised (live not expired, full is success), got %d", n)
	}
	liveGot, _ := s.Get(ctx, live.ID)
	if liveGot.Status != StatusActive {
		t.Errorf("live squad should stay active, got %s", liveGot.Status)
	}
	fullGot, _ := s.Get(ctx, full.ID)
	if fullGot.Status != StatusSuccess {
		t.Errorf("full squad should stay success, got %s", fullGot.Status)
	}
}

func TestFinalizeExpired_CrossTenantScoped(t *testing.T) {
	clock := fixedNow
	s := newSvc(t, WithNowFunc(func() time.Time { return clock }))
	ctxA := ctxWithTenant(50021)
	ctxB := ctxWithTenant(50022)
	// both tenants have an expired, unfilled squad
	gA, _ := s.Create(ctxA, CreateInput{Title: "a", ProductID: 1, RequiredCount: 3, ValidHours: 1})
	s.Create(ctxB, CreateInput{Title: "b", ProductID: 1, RequiredCount: 3, ValidHours: 1})

	clock = clock.Add(2 * time.Hour)
	n, err := s.FinalizeExpired(ctxA, clock)
	if err != nil {
		t.Fatalf("FinalizeExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("expect only tenant A finalised (1), got %d", n)
	}
	got, _ := s.Get(ctxA, gA.ID)
	if got.Status != StatusFailed {
		t.Fatalf("tenant A squad should be failed, got %s", got.Status)
	}
}

func TestListParticipants(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	g := mustCreate(t, s, CreateInput{Title: "t", ProductID: 1, RequiredCount: 3})
	s.Join(ctx, g.ID, 7001)
	s.Join(ctx, g.ID, 7002)
	list, err := s.ListParticipants(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("expect 2 participants, got %d", len(list))
	}
	// ordered by participant id ascending
	if list[0].MemberID != 7001 || list[1].MemberID != 7002 {
		t.Errorf("unexpected order: %+v", list)
	}
	// GetParticipant one that joined vs one that didn't
	if _, err := s.repo.GetParticipant(ctx, 50021, g.ID, 7001); err != nil {
		t.Errorf("GetParticipant joined: %v", err)
	}
	if _, err := s.repo.GetParticipant(ctx, 50021, g.ID, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetParticipant not-joined should ErrNotFound, got %v", err)
	}
}

func TestMarkPaid_JoinedToPaid(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	g := mustCreate(t, s, CreateInput{Title: "t", ProductID: 1, RequiredCount: 3})
	s.Join(ctx, g.ID, 7001)
	p, err := s.repo.MarkPaid(ctx, 50021, g.ID, 7001, fixedNow)
	if err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	if p.Status != PStatusPaid {
		t.Errorf("expect paid, got %s", p.Status)
	}
	// idempotent on already-paid
	p2, err := s.repo.MarkPaid(ctx, 50021, g.ID, 7001, fixedNow)
	if err != nil || p2.Status != PStatusPaid {
		t.Fatalf("idempotent MarkPaid failed: %v status=%s", err, p2.Status)
	}
	// unknown member → ErrNotFound
	if _, err := s.repo.MarkPaid(ctx, 50021, g.ID, 9999, fixedNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkPaid unknown member should ErrNotFound, got %v", err)
	}
}

func TestCrossTenant_Rejected(t *testing.T) {
	s := newSvc(t)
	ctxA := ctxWithTenant(50021)
	ctxB := ctxWithTenant(50022)
	g := mustCreate(t, s, CreateInput{Title: "t", ProductID: 1, RequiredCount: 3})
	s.Join(ctxA, g.ID, 7001)

	// tenant B cannot read tenant A's squad
	if _, err := s.Get(ctxB, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Get should ErrNotFound, got %v", err)
	}
	// tenant B cannot join tenant A's squad
	if _, _, err := s.Join(ctxB, g.ID, 7002); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Join should ErrNotFound, got %v", err)
	}
	// tenant B cannot list participants of tenant A's squad
	if _, err := s.ListParticipants(ctxB, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant ListParticipants should ErrNotFound, got %v", err)
	}
	// tenant B cannot cancel tenant A's squad
	if _, err := s.Cancel(ctxB, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Cancel should ErrNotFound, got %v", err)
	}
	// tenant B's active list must not include tenant A's squad
	listB, err := s.ListActive(ctxB, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listB) != 0 {
		t.Fatalf("tenant B active list should be empty, got %d", len(listB))
	}
	listA, _ := s.ListActive(ctxA, 0)
	if len(listA) != 1 || listA[0].ID != g.ID {
		t.Fatalf("tenant A active list should contain the squad, got %v", listA)
	}
}

func TestJoin_MemberLookuper(t *testing.T) {
	ctx := ctxWithTenant(50021)
	// member does not exist → reject
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: false}))
	g := mustCreate(t, s, CreateInput{Title: "t", ProductID: 1, RequiredCount: 3})
	if _, _, err := s.Join(ctx, g.ID, 7001); err == nil {
		t.Fatal("non-existent member should reject")
	}
	// member exists → ok
	s2 := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	g2 := mustCreate(t, s2, CreateInput{Title: "t", ProductID: 1, RequiredCount: 3})
	if _, _, err := s2.Join(ctx, g2.ID, 7001); err != nil {
		t.Fatalf("existing member join failed: %v", err)
	}
}

func TestNoTenant_Rejected(t *testing.T) {
	s := newSvc(t)
	// no tenant in ctx → every method refuses
	if _, err := s.Create(context.Background(), CreateInput{Title: "t", ProductID: 1}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("Create without tenant should ErrNoTenant, got %v", err)
	}
	if _, err := s.Get(context.Background(), 1); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("Get without tenant should ErrNoTenant, got %v", err)
	}
	if _, _, err := s.Join(context.Background(), 1, 1); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("Join without tenant should ErrNoTenant, got %v", err)
	}
	if _, err := s.FinalizeExpired(context.Background(), fixedNow); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("FinalizeExpired without tenant should ErrNoTenant, got %v", err)
	}
}

// TestJoin_ConcurrentDoesNotOvershoot stresses the atomic critical section under
// -race: 50 distinct members race for a 3-seat squad. Exactly 3 must succeed and
// the persisted Joined/Status must be 3/success with no over-shoot.
func TestJoin_ConcurrentDoesNotOvershoot(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	g := mustCreate(t, s, CreateInput{Title: "race", ProductID: 1, RequiredCount: 3})

	const N = 50
	var wg sync.WaitGroup
	var succ int64
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			memberID := int64(7000 + i)
			_, _, err := s.Join(ctx, g.ID, memberID)
			if err == nil {
				atomic.AddInt64(&succ, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if succ != 3 {
		t.Fatalf("expected exactly 3 successful joins, got %d", succ)
	}
	got, _ := s.Get(ctx, g.ID)
	if got.Joined != 3 {
		t.Fatalf("persisted Joined must be 3, got %d", got.Joined)
	}
	if got.Status != StatusSuccess {
		t.Fatalf("persisted Status must be success, got %s", got.Status)
	}
	// all 3 participants are distinct
	list, _ := s.ListParticipants(ctx, g.ID)
	if len(list) != 3 {
		t.Fatalf("expected 3 participants persisted, got %d", len(list))
	}
}

// TestCreate_WithStrategyID verifies the groupbuy→strategy linkage: a
// CreateInput.StrategyID is persisted onto Groupbuy.StrategyID so the
// groupbuy_success 归因 metric (CONTRACT §14.3.1) can be computed.
func TestCreate_WithStrategyID(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	g, err := s.Create(ctx, CreateInput{
		Title:      "归因团",
		ProductID:  7,
		StrategyID: 9001,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if g.StrategyID != 9001 {
		t.Errorf("expect StrategyID=9001, got %d", g.StrategyID)
	}
	// persisted state matches
	got, _ := s.Get(ctx, g.ID)
	if got.StrategyID != 9001 {
		t.Errorf("persisted StrategyID wrong: got %d", got.StrategyID)
	}
	// default (omitted) → 0 = 非AI来源
	g0, _ := s.Create(ctx, CreateInput{Title: "no-strategy", ProductID: 1})
	if g0.StrategyID != 0 {
		t.Errorf("omitted StrategyID should default 0, got %d", g0.StrategyID)
	}
}

// TestStatsByStrategy builds three squads under one strategy in three states
// (active / success / failed) over a single shared repo, then asserts
// started=3, succeeded=1. Also covers strategyID<=0 short-circuit and unknown
// strategy returns empty.
func TestStatsByStrategy(t *testing.T) {
	repo := NewMemoryGroupbuyRepo()
	ctx := ctxWithTenant(50021)

	// one shared clock; FinalizeExpired advances it to expire the failed squad
	clock := fixedNow
	svc := New(repo, WithNowFunc(func() time.Time { return clock }))

	// active squad: 24h window, required 3, nobody joins → stays active
	gActive, err := svc.Create(ctx, CreateInput{
		Title: "active", ProductID: 1, RequiredCount: 3, ValidHours: 24, StrategyID: 9001,
	})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	// success squad: required 1, one join flips to success
	gSuccess, err := svc.Create(ctx, CreateInput{
		Title: "success", ProductID: 1, RequiredCount: 1, StrategyID: 9001,
	})
	if err != nil {
		t.Fatalf("create success: %v", err)
	}
	if _, _, err := svc.Join(ctx, gSuccess.ID, 7001); err != nil {
		t.Fatalf("join success squad: %v", err)
	}
	// failed squad: 1h window, required 3, one join, then expire past window
	gFailed, err := svc.Create(ctx, CreateInput{
		Title: "failed", ProductID: 1, RequiredCount: 3, ValidHours: 1, StrategyID: 9001,
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if _, _, err := svc.Join(ctx, gFailed.ID, 7002); err != nil {
		t.Fatalf("join failed squad: %v", err)
	}
	clock = clock.Add(2 * time.Hour) // past gFailed.EndAt (1h); gActive (24h) still live
	if n, err := svc.FinalizeExpired(ctx, clock); err != nil || n != 1 {
		t.Fatalf("FinalizeExpired expect 1 finalised, got %d (%v)", n, err)
	}

	// sanity: three distinct states
	gotA, _ := svc.Get(ctx, gActive.ID)
	gotS, _ := svc.Get(ctx, gSuccess.ID)
	gotF, _ := svc.Get(ctx, gFailed.ID)
	if gotA.Status != StatusActive {
		t.Fatalf("active squad should stay active, got %s", gotA.Status)
	}
	if gotS.Status != StatusSuccess {
		t.Fatalf("success squad should be success, got %s", gotS.Status)
	}
	if gotF.Status != StatusFailed {
		t.Fatalf("failed squad should be failed, got %s", gotF.Status)
	}

	// the actual metric
	started, succeeded, err := svc.StatsByStrategy(ctx, 9001)
	if err != nil {
		t.Fatalf("StatsByStrategy: %v", err)
	}
	if started != 3 {
		t.Errorf("expect started=3 (active+success+failed), got %d", started)
	}
	if succeeded != 1 {
		t.Errorf("expect succeeded=1, got %d", succeeded)
	}

	// strategyID<=0 → (0,0,nil) short-circuit, no repo read
	if st, su, err := svc.StatsByStrategy(ctx, 0); err != nil || st != 0 || su != 0 {
		t.Errorf("strategyID<=0 should (0,0,nil), got (%d,%d,%v)", st, su, err)
	}
	// unknown strategyID → empty (started=0)
	if st, su, err := svc.StatsByStrategy(ctx, 9999); err != nil || st != 0 || su != 0 {
		t.Errorf("unknown strategy should (0,0,nil), got (%d,%d,%v)", st, su, err)
	}
}

// TestListByStrategy_CrossTenant asserts a strategy's groupbuys in tenant A are
// invisible to tenant B (ListByStrategy is tenant-scoped), and strategyID<=0
// returns an empty slice.
func TestListByStrategy_CrossTenant(t *testing.T) {
	repo := NewMemoryGroupbuyRepo()
	ctxA := ctxWithTenant(50021)
	ctxB := ctxWithTenant(50022)
	svcA := New(repo, WithNowFunc(func() time.Time { return fixedNow }))

	gA, _ := svcA.Create(ctxA, CreateInput{Title: "a", ProductID: 1, StrategyID: 9001})
	// tenant A owns strategy 9001's groupbuy; tenant B must not see it
	listA, err := repo.ListByStrategy(ctxA, 50021, 9001)
	if err != nil {
		t.Fatalf("ListByStrategy A: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != gA.ID {
		t.Fatalf("tenant A should see 1 groupbuy for strategy 9001, got %v", listA)
	}
	listB, err := repo.ListByStrategy(ctxB, 50022, 9001)
	if err != nil {
		t.Fatalf("ListByStrategy B: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("tenant B must not see tenant A's groupbuys, got %d", len(listB))
	}
	// strategyID<=0 → empty (non-nil) slice
	empty, err := repo.ListByStrategy(ctxA, 50021, 0)
	if err != nil {
		t.Fatalf("ListByStrategy zero: %v", err)
	}
	if empty == nil {
		t.Fatal("expect non-nil empty slice for strategyID<=0")
	}
	if len(empty) != 0 {
		t.Fatalf("strategyID<=0 should yield empty, got %d", len(empty))
	}
}

// --- fakes ---

type fakeLookuper struct{ exists bool }

func (f fakeLookuper) Exists(_ context.Context, _, _ int64) (bool, error) {
	return f.exists, nil
}
