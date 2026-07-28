package marketing

// Service orchestrates coupon-template CRUD, member-coupon issuance and
// redemption (BE-B-02, CONTRACT §10.3). Every method first resolves the tenant
// from ctx via multitenancy.EnforceTenant — tenant is NEVER trusted from a
// request field. The service depends only on the CouponRepo interface plus the
// optional MemberLookuper (member existence) and MarginGuard (gross-margin
// rail) anti-corruption ports, so the whole flow is deterministic in unit
// tests (in-memory repo + injected fake lookuper/guard + fixed clock).
//
// IssueToMember and RedeemAtomic are atomic at the repo layer; the service
// layer adds the member-existence and margin-guard pre-checks around Issue.

import (
	"context"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service is the marketing orchestrator.
type Service struct {
	repo     CouponRepo
	lookuper MemberLookuper // nil = skip member-existence check (dev convenience)
	guard    MarginGuard    // nil = skip margin check
	now      func() time.Time
}

// Option customises a Service.
type Option func(*Service)

// WithMemberLookuper wires the member-existence port (main thread adapts
// *membership.Service to it). Without it, Issue skips the member check.
func WithMemberLookuper(l MemberLookuper) Option {
	return func(s *Service) { s.lookuper = l }
}

// WithMarginGuard wires the gross-margin rail (main thread adapts margin.Guard).
// Without it, Issue skips the margin check.
func WithMarginGuard(g MarginGuard) Option {
	return func(s *Service) { s.guard = g }
}

// WithNowFunc overrides the service clock (deterministic valid-window tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// New constructs a marketing Service. A nil clock falls back to time.Now.
func New(repo CouponRepo, opts ...Option) *Service {
	s := &Service{repo: repo, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// validCouponType reports whether t is one of the four coupon types.
func validCouponType(t string) bool {
	switch t {
	case TypeFullReduction, TypeDiscount, TypeCash, TypeGift:
		return true
	}
	return false
}

// validSource reports whether s is one of the four member-coupon sources.
func validSource(s string) bool {
	switch s {
	case SourceAIStrategy, SourceManual, SourceGroupbuy, SourceNewmember:
		return true
	}
	return false
}

// storeScope returns in.StoreID, falling back to the store carried in ctx when
// the caller passed zero.
func storeScope(ctx context.Context, storeID int64) int64 {
	if storeID > 0 {
		return storeID
	}
	if mc, ok := multitenancy.FromContext(ctx); ok {
		return mc.StoreID
	}
	return 0
}

// isAllowedTransition reports whether moving from→to is a legal status change.
// draft → active | ended; active → paused | ended; paused → active | ended;
// ended is terminal.
func isAllowedTransition(from, to string) bool {
	switch from {
	case StatusDraft:
		return to == StatusActive || to == StatusEnded
	case StatusActive:
		return to == StatusPaused || to == StatusEnded
	case StatusPaused:
		return to == StatusActive || to == StatusEnded
	}
	return false
}

// CreateCoupon creates a draft coupon template. Name must be non-empty and
// unique within the tenant; Type must be one of the four coupon types;
// ValidFrom < ValidTo (ValidFrom zero defaults to now).
func (s *Service) CreateCoupon(ctx context.Context, in CreateCouponInput) (*Coupon, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if in.Name == "" || !validCouponType(in.Type) {
		return nil, errcode.ErrParam
	}
	now := s.now()
	if in.ValidFrom.IsZero() {
		in.ValidFrom = now
	}
	if !in.ValidTo.After(in.ValidFrom) {
		return nil, errcode.ErrParam
	}
	// Name uniqueness within tenant.
	if _, err := s.repo.GetByName(ctx, tenantID, in.Name); err == nil {
		return nil, errcode.ErrParam // name already exists
	}
	c := &Coupon{
		TenantID:   tenantID,
		StoreID:    storeScope(ctx, in.StoreID),
		Name:       in.Name,
		Type:       in.Type,
		Params:     in.Params,
		ValidFrom:  in.ValidFrom,
		ValidTo:    in.ValidTo,
		TotalQuota: in.TotalQuota,
		Status:     StatusDraft,
		CreatedBy:  in.CreatedBy,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	return s.repo.CreateCoupon(ctx, c)
}

// UpdateStatus transitions a coupon's status. Legal transitions: draft→active,
// active↔paused, *→ended. An illegal transition yields errcode.ErrParam.
func (s *Service) UpdateStatus(ctx context.Context, id int64, status string) (*Coupon, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if status != StatusDraft && status != StatusActive && status != StatusPaused && status != StatusEnded {
		return nil, errcode.ErrParam
	}
	c, err := s.repo.GetCoupon(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if !isAllowedTransition(c.Status, status) {
		return nil, errcode.ErrParam
	}
	c.Status = status
	return s.repo.UpdateCoupon(ctx, c)
}

// GetCoupon loads one coupon for the ctx tenant. Cross-tenant → ErrNotFound.
func (s *Service) GetCoupon(ctx context.Context, id int64) (*Coupon, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.GetCoupon(ctx, tenantID, id)
}

// ListCoupons returns coupons matching the filter for the ctx tenant.
func (s *Service) ListCoupons(ctx context.Context, f CouponFilter) ([]*Coupon, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListCoupons(ctx, tenantID, f)
}

// Issue creates one member-coupon instance. It checks (when the ports are
// wired) that the member exists and the coupon clears the margin guard, then
// delegates to the repo's atomic IssueToMember which validates the coupon is
// active, within its valid window, and under quota. Source defaults to manual;
// a non-empty source must be one of the four sources.
func (s *Service) Issue(ctx context.Context, in IssueInput) (*MemberCoupon, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if in.MemberID <= 0 || in.CouponID <= 0 {
		return nil, errcode.ErrParam
	}
	source := in.Source
	if source == "" {
		source = SourceManual
	}
	if !validSource(source) {
		return nil, errcode.ErrParam
	}

	// Member existence (anti-corruption port; skipped when not wired).
	if s.lookuper != nil {
		exists, err := s.lookuper.Exists(ctx, tenantID, in.MemberID)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, errcode.ErrParam // member_id refers to no member in this tenant
		}
	}

	// Margin guardrail (optional; AI-issued coupons wire this in M3).
	if s.guard != nil {
		c, err := s.repo.GetCoupon(ctx, tenantID, in.CouponID)
		if err != nil {
			return nil, err
		}
		if err := s.guard.ValidateCoupon(ctx, c); err != nil {
			return nil, err
		}
	}

	now := s.now()
	return s.repo.IssueToMember(ctx, tenantID, in.MemberID, in.CouponID, source, in.StrategyID, in.ExpireAt, now)
}

// Redeem transitions a member-coupon unused→used (the till redemption). The
// repo's RedeemAtomic enforces unused state and expiry atomically.
func (s *Service) Redeem(ctx context.Context, in RedeemInput) (*MemberCoupon, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if in.MemberCouponID <= 0 {
		return nil, errcode.ErrParam
	}
	now := s.now()
	return s.repo.RedeemAtomic(ctx, tenantID, in.MemberCouponID, in.RefOrderID, now)
}

// ListMemberCoupons returns a member's coupons, optionally filtered by status.
func (s *Service) ListMemberCoupons(ctx context.Context, memberID int64, status string) ([]*MemberCoupon, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if memberID <= 0 {
		return nil, errcode.ErrParam
	}
	return s.repo.ListMemberCoupons(ctx, tenantID, memberID, status)
}

// ListByStrategy returns all member-coupons attributed to one strategy (M3-S3
// attribution link, member_coupons.strategy_id). status filters by member-coupon
// status; "" = all. Used by the attribution engine to compute coupon_redeem.
func (s *Service) ListByStrategy(ctx context.Context, strategyID int64, status string) ([]*MemberCoupon, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if strategyID <= 0 {
		return nil, errcode.ErrParam
	}
	return s.repo.ListByStrategy(ctx, tenantID, strategyID, status)
}
