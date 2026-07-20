package membership

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// baseNow is the deterministic clock anchor; coderNow is advanced by tests to
// push a code past its TTL.
var baseNow = time.Date(2026, 7, 20, 15, 4, 5, 0, time.Local)

func ctxFor(tenantID, storeID, userID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID,
		StoreID:  storeID,
		UserID:   userID,
		Source:   multitenancy.SourceMerchant,
	})
}

// rig bundles a service, its repo, and a controllable coder clock.
type rig struct {
	svc      *Service
	repo     *MemoryMemberRepo
	coderNow *time.Time
}

func newRig() *rig {
	coderNow := baseNow
	r := &rig{coderNow: &coderNow}
	repo := NewMemoryMemberRepo()
	coder := NewMemoryMemberCoder([]byte("test-secret"),
		WithCoderNowFunc(func() time.Time { return *r.coderNow }))
	r.repo = repo
	r.svc = New(repo, coder, WithNowFunc(func() time.Time { return baseNow }))
	return r
}

// advance pushes the coder clock forward (to expire codes).
func (r *rig) advance(d time.Duration) { *r.coderNow = r.coderNow.Add(d) }

// fund tops up a member's stored balance directly via the repo (simulating a
// recharge that M1-S2 does not expose as a service method).
func (r *rig) fund(t *testing.T, ctx context.Context, memberID, cents int64) {
	t.Helper()
	acc, err := r.repo.GetAccount(ctx, mustTenant(t, ctx), memberID)
	if err != nil {
		t.Fatalf("GetAccount for fund: %v", err)
	}
	acc.BalanceCents = cents
	if _, err := r.repo.UpsertAccount(ctx, acc); err != nil {
		t.Fatalf("UpsertAccount for fund: %v", err)
	}
}

func mustTenant(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	id, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		t.Fatalf("no tenant in ctx: %v", err)
	}
	return id
}

// --- Join ---

func TestJoin_CreatesMemberAndAccount(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)

	m, err := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-1", Name: "张三"})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if m.ID <= 9000 {
		t.Errorf("member id = %d, want > 9000", m.ID)
	}
	if m.TenantID != 100 || m.StoreID != 10 {
		t.Errorf("tenant/store = %d/%d", m.TenantID, m.StoreID)
	}
	if m.Level != memberLevelNormal || m.Points != 0 || m.Status != memberStatusActive {
		t.Errorf("level/points/status = %q/%d/%q", m.Level, m.Points, m.Status)
	}
	if !m.CreatedAt.Equal(baseNow) {
		t.Errorf("CreatedAt = %v, want %v", m.CreatedAt, baseNow)
	}

	// Account auto-opened with zero balance.
	acc, err := r.repo.GetAccount(ctx, 100, m.ID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if acc.ID <= 9500 || acc.BalanceCents != 0 || acc.MemberID != m.ID || acc.TenantID != 100 {
		t.Errorf("account = %+v", acc)
	}
}

func TestJoin_RequiresPhoneOrOpenID(t *testing.T) {
	r := newRig()
	if _, err := r.svc.Join(ctxFor(100, 10, 7), JoinInput{StoreID: 10}); err != errcode.ErrParam {
		t.Fatalf("empty identity err = %v, want ErrParam", err)
	}
}

func TestJoin_IdempotentByPhone(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)

	first, err := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-dup", Name: "第一次"})
	if err != nil {
		t.Fatal(err)
	}
	// Same phone again (different name) -> reuse, not duplicate.
	second, err := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-dup", Name: "第二次"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("repeat Join created new member: first=%d second=%d", first.ID, second.ID)
	}
	if second.Name != "第一次" {
		t.Errorf("idempotent Join should return the ORIGINAL member, name = %q", second.Name)
	}
	// Exactly one account exists for the member.
	if _, err := r.repo.GetAccount(ctx, 100, first.ID); err != nil {
		t.Fatalf("account missing after idempotent Join: %v", err)
	}
}

func TestJoin_IdempotentByOpenID(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	a, err := r.svc.Join(ctx, JoinInput{StoreID: 10, WxOpenID: "wx-1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.svc.Join(ctx, JoinInput{StoreID: 10, WxOpenID: "wx-1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Errorf("repeat Join by openid created new member: %d vs %d", a.ID, b.ID)
	}
}

func TestJoin_SamePhoneDifferentTenantsAreDistinct(t *testing.T) {
	r := newRig()
	ma, err := r.svc.Join(ctxFor(100, 10, 7), JoinInput{StoreID: 10, PhoneHash: "shared-phone"})
	if err != nil {
		t.Fatal(err)
	}
	mb, err := r.svc.Join(ctxFor(200, 20, 8), JoinInput{StoreID: 20, PhoneHash: "shared-phone"})
	if err != nil {
		t.Fatal(err)
	}
	if ma.ID == mb.ID {
		t.Errorf("same phone across tenants must be distinct members, both id=%d", ma.ID)
	}
}

func TestJoin_NoTenantRejected(t *testing.T) {
	r := newRig()
	if _, err := r.svc.Join(context.Background(), JoinInput{PhoneHash: "ph"}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant Join err = %v, want ErrNoTenant", err)
	}
}

// --- GetMember + cross-tenant ---

func TestGetMember_CrossTenantIsolated(t *testing.T) {
	r := newRig()
	m, err := r.svc.Join(ctxFor(100, 10, 7), JoinInput{StoreID: 10, PhoneHash: "ph-ct"})
	if err != nil {
		t.Fatal(err)
	}
	// Same tenant reads it.
	if _, err := r.svc.GetMember(ctxFor(100, 10, 7), m.ID); err != nil {
		t.Fatalf("same-tenant GetMember: %v", err)
	}
	// Tenant B must NOT see tenant A's member.
	if _, err := r.svc.GetMember(ctxFor(200, 20, 8), m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant GetMember err = %v, want ErrNotFound", err)
	}
}

// --- IssueCode ---

func TestIssueCode_ActiveMember(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-ic"})
	code, err := r.svc.IssueCode(ctx, m.ID)
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	if code == "" {
		t.Fatal("empty code")
	}
}

func TestIssueCode_BlockedMemberRejected(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-blk"})
	// Block the member directly.
	stored, err := r.repo.GetMember(ctx, 100, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Status = memberStatusBlocked
	if _, err := r.repo.UpdateMember(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.IssueCode(ctx, m.ID); !errors.Is(err, ErrMemberBlocked) {
		t.Fatalf("blocked IssueCode err = %v, want ErrMemberBlocked", err)
	}
}

func TestIssueCode_UnknownMemberRejected(t *testing.T) {
	r := newRig()
	if _, err := r.svc.IssueCode(ctxFor(100, 10, 7), 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown IssueCode err = %v, want ErrNotFound", err)
	}
}

// --- Redeem: success paths ---

func TestRedeem_EarnAndConsume(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-rc"})
	r.fund(t, ctx, m.ID, 1000) // ¥10.00

	code, err := r.svc.IssueCode(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.Redeem(ctx, RedeemInput{
		StoreID: 10, OperatorID: 7, RefOrderID: 555,
		Code: code, ConsumeCents: 300, EarnPoints: 30,
	})
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if res.MemberID != m.ID {
		t.Errorf("MemberID = %d, want %d", res.MemberID, m.ID)
	}
	if res.Points != 30 {
		t.Errorf("Points = %d, want 30", res.Points)
	}
	if res.ConsumedCents != 300 {
		t.Errorf("ConsumedCents = %d, want 300", res.ConsumedCents)
	}
	if res.BalanceCents != 700 {
		t.Errorf("BalanceCents = %d, want 700 (1000-300)", res.BalanceCents)
	}

	// Member state updated.
	got, _ := r.svc.GetMember(ctx, m.ID)
	if got.Points != 30 {
		t.Errorf("member.Points = %d, want 30", got.Points)
	}
	if got.LastVisitAt == nil || !got.LastVisitAt.Equal(baseNow) {
		t.Errorf("LastVisitAt = %v, want %v", got.LastVisitAt, baseNow)
	}

	// Audit flows: one earn (points_after=30), one consume (balance_after=700).
	if n := r.repo.pointsLogCount(100, m.ID); n != 1 {
		t.Errorf("points flows = %d, want 1", n)
	}
	if pa, ok := r.repo.lastPointsAfter(100, m.ID); !ok || pa != 30 {
		t.Errorf("points_after = %d ok=%v, want 30", pa, ok)
	}
	if n := r.repo.balanceLogCount(100, m.ID); n != 1 {
		t.Errorf("balance flows = %d, want 1", n)
	}
	if ba, ok := r.repo.lastBalanceAfter(100, m.ID); !ok || ba != 700 {
		t.Errorf("balance_after = %d ok=%v, want 700", ba, ok)
	}
}

func TestRedeem_EarnOnlyReportsBalance(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-eo"})
	r.fund(t, ctx, m.ID, 500)

	code, _ := r.svc.IssueCode(ctx, m.ID)
	res, err := r.svc.Redeem(ctx, RedeemInput{Code: code, EarnPoints: 12})
	if err != nil {
		t.Fatal(err)
	}
	if res.Points != 12 || res.ConsumedCents != 0 {
		t.Errorf("Points/Consumed = %d/%d, want 12/0", res.Points, res.ConsumedCents)
	}
	if res.BalanceCents != 500 {
		t.Errorf("BalanceCents = %d, want 500 (untouched)", res.BalanceCents)
	}
	// Only an earn flow, no consume flow.
	if n := r.repo.balanceLogCount(100, m.ID); n != 0 {
		t.Errorf("balance flows = %d, want 0", n)
	}
	if n := r.repo.pointsLogCount(100, m.ID); n != 1 {
		t.Errorf("points flows = %d, want 1", n)
	}
}

func TestRedeem_ConsumeOnlyNoPoints(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-co"})
	r.fund(t, ctx, m.ID, 800)

	code, _ := r.svc.IssueCode(ctx, m.ID)
	res, err := r.svc.Redeem(ctx, RedeemInput{Code: code, ConsumeCents: 800})
	if err != nil {
		t.Fatal(err)
	}
	if res.BalanceCents != 0 || res.Points != 0 {
		t.Errorf("Balance/Points = %d/%d, want 0/0", res.BalanceCents, res.Points)
	}
	if n := r.repo.pointsLogCount(100, m.ID); n != 0 {
		t.Errorf("points flows = %d, want 0", n)
	}
}

// --- Redeem: rejection paths ---

func TestRedeem_ExpiredCodeRejected(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-exp"})
	r.fund(t, ctx, m.ID, 1000)

	code, _ := r.svc.IssueCode(ctx, m.ID)
	r.advance(61 * time.Second) // beyond the 60s TTL

	if _, err := r.svc.Redeem(ctx, RedeemInput{Code: code, EarnPoints: 10}); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("expired code err = %v, want ErrCodeInvalid", err)
	}
	// No points accrued.
	if got, _ := r.svc.GetMember(ctx, m.ID); got.Points != 0 {
		t.Errorf("Points = %d after expired redeem, want 0", got.Points)
	}
}

func TestRedeem_ForgedCodeRejected(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-fg"})

	forged := []string{
		"100.9001.9999999999.deadbeef", // never issued
		"not-a-code",                   // malformed
		"",                             // empty -> ErrParam actually
		"100.9001.notanum.aa",          // bad expiry
	}
	for _, c := range forged[:2] {
		if _, err := r.svc.Redeem(ctx, RedeemInput{Code: c, EarnPoints: 5}); !errors.Is(err, ErrCodeInvalid) {
			t.Errorf("forged %q err = %v, want ErrCodeInvalid", c, err)
		}
	}
	if _, err := r.svc.Redeem(ctx, RedeemInput{Code: forged[3], EarnPoints: 5}); !errors.Is(err, ErrCodeInvalid) {
		t.Errorf("forged %q err = %v, want ErrCodeInvalid", forged[3], err)
	}
	_ = m
}

func TestRedeem_TamperedMemberIDRejected(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-tmp"})
	code, _ := r.svc.IssueCode(ctx, m.ID)

	// Take the real code and try to redeem it under a DIFFERENT member id by
	// editing the payload — signature/registry mismatch must reject.
	// (The registry maps the exact code string; a modified payload is a new,
	// never-issued string -> ErrCodeInvalid.)
	tampered := code[:len(code)-2] + "00"
	if _, err := r.svc.Redeem(ctx, RedeemInput{Code: tampered, EarnPoints: 5}); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("tampered code err = %v, want ErrCodeInvalid", err)
	}
}

func TestRedeem_InsufficientBalance(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-ins"})
	r.fund(t, ctx, m.ID, 100)

	code, _ := r.svc.IssueCode(ctx, m.ID)
	_, err := r.svc.Redeem(ctx, RedeemInput{Code: code, ConsumeCents: 500})
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("overdraw err = %v, want ErrInsufficientBalance", err)
	}
	// Balance unchanged, no consume flow, member points untouched.
	acc, _ := r.repo.GetAccount(ctx, 100, m.ID)
	if acc.BalanceCents != 100 {
		t.Errorf("BalanceCents = %d after failed redeem, want 100 (unchanged)", acc.BalanceCents)
	}
	if n := r.repo.balanceLogCount(100, m.ID); n != 0 {
		t.Errorf("balance flows = %d, want 0 (nothing written)", n)
	}
}

func TestRedeem_BlockedMemberRejected(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-bm"})
	// Issue a code while active, then block.
	code, _ := r.svc.IssueCode(ctx, m.ID)
	stored, _ := r.repo.GetMember(ctx, 100, m.ID)
	stored.Status = memberStatusBlocked
	if _, err := r.repo.UpdateMember(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Redeem(ctx, RedeemInput{Code: code, EarnPoints: 5}); !errors.Is(err, ErrMemberBlocked) {
		t.Fatalf("blocked redeem err = %v, want ErrMemberBlocked", err)
	}
}

func TestRedeem_NoOpRejected(t *testing.T) {
	r := newRig()
	ctx := ctxFor(100, 10, 7)
	m, _ := r.svc.Join(ctx, JoinInput{StoreID: 10, PhoneHash: "ph-noop"})
	code, _ := r.svc.IssueCode(ctx, m.ID)
	// Neither consume nor earn -> ErrParam.
	if _, err := r.svc.Redeem(ctx, RedeemInput{Code: code}); err != errcode.ErrParam {
		t.Fatalf("no-op redeem err = %v, want ErrParam", err)
	}
}

func TestRedeem_NoTenantRejected(t *testing.T) {
	r := newRig()
	if _, err := r.svc.Redeem(context.Background(), RedeemInput{Code: "x", EarnPoints: 1}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant redeem err = %v, want ErrNoTenant", err)
	}
}

// --- Cross-tenant redemption matrix ---

func TestRedeem_CrossTenantCodeRejected(t *testing.T) {
	r := newRig()
	ctxA := ctxFor(100, 10, 7)
	ctxB := ctxFor(200, 20, 8)

	// Tenant B's member + code.
	mb, _ := r.svc.Join(ctxB, JoinInput{StoreID: 20, PhoneHash: "ph-xb"})
	codeB, err := r.svc.IssueCode(ctxB, mb.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Tenant A tries to redeem tenant B's code. The code's embedded tenant is
	// 200, so resolving under tenant 100 must fail with ErrCodeInvalid.
	if _, err := r.svc.Redeem(ctxA, RedeemInput{Code: codeB, EarnPoints: 5}); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("cross-tenant redeem err = %v, want ErrCodeInvalid", err)
	}
	// Tenant B can still redeem its own code.
	if _, err := r.svc.Redeem(ctxB, RedeemInput{Code: codeB, EarnPoints: 5}); err != nil {
		t.Fatalf("own-tenant redeem failed: %v", err)
	}
}

func TestRedeem_CrossTenantMemberInvisible(t *testing.T) {
	r := newRig()
	// A code issued for tenant A's member cannot be redeemed by tenant B even if
	// B somehow guesses the member id, because Resolve is tenant-scoped.
	ctxA := ctxFor(100, 10, 7)
	ma, _ := r.svc.Join(ctxA, JoinInput{StoreID: 10, PhoneHash: "ph-xa"})
	codeA, _ := r.svc.IssueCode(ctxA, ma.ID)

	// Tenant B reads tenant A's member id directly -> ErrNotFound.
	if _, err := r.svc.GetMember(ctxFor(200, 20, 8), ma.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant GetMember err = %v, want ErrNotFound", err)
	}
	// Tenant B redeeming tenant A's code -> ErrCodeInvalid.
	if _, err := r.svc.Redeem(ctxFor(200, 20, 8), RedeemInput{Code: codeA, EarnPoints: 1}); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("cross-tenant redeem err = %v, want ErrCodeInvalid", err)
	}
}

// --- Coder unit behaviour ---

func TestCoder_IssueResolveRoundTrip(t *testing.T) {
	now := baseNow
	c := NewMemoryMemberCoder([]byte("s"), WithCoderNowFunc(func() time.Time { return now }))
	ctx := ctxFor(100, 10, 7)
	code, err := c.Issue(ctx, 100, 9001, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Resolve(ctx, 100, code)
	if err != nil || id != 9001 {
		t.Fatalf("Resolve = %d, %v; want 9001", id, err)
	}
}

func TestCoder_TwoCodesSameMemberSameSecondAreDistinct(t *testing.T) {
	now := baseNow
	c := NewMemoryMemberCoder([]byte("s"), WithCoderNowFunc(func() time.Time { return now }))
	ctx := ctxFor(100, 10, 7)
	c1, _ := c.Issue(ctx, 100, 9001, 60*time.Second)
	c2, _ := c.Issue(ctx, 100, 9001, 60*time.Second)
	if c1 == c2 {
		t.Errorf("two codes identical: %q", c1)
	}
}

func TestCoder_NoTenantRejected(t *testing.T) {
	c := NewMemoryMemberCoder([]byte("s"))
	if _, err := c.Issue(context.Background(), 100, 1, time.Second); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("Issue no-tenant err = %v, want ErrNoTenant", err)
	}
	if _, err := c.Resolve(context.Background(), 100, "x"); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("Resolve no-tenant err = %v, want ErrNoTenant", err)
	}
}

// --- MemoryMemberRepo defensive copy ---

func TestMemoryMemberRepo_DefensiveCopy(t *testing.T) {
	repo := NewMemoryMemberRepo()
	ctx := context.Background()
	m := &Member{TenantID: 100, StoreID: 10, PhoneHash: "ph", Status: "active", Level: "normal"}
	created, err := repo.CreateMember(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating input + returned record must not corrupt the store.
	m.PhoneHash = "hacked"
	created.Points = 9999

	got, err := repo.GetMember(ctx, 100, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Points != 0 {
		t.Errorf("stored member mutated: points=%d", got.Points)
	}
	if got.PhoneHash != "ph" {
		t.Errorf("stored phone mutated: %q", got.PhoneHash)
	}
	// Idempotency index still keyed by the original phone.
	if _, ok := repo.findMemberIDByPhone(100, "ph"); !ok {
		t.Error("phone idempotency index lost after input mutation")
	}
}
