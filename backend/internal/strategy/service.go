package strategy

// Service orchestrates daily-advice persistence and the per-item execution /
// feedback state machines (DB-04, CONTRACT §12.3.1). Every method first
// resolves the tenant from ctx via multitenancy.EnforceTenant — tenant is NEVER
// trusted from a request field. The service depends only on the StrategyRepo
// interface, so the whole flow is deterministic in unit tests (in-memory repo +
// injected fixed clock).
//
// SaveAdvice validates the advice and delegates the atomic upsert to the repo.
// UpdateItemStatus computes executedAt from the target status and delegates the
// transition (validated under the repo write lock) to the repo. Both GetToday
// and RecordFeedback accept a zero date / handle defaults at this layer.

import (
	"context"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service is the strategy orchestrator.
type Service struct {
	repo StrategyRepo
	now  func() time.Time
}

// Option customises a Service.
type Option func(*Service)

// WithNowFunc overrides the service clock (deterministic date-default +
// executedAt tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// New constructs a strategy Service. A nil clock falls back to time.Now.
func New(repo StrategyRepo, opts ...Option) *Service {
	s := &Service{repo: repo, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// storeScope returns the caller-supplied storeID, falling back to the store
// carried in ctx when the caller passed zero.
func storeScope(ctx context.Context, storeID int64) int64 {
	if storeID > 0 {
		return storeID
	}
	if mc, ok := multitenancy.FromContext(ctx); ok {
		return mc.StoreID
	}
	return 0
}

// SaveAdvice validates and atomically upserts one day's advice. ForDate must be
// a valid YYYY-MM-DD; GeneratedBy must be one of the four generators; each item
// must have a unique Rank in 1-3 with a known ActionType / ReasonType. TenantID
// / item ownership are forced from ctx (never trusted from the caller). Status
// defaults to StatusGenerated when empty.
func (s *Service) SaveAdvice(ctx context.Context, st *Strategy) (*Strategy, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, ErrParam
	}
	if _, err := time.Parse("2006-01-02", st.ForDate); err != nil {
		return nil, ErrParam
	}
	if !validGeneratedBy[st.GeneratedBy] {
		return nil, ErrParam
	}
	if st.Status != "" && !validStatuses[st.Status] {
		return nil, ErrParam
	}
	st.TenantID = tenantID
	st.StoreID = storeScope(ctx, st.StoreID)
	if st.StoreID == 0 {
		return nil, ErrParam
	}
	seenRank := make(map[int]bool, len(st.Items))
	for i := range st.Items {
		it := &st.Items[i]
		if it.Rank < 1 || it.Rank > 3 {
			return nil, ErrParam
		}
		if seenRank[it.Rank] {
			return nil, ErrParam
		}
		seenRank[it.Rank] = true
		if !validActionTypes[it.ActionType] {
			return nil, ErrParam
		}
		if !validReasonTypes[it.ReasonType] {
			return nil, ErrParam
		}
		it.TenantID = tenantID
	}
	if st.Status == "" {
		st.Status = StatusGenerated
	}
	return s.repo.SaveAdvice(ctx, st)
}

// Get loads one strategy (with items) for the ctx tenant. Cross-tenant →
// ErrNotFound.
func (s *Service) Get(ctx context.Context, id int64) (*Strategy, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrParam
	}
	return s.repo.Get(ctx, tenantID, id)
}

// GetToday loads the advice for (ctx tenant, storeID, date). Empty date defaults
// to today (per the service clock). Cross-tenant → ErrNotFound.
func (s *Service) GetToday(ctx context.Context, storeID int64, date string) (*Strategy, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if date == "" {
		date = s.now().Format("2006-01-02")
	} else if _, err := time.Parse("2006-01-02", date); err != nil {
		return nil, ErrParam
	}
	return s.repo.GetToday(ctx, tenantID, storeScope(ctx, storeID), date)
}

// List returns strategies matching the filter for the ctx tenant.
func (s *Service) List(ctx context.Context, f StrategyFilter) ([]*Strategy, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.List(ctx, tenantID, f)
}

// GetItem loads one item for the ctx tenant. Cross-tenant → ErrNotFound.
func (s *Service) GetItem(ctx context.Context, itemID int64) (*StrategyItem, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if itemID <= 0 {
		return nil, ErrParam
	}
	return s.repo.GetItem(ctx, tenantID, itemID)
}

// UpdateItemStatus transitions one item's execution_status. executedAt is
// derived from status: terminal states (executed/blocked/failed/skipped) stamp
// now; pending clears it. The legal-transition check itself happens under the
// repo write lock (race-safe); an illegal migration yields ErrParam.
func (s *Service) UpdateItemStatus(ctx context.Context, itemID int64, status string) (*StrategyItem, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if itemID <= 0 {
		return nil, ErrParam
	}
	if !validItemStatuses[status] {
		return nil, ErrParam
	}
	now := s.now()
	var executedAt *time.Time
	if status != ItemPending {
		v := now
		executedAt = &v
	}
	return s.repo.UpdateItemStatus(ctx, tenantID, itemID, status, executedAt, now)
}

// RecordFeedback upserts the boss's feedback for one item (UNIQUE per item →
// repeat overwrites). Absent/cross-tenant items yield ErrNotFound.
func (s *Service) RecordFeedback(ctx context.Context, itemID int64, useful bool) (*StrategyFeedback, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if itemID <= 0 {
		return nil, ErrParam
	}
	return s.repo.RecordFeedback(ctx, tenantID, itemID, useful, s.now())
}
