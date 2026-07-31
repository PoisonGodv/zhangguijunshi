package membership

// Recharge + regional-cap tests for M4-S1 BE-B-12 (stored-value top-up +
// anti-two-settlement bookkeeping + regional per-card compliance). These are
// purely additive: the existing Join/IssueCode/Redeem/ListMembers tests in
// service_test.go are untouched. The rig/ctxFor/mustTenant helpers defined in
// service_test.go are reused (same package).
//
// Coverage (DoD §15.6 A4): recharge-tier gift calc (table), cap rejection with
// balance unchanged, cap=0 means uncapped, cap boundary, cross-tenant reject,
// blocked-member reject, non-positive amount reject, no-tenant reject, recharge
// audit log, SetRegionCap success + cross-tenant + no-tenant, RegionCapCents
// round-trip through UpsertAccount/GetAccount, and concurrent recharge -race
// safety.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// --- GiftForAmount pure function ---

func TestGiftForAmount_Tiers(t *testing.T) {
	cases := []struct {
		amount int64
		gift   int64
	}{
		{0, 0},          // nothing recharged
		{1, 0},          // below every tier
		{19999, 0},      // just below the ¥200 tier
		{20000, 2000},   // 充200送20 (exact tier)
		{30000, 2000},   // between ¥200 and ¥500 → still ¥20 tier
		{49999, 2000},   // just below ¥500
		{50000, 8000},   // 充500送80 (exact tier)
		{99999, 8000},   // just below ¥1000
		{100000, 20000}, // 充1000送200 (exact tier)
		{250000, 20000}, // above the top tier → top gift
	}
	for _, c := range cases {
		if got := GiftForAmount(c.amount); got != c.gift {
			t.Errorf("GiftForAmount(%d) = %d, want %d", c.amount, got, c.gift)
		}
	}
}

func TestGiftForAmount_DoesNotMutateSeed(t *testing.T) {
	before := append([]RechargeTier(nil), rechargeTiers...)
	_ = GiftForAmount(999999999)
	if len(rechargeTiers) != len(before) {
		t.Fatalf("rechargeTiers len changed: %d vs %d", len(rechargeTiers), len(before))
	}
	for i := range before {
		if rechargeTiers[i] != before[i] {
			t.Errorf("rechargeTiers mutated: %+v vs %+v", rechargeTiers[i], before[i])
		}
	}
}

// --- Recharge success paths ---

func TestRecharge_CreditsBalanceAndGift(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-rc1"})

	res, err := r.svc.Recharge(ctx, RechargeInput{
		MemberID:     m.ID,
		AmountCents:  20000, // ¥200 → gift ¥20
		RefPaymentID: 777,
		OperatorID:   7,
	})
	if err != nil {
		t.Fatalf("Recharge: %v", err)
	}
	if res.MemberID != m.ID {
		t.Errorf("MemberID = %d, want %d", res.MemberID, m.ID)
	}
	if res.RechargedCents != 20000 {
		t.Errorf("RechargedCents = %d, want 20000", res.RechargedCents)
	}
	if res.GiftedCents != 2000 {
		t.Errorf("GiftedCents = %d, want 2000", res.GiftedCents)
	}
	if res.BalanceCents != 20000 {
		t.Errorf("BalanceCents = %d, want 20000", res.BalanceCents)
	}
	if res.GiftCents != 2000 {
		t.Errorf("GiftCents = %d, want 2000", res.GiftCents)
	}

	// Persisted account reflects the credit.
	acc, _ := r.repo.GetAccount(ctx, 100, m.ID)
	if acc.BalanceCents != 20000 || acc.GiftCents != 2000 {
		t.Errorf("account balance/gift = %d/%d, want 20000/2000", acc.BalanceCents, acc.GiftCents)
	}

	// One recharge audit row; typ=recharge, amount=20000, balanceAfter=20000.
	if n := r.repo.balanceLogCount(100, m.ID); n != 1 {
		t.Errorf("balance flows = %d, want 1", n)
	}
	typ, amt, ok := r.repo.lastBalanceFlow(100, m.ID)
	if !ok || typ != "recharge" || amt != 20000 {
		t.Errorf("last balance flow = %q/%d ok=%v, want recharge/20000", typ, amt, ok)
	}
	if ba, ok := r.repo.lastBalanceAfter(100, m.ID); !ok || ba != 20000 {
		t.Errorf("balance_after = %d ok=%v, want 20000", ba, ok)
	}
}

func TestRecharge_BestTierGift(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	mA, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-rc2"})
	mB, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-rc3"})
	mC, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-rc4"})

	// ¥500 → ¥80; ¥1000 → ¥200; a second ¥500 top-up stacks onto the balance.
	if res, err := r.svc.Recharge(ctx, RechargeInput{MemberID: mA.ID, AmountCents: 50000}); err != nil || res.GiftedCents != 8000 {
		t.Errorf("50000 recharge: gift=%d err=%v, want 8000", res.GiftedCents, err)
	}
	if res, err := r.svc.Recharge(ctx, RechargeInput{MemberID: mB.ID, AmountCents: 100000}); err != nil || res.GiftedCents != 20000 {
		t.Errorf("100000 recharge: gift=%d err=%v, want 20000", res.GiftedCents, err)
	}
	if _, err := r.svc.Recharge(ctx, RechargeInput{MemberID: mA.ID, AmountCents: 50000}); err != nil {
		t.Fatalf("second 50000 recharge: %v", err)
	}
	accA, _ := r.repo.GetAccount(ctx, 100, mA.ID)
	if accA.BalanceCents != 100000 || accA.GiftCents != 16000 {
		t.Errorf("stacked balance/gift = %d/%d, want 100000/16000", accA.BalanceCents, accA.GiftCents)
	}
	_ = mC
}

func TestRecharge_BelowEveryTierEarnsNoGift(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-rc5"})

	res, err := r.svc.Recharge(ctx, RechargeInput{MemberID: m.ID, AmountCents: 10000}) // ¥100, below ¥200 tier
	if err != nil {
		t.Fatalf("Recharge: %v", err)
	}
	if res.GiftedCents != 0 || res.GiftCents != 0 {
		t.Errorf("gift = %d/%d, want 0/0 (below every tier)", res.GiftedCents, res.GiftCents)
	}
	if res.BalanceCents != 10000 {
		t.Errorf("BalanceCents = %d, want 10000", res.BalanceCents)
	}
}

// --- Recharge cap (regional compliance) ---

func TestRecharge_CapRejectsAndLeavesBalanceUnchanged(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-cap1"})

	// Cap at ¥210 (21000 cents). A ¥200 recharge + ¥20 gift = 22000 > 21000 → reject.
	if err := r.svc.SetRegionCap(ctx, m.ID, 21000); err != nil {
		t.Fatalf("SetRegionCap: %v", err)
	}
	_, err := r.svc.Recharge(ctx, RechargeInput{MemberID: m.ID, AmountCents: 20000})
	if !errors.Is(err, errcode.ErrBalanceOver) {
		t.Fatalf("over-cap err = %v, want errcode.ErrBalanceOver", err)
	}

	// Balance/gift untouched, no audit row written.
	acc, _ := r.repo.GetAccount(ctx, 100, m.ID)
	if acc.BalanceCents != 0 || acc.GiftCents != 0 {
		t.Errorf("balance/gift = %d/%d after rejected recharge, want 0/0", acc.BalanceCents, acc.GiftCents)
	}
	if n := r.repo.balanceLogCount(100, m.ID); n != 0 {
		t.Errorf("balance flows = %d after rejected recharge, want 0", n)
	}
}

func TestRecharge_CapBoundaryAllowsExactFit(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-cap2"})

	// Cap at ¥220 (22000). balance(0)+20000+2000 = 22000 <= 22000 → allowed.
	if err := r.svc.SetRegionCap(ctx, m.ID, 22000); err != nil {
		t.Fatalf("SetRegionCap: %v", err)
	}
	res, err := r.svc.Recharge(ctx, RechargeInput{MemberID: m.ID, AmountCents: 20000})
	if err != nil {
		t.Fatalf("boundary recharge: %v", err)
	}
	if res.BalanceCents != 20000 || res.GiftCents != 2000 {
		t.Errorf("balance/gift = %d/%d, want 20000/2000", res.BalanceCents, res.GiftCents)
	}
}

func TestRecharge_CapZeroMeansUnlimited(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-cap3"})

	// Join opens the account with RegionCapCents=0 (uncapped). A huge recharge
	// must succeed without any cap rejection.
	res, err := r.svc.Recharge(ctx, RechargeInput{MemberID: m.ID, AmountCents: 100000})
	if err != nil {
		t.Fatalf("uncapped recharge: %v", err)
	}
	if res.BalanceCents != 100000 {
		t.Errorf("BalanceCents = %d, want 100000", res.BalanceCents)
	}
	acc, _ := r.repo.GetAccount(ctx, 100, m.ID)
	if acc.RegionCapCents != 0 {
		t.Errorf("RegionCapCents = %d, want 0 (uncapped)", acc.RegionCapCents)
	}
}

// --- Recharge rejection paths ---

func TestRecharge_CrossTenantRejected(t *testing.T) {
	r := newRig()
	ctxA := ctxFor(100, 10, 7)
	ctxB := ctxFor(200, 20, 8)
	ma, _ := r.svc.Join(ctxA, JoinInput{StoreID: 10, PhoneHash: "ph-xt1"})

	// Tenant B cannot top up tenant A's member: GetMember → ErrNotFound.
	_, err := r.svc.Recharge(ctxB, RechargeInput{MemberID: ma.ID, AmountCents: 20000})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant recharge err = %v, want ErrNotFound", err)
	}
	// Tenant A's balance untouched.
	acc, _ := r.repo.GetAccount(ctxA, 100, ma.ID)
	if acc.BalanceCents != 0 {
		t.Errorf("tenant A balance = %d after cross-tenant attempt, want 0", acc.BalanceCents)
	}
}

func TestRecharge_BlockedMemberRejected(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-blk2"})
	stored, _ := r.repo.GetMember(ctx, 100, m.ID)
	stored.Status = memberStatusBlocked
	if _, err := r.repo.UpdateMember(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Recharge(ctx, RechargeInput{MemberID: m.ID, AmountCents: 20000}); !errors.Is(err, ErrMemberBlocked) {
		t.Fatalf("blocked recharge err = %v, want ErrMemberBlocked", err)
	}
}

func TestRecharge_NonPositiveAmountRejected(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-neg"})
	for _, amt := range []int64{0, -100} {
		if _, err := r.svc.Recharge(ctx, RechargeInput{MemberID: m.ID, AmountCents: amt}); err != errcode.ErrParam {
			t.Errorf("amount=%d err = %v, want errcode.ErrParam", amt, err)
		}
	}
}

func TestRecharge_UnknownMemberRejected(t *testing.T) {
	r := newRig()
	if _, err := r.svc.Recharge(ctxFor(100, 10, 7), RechargeInput{MemberID: 999999, AmountCents: 20000}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown member recharge err = %v, want ErrNotFound", err)
	}
}

func TestRecharge_NoTenantRejected(t *testing.T) {
	r := newRig()
	if _, err := r.svc.Recharge(context.Background(), RechargeInput{MemberID: 1, AmountCents: 20000}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant recharge err = %v, want ErrNoTenant", err)
	}
}

// --- SetRegionCap ---

func TestSetRegionCap_Success(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-src1"})

	if err := r.svc.SetRegionCap(ctx, m.ID, 50000); err != nil {
		t.Fatalf("SetRegionCap: %v", err)
	}
	acc, _ := r.repo.GetAccount(ctx, 100, m.ID)
	if acc.RegionCapCents != 50000 {
		t.Errorf("RegionCapCents = %d, want 50000", acc.RegionCapCents)
	}
}

func TestSetRegionCap_CrossTenantRejected(t *testing.T) {
	r := newRig()
	ctxA := ctxFor(100, 10, 7)
	ctxB := ctxFor(200, 20, 8)
	ma, _ := r.svc.Join(ctxA, JoinInput{StoreID: 10, PhoneHash: "ph-src2"})

	// Tenant B cannot set the cap on tenant A's account.
	if err := r.svc.SetRegionCap(ctxB, ma.ID, 50000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant SetRegionCap err = %v, want ErrNotFound", err)
	}
	// Tenant A's cap remains the default 0.
	acc, _ := r.repo.GetAccount(ctxA, 100, ma.ID)
	if acc.RegionCapCents != 0 {
		t.Errorf("tenant A cap changed by cross-tenant call: %d", acc.RegionCapCents)
	}
}

func TestSetRegionCap_UnknownMemberRejected(t *testing.T) {
	r := newRig()
	if err := r.svc.SetRegionCap(ctxFor(100, 10, 7), 999999, 50000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown SetRegionCap err = %v, want ErrNotFound", err)
	}
}

func TestSetRegionCap_NoTenantRejected(t *testing.T) {
	r := newRig()
	if err := r.svc.SetRegionCap(context.Background(), 1, 50000); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant SetRegionCap err = %v, want ErrNoTenant", err)
	}
}

// --- RegionCapCents field round-trip ---

func TestBalanceAccount_RegionCapCentsRoundTrip(t *testing.T) {
	repo := NewMemoryMemberRepo()
	ctx := context.Background()
	// Upsert an account with a non-zero cap directly.
	if _, err := repo.UpsertAccount(ctx, &BalanceAccount{
		TenantID:       100,
		MemberID:       9001,
		BalanceCents:   0,
		GiftCents:      0,
		RegionCapCents: 75000,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetAccount(ctx, 100, 9001)
	if err != nil {
		t.Fatal(err)
	}
	if got.RegionCapCents != 75000 {
		t.Errorf("RegionCapCents round-trip = %d, want 75000", got.RegionCapCents)
	}
	// Cross-tenant GetAccount must not leak the account.
	if _, err := repo.GetAccount(ctx, 200, 9001); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant GetAccount err = %v, want ErrNotFound", err)
	}
}

// --- -race safety: concurrent recharge ---

func TestRecharge_ConcurrentSafe(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-race"})

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			// ¥200 tier; no cap (uncapped). Each call credits 20000 + 2000 gift.
			_, _ = r.svc.Recharge(ctx, RechargeInput{MemberID: m.ID, AmountCents: 20000})
		}()
	}
	wg.Wait()

	acc, _ := r.repo.GetAccount(ctx, 100, m.ID)
	wantBalance := int64(n) * 20000
	wantGift := int64(n) * 2000
	if acc.BalanceCents != wantBalance {
		t.Errorf("balance after %d concurrent recharges = %d, want %d", n, acc.BalanceCents, wantBalance)
	}
	if acc.GiftCents != wantGift {
		t.Errorf("gift after %d concurrent recharges = %d, want %d", n, acc.GiftCents, wantGift)
	}
	if n2 := r.repo.balanceLogCount(100, m.ID); n2 != n {
		t.Errorf("recharge log rows = %d, want %d (one per successful recharge)", n2, n)
	}
}

// --- rechargeAtomic directly: missing account ---

func TestRechargeAtomic_MissingAccount(t *testing.T) {
	r := newRig()
	// No Join → no account for member 9999 under tenant 100.
	if _, err := r.repo.rechargeAtomic(ctx, 100, 9999, baseNow, 20000, 2000, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("rechargeAtomic missing account err = %v, want ErrNotFound", err)
	}
}

// rechargeAtomic cap rejection returns errcode.ErrBalanceOver and is observed by
// the service even when invoked directly (defence-in-depth: the repo enforces
// the cap independent of the service).
func TestRechargeAtomic_CapBreachesReturnErrBalanceOver(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-ra-cap"})
	if err := r.svc.SetRegionCap(ctx, m.ID, 10000); err != nil {
		t.Fatal(err)
	}
	// 0 + 20000 + 2000 = 22000 > 10000 → reject.
	if _, err := r.repo.rechargeAtomic(ctx, 100, m.ID, baseNow, 20000, 2000, 0, 0); !errors.Is(err, errcode.ErrBalanceOver) {
		t.Errorf("rechargeAtomic cap err = %v, want errcode.ErrBalanceOver", err)
	}
}
