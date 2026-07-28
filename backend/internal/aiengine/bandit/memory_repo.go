package bandit

// memory_repo.go implements MemoryRepo — the dev/test Arm store (CONTRACT
// §14.3.3 / §14.5.4 dual implementation). It keeps arms in a nested map
// keyed by (tenantID, ruleID), guarded by a sync.Mutex so concurrent
// Select/Update from the pipeline and the cron are -race safe. Reads return
// defensive copies so a caller can never mutate a stored arm in place. An
// unseen rule returns a zero-value Arm{RuleID, Weight:0.5} neutral prior
// (cold-start-safe, §14.5.8), and cross-tenant reads return that same
// neutral prior — another tenant's weights are never exposed.

import (
	"context"
	"sync"
)

// armKey is the composite map key (tenant, rule). Tenant isolation is
// structural: an arm stored under tenant A is simply absent under tenant B.
type armKey struct {
	tenantID int64
	ruleID   string
}

// MemoryRepo is the in-memory Repo (CONTRACT §14.3.3). The zero value is NOT
// usable (the map would be nil); always construct via NewMemoryRepo.
type MemoryRepo struct {
	mu   sync.Mutex
	arms map[armKey]Arm
}

// NewMemoryRepo constructs an empty MemoryRepo.
func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{arms: make(map[armKey]Arm)}
}

// Get returns the arm for (tenantID, ruleID). An unseen rule returns a fresh
// neutral-prior arm &Arm{RuleID, Weight:0.5, Trials:0, RewardSum:0} (§14.3.3
// "未见规则 Get 返零值 Arm{Weight:0.5}"). Cross-tenant is the same path — the
// key simply misses — so no data leaks. The returned pointer is a copy.
func (r *MemoryRepo) Get(ctx context.Context, tenantID int64, ruleID string) (*Arm, error) {
	if r == nil {
		return &Arm{RuleID: ruleID, Weight: neutralPrior}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.arms[armKey{tenantID: tenantID, ruleID: ruleID}]
	if !ok {
		return &Arm{RuleID: ruleID, Weight: neutralPrior}, nil
	}
	cp := a
	return &cp, nil
}

// Put upserts arm under (tenantID, arm.RuleID) (§14.3.3). A nil arm is a
// no-op (the caller bug is swallowed rather than panicking, mirroring the
// repo's defensive style). The stored value is a copy of *arm.
func (r *MemoryRepo) Put(ctx context.Context, tenantID int64, arm *Arm) error {
	if r == nil || arm == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.arms[armKey{tenantID: tenantID, ruleID: arm.RuleID}] = *arm
	return nil
}

// All returns every arm belonging to tenantID (§14.3.3), in a stable order
// (sorted by RuleID) so route bias is deterministic across calls. A tenant
// with no arms returns an empty (non-nil) slice. Each element is a copy.
func (r *MemoryRepo) All(ctx context.Context, tenantID int64) ([]Arm, error) {
	out := []Arm{}
	if r == nil {
		return out, nil
	}
	r.mu.Lock()
	for k, a := range r.arms {
		if k.tenantID == tenantID {
			out = append(out, a)
		}
	}
	r.mu.Unlock()
	// Stable order: sort by RuleID so callers see a deterministic arm list.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].RuleID < out[j-1].RuleID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}
