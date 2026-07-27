package groupbuy

// Service orchestrates the group-buy lifecycle (BE-B-03, CONTRACT §11.3.1).
// Every method first resolves the tenant from ctx via multitenancy.EnforceTenant
// — tenant is NEVER trusted from a request field. The atomic Join critical
// section lives in the repo (JoinAtomic); the service adds the member-existence
// pre-check (when MemberLookuper is wired) plus the Cancel and FinalizeExpired
// orchestration. The whole flow is deterministic in unit tests (in-memory repo
// + injected fake lookuper + fixed clock).

import (
	"context"
	"errors"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service is the groupbuy orchestrator.
type Service struct {
	repo     GroupbuyRepo
	lookuper MemberLookuper // nil = skip member-existence check (dev convenience)
	now      func() time.Time
}

// Option customises a Service.
type Option func(*Service)

// WithMemberLookuper wires the member-existence port (main thread adapts
// *membership.Service to it). Without it, Join skips the member check.
func WithMemberLookuper(l MemberLookuper) Option {
	return func(s *Service) { s.lookuper = l }
}

// WithNowFunc overrides the service clock (deterministic window/expiry tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// New constructs a groupbuy Service. A nil clock falls back to time.Now.
func New(repo GroupbuyRepo, opts ...Option) *Service {
	s := &Service{repo: repo, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// storeScope returns in.StoreID, falling back to the store carried in ctx when
// the caller passed zero (matches the marketing / billing convention).
func storeScope(ctx context.Context, storeID int64) int64 {
	if storeID > 0 {
		return storeID
	}
	if mc, ok := multitenancy.FromContext(ctx); ok {
		return mc.StoreID
	}
	return 0
}

// resolveRequiredCount: in.RequiredCount → in.Params.GroupSize → 3.
func resolveRequiredCount(in CreateInput) int {
	if in.RequiredCount > 0 {
		return in.RequiredCount
	}
	if in.Params.GroupSize > 0 {
		return in.Params.GroupSize
	}
	return 3
}

// resolveValidHours: in.ValidHours → in.Params.ValidHours → 24.
func resolveValidHours(in CreateInput) int {
	if in.ValidHours > 0 {
		return in.ValidHours
	}
	if in.Params.ValidHours > 0 {
		return in.Params.ValidHours
	}
	return 24
}

// Create opens a new active squad. Title must be non-empty and ProductID > 0.
// StartAt = now, EndAt = now + ValidHours (resolved with fallbacks). Status
// starts active (there is no draft state for groupbuys).
func (s *Service) Create(ctx context.Context, in CreateInput) (*Groupbuy, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if in.Title == "" || in.ProductID <= 0 {
		return nil, errcode.ErrParam
	}
	required := resolveRequiredCount(in)
	validHours := resolveValidHours(in)
	now := s.now()
	g := &Groupbuy{
		TenantID:      tenantID,
		StoreID:       storeScope(ctx, in.StoreID),
		ProductID:     in.ProductID,
		Title:         in.Title,
		Params:        in.Params,
		RequiredCount: required,
		Joined:        0,
		Status:        StatusActive,
		StartAt:       now,
		EndAt:         now.Add(time.Duration(validHours) * time.Hour),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	return s.repo.Create(ctx, g)
}

// Get loads one squad for the ctx tenant. Cross-tenant → ErrNotFound.
func (s *Service) Get(ctx context.Context, id int64) (*Groupbuy, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, errcode.ErrParam
	}
	return s.repo.GetByID(ctx, tenantID, id)
}

// ListActive returns active squads for the ctx tenant. storeID==0 means all
// stores in the tenant.
func (s *Service) ListActive(ctx context.Context, storeID int64) ([]*Groupbuy, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListActive(ctx, tenantID, storeID)
}

// Cancel transitions active → cancelled. A non-active squad yields
// ErrGroupbuyClosed. Cross-tenant → ErrNotFound.
func (s *Service) Cancel(ctx context.Context, id int64) (*Groupbuy, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, errcode.ErrParam
	}
	g, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if g.Status != StatusActive {
		return nil, ErrGroupbuyClosed
	}
	return s.repo.UpdateStatus(ctx, tenantID, id, StatusCancelled, s.now())
}

// Join adds a member to a squad. When MemberLookuper is wired, the member must
// exist in this tenant first. The atomic validate+insert+flip lives in
// JoinAtomic; this method only adds the member-existence pre-check.
func (s *Service) Join(ctx context.Context, groupbuyID, memberID int64) (*Participant, *Groupbuy, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, nil, err
	}
	if groupbuyID <= 0 || memberID <= 0 {
		return nil, nil, errcode.ErrParam
	}
	if s.lookuper != nil {
		exists, err := s.lookuper.Exists(ctx, tenantID, memberID)
		if err != nil {
			return nil, nil, err
		}
		if !exists {
			return nil, nil, errcode.ErrParam
		}
	}
	now := s.now()
	return s.repo.JoinAtomic(ctx, tenantID, groupbuyID, memberID, now)
}

// ListParticipants returns every participant of a squad (matched by ID +
// tenant). Cross-tenant → ErrNotFound.
func (s *Service) ListParticipants(ctx context.Context, groupbuyID int64) ([]*Participant, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if groupbuyID <= 0 {
		return nil, errcode.ErrParam
	}
	return s.repo.ListParticipants(ctx, tenantID, groupbuyID)
}

// FinalizeExpired is the jobs hook that closes stale squads: every active squad
// whose EndAt is before now AND whose Joined is short of RequiredCount is
// transitioned to failed. It returns the count finalised. A squad that raced to
// success/cancel between the sweep and the status update is skipped silently
// (UpdateStatus returns ErrGroupbuyClosed for the now-terminal squad). Cron
// scheduling is deferred to M3.
func (s *Service) FinalizeExpired(ctx context.Context, now time.Time) (int, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return 0, err
	}
	active, err := s.repo.ListActive(ctx, tenantID, 0) // 0 = all stores in tenant
	if err != nil {
		return 0, err
	}
	n := 0
	for _, g := range active {
		if !g.EndAt.Before(now) {
			continue
		}
		if g.Joined >= g.RequiredCount {
			continue
		}
		if _, err := s.repo.UpdateStatus(ctx, tenantID, g.ID, StatusFailed, now); err != nil {
			// Raced: a concurrent Join/Cancel already moved it off active.
			if errors.Is(err, ErrGroupbuyClosed) {
				continue
			}
			return n, err
		}
		n++
	}
	return n, nil
}
