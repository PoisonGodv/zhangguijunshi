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
	Status                string // active|blocked
	CreatedAt, UpdatedAt  time.Time
}

// BalanceAccount is a member's stored-value (储值) book account. Exactly one
// account exists per (tenant, member); Join creates it with a zero balance.
// BalanceCents is the real paid-in balance, GiftCents the bonus/granted part.
type BalanceAccount struct {
	ID, TenantID, MemberID  int64
	BalanceCents, GiftCents int64
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
