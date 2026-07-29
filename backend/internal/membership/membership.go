// Package membership implements the member-profile + stored-balance + points
// domain (tech plan §5.3.4, WBS BE-B-01 member profile and BE-A-07 dynamic
// member-code redemption). A member belongs to exactly one tenant (merchant);
// every repository and service method resolves the tenant from ctx via
// multitenancy.EnforceTenant — tenant is NEVER trusted from a request field.
//
// The domain covers: idempotent member enrolment (Join) with an automatic
// zero-balance stored-value account, a short-lived dynamic member code
// (MemberCoder) that identifies a member at the till, and a redemption
// (Redeem) that atomically deducts stored balance and/or accrues points while
// appending the audit logs. M1-S2 only moves BOOK balance + member_balance_log
// (no real fund clearing — that is M4); points are book-keeping only.
//
// The package is deliberately infrastructure-light: MemberRepo is an interface
// with a concurrency-safe in-memory implementation, and MemberCoder is an
// interface with an HMAC + server-side anti-replay in-memory implementation,
// so the whole flow runs in unit tests without PostgreSQL/Redis.
package membership

import (
	"context"
	"errors"
	"time"
)

// Member is a registered customer of one tenant. At least one of PhoneHash /
// WxOpenID is non-empty (Join enforces this); they act as the idempotency keys
// so a repeat enrolment with the same identity reuses the existing member.
type Member struct {
	ID, TenantID, StoreID int64
	PhoneHash, WxOpenID   string // 至少一个非空
	Name, Level           string // level: normal|vip|blackcard
	Points                int64
	TotalSpentCents       int64
	LastVisitAt           *time.Time
	Birthday              *time.Time // 可空, YYYY-MM-DD; M3-S2 signal 当日生日枚举用
	Status                string     // active|blocked
	CreatedAt, UpdatedAt  time.Time
}

// BalanceAccount is a member's stored-value (储值) book account. Exactly one
// account exists per (tenant, member); Join creates it with a zero balance.
// BalanceCents is the real paid-in balance, GiftCents the bonus/granted part.
// RegionCapCents is the per-card stored-value ceiling mandated by regional
// pre-paid-card compliance (e.g. Shanghai/Jiangsu); 0 means uncapped. Recharge
// enforces balance+amount+gift <= RegionCapCents (errcode.ErrBalanceOver on
// breach) so a single card never holds more stored value than the law allows.
type BalanceAccount struct {
	ID, TenantID, MemberID  int64
	BalanceCents, GiftCents int64
	RegionCapCents          int64 `json:"region_cap_cents"` // 地区单卡余额上限(0=不限)
	CreatedAt, UpdatedAt    time.Time
}

// Sentinel errors. Cross-tenant access is reported as ErrNotFound (absence and
// foreign ownership are indistinguishable by design — ARCH-01).
var (
	ErrNotFound            = errors.New("membership: member not found")
	ErrInsufficientBalance = errors.New("membership: insufficient balance")
	ErrMemberBlocked       = errors.New("membership: member blocked")
	ErrCodeInvalid         = errors.New("membership: member code invalid or expired")
)

// MemberRepo persists members, their stored-value accounts, and the points /
// balance audit logs. The real implementation will be pgx-backed (members /
// member_balance_accounts / member_balance_log / member_points_log tables with
// tenant_id row isolation); an in-memory implementation covers dev and tests.
//
// Every method that reads or mutates tenant-scoped state takes the resolved
// tenantID and MUST treat a record owned by another tenant as absent.
type MemberRepo interface {
	// CreateMember persists a new member (id/timestamps allocated by the repo).
	CreateMember(ctx context.Context, m *Member) (*Member, error)

	// GetMember loads a member by id within tenantID. ErrNotFound when absent
	// or owned by another tenant.
	GetMember(ctx context.Context, tenantID, id int64) (*Member, error)

	// UpdateMember rewrites a member (matched by ID + tenant). ErrNotFound when
	// absent or owned by another tenant.
	UpdateMember(ctx context.Context, m *Member) (*Member, error)

	// UpsertAccount creates-or-replaces the stored-value account for
	// (a.TenantID, a.MemberID). Used by Join to open the zero-balance account.
	UpsertAccount(ctx context.Context, a *BalanceAccount) (*BalanceAccount, error)

	// GetAccount loads the account for (tenantID, memberID). ErrNotFound when
	// no account exists (or it belongs to another tenant).
	GetAccount(ctx context.Context, tenantID, memberID int64) (*BalanceAccount, error)

	// AppendPointsLog appends one points-flow row. typ is e.g. "earn"/"spend";
	// delta is the signed change; pointsAfter is the resulting member points.
	AppendPointsLog(ctx context.Context, tenantID, memberID int64, typ string, delta, pointsAfter, refOrderID int64) error

	// AppendBalanceLog appends one balance-flow row. typ is e.g.
	// "consume"/"recharge"; amountCents is the signed change; balanceAfter is
	// the resulting account balance.
	AppendBalanceLog(ctx context.Context, tenantID, memberID, accountID int64, typ string, amountCents, balanceAfter, refOrderID int64) error

	// ListPointsLog returns the points-flow rows for (tenantID, memberID), newest
	// first. limit<=0 defaults to 50 (FE-MP-02 CONTRACT §16.3(8): C 端积分明细).
	// Cross-tenant rows are never returned. PointsLog is the public read shape.
	ListPointsLog(ctx context.Context, tenantID, memberID int64, limit int) ([]PointsLog, error)

	// ListMembers enumerates members within tenantID matching f (M3-S2 signal
	// real enumeration). Empty filter fields are not filtering; Limit<=0 defaults
	// to 500. Cross-tenant members are never returned.
	ListMembers(ctx context.Context, tenantID int64, f MemberFilter) ([]*Member, error)

	// ListMembersPage returns one page of tenantID's members matching f plus
	// the total matching count (FE-MP-02 / FE-ADMIN boss list, CONTRACT §17.11).
	// Limit<=0 defaults to 20; Offset<=0 starts at 0. Total is independent of
	// Offset/Limit. Cross-tenant members are never returned.
	ListMembersPage(ctx context.Context, tenantID int64, f MemberFilter) (ListMembersResult, error)

	// findMemberIDByPhone returns the id of the member registered under
	// (tenantID, phoneHash); ok=false when none. Drives Join idempotency.
	findMemberIDByPhone(tenantID int64, phoneHash string) (id int64, ok bool)

	// findMemberIDByOpenID returns the id of the member registered under
	// (tenantID, openID); ok=false when none. Drives Join idempotency.
	findMemberIDByOpenID(tenantID int64, openID string) (id int64, ok bool)

	// redeemAtomic performs the Redeem critical section — load account, check
	// balance, deduct, append consume log; accrue points, append earn log;
	// update member Points/LastVisitAt — as one atomic step. It returns the
	// post-state so the service can build RedeemResult without a racy re-read.
	// Implementations MUST change NO state when the balance would overdraw
	// (return ErrInsufficientBalance).
	redeemAtomic(tenantID, memberID int64, now time.Time, consumeCents, earnPoints, refOrderID int64) (*redeemOutcome, error)

	// rechargeAtomic performs the Recharge critical section (M4-S1 BE-B-12) —
	// load account (absent → ErrNotFound), enforce the regional per-card cap
	// (balance+amount+gift <= RegionCapCents; 0 = uncapped; breach →
	// errcode.ErrBalanceOver with NO state change), then credit
	// BalanceCents+=amount + GiftCents+=gift, append the recharge balance-flow,
	// and return the updated account — as one atomic step. The fund flow itself
	// is settled by WeChat Pay directly to the sub-merchant (反二清: the platform
	// never holds the money); this method only books the balance.
	rechargeAtomic(tenantID, memberID int64, now time.Time, amountCents, giftCents, refPaymentID, operatorID int64) (*BalanceAccount, error)

	// UpdateAccountRegionCap sets RegionCapCents for the (tenantID, memberID)
	// account (admin / compliance; the main thread seeds it from the tenant's
	// province). Cross-tenant or absent account → ErrNotFound.
	UpdateAccountRegionCap(ctx context.Context, tenantID, memberID, regionCapCents int64) error
}

// redeemOutcome carries the post-redemption account/member state from
// redeemAtomic back to the service.
type redeemOutcome struct {
	account *BalanceAccount // nil when no stored value was deducted
	member  *Member
}

// MemberCoder issues and resolves short-lived dynamic member codes (the QR /
// number a member shows at the till). It is an anti-corruption boundary: the
// service depends only on this interface, not on any concrete signing scheme.
// Resolve MUST return ErrCodeInvalid for a forged or expired code.
type MemberCoder interface {
	// Issue returns a fresh code identifying memberID within tenantID, valid
	// for ttl.
	Issue(ctx context.Context, tenantID, memberID int64, ttl time.Duration) (string, error)

	// Resolve validates code within tenantID and returns the memberID it
	// identifies. Expired or forged codes yield ErrCodeInvalid.
	Resolve(ctx context.Context, tenantID int64, code string) (memberID int64, err error)
}

// JoinInput carries the caller-supplied enrolment fields. TenantID is NOT
// trusted from the caller (it comes from ctx); StoreID falls back to the ctx
// store when zero.
type JoinInput struct {
	StoreID                   int64
	PhoneHash, WxOpenID, Name string
}

// RedeemInput is one redemption at the till: the dynamic code plus the
// optional stored-value deduction (ConsumeCents) and/or points accrual
// (EarnPoints). At least one of ConsumeCents / EarnPoints is positive.
type RedeemInput struct {
	StoreID, OperatorID, RefOrderID int64
	Code                            string
	ConsumeCents                    int64 // >0 时扣储值
	EarnPoints                      int64 // >0 时累积分
}

// RedeemResult reports the member's post-redemption state.
type RedeemResult struct {
	MemberID, Points, BalanceCents int64
	ConsumedCents                  int64
}

// RechargeTier is one stored-value recharge bracket: recharge AmountCents earns
// GiftCents bonus (plan §7.4 "充200送20、充500送80"). The in-code seed table
// (rechargeTiers) is the single source of truth for the gift schedule.
type RechargeTier struct {
	AmountCents int64
	GiftCents   int64
}

// rechargeTiers is the in-code recharge-gift schedule (M4-S1 BE-B-12 seed). A
// recharge qualifies for every tier whose AmountCents it meets or exceeds;
// GiftForAmount returns the best (highest) qualifying gift. Ordered ascending
// so the iteration in GiftForAmount naturally picks the largest match.
var rechargeTiers = []RechargeTier{
	{AmountCents: 20000, GiftCents: 2000},   // 充200送20
	{AmountCents: 50000, GiftCents: 8000},   // 充500送80
	{AmountCents: 100000, GiftCents: 20000}, // 充1000送200
}

// GiftForAmount returns the bonus gift for a recharge of amountCents, matching
// the best qualifying tier; an amount below every tier earns no gift (0). Pure
// function (no state), so it is directly unit-testable.
func GiftForAmount(amountCents int64) int64 {
	var gift int64
	for _, t := range rechargeTiers {
		if amountCents >= t.AmountCents && t.GiftCents > gift {
			gift = t.GiftCents
		}
	}
	return gift
}

// RechargeInput carries a stored-value recharge request (M4-S1 BE-B-12). The
// funds are settled by WeChat Pay directly to the sub-merchant (反二清: the
// platform never holds the money); Recharge only books the balance. MemberID is
// the member whose account is topped up; AmountCents is the real paid-in amount
// (the gift is computed internally via GiftForAmount, NOT trusted from caller);
// RefPaymentID optionally links the auditing payment.ID; OperatorID is the
// staff member who performed the top-up.
type RechargeInput struct {
	MemberID     int64
	AmountCents  int64
	RefPaymentID int64
	OperatorID   int64
}

// RechargeResult reports the post-recharge account state. BalanceCents /
// GiftCents are the resulting totals; RechargedCents / GiftedCents are this
// call's increments (echoed back so the caller can render "充200送20" receipts).
type RechargeResult struct {
	MemberID       int64
	BalanceCents   int64 // 入账后实付余额
	GiftCents      int64 // 入账后赠送余额
	RechargedCents int64 // 本次实付
	GiftedCents    int64 // 本次赠送(档位规则算)
}

// MemberFilter narrows a ListMembers query (M3-S2 signal real enumeration +
// FE-MP-02 boss membership list). TenantID is NOT part of the filter: it is
// resolved from ctx by every repo / service call (ARCH-01) and injected
// separately. A nil/zero field means "do not filter on this dimension".
type MemberFilter struct {
	// SleepBefore selects members whose LastVisitAt is before this value
	// (30-day-sleeping signal). A member who never visited (LastVisitAt nil) is
	// treated as sleeping and matches. Nil = do not filter.
	SleepBefore *time.Time
	// MinPoints selects members with Points >= MinPoints (high-value signal).
	// 0 = do not filter.
	MinPoints int64
	// BirthdayOn selects members whose Birthday month+day equals this value's
	// month+day (birthday-today signal). A member with nil Birthday never matches.
	// Nil = do not filter.
	BirthdayOn *time.Time
	// Level filters by member level (normal|vip|blackcard). Empty = do not
	// filter (FE-MP-02 boss list query). Memory repo applies it post-scan; the
	// pg repo applies it as a SQL equality predicate.
	Level string
	// Query is a loose substring match against the member's Name OR PhoneHash
	// (FE-MP-02 boss list ?q=). Empty = do not filter. The match is
	// case-sensitive substring (phone hash is a fixed string; names are short).
	Query string
	// Offset skips the first Offset matches (pager). <=0 = start at 0.
	Offset int
	// Limit caps the result size; <=0 defaults to 500 (M3-S2 signal) / 20
	// (boss list, when called via ListMembersPage).
	Limit int
}

// PointsLog is one member_points_log row, the C-side 积分明细 read shape
// (FE-MP-02 CONTRACT §16.3(8)). It is the public mirror of the unexported
// pointsFlow audit row: typ (earn/redeem/expire/adjust), the signed delta,
// the resulting pointsAfter, the optional ref_order_id and the created_at.
type PointsLog struct {
	ID          int64     `json:"id"`
	TenantID    int64     `json:"tenant_id"`
	MemberID    int64     `json:"member_id"`
	Type        string    `json:"type"` // earn | redeem | expire | adjust
	Delta       int64     `json:"delta"`
	PointsAfter int64     `json:"points_after"`
	RefOrderID  int64     `json:"ref_order_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// ListMembersResult carries a ListMembers page plus the total count of
// matching members (FE-MP-02 / FE-ADMIN, CONTRACT §17.11 boss membership list).
// Total is independent of Limit/Offset so the pager can render "共 N 条".
type ListMembersResult struct {
	Items []*Member `json:"items"`
	Total int64     `json:"total"`
}
