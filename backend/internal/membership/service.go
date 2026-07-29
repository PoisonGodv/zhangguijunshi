package membership

// Service orchestrates member enrolment (Join), profile lookup (GetMember),
// dynamic member-code issuing (IssueCode) and redemption (Redeem). Every method
// first resolves the tenant from ctx via multitenancy.EnforceTenant — tenant is
// NEVER trusted from a request field. The service depends only on the MemberRepo
// and MemberCoder interfaces plus an injected clock, so the whole flow is
// deterministic in unit tests (in-memory repo + fixed now + fixed coder clock).
//
// Redeem's "deduct balance + accrue points + update member" critical section is
// atomic: against the in-memory repo it runs inside a single locked helper; the
// production pgx repo will wrap the same steps in a real transaction.

import (
	"context"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Defaults applied by the service.
const (
	memberStatusActive  = "active"
	memberStatusBlocked = "blocked"
	memberLevelNormal   = "normal"
	codeTTL             = 60 // seconds; IssueCode ttl (contract §9.3)
)

// Service is the membership orchestrator.
type Service struct {
	repo  MemberRepo
	coder MemberCoder
	now   func() time.Time
}

// Option customises a Service (primarily for deterministic tests).
type Option func(*Service)

// WithNowFunc overrides the service clock (deterministic CreatedAt/LastVisitAt).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// New constructs a membership Service. A nil clock falls back to time.Now.
func New(repo MemberRepo, coder MemberCoder, opts ...Option) *Service {
	s := &Service{
		repo:  repo,
		coder: coder,
		now:   time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
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

// Join enrols a member under the ctx tenant and opens a zero-balance stored
// account. It is idempotent: if a member already exists for the same non-empty
// phone_hash OR the same non-empty wx_openid within the tenant, the existing
// member is returned unchanged (no duplicate, no second account).
func (s *Service) Join(ctx context.Context, in JoinInput) (*Member, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if in.PhoneHash == "" && in.WxOpenID == "" {
		return nil, errcode.ErrParam // 至少其一
	}

	// Idempotency: reuse an existing member keyed by either identity.
	if existing := s.findExisting(ctx, tenantID, in.PhoneHash, in.WxOpenID); existing != nil {
		return existing, nil
	}

	now := s.now()
	m := &Member{
		TenantID:  tenantID,
		StoreID:   storeScope(ctx, in.StoreID),
		PhoneHash: in.PhoneHash,
		WxOpenID:  in.WxOpenID,
		Name:      in.Name,
		Level:     memberLevelNormal,
		Points:    0,
		Status:    memberStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	created, err := s.repo.CreateMember(ctx, m)
	if err != nil {
		return nil, errcode.ErrInternal
	}

	// Open the zero-balance stored-value account for the new member.
	if _, err := s.repo.UpsertAccount(ctx, &BalanceAccount{
		TenantID:     tenantID,
		MemberID:     created.ID,
		BalanceCents: 0,
		GiftCents:    0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		return nil, errcode.ErrInternal
	}
	return created, nil
}

// findExisting returns the already-enrolled member matching either non-empty
// identity within the tenant, or nil. It prefers the phone_hash match, then the
// wx_openid match. Only same-tenant members are visible (repo enforces this).
func (s *Service) findExisting(ctx context.Context, tenantID int64, phoneHash, openID string) *Member {
	lookup := func(id int64, ok bool) *Member {
		if !ok {
			return nil
		}
		m, err := s.repo.GetMember(ctx, tenantID, id)
		if err != nil {
			return nil
		}
		return m
	}
	if phoneHash != "" {
		if id, ok := s.repo.findMemberIDByPhone(tenantID, phoneHash); ok {
			return lookup(id, true)
		}
	}
	if openID != "" {
		if id, ok := s.repo.findMemberIDByOpenID(tenantID, openID); ok {
			return lookup(id, true)
		}
	}
	return nil
}

// GetMember loads one member for the ctx tenant. Cross-tenant ids yield
// ErrNotFound.
func (s *Service) GetMember(ctx context.Context, id int64) (*Member, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.GetMember(ctx, tenantID, id)
}

// GetAccount loads the stored-value (储值) account for memberID within the ctx
// tenant (CONTRACT §16.8: backs the C-side GET /v1/wxshop/member/balance read).
// Cross-tenant or absent account → ErrNotFound (the C 端 balance handler
// degrades a member with no account yet to 0/0 rather than 404). tenantID is
// resolved from ctx (ARCH-01), NEVER trusted from the caller.
func (s *Service) GetAccount(ctx context.Context, memberID int64) (*BalanceAccount, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.GetAccount(ctx, tenantID, memberID)
}

// ListMembers enumerates the ctx tenant's members matching f (M3-S2 signal real
// enumeration). Empty filter fields are not filtering; Limit<=0 defaults to 500.
// Cross-tenant members are never visible (repo enforces this).
func (s *Service) ListMembers(ctx context.Context, f MemberFilter) ([]*Member, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListMembers(ctx, tenantID, f)
}

// IssueCode issues a short-lived (60s) dynamic member code for an existing,
// active member of the ctx tenant. A blocked member is rejected with
// ErrMemberBlocked; an unknown / cross-tenant member with ErrNotFound.
func (s *Service) IssueCode(ctx context.Context, memberID int64) (string, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return "", err
	}
	m, err := s.repo.GetMember(ctx, tenantID, memberID)
	if err != nil {
		return "", err // ErrNotFound (incl. cross-tenant)
	}
	if m.Status == memberStatusBlocked {
		return "", ErrMemberBlocked
	}
	if m.Status != memberStatusActive {
		return "", ErrMemberBlocked
	}
	return s.coder.Issue(ctx, tenantID, memberID, codeTTL*time.Second)
}

// Redeem redeems a dynamic member code at the till (BE-A-07). It resolves the
// code to a member of the ctx tenant (forged/expired → ErrCodeInvalid),
// validates the member belongs to this tenant and is active, then atomically:
//   - ConsumeCents>0: deduct stored balance (balance<consume →
//     ErrInsufficientBalance, no state changed) and append a consume flow;
//   - EarnPoints>0: accrue points and append an earn flow;
//   - always: update member Points/LastVisitAt.
func (s *Service) Redeem(ctx context.Context, in RedeemInput) (*RedeemResult, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if in.Code == "" || (in.ConsumeCents <= 0 && in.EarnPoints <= 0) {
		return nil, errcode.ErrParam
	}
	if in.ConsumeCents < 0 || in.EarnPoints < 0 {
		return nil, errcode.ErrParam
	}

	// Resolve the dynamic code to a member id (forged/expired → ErrCodeInvalid).
	memberID, err := s.coder.Resolve(ctx, tenantID, in.Code)
	if err != nil {
		return nil, err
	}

	// The member must belong to THIS tenant and be active.
	m, err := s.repo.GetMember(ctx, tenantID, memberID)
	if err != nil {
		return nil, err // ErrNotFound (incl. a code pointing at another tenant's member)
	}
	if m.Status != memberStatusActive {
		return nil, ErrMemberBlocked
	}

	now := s.now()
	out, err := s.repo.redeemAtomic(tenantID, memberID, now, in.ConsumeCents, in.EarnPoints, in.RefOrderID)
	if err != nil {
		return nil, err // ErrInsufficientBalance / ErrNotFound
	}

	res := &RedeemResult{
		MemberID:      memberID,
		Points:        out.member.Points,
		ConsumedCents: in.ConsumeCents,
	}
	if out.account != nil {
		res.BalanceCents = out.account.BalanceCents
	} else {
		// Points-only redemption: report the current balance without deducting.
		if acc, err := s.repo.GetAccount(ctx, tenantID, memberID); err == nil {
			res.BalanceCents = acc.BalanceCents
		}
	}
	return res, nil
}

// Recharge books a stored-value top-up (M4-S1 BE-B-12). The fund flow is
// settled by WeChat Pay directly to the sub-merchant (反二清: the platform never
// holds the money); this method only credits the book balance + gift and
// appends the recharge audit log. The gift is computed internally from the
// recharge-tier schedule (GiftForAmount) — it is NEVER trusted from the caller.
//
// Steps: EnforceTenant; amountCents>0 (else errcode.ErrParam); member exists
// (ErrNotFound incl. cross-tenant) and is active (else ErrMemberBlocked);
// rechargeAtomic enforces the regional per-card cap (breach →
// errcode.ErrBalanceOver, balance unchanged) and credits amount+gift.
func (s *Service) Recharge(ctx context.Context, in RechargeInput) (*RechargeResult, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if in.AmountCents <= 0 {
		return nil, errcode.ErrParam
	}

	// The member must belong to THIS tenant and be active.
	m, err := s.repo.GetMember(ctx, tenantID, in.MemberID)
	if err != nil {
		return nil, err // ErrNotFound (incl. cross-tenant)
	}
	if m.Status != memberStatusActive {
		return nil, ErrMemberBlocked
	}

	gift := GiftForAmount(in.AmountCents)
	now := s.now()
	acc, err := s.repo.rechargeAtomic(tenantID, in.MemberID, now, in.AmountCents, gift, in.RefPaymentID, in.OperatorID)
	if err != nil {
		return nil, err // errcode.ErrBalanceOver (cap) / ErrNotFound
	}
	return &RechargeResult{
		MemberID:       in.MemberID,
		BalanceCents:   acc.BalanceCents,
		GiftCents:      acc.GiftCents,
		RechargedCents: in.AmountCents,
		GiftedCents:    gift,
	}, nil
}

// SetRegionCap sets the per-card stored-value ceiling for a member's account
// (M4-S1 BE-B-12). The main thread seeds it from the tenant's province at
// onboarding; admin/compliance staff may adjust it later. Cross-tenant or
// absent account → ErrNotFound.
func (s *Service) SetRegionCap(ctx context.Context, memberID, regionCapCents int64) error {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return err
	}
	return s.repo.UpdateAccountRegionCap(ctx, tenantID, memberID, regionCapCents)
}
