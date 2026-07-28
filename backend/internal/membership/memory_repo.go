package membership

// MemoryMemberRepo is an in-memory, concurrency-safe MemberRepo for dev and
// tests. It enforces the same tenant-isolation contract as the production pgx
// repo: a read or mutation scoped to tenant A never sees tenant B's members /
// accounts — cross-tenant access returns ErrNotFound, indistinguishable from
// absence. All reads return defensive copies so callers cannot mutate stored
// records.
//
// The repo keeps the member table keyed by id plus two unique-idempotency
// indexes ((tenant, phone_hash) and (tenant, wx_openid)), the account table
// keyed by id plus a unique (tenant, member_id) index, and two append-only
// audit-log slices (points / balance). Redeem's "deduct balance + accrue
// points + update member" critical section is serialised by the repo's single
// mutex (redeemAtomic), giving the in-memory flow its atomicity (the pgx
// implementation will use a real transaction).

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
)

// defaultListMembersLimit is used when MemberFilter.Limit <= 0.
const defaultListMembersLimit = 500

// pointsFlow is one member_points_log row (audit, no soft delete).
type pointsFlow struct {
	id          int64
	tenantID    int64
	memberID    int64
	typ         string
	delta       int64
	pointsAfter int64
	refOrderID  int64
	createdAt   time.Time
}

// balanceFlow is one member_balance_log row (audit, no soft delete).
type balanceFlow struct {
	id           int64
	tenantID     int64
	memberID     int64
	accountID    int64
	typ          string
	amountCents  int64
	balanceAfter int64
	refOrderID   int64
	createdAt    time.Time
}

// MemoryMemberRepo stores members/accounts/logs in maps and slices.
type MemoryMemberRepo struct {
	mu sync.RWMutex

	membersByID      map[int64]*Member
	memberIDByPhone  map[string]int64 // key: tenant|phone_hash
	memberIDByOpenID map[string]int64 // key: tenant|wx_openid

	accountsByID     map[int64]*BalanceAccount
	accountIDByTuple map[string]int64 // key: tenant|member_id

	pointsLog  []pointsFlow
	balanceLog []balanceFlow

	nextMemberID  atomic.Int64
	nextAccountID atomic.Int64
	nextLogID     atomic.Int64
}

// NewMemoryMemberRepo constructs an empty repo. Member ids start at 9000 and
// account ids at 9500 so they read realistically in logs and never collide
// with other domains' in-memory id ranges.
func NewMemoryMemberRepo() *MemoryMemberRepo {
	r := &MemoryMemberRepo{
		membersByID:      make(map[int64]*Member),
		memberIDByPhone:  make(map[string]int64),
		memberIDByOpenID: make(map[string]int64),
		accountsByID:     make(map[int64]*BalanceAccount),
		accountIDByTuple: make(map[string]int64),
	}
	r.nextMemberID.Store(9000)
	r.nextAccountID.Store(9500)
	return r
}

// identityKey builds a composite tenant|identity index key.
func identityKey(tenantID int64, identity string) string {
	return strconv.FormatInt(tenantID, 10) + "|" + identity
}

// accountKey builds the composite tenant|member account index key.
func accountKey(tenantID, memberID int64) string {
	return strconv.FormatInt(tenantID, 10) + "|" + strconv.FormatInt(memberID, 10)
}

// CreateMember implements MemberRepo. It deep-copies the member, allocates an
// id, and records the (tenant, phone_hash) / (tenant, wx_openid) idempotency
// indexes for whichever identities are non-empty.
func (r *MemoryMemberRepo) CreateMember(_ context.Context, m *Member) (*Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored := cloneMember(m)
	stored.ID = r.nextMemberID.Add(1)
	r.membersByID[stored.ID] = stored
	if stored.PhoneHash != "" {
		r.memberIDByPhone[identityKey(stored.TenantID, stored.PhoneHash)] = stored.ID
	}
	if stored.WxOpenID != "" {
		r.memberIDByOpenID[identityKey(stored.TenantID, stored.WxOpenID)] = stored.ID
	}
	return cloneMember(stored), nil
}

// GetMember implements MemberRepo. Cross-tenant ids return ErrNotFound.
func (r *MemoryMemberRepo) GetMember(_ context.Context, tenantID, id int64) (*Member, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.membersByID[id]
	if !ok || m.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneMember(m), nil
}

// UpdateMember implements MemberRepo. It rewrites the stored member (matched by
// ID + tenant) and refreshes the identity indexes. Cross-tenant ids return
// ErrNotFound.
func (r *MemoryMemberRepo) UpdateMember(_ context.Context, m *Member) (*Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur, ok := r.membersByID[m.ID]
	if !ok || cur.TenantID != m.TenantID {
		return nil, ErrNotFound
	}
	// Drop stale identity index entries if the identities changed.
	if cur.PhoneHash != "" && cur.PhoneHash != m.PhoneHash {
		delete(r.memberIDByPhone, identityKey(cur.TenantID, cur.PhoneHash))
	}
	if cur.WxOpenID != "" && cur.WxOpenID != m.WxOpenID {
		delete(r.memberIDByOpenID, identityKey(cur.TenantID, cur.WxOpenID))
	}

	stored := cloneMember(m)
	r.membersByID[stored.ID] = stored
	if stored.PhoneHash != "" {
		r.memberIDByPhone[identityKey(stored.TenantID, stored.PhoneHash)] = stored.ID
	}
	if stored.WxOpenID != "" {
		r.memberIDByOpenID[identityKey(stored.TenantID, stored.WxOpenID)] = stored.ID
	}
	return cloneMember(stored), nil
}

// UpsertAccount implements MemberRepo. It creates the account for
// (a.TenantID, a.MemberID) when absent, otherwise replaces the stored record in
// place (id preserved). The caller scopes a.TenantID to the enforced tenant.
func (r *MemoryMemberRepo) UpsertAccount(_ context.Context, a *BalanceAccount) (*BalanceAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := accountKey(a.TenantID, a.MemberID)
	if id, ok := r.accountIDByTuple[key]; ok {
		stored := cloneAccount(a)
		stored.ID = id
		r.accountsByID[id] = stored
		return cloneAccount(stored), nil
	}

	stored := cloneAccount(a)
	stored.ID = r.nextAccountID.Add(1)
	r.accountsByID[stored.ID] = stored
	r.accountIDByTuple[key] = stored.ID
	return cloneAccount(stored), nil
}

// GetAccount implements MemberRepo. ErrNotFound when no account exists for
// (tenantID, memberID).
func (r *MemoryMemberRepo) GetAccount(_ context.Context, tenantID, memberID int64) (*BalanceAccount, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.accountIDByTuple[accountKey(tenantID, memberID)]
	if !ok {
		return nil, ErrNotFound
	}
	a, ok := r.accountsByID[id]
	if !ok || a.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneAccount(a), nil
}

// AppendPointsLog implements MemberRepo; it appends one immutable points-flow
// row scoped to the tenant.
func (r *MemoryMemberRepo) AppendPointsLog(_ context.Context, tenantID, memberID int64, typ string, delta, pointsAfter, refOrderID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pointsLog = append(r.pointsLog, pointsFlow{
		id:          r.nextLogID.Add(1),
		tenantID:    tenantID,
		memberID:    memberID,
		typ:         typ,
		delta:       delta,
		pointsAfter: pointsAfter,
		refOrderID:  refOrderID,
		createdAt:   time.Now(),
	})
	return nil
}

// AppendBalanceLog implements MemberRepo; it appends one immutable
// balance-flow row scoped to the tenant.
func (r *MemoryMemberRepo) AppendBalanceLog(_ context.Context, tenantID, memberID, accountID int64, typ string, amountCents, balanceAfter, refOrderID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.balanceLog = append(r.balanceLog, balanceFlow{
		id:           r.nextLogID.Add(1),
		tenantID:     tenantID,
		memberID:     memberID,
		accountID:    accountID,
		typ:          typ,
		amountCents:  amountCents,
		balanceAfter: balanceAfter,
		refOrderID:   refOrderID,
		createdAt:    time.Now(),
	})
	return nil
}

// ListMembers implements MemberRepo (M3-S2). It scans the member table under a
// read lock and applies the filter dimensions in turn: SleepBefore (members who
// never visited count as sleeping), MinPoints, BirthdayOn (month+day match), and
// Limit (default 500). Only members of tenantID are ever visible. Results are
// defensive copies ordered by member id (deterministic for tests).
func (r *MemoryMemberRepo) ListMembers(_ context.Context, tenantID int64, f MemberFilter) ([]*Member, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListMembersLimit
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Collect candidate ids in ascending order so output is deterministic.
	ids := make([]int64, 0)
	for id, m := range r.membersByID {
		if m.TenantID != tenantID {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]*Member, 0, limit)
	for _, id := range ids {
		m := r.membersByID[id]
		if m.TenantID != tenantID { // defence-in-depth
			continue
		}
		if f.SleepBefore != nil {
			// A member who never visited is treated as sleeping (matches).
			if m.LastVisitAt != nil && !m.LastVisitAt.Before(*f.SleepBefore) {
				continue
			}
		}
		if f.MinPoints > 0 && m.Points < f.MinPoints {
			continue
		}
		if f.BirthdayOn != nil {
			if m.Birthday == nil {
				continue
			}
			if m.Birthday.Month() != f.BirthdayOn.Month() || m.Birthday.Day() != f.BirthdayOn.Day() {
				continue
			}
		}
		out = append(out, cloneMember(m))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// findMemberIDByPhone implements MemberRepo.
func (r *MemoryMemberRepo) findMemberIDByPhone(tenantID int64, phoneHash string) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.memberIDByPhone[identityKey(tenantID, phoneHash)]
	return id, ok
}

// findMemberIDByOpenID implements MemberRepo.
func (r *MemoryMemberRepo) findMemberIDByOpenID(tenantID int64, openID string) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.memberIDByOpenID[identityKey(tenantID, openID)]
	return id, ok
}

// redeemAtomic implements MemberRepo. It runs the whole Redeem critical section
// under the repo's single write lock: re-validate the member, deduct stored
// balance + append the consume flow when consumeCents>0 (ErrInsufficientBalance
// with NO state changed when it would overdraw), accrue points + append the
// earn flow when earnPoints>0, then touch LastVisitAt/UpdatedAt.
func (r *MemoryMemberRepo) redeemAtomic(tenantID, memberID int64, now time.Time, consumeCents, earnPoints, refOrderID int64) (*redeemOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	m, ok := r.membersByID[memberID]
	if !ok || m.TenantID != tenantID {
		return nil, ErrNotFound
	}

	// Account is only needed when deducting stored value.
	var acc *BalanceAccount
	if consumeCents > 0 {
		accID, ok := r.accountIDByTuple[accountKey(tenantID, memberID)]
		if !ok {
			return nil, ErrNotFound
		}
		acc = r.accountsByID[accID]
		if acc == nil || acc.TenantID != tenantID {
			return nil, ErrNotFound
		}
		if acc.BalanceCents < consumeCents {
			return nil, ErrInsufficientBalance // 不得透支, 不落任何状态
		}
	}

	// Deduct stored balance + consume flow.
	if consumeCents > 0 {
		acc.BalanceCents -= consumeCents
		acc.UpdatedAt = now
		r.balanceLog = append(r.balanceLog, balanceFlow{
			id:           r.nextLogID.Add(1),
			tenantID:     tenantID,
			memberID:     memberID,
			accountID:    acc.ID,
			typ:          "consume",
			amountCents:  -consumeCents,
			balanceAfter: acc.BalanceCents,
			refOrderID:   refOrderID,
			createdAt:    now,
		})
	}

	// Accrue points + earn flow.
	if earnPoints > 0 {
		m.Points += earnPoints
		r.pointsLog = append(r.pointsLog, pointsFlow{
			id:          r.nextLogID.Add(1),
			tenantID:    tenantID,
			memberID:    memberID,
			typ:         "earn",
			delta:       earnPoints,
			pointsAfter: m.Points,
			refOrderID:  refOrderID,
			createdAt:   now,
		})
	}

	// Touch last-visit + update timestamp.
	lv := now
	m.LastVisitAt = &lv
	m.UpdatedAt = now

	out := &redeemOutcome{member: cloneMember(m)}
	if acc != nil {
		out.account = cloneAccount(acc)
	}
	return out, nil
}

// rechargeAtomic implements MemberRepo (M4-S1 BE-B-12). It runs the whole
// Recharge critical section under the repo's single write lock: re-load the
// account, enforce the regional per-card cap (errcode.ErrBalanceOver with NO
// state changed on breach), credit BalanceCents+=amount + GiftCents+=gift, and
// append the recharge balance-flow. The fund flow is settled by WeChat Pay
// directly to the sub-merchant (反二清: the platform never holds the money); this
// method books the balance only. operatorID is accepted for the audit contract
// but not yet persisted (the M1-S2 balance-flow row has no operator column).
func (r *MemoryMemberRepo) rechargeAtomic(tenantID, memberID int64, now time.Time, amountCents, giftCents, refPaymentID, operatorID int64) (*BalanceAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	accID, ok := r.accountIDByTuple[accountKey(tenantID, memberID)]
	if !ok {
		return nil, ErrNotFound
	}
	acc := r.accountsByID[accID]
	if acc == nil || acc.TenantID != tenantID {
		return nil, ErrNotFound
	}

	// Regional per-card stored-value cap (0 = uncapped). A breach leaves ALL
	// state untouched (no balance credit, no audit row).
	if acc.RegionCapCents > 0 && acc.BalanceCents+amountCents+giftCents > acc.RegionCapCents {
		return nil, errcode.ErrBalanceOver
	}

	acc.BalanceCents += amountCents
	acc.GiftCents += giftCents
	acc.UpdatedAt = now
	r.balanceLog = append(r.balanceLog, balanceFlow{
		id:           r.nextLogID.Add(1),
		tenantID:     tenantID,
		memberID:     memberID,
		accountID:    acc.ID,
		typ:          "recharge",
		amountCents:  amountCents,
		balanceAfter: acc.BalanceCents,
		refOrderID:   refPaymentID,
		createdAt:    now,
	})
	_ = operatorID // reserved for future operator-audit column
	return cloneAccount(acc), nil
}

// UpdateAccountRegionCap implements MemberRepo (M4-S1 BE-B-12). It sets
// RegionCapCents on the (tenantID, memberID) account (admin/compliance; the
// main thread seeds it from the tenant's province). Cross-tenant or absent
// account → ErrNotFound (ARCH-01: absence and foreign ownership are
// indistinguishable).
func (r *MemoryMemberRepo) UpdateAccountRegionCap(_ context.Context, tenantID, memberID, regionCapCents int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	accID, ok := r.accountIDByTuple[accountKey(tenantID, memberID)]
	if !ok {
		return ErrNotFound
	}
	acc := r.accountsByID[accID]
	if acc == nil || acc.TenantID != tenantID {
		return ErrNotFound
	}
	acc.RegionCapCents = regionCapCents
	return nil
}

// --- test introspection helpers (not part of MemberRepo) ---

// pointsLogCount reports how many points-flow rows exist for (tenantID,
// memberID).
func (r *MemoryMemberRepo) pointsLogCount(tenantID, memberID int64) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, f := range r.pointsLog {
		if f.tenantID == tenantID && f.memberID == memberID {
			n++
		}
	}
	return n
}

// balanceLogCount reports how many balance-flow rows exist for (tenantID,
// memberID).
func (r *MemoryMemberRepo) balanceLogCount(tenantID, memberID int64) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, f := range r.balanceLog {
		if f.tenantID == tenantID && f.memberID == memberID {
			n++
		}
	}
	return n
}

// lastPointsAfter returns the pointsAfter of the most recent points-flow row
// for (tenantID, memberID); ok=false when none.
func (r *MemoryMemberRepo) lastPointsAfter(tenantID, memberID int64) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := len(r.pointsLog) - 1; i >= 0; i-- {
		f := r.pointsLog[i]
		if f.tenantID == tenantID && f.memberID == memberID {
			return f.pointsAfter, true
		}
	}
	return 0, false
}

// lastBalanceAfter returns the balanceAfter of the most recent balance-flow row
// for (tenantID, memberID); ok=false when none.
func (r *MemoryMemberRepo) lastBalanceAfter(tenantID, memberID int64) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := len(r.balanceLog) - 1; i >= 0; i-- {
		f := r.balanceLog[i]
		if f.tenantID == tenantID && f.memberID == memberID {
			return f.balanceAfter, true
		}
	}
	return 0, false
}

// lastBalanceFlow returns the typ + amountCents of the most recent balance-flow
// row for (tenantID, memberID); ok=false when none. Used by Recharge tests to
// assert a "recharge" row was appended with the credited amount.
func (r *MemoryMemberRepo) lastBalanceFlow(tenantID, memberID int64) (typ string, amountCents int64, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := len(r.balanceLog) - 1; i >= 0; i-- {
		f := r.balanceLog[i]
		if f.tenantID == tenantID && f.memberID == memberID {
			return f.typ, f.amountCents, true
		}
	}
	return "", 0, false
}

// cloneMember returns a deep copy of m (LastVisitAt / Birthday pointers
// re-pointed at fresh time values) so internal state is never aliased.
func cloneMember(m *Member) *Member {
	if m == nil {
		return nil
	}
	cp := *m
	if m.LastVisitAt != nil {
		t := *m.LastVisitAt
		cp.LastVisitAt = &t
	}
	if m.Birthday != nil {
		t := *m.Birthday
		cp.Birthday = &t
	}
	return &cp
}

// cloneAccount returns a copy of a so internal state is never aliased.
func cloneAccount(a *BalanceAccount) *BalanceAccount {
	if a == nil {
		return nil
	}
	cp := *a
	return &cp
}
