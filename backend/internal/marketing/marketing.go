// Package marketing implements the coupon-template + member-coupon domain
// (BE-B-02, CONTRACT §10.3 / tech plan §4.2). A coupon template (Coupon) is a
// tenant-scoped marketing instrument of one of four types (满减 / 折扣 / 代金 /
// 赠品); a member-coupon (MemberCoupon) is one issued instance in a member's
// wallet, carrying the source and strategy_id attribution that lets the AI
// engine close the effect-feedback loop (券核销率 → 策略归因, ch04 §4.2.1).
//
// Every repository and service method resolves the tenant from ctx via
// multitenancy.EnforceTenant — tenant is NEVER trusted from a request field.
// The package is infrastructure-light and anti-corruption-clean: it imports
// neither membership, margin, nor catalog. Member existence is checked through
// the injected MemberLookuper interface; the gross-margin guardrail is an
// optional MarginGuard hook (AI-issued coupons will wire it in M3). Both are
// adapted at the composition root (cmd/api/main.go).
package marketing

import (
	"context"
	"errors"
	"time"
)

// Coupon is a tenant-scoped coupon template. Status follows the state machine
// draft → active ↔ paused → ended (CONTRACT §10.3). TotalQuota=0 means
// unlimited issuance; Issued tracks how many member-coupon instances have been
// created against the quota.
type Coupon struct {
	ID, TenantID, StoreID int64
	Name                  string
	Type                  string // full_reduction|discount|cash|gift
	Params                CouponParams
	ValidFrom, ValidTo    time.Time
	TotalQuota            int64 // 0 = 无限发行
	Issued                int64
	Status                string // draft|active|paused|ended
	CreatedBy             int64
	CreatedAt, UpdatedAt  time.Time
}

// CouponParams holds the elastic parameters (stored as JSONB in PostgreSQL).
// Fields are consumed according to Type:
//
//	full_reduction: ThresholdCents + OffCents (满 threshold 减 off)
//	discount:       DiscountPermille (‰, 850 = 85% = 8.5折)
//	cash:           OffCents (代金直减, 无门槛)
//	gift:           GiftProductID (赠品; M2 不校验赠品成本)
type CouponParams struct {
	ThresholdCents   int64 `json:"threshold_cents,omitempty"`
	OffCents         int64 `json:"off_cents,omitempty"`
	DiscountPermille int64 `json:"discount_permille,omitempty"` // 0-1000
	GiftProductID    int64 `json:"gift_product_id,omitempty"`
}

// MemberCoupon is one issued coupon instance in a member's wallet. The
// source + strategy_id pair is the AI attribution link (ch04 §4.2.1): an
// AI-strategy-issued coupon carries strategy_id so its redemption rate can be
// traced back to the strategy item that suggested it.
type MemberCoupon struct {
	ID, TenantID, MemberID, CouponID int64
	Status                           string // unused|used|expired|revoked
	Source                           string // ai_strategy|manual|groupbuy|newmember
	StrategyID                       int64  // 归因(0=非AI来源)
	IssuedAt                         time.Time
	UsedAt                           *time.Time
	ExpireAt                         *time.Time // nil=用 coupon.ValidTo
	RefOrderID                       int64
}

// Sentinel errors. Cross-tenant access is reported as ErrNotFound (absence and
// foreign ownership are indistinguishable by design — ARCH-01).
var (
	ErrNotFound       = errors.New("marketing: coupon not found")
	ErrQuotaExhausted = errors.New("marketing: coupon quota exhausted")
	ErrCouponInactive = errors.New("marketing: coupon not active")
	ErrCouponExpired  = errors.New("marketing: member coupon expired")
	ErrCouponUsed     = errors.New("marketing: member coupon already used")
)

// Coupon type / status / source constants.
const (
	TypeFullReduction = "full_reduction"
	TypeDiscount      = "discount"
	TypeCash          = "cash"
	TypeGift          = "gift"

	StatusDraft  = "draft"
	StatusActive = "active"
	StatusPaused = "paused"
	StatusEnded  = "ended"

	MCStatusUnused  = "unused"
	MCStatusUsed    = "used"
	MCStatusExpired = "expired"
	MCStatusRevoked = "revoked"

	SourceAIStrategy = "ai_strategy"
	SourceManual     = "manual"
	SourceGroupbuy   = "groupbuy"
	SourceNewmember  = "newmember"
)

// CouponFilter narrows a coupon listing. Zero-value fields are not applied.
type CouponFilter struct {
	StoreID int64
	Status  string // 空不过滤
	Type    string // 空不过滤
	Limit   int    // <=0 默认 50
	Offset  int
}

// CouponRepo persists coupon templates and member-coupon instances. The real
// implementation will be pgx-backed (coupons / member_coupons tables with
// tenant_id row isolation); an in-memory implementation covers dev and tests.
//
// IssueToMember and RedeemAtomic are the two atomic critical sections: the
// in-memory implementation serialises them under a single mutex; the pgx
// implementation will wrap them in real transactions. Every method treats a
// record owned by another tenant as absent (ErrNotFound).
type CouponRepo interface {
	CreateCoupon(ctx context.Context, c *Coupon) (*Coupon, error)
	GetCoupon(ctx context.Context, tenantID, id int64) (*Coupon, error)
	GetByName(ctx context.Context, tenantID int64, name string) (*Coupon, error)
	UpdateCoupon(ctx context.Context, c *Coupon) (*Coupon, error)
	ListCoupons(ctx context.Context, tenantID int64, f CouponFilter) ([]*Coupon, error)

	// IssueToMember atomically validates the coupon (active, within valid
	// window, under quota) and creates one member-coupon instance. source /
	// strategyID carry the attribution; expireAt nil falls back to
	// coupon.ValidTo. The coupon's Issued counter is incremented. Any
	// precondition failure returns the matching sentinel with no state change.
	IssueToMember(ctx context.Context, tenantID, memberID, couponID int64, source string, strategyID int64, expireAt *time.Time, now time.Time) (*MemberCoupon, error)

	GetMemberCoupon(ctx context.Context, tenantID, id int64) (*MemberCoupon, error)
	ListMemberCoupons(ctx context.Context, tenantID, memberID int64, status string) ([]*MemberCoupon, error)

	// ListByStrategy returns all member-coupons attributed to one strategy
	// (member_coupons.strategy_id, the AI attribution link). status filters by
	// member-coupon status (unused|used|expired|revoked); "" returns all. Used
	// by the M3-S3 attribution engine (CouponStats) to compute coupon_redeem
	// (redeemed/sent) per strategy. Cross-tenant records are never returned.
	ListByStrategy(ctx context.Context, tenantID, strategyID int64, status string) ([]*MemberCoupon, error)

	// RedeemAtomic atomically transitions a member-coupon unused→used and stamps
	// UsedAt / RefOrderID. A non-unused coupon yields ErrCouponUsed; an expired
	// one yields ErrCouponExpired. No state changes on failure.
	RedeemAtomic(ctx context.Context, tenantID, id, refOrderID int64, now time.Time) (*MemberCoupon, error)
}

// MemberLookuper is the anti-corruption port for member existence (marketing
// must not import membership). The main thread adapts *membership.Service to it.
type MemberLookuper interface {
	Exists(ctx context.Context, tenantID, memberID int64) (bool, error)
}

// MarginGuard is the optional gross-margin guardrail hook (AI-02 seed). When
// wired, Issue runs ValidateCoupon before issuing; a rejection aborts the issue.
// nil means no margin check (manual issuance is allowed without it).
type MarginGuard interface {
	ValidateCoupon(ctx context.Context, c *Coupon) error
}

// CreateCouponInput carries the caller-supplied template fields. TenantID is
// NOT trusted from the caller (it comes from ctx); StoreID falls back to the
// ctx store when zero.
type CreateCouponInput struct {
	StoreID    int64
	Name       string
	Type       string
	Params     CouponParams
	ValidFrom  time.Time
	ValidTo    time.Time
	TotalQuota int64
	CreatedBy  int64
}

// IssueInput is one issuance: which member gets which coupon, with attribution.
type IssueInput struct {
	MemberID   int64
	CouponID   int64
	Source     string // 空=manual
	StrategyID int64
	ExpireAt   *time.Time // nil=用 coupon.ValidTo
}

// RedeemInput is one redemption at the till.
type RedeemInput struct {
	MemberCouponID int64
	RefOrderID     int64
}
