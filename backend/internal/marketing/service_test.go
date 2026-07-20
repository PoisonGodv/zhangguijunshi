package marketing

import (
	"context"
	"errors"
	"strconv"
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

// fixedNow is the service clock used across tests; coupons are given a wide
// valid window around it.
var fixedNow = time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)

var nameSeq int64

func newSvc(t *testing.T, opts ...Option) *Service {
	t.Helper()
	return New(NewMemoryCouponRepo(), append([]Option{WithNowFunc(func() time.Time { return fixedNow })}, opts...)...)
}

// validWindow returns a [valid_from, valid_to] straddling fixedNow.
func validWindow() (time.Time, time.Time) {
	return fixedNow.Add(-24 * time.Hour), fixedNow.Add(7 * 24 * time.Hour)
}

func mustCreate(t *testing.T, s *Service, typ string, params CouponParams) *Coupon {
	t.Helper()
	vf, vt := validWindow()
	seq := atomic.AddInt64(&nameSeq, 1)
	c, err := s.CreateCoupon(ctxWithTenant(50021), CreateCouponInput{
		Name: typ + "-coupon-" + strconv.FormatInt(seq, 10), Type: typ, Params: params, ValidFrom: vf, ValidTo: vt,
	})
	if err != nil {
		t.Fatalf("CreateCoupon %s: %v", typ, err)
	}
	return c
}

func TestCreateCoupon_FourTypes(t *testing.T) {
	cases := []struct {
		typ    string
		params CouponParams
	}{
		{TypeFullReduction, CouponParams{ThresholdCents: 1000, OffCents: 200}},
		{TypeDiscount, CouponParams{DiscountPermille: 850}},
		{TypeCash, CouponParams{OffCents: 500}},
		{TypeGift, CouponParams{GiftProductID: 42}},
	}
	for _, c := range cases {
		s := newSvc(t)
		got, err := s.CreateCoupon(ctxWithTenant(50021), CreateCouponInput{
			Name: c.typ + "-x", Type: c.typ, Params: c.params,
			ValidFrom: fixedNow, ValidTo: fixedNow.Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("type %s: %v", c.typ, err)
		}
		if got.Status != StatusDraft {
			t.Errorf("type %s: default status should be draft, got %s", c.typ, got.Status)
		}
		if got.ID <= 0 {
			t.Errorf("type %s: id should be allocated", c.typ)
		}
	}
}

func TestCreateCoupon_Validation(t *testing.T) {
	s := newSvc(t)
	vf, vt := validWindow()
	// empty name
	if _, err := s.CreateCoupon(ctxWithTenant(50021), CreateCouponInput{Name: "", Type: TypeCash, ValidFrom: vf, ValidTo: vt}); err == nil {
		t.Fatal("empty name should reject")
	}
	// invalid type
	if _, err := s.CreateCoupon(ctxWithTenant(50021), CreateCouponInput{Name: "x", Type: "bogus", ValidFrom: vf, ValidTo: vt}); err == nil {
		t.Fatal("invalid type should reject")
	}
	// valid_to <= valid_from
	if _, err := s.CreateCoupon(ctxWithTenant(50021), CreateCouponInput{Name: "x", Type: TypeCash, ValidFrom: vt, ValidTo: vf}); err == nil {
		t.Fatal("valid_to <= valid_from should reject")
	}
}

func TestCreateCoupon_NameUniqueWithinTenant(t *testing.T) {
	s := newSvc(t)
	vf, vt := validWindow()
	if _, err := s.CreateCoupon(ctxWithTenant(50021), CreateCouponInput{Name: "dup", Type: TypeCash, ValidFrom: vf, ValidTo: vt}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCoupon(ctxWithTenant(50021), CreateCouponInput{Name: "dup", Type: TypeCash, ValidFrom: vf, ValidTo: vt}); err == nil {
		t.Fatal("duplicate name within tenant should reject")
	}
	// same name in a DIFFERENT tenant is fine
	if _, err := s.CreateCoupon(ctxWithTenant(50022), CreateCouponInput{Name: "dup", Type: TypeCash, ValidFrom: vf, ValidTo: vt}); err != nil {
		t.Fatalf("same name in different tenant should be allowed: %v", err)
	}
}

func TestUpdateStatus_Transitions(t *testing.T) {
	s := newSvc(t)
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	ctx := ctxWithTenant(50021)

	// draft → active
	c, err := s.UpdateStatus(ctx, c.ID, StatusActive)
	if err != nil || c.Status != StatusActive {
		t.Fatalf("draft→active failed: %v status=%s", err, c.Status)
	}
	// active → paused
	c, err = s.UpdateStatus(ctx, c.ID, StatusPaused)
	if err != nil || c.Status != StatusPaused {
		t.Fatalf("active→paused failed: %v", err)
	}
	// paused → active
	c, err = s.UpdateStatus(ctx, c.ID, StatusActive)
	if err != nil || c.Status != StatusActive {
		t.Fatalf("paused→active failed: %v", err)
	}
	// active → ended
	c, err = s.UpdateStatus(ctx, c.ID, StatusEnded)
	if err != nil || c.Status != StatusEnded {
		t.Fatalf("active→ended failed: %v", err)
	}
	// ended → active (illegal)
	if _, err := s.UpdateStatus(ctx, c.ID, StatusActive); err == nil {
		t.Fatal("ended→active should be illegal")
	}
	// draft → paused (illegal): make a fresh draft
	c2 := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	if _, err := s.UpdateStatus(ctx, c2.ID, StatusPaused); err == nil {
		t.Fatal("draft→paused should be illegal")
	}
}

func TestIssue_ActiveCoupon_Success(t *testing.T) {
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	c := mustCreate(t, s, TypeFullReduction, CouponParams{ThresholdCents: 1000, OffCents: 200})
	ctx := ctxWithTenant(50021)
	if _, err := s.UpdateStatus(ctx, c.ID, StatusActive); err != nil {
		t.Fatal(err)
	}
	mc, err := s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID})
	if err != nil {
		t.Fatalf("issue failed: %v", err)
	}
	if mc.Status != MCStatusUnused {
		t.Errorf("expect unused, got %s", mc.Status)
	}
	if mc.Source != SourceManual {
		t.Errorf("expect default manual source, got %s", mc.Source)
	}
	if mc.StrategyID != 0 {
		t.Errorf("expect strategy_id 0, got %d", mc.StrategyID)
	}
	if mc.ExpireAt == nil || !mc.ExpireAt.Equal(c.ValidTo) {
		t.Errorf("expect expire = coupon.ValidTo, got %v", mc.ExpireAt)
	}
	// coupon.Issued should be 1
	got, _ := s.GetCoupon(ctx, c.ID)
	if got.Issued != 1 {
		t.Errorf("expect issued=1, got %d", got.Issued)
	}
}

func TestIssue_AIStrategyAttribution(t *testing.T) {
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	c := mustCreate(t, s, TypeDiscount, CouponParams{DiscountPermille: 900})
	ctx := ctxWithTenant(50021)
	s.UpdateStatus(ctx, c.ID, StatusActive)
	mc, err := s.Issue(ctx, IssueInput{
		MemberID: 7001, CouponID: c.ID, Source: SourceAIStrategy, StrategyID: 9001,
	})
	if err != nil {
		t.Fatalf("issue failed: %v", err)
	}
	if mc.Source != SourceAIStrategy || mc.StrategyID != 9001 {
		t.Errorf("attribution wrong: source=%s strategy=%d", mc.Source, mc.StrategyID)
	}
}

func TestIssue_InactiveCoupon_Rejects(t *testing.T) {
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100}) // still draft
	ctx := ctxWithTenant(50021)
	_, err := s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID})
	if !errors.Is(err, ErrCouponInactive) {
		t.Fatalf("draft coupon issue should ErrCouponInactive, got %v", err)
	}
}

func TestIssue_OutsideValidWindow_Rejects(t *testing.T) {
	// Coupon valid in the past; fixedNow is after valid_to.
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	ctx := ctxWithTenant(50021)
	c, err := s.CreateCoupon(ctx, CreateCouponInput{
		Name: "past", Type: TypeCash, Params: CouponParams{OffCents: 100},
		ValidFrom: fixedNow.Add(-48 * time.Hour), ValidTo: fixedNow.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.UpdateStatus(ctx, c.ID, StatusActive)
	_, err = s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID})
	if !errors.Is(err, ErrCouponInactive) {
		t.Fatalf("expired-window issue should ErrCouponInactive, got %v", err)
	}
}

func TestIssue_QuotaExhausted_Rejects(t *testing.T) {
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	vf, vt := validWindow()
	ctx := ctxWithTenant(50021)
	c, err := s.CreateCoupon(ctx, CreateCouponInput{
		Name: "limited", Type: TypeCash, Params: CouponParams{OffCents: 100},
		ValidFrom: vf, ValidTo: vt, TotalQuota: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.UpdateStatus(ctx, c.ID, StatusActive)
	if _, err := s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID}); err != nil {
		t.Fatalf("1st issue should succeed: %v", err)
	}
	_, err = s.Issue(ctx, IssueInput{MemberID: 7002, CouponID: c.ID})
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("2nd issue should ErrQuotaExhausted, got %v", err)
	}
}

func TestIssue_MemberNotFound_Rejects(t *testing.T) {
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: false}))
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	ctx := ctxWithTenant(50021)
	s.UpdateStatus(ctx, c.ID, StatusActive)
	_, err := s.Issue(ctx, IssueInput{MemberID: 9999, CouponID: c.ID})
	if err == nil {
		t.Fatal("member-not-exist should reject")
	}
}

func TestIssue_MarginGuardRejects(t *testing.T) {
	s := newSvc(t,
		WithMemberLookuper(fakeLookuper{exists: true}),
		WithMarginGuard(fakeGuard{reject: true}),
	)
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	ctx := ctxWithTenant(50021)
	s.UpdateStatus(ctx, c.ID, StatusActive)
	_, err := s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID})
	if !errors.Is(err, errGuardRejected) {
		t.Fatalf("margin guard rejection should propagate, got %v", err)
	}
}

func TestRedeem_UnusedToUsed_Success(t *testing.T) {
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	ctx := ctxWithTenant(50021)
	s.UpdateStatus(ctx, c.ID, StatusActive)
	mc, _ := s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID})

	out, err := s.Redeem(ctx, RedeemInput{MemberCouponID: mc.ID, RefOrderID: 8001})
	if err != nil {
		t.Fatalf("redeem failed: %v", err)
	}
	if out.Status != MCStatusUsed {
		t.Errorf("expect used, got %s", out.Status)
	}
	if out.UsedAt == nil || out.RefOrderID != 8001 {
		t.Errorf("used_at/ref_order wrong: used_at=%v ref=%d", out.UsedAt, out.RefOrderID)
	}
}

func TestRedeem_AlreadyUsed_Rejects(t *testing.T) {
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	ctx := ctxWithTenant(50021)
	s.UpdateStatus(ctx, c.ID, StatusActive)
	mc, _ := s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID})
	s.Redeem(ctx, RedeemInput{MemberCouponID: mc.ID})
	_, err := s.Redeem(ctx, RedeemInput{MemberCouponID: mc.ID})
	if !errors.Is(err, ErrCouponUsed) {
		t.Fatalf("2nd redeem should ErrCouponUsed, got %v", err)
	}
}

func TestRedeem_Expired_Rejects(t *testing.T) {
	// Issue with an explicit short expire, then advance the clock past it.
	clock := fixedNow
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}), WithNowFunc(func() time.Time { return clock }))
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	ctx := ctxWithTenant(50021)
	s.UpdateStatus(ctx, c.ID, StatusActive)
	shortExpire := clock.Add(1 * time.Hour)
	mc, _ := s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID, ExpireAt: &shortExpire})

	clock = clock.Add(2 * time.Hour) // past expire
	_, err := s.Redeem(ctx, RedeemInput{MemberCouponID: mc.ID})
	if !errors.Is(err, ErrCouponExpired) {
		t.Fatalf("expired redeem should ErrCouponExpired, got %v", err)
	}
}

func TestIssue_Redeem_CrossTenantRejected(t *testing.T) {
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	ctxA := ctxWithTenant(50021)
	ctxB := ctxWithTenant(50022)
	s.UpdateStatus(ctxA, c.ID, StatusActive)

	// tenant B cannot issue tenant A's coupon
	_, err := s.Issue(ctxB, IssueInput{MemberID: 7001, CouponID: c.ID})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant issue should ErrNotFound, got %v", err)
	}
	// tenant A issues, tenant B cannot see/redeem the member-coupon
	mc, _ := s.Issue(ctxA, IssueInput{MemberID: 7001, CouponID: c.ID})
	_, err = s.Redeem(ctxB, RedeemInput{MemberCouponID: mc.ID})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant redeem should ErrNotFound, got %v", err)
	}
	// tenant B cannot list tenant A's member-coupons
	list, err := s.ListMemberCoupons(ctxB, 7001, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("cross-tenant list should be empty, got %d", len(list))
	}
}

func TestGetCoupon_CrossTenantNotFound(t *testing.T) {
	s := newSvc(t)
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	_, err := s.GetCoupon(ctxWithTenant(50022), c.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get should ErrNotFound, got %v", err)
	}
}

func TestListCoupons_FilterByStatus(t *testing.T) {
	s := newSvc(t)
	ctx := ctxWithTenant(50021)
	c1 := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	mustCreate(t, s, TypeDiscount, CouponParams{DiscountPermille: 900}) // c2 stays draft
	s.UpdateStatus(ctx, c1.ID, StatusActive)

	active, _ := s.ListCoupons(ctx, CouponFilter{Status: StatusActive})
	if len(active) != 1 || active[0].ID != c1.ID {
		t.Fatalf("expect 1 active coupon, got %v", active)
	}
	drafts, _ := s.ListCoupons(ctx, CouponFilter{Status: StatusDraft})
	if len(drafts) != 1 {
		t.Fatalf("expect 1 draft coupon, got %d", len(drafts))
	}
}

func TestListMemberCoupons_ByStatus(t *testing.T) {
	s := newSvc(t, WithMemberLookuper(fakeLookuper{exists: true}))
	c := mustCreate(t, s, TypeCash, CouponParams{OffCents: 100})
	ctx := ctxWithTenant(50021)
	s.UpdateStatus(ctx, c.ID, StatusActive)
	mc1, _ := s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID})
	mc2, _ := s.Issue(ctx, IssueInput{MemberID: 7001, CouponID: c.ID})
	s.Redeem(ctx, RedeemInput{MemberCouponID: mc1.ID})

	unused, _ := s.ListMemberCoupons(ctx, 7001, MCStatusUnused)
	if len(unused) != 1 || unused[0].ID != mc2.ID {
		t.Fatalf("expect 1 unused, got %v", unused)
	}
	used, _ := s.ListMemberCoupons(ctx, 7001, MCStatusUsed)
	if len(used) != 1 || used[0].ID != mc1.ID {
		t.Fatalf("expect 1 used, got %v", used)
	}
	all, _ := s.ListMemberCoupons(ctx, 7001, "")
	if len(all) != 2 {
		t.Fatalf("expect 2 total, got %d", len(all))
	}
}

// --- fakes ---

type fakeLookuper struct{ exists bool }

func (f fakeLookuper) Exists(_ context.Context, _, _ int64) (bool, error) {
	return f.exists, nil
}

var errGuardRejected = errors.New("fake guard rejected")

type fakeGuard struct{ reject bool }

func (g fakeGuard) ValidateCoupon(_ context.Context, _ *Coupon) error {
	if g.reject {
		return errGuardRejected
	}
	return nil
}
