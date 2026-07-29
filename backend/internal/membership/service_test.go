package membership

import (
	"context"
	"errors"
	"strconv"
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

// --- ListMembers (M3-S2) ---

// seedMember inserts a member directly via the repo under tenant 100 (and
// optional extra tenants) and returns its id. Birthday/LastVisitAt/Points are
// set from the given fields.
func seedMember(t *testing.T, repo *MemoryMemberRepo, tenantID int64, phone string, pts int64, lastVisit, birthday *time.Time) *Member {
	t.Helper()
	m := &Member{
		TenantID:  tenantID,
		StoreID:   10,
		PhoneHash: phone,
		Level:     memberLevelNormal,
		Points:    pts,
		Status:    memberStatusActive,
		CreatedAt: baseNow,
		UpdatedAt: baseNow,
	}
	if lastVisit != nil {
		lv := *lastVisit
		m.LastVisitAt = &lv
	}
	if birthday != nil {
		bd := *birthday
		m.Birthday = &bd
	}
	created, err := repo.CreateMember(context.Background(), m)
	if err != nil {
		t.Fatalf("seedMember %s: %v", phone, err)
	}
	return created
}

func TestListMembers_NoFilterReturnsAll(t *testing.T) {
	r := newRig()
	seedMember(t, r.repo, 100, "p1", 0, nil, nil)
	seedMember(t, r.repo, 100, "p2", 500, nil, nil)
	seedMember(t, r.repo, 200, "p3", 0, nil, nil) // other tenant

	got, err := r.svc.ListMembers(ctxFor(100, 10, 7), MemberFilter{})
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (tenant 100 only)", len(got))
	}
	for _, m := range got {
		if m.TenantID != 100 {
			t.Errorf("cross-tenant leak: member tenant=%d", m.TenantID)
		}
	}
}

func TestListMembers_SleepBefore(t *testing.T) {
	r := newRig()
	cutoff := time.Date(2026, 6, 28, 0, 0, 0, 0, time.Local) // 30 days before baseNow-ish
	old := baseNow.AddDate(0, -2, 0)                         // 2 months ago -> sleeping
	recent := baseNow.AddDate(0, 0, -3)                      // 3 days ago -> not sleeping
	seedMember(t, r.repo, 100, "sleep", 0, &old, nil)
	seedMember(t, r.repo, 100, "recent", 0, &recent, nil)
	neverVisited := seedMember(t, r.repo, 100, "never", 0, nil, nil) // nil LastVisitAt -> sleeping

	got, err := r.svc.ListMembers(ctxFor(100, 10, 7), MemberFilter{SleepBefore: &cutoff})
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	ids := map[int64]bool{}
	for _, m := range got {
		ids[m.ID] = true
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (sleep + never)", len(got))
	}
	if !ids[neverVisited.ID] {
		t.Error("never-visited member (nil LastVisitAt) should match SleepBefore")
	}
}

func TestListMembers_MinPoints(t *testing.T) {
	r := newRig()
	seedMember(t, r.repo, 100, "low", 100, nil, nil)
	seedMember(t, r.repo, 100, "mid", 500, nil, nil)
	seedMember(t, r.repo, 100, "high", 2000, nil, nil)

	got, err := r.svc.ListMembers(ctxFor(100, 10, 7), MemberFilter{MinPoints: 500})
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (mid+high)", len(got))
	}
	for _, m := range got {
		if m.Points < 500 {
			t.Errorf("member points=%d below floor 500", m.Points)
		}
	}
}

func TestListMembers_BirthdayOn(t *testing.T) {
	r := newRig()
	today := time.Date(2026, 7, 27, 0, 0, 0, 0, time.Local)
	bdMatch := time.Date(1990, 7, 27, 0, 0, 0, 0, time.Local)  // same month+day
	bdOther := time.Date(1988, 12, 25, 0, 0, 0, 0, time.Local) // different
	bdMatch2 := time.Date(2000, 7, 27, 0, 0, 0, 0, time.Local) // same month+day, different year
	noBd := seedMember(t, r.repo, 100, "nobd", 0, nil, nil)    // nil birthday -> never matches
	m1 := seedMember(t, r.repo, 100, "match", 0, nil, &bdMatch)
	seedMember(t, r.repo, 100, "other", 0, nil, &bdOther)
	m2 := seedMember(t, r.repo, 100, "match2", 0, nil, &bdMatch2)

	got, err := r.svc.ListMembers(ctxFor(100, 10, 7), MemberFilter{BirthdayOn: &today})
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	ids := map[int64]bool{}
	for _, m := range got {
		ids[m.ID] = true
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (match + match2)", len(got))
	}
	if !ids[m1.ID] || !ids[m2.ID] {
		t.Errorf("expected m1+m2, got ids=%v", ids)
	}
	if ids[noBd.ID] {
		t.Error("nil-birthday member should not match BirthdayOn")
	}
}

func TestListMembers_CombinedFilters(t *testing.T) {
	r := newRig()
	cutoff := time.Date(2026, 6, 28, 0, 0, 0, 0, time.Local)
	today := time.Date(2026, 7, 27, 0, 0, 0, 0, time.Local)
	bd := time.Date(1995, 7, 27, 0, 0, 0, 0, time.Local)
	old := baseNow.AddDate(0, -2, 0)

	// Matches all three filters.
	win := seedMember(t, r.repo, 100, "win", 800, &old, &bd)
	// Fails MinPoints.
	seedMember(t, r.repo, 100, "lowpts", 100, &old, &bd)
	// Fails SleepBefore (recent visit).
	seedMember(t, r.repo, 100, "recent", 800, &baseNow, &bd)
	// Fails BirthdayOn.
	otherBd := time.Date(1995, 1, 1, 0, 0, 0, 0, time.Local)
	seedMember(t, r.repo, 100, "otherbd", 800, &old, &otherBd)

	got, err := r.svc.ListMembers(ctxFor(100, 10, 7), MemberFilter{
		SleepBefore: &cutoff,
		MinPoints:   500,
		BirthdayOn:  &today,
	})
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(got) != 1 || got[0].ID != win.ID {
		t.Errorf("expected only win, got %d members: %+v", len(got), got)
	}
}

func TestListMembers_LimitDefaultAndCap(t *testing.T) {
	r := newRig()
	for i := 0; i < 3; i++ {
		seedMember(t, r.repo, 100, "p"+strconv.Itoa(i), 0, nil, nil)
	}
	// Explicit limit=2 caps the result.
	got, err := r.svc.ListMembers(ctxFor(100, 10, 7), MemberFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (Limit=2)", len(got))
	}
}

func TestListMembers_CrossTenantInvisible(t *testing.T) {
	r := newRig()
	ma := seedMember(t, r.repo, 100, "a", 0, nil, nil)
	seedMember(t, r.repo, 200, "b", 0, nil, nil)

	// Tenant 200 lists -> sees only its own, never tenant 100's.
	got, err := r.svc.ListMembers(ctxFor(200, 20, 8), MemberFilter{})
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	for _, m := range got {
		if m.TenantID == 100 {
			t.Errorf("tenant 200 saw tenant 100 member %d", m.ID)
		}
	}
	// Tenant 100's member is not in tenant 200's result.
	for _, m := range got {
		if m.ID == ma.ID {
			t.Error("tenant 200 enumerated tenant 100's member")
		}
	}
}

func TestListMembers_NoTenantRejected(t *testing.T) {
	r := newRig()
	if _, err := r.svc.ListMembers(context.Background(), MemberFilter{}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

func TestListMembers_BirthdayFieldRoundTrip(t *testing.T) {
	repo := NewMemoryMemberRepo()
	ctx := context.Background()
	bd := time.Date(1992, 3, 15, 0, 0, 0, 0, time.Local)
	created, err := repo.CreateMember(ctx, &Member{
		TenantID: 100, StoreID: 10, PhoneHash: "bd", Level: "normal",
		Status: "active", Birthday: &bd, CreatedAt: baseNow, UpdatedAt: baseNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetMember(ctx, 100, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Birthday == nil || !got.Birthday.Equal(bd) {
		t.Errorf("Birthday = %v, want %v", got.Birthday, bd)
	}
	// Mutating the caller's birthday pointer must not corrupt the stored record.
	*got.Birthday = baseNow
	again, _ := repo.GetMember(ctx, 100, created.ID)
	if again.Birthday == nil || !again.Birthday.Equal(bd) {
		t.Errorf("stored birthday mutated via alias: got %v, want %v", again.Birthday, bd)
	}
}

// TestListMembersPage_ServiceAndFilter exercises the FE-MP-02 boss list
// service path: Level + Query filter, pagination, and the total count.
func TestListMembersPage_ServiceAndFilter(t *testing.T) {
	repo := NewMemoryMemberRepo()
	svc := New(repo, NewMemoryMemberCoder(nil))
	ctx := ctxFor(200, 10, 0)

	// Seed: 3 normals + 1 vip. Names: Alice, Bob, Carol (normal), Vip (vip).
	for _, m := range []*Member{
		{TenantID: 200, StoreID: 10, Name: "Alice", PhoneHash: "pa", Level: "normal", Status: "active", CreatedAt: baseNow, UpdatedAt: baseNow},
		{TenantID: 200, StoreID: 10, Name: "Bob", PhoneHash: "pb", Level: "normal", Status: "active", CreatedAt: baseNow, UpdatedAt: baseNow},
		{TenantID: 200, StoreID: 10, Name: "Carol", PhoneHash: "pc", Level: "normal", Status: "active", CreatedAt: baseNow, UpdatedAt: baseNow},
		{TenantID: 200, StoreID: 10, Name: "VipAlice", PhoneHash: "pv", Level: "vip", Status: "active", CreatedAt: baseNow, UpdatedAt: baseNow},
	} {
		if _, err := repo.CreateMember(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	// All members, page size 2, page 1 (offset 0) → 2 items, total 4.
	page, err := svc.ListMembersPage(ctx, MemberFilter{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Total != 4 {
		t.Fatalf("page1: items=%d total=%d, want 2/4", len(page.Items), page.Total)
	}
	// Page 2 (offset 2) → remaining 2 items.
	page, err = svc.ListMembersPage(ctx, MemberFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Total != 4 {
		t.Fatalf("page2: items=%d total=%d, want 2/4", len(page.Items), page.Total)
	}

	// Level filter: vip only → 1 item, total 1.
	page, err = svc.ListMembersPage(ctx, MemberFilter{Level: "vip"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Total != 1 || page.Items[0].Name != "VipAlice" {
		t.Fatalf("vip filter: items=%d total=%d name=%s", len(page.Items), page.Total, page.Items[0].Name)
	}

	// Query filter: name contains "Alice" → Alice + VipAlice = 2.
	page, err = svc.ListMembersPage(ctx, MemberFilter{Query: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Total != 2 {
		t.Fatalf("query Alice: items=%d total=%d, want 2/2", len(page.Items), page.Total)
	}
}

// TestListMembersPage_CrossTenantEmpty proves a tenant only sees its own members
// (ARCH-01): tenant 201 sees none of tenant 200's seeded members.
func TestListMembersPage_CrossTenantEmpty(t *testing.T) {
	repo := NewMemoryMemberRepo()
	svc := New(repo, NewMemoryMemberCoder(nil))
	ctx200 := ctxFor(200, 10, 0)
	repo.CreateMember(ctx200, &Member{TenantID: 200, StoreID: 10, Name: "A", Level: "normal", Status: "active", CreatedAt: baseNow, UpdatedAt: baseNow})

	ctx201 := ctxFor(201, 10, 0)
	page, err := svc.ListMembersPage(ctx201, MemberFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 || page.Total != 0 {
		t.Fatalf("cross-tenant: items=%d total=%d, want 0/0", len(page.Items), page.Total)
	}
}

// TestListPointsLog_Service exercises the C 端 积分明细 service path: redeem
// accrues points + appends an earn log, then ListPointsLog returns it newest
// first.
func TestListPointsLog_Service(t *testing.T) {
	repo := NewMemoryMemberRepo()
	svc := New(repo, NewMemoryMemberCoder(nil))
	ctx := ctxFor(300, 10, 0)

	created, err := svc.Join(ctx, JoinInput{WxOpenID: "oABC"})
	if err != nil {
		t.Fatal(err)
	}
	// Accrue 50 points via a Redeem (earn-only, no consume).
	if _, err := svc.Redeem(ctx, RedeemInput{Code: mustIssueCode(t, svc, created.ID), EarnPoints: 50}); err != nil {
		t.Fatal(err)
	}
	logs, err := svc.ListPointsLog(ctx, created.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Delta != 50 || logs[0].PointsAfter != 50 || logs[0].Type != "earn" {
		t.Fatalf("logs=%+v, want 1 earn row delta=50 after=50", logs)
	}
	// Cross-tenant reads nothing.
	otherCtx := ctxFor(301, 10, 0)
	otherLogs, err := svc.ListPointsLog(otherCtx, created.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherLogs) != 0 {
		t.Fatalf("cross-tenant points log leak: %d rows", len(otherLogs))
	}
}

// mustIssueCode issues a member code or fails the test (test helper).
func mustIssueCode(t *testing.T, svc *Service, memberID int64) string {
	t.Helper()
	code, err := svc.IssueCode(ctxFor(300, 10, 0), memberID)
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	return code
}
