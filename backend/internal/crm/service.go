package crm

// Service orchestrates community-group SOP advancement and living-code store
// resolution (BE-B-06, CONTRACT §11.3.5). Every method first resolves the tenant
// from ctx via multitenancy.EnforceTenant — tenant is NEVER trusted from a
// request field. The service depends only on the Repo interface plus the optional
// StorePicker (nearest-store LBS) anti-corruption port, so the whole flow is
// deterministic in unit tests (in-memory repo + injected fake picker + fixed
// clock).
//
// AdvanceSOP walks the recruit→activate→convert→refer funnel (refer is terminal).
// ResolveCode fans a living code out to one store: round_robin rotates an
// in-memory per-code cursor across the tenant's stores; nearest_store delegates
// to StorePicker and falls back to the code's own store when the port is absent
// or errors.

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service is the crm orchestrator.
type Service struct {
	repo   Repo
	picker StorePicker // nil = nearest_store falls back to code.StoreID
	now    func() time.Time

	// rrCursor guards the round-robin rotation: tenantID -> codeID -> next index.
	// Serialised by rrMu so concurrent resolves of the same code advance the
	// cursor atomically.
	rrMu     sync.Mutex
	rrCursor map[int64]map[int64]int
}

// New constructs a crm Service. A nil clock falls back to time.Now.
func New(repo Repo, opts ...Option) *Service {
	s := &Service{
		repo:     repo,
		now:      time.Now,
		rrCursor: make(map[int64]map[int64]int),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// nextSOPStage returns the stage that follows cur, or ok=false when cur is the
// terminal refer stage (or unrecognised beyond the funnel). An empty stage is
// treated as "pre-recruit" and advances to recruit to seed the funnel.
func nextSOPStage(cur string) (string, bool) {
	if cur == "" {
		return SOPRecruit, true
	}
	for i, s := range sopOrder {
		if s == cur {
			if i+1 >= len(sopOrder) {
				return "", false // terminal
			}
			return sopOrder[i+1], true
		}
	}
	return "", false
}

// AdvanceSOP advances a group one step along the funnel:
// recruit→activate→convert→refer. refer is terminal; advancing it yields
// ErrSOPTerminal. An empty stage (SOP not started) seeds to recruit. The
// returned group reflects the post-advance stage.
func (s *Service) AdvanceSOP(ctx context.Context, groupID int64) (*CommunityGroup, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	g, err := s.repo.GetGroup(ctx, tenantID, groupID)
	if err != nil {
		return nil, err
	}
	next, ok := nextSOPStage(g.SOPStage)
	if !ok {
		return nil, ErrSOPTerminal
	}
	return s.repo.UpdateGroupSOP(ctx, tenantID, groupID, next, s.now())
}

// tenantStores returns the sorted, de-duplicated store IDs the tenant owns,
// derived from its community groups and living codes. This is the round-robin
// candidate set and the nearest-store fallback pool. The set always includes 0
// only if a record genuinely carries StoreID=0; callers treat 0 as "no store".
func (s *Service) tenantStores(ctx context.Context, tenantID int64) []int64 {
	seen := make(map[int64]struct{})
	collect := func(sid int64) {
		if sid > 0 {
			seen[sid] = struct{}{}
		}
	}
	groups, _ := s.repo.ListGroups(ctx, tenantID, 0)
	for _, g := range groups {
		collect(g.StoreID)
	}
	codes, _ := s.repo.ListCodes(ctx, tenantID, 0)
	for _, lc := range codes {
		collect(lc.StoreID)
	}
	out := make([]int64, 0, len(seen))
	for sid := range seen {
		out = append(out, sid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ResolveCode resolves the store + welcome config for one inbound living-code
// scan. It looks up the code (cross-tenant / unknown → ErrNotFound) then picks a
// store by Config.AutoAssignRule:
//
//   - round_robin: rotate an in-memory per-code cursor across the tenant's
//     stores. Falls back to the code's own store when the tenant has only one.
//   - nearest_store: delegate to StorePicker when wired; on absence or error fall
//     back to the code's own store, then to the tenant's first store.
//
// An unknown rule falls back to the code's own store (lenient).
func (s *Service) ResolveCode(ctx context.Context, code string) (storeID int64, cfg LivingCodeConfig, err error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return 0, LivingCodeConfig{}, err
	}
	lc, err := s.repo.GetCode(ctx, tenantID, code)
	if err != nil {
		return 0, LivingCodeConfig{}, err
	}

	picked := s.pickStore(ctx, tenantID, lc)
	return picked, lc.Config, nil
}

// pickStore resolves the destination store for lc according to its rule. It is
// the single place the round-robin cursor advances, guarded by rrMu.
func (s *Service) pickStore(ctx context.Context, tenantID int64, lc *LivingCode) int64 {
	switch lc.Config.AutoAssignRule {
	case RuleRoundRobin:
		candidates := s.tenantStores(ctx, tenantID)
		// Always consider the code's own store; if it is the only candidate the
		// rotation trivially returns it.
		if len(candidates) == 0 {
			return lc.StoreID
		}
		s.rrMu.Lock()
		defer s.rrMu.Unlock()
		byCode, ok := s.rrCursor[tenantID]
		if !ok {
			byCode = make(map[int64]int)
			s.rrCursor[tenantID] = byCode
		}
		idx := byCode[lc.ID]
		byCode[lc.ID] = idx + 1
		return candidates[idx%len(candidates)]
	case RuleNearestStore:
		if s.picker != nil {
			if sid, perr := s.picker.Pick(ctx, tenantID, RuleNearestStore); perr == nil && sid > 0 {
				return sid
			}
		}
		// Fallback chain: code's own store, else the tenant's first store.
		if lc.StoreID > 0 {
			return lc.StoreID
		}
		if stores := s.tenantStores(ctx, tenantID); len(stores) > 0 {
			return stores[0]
		}
		return lc.StoreID
	default:
		// Unknown rule: lenient — route to the code's own store.
		return lc.StoreID
	}
}

// CreateGroup persists a community group. TenantID is forced from ctx (the
// caller's value is ignored); an empty SOPStage defaults to recruit (enforced in
// the repo). Returns the stored group with its allocated id.
func (s *Service) CreateGroup(ctx context.Context, g *CommunityGroup) (*CommunityGroup, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	g.TenantID = tenantID
	return s.repo.CreateGroup(ctx, g)
}

// GetGroup loads one community group for the ctx tenant. Cross-tenant →
// ErrNotFound.
func (s *Service) GetGroup(ctx context.Context, id int64) (*CommunityGroup, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.GetGroup(ctx, tenantID, id)
}

// ListGroups returns the tenant's groups, optionally narrowed by store
// (storeID <= 0 = all stores).
func (s *Service) ListGroups(ctx context.Context, storeID int64) ([]*CommunityGroup, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListGroups(ctx, tenantID, storeID)
}

// AddGroupMembers applies a signed delta to a group's MemberCount (clamped at 0
// in the repo). delta may be negative (members leaving).
func (s *Service) AddGroupMembers(ctx context.Context, id int64, delta int) (*CommunityGroup, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.AddGroupMembers(ctx, tenantID, id, delta, s.now())
}

// CreateCode persists a living code. TenantID is forced from ctx.
func (s *Service) CreateCode(ctx context.Context, lc *LivingCode) (*LivingCode, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	lc.TenantID = tenantID
	return s.repo.CreateCode(ctx, lc)
}

// ListCodes returns the tenant's living codes, optionally narrowed by store
// (storeID <= 0 = all stores).
func (s *Service) ListCodes(ctx context.Context, storeID int64) ([]*LivingCode, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListCodes(ctx, tenantID, storeID)
}
