package crm

import (
	"context"
	"errors"
	"testing"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

func ctxWithTenant(tid int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tid,
		Source:   multitenancy.SourceMerchant,
	})
}

// newSvc builds a Service over a fresh MemoryRepo with a fixed clock.
func newSvc(t *testing.T, opts ...Option) *Service {
	t.Helper()
	return New(NewMemoryRepo(), opts...)
}

// fakePicker is a controllable StorePicker for nearest_store tests.
type fakePicker struct {
	store int64
	err   error
	calls int
}

func (f *fakePicker) Pick(_ context.Context, _ int64, _ string) (int64, error) {
	f.calls++
	return f.store, f.err
}

// mustCreateGroup creates a group under tid with the given store + initial stage
// (empty allowed to verify the recruit default).
func mustCreateGroup(t *testing.T, s *Service, tid, store int64, chatID, name, stage string) *CommunityGroup {
	t.Helper()
	g, err := s.CreateGroup(ctxWithTenant(tid), &CommunityGroup{
		StoreID:      store,
		WeworkChatID: chatID,
		Name:         name,
		SOPStage:     stage,
	})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return g
}

func mustCreateCode(t *testing.T, s *Service, tid, store int64, code, scene, rule, welcome string) *LivingCode {
	t.Helper()
	lc, err := s.CreateCode(ctxWithTenant(tid), &LivingCode{
		StoreID: store,
		Code:    code,
		Scene:   scene,
		Config:  LivingCodeConfig{AutoAssignRule: rule, WelcomeMsg: welcome},
	})
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	return lc
}

// --- SOP funnel ----------------------------------------------------------

func TestCreateGroup_DefaultsSOPRecruit(t *testing.T) {
	s := newSvc(t)
	g := mustCreateGroup(t, s, 60001, 101, "chat-1", "VIP群", "")
	if g.SOPStage != SOPRecruit {
		t.Fatalf("empty SOPStage should default to recruit, got %q", g.SOPStage)
	}
	if g.ID <= 0 {
		t.Fatalf("id should be allocated, got %d", g.ID)
	}
	if g.TenantID != 60001 {
		t.Fatalf("tenant should be forced from ctx, got %d", g.TenantID)
	}
	if g.CreatedAt.IsZero() || g.UpdatedAt.IsZero() {
		t.Fatalf("timestamps should be set")
	}
}

func TestAdvanceSOP_FullFunnel(t *testing.T) {
	s := newSvc(t)
	g := mustCreateGroup(t, s, 60001, 101, "chat-1", "VIP群", "")

	want := []string{SOPActivate, SOPConvert, SOPRefer}
	for i, stage := range want {
		got, err := s.AdvanceSOP(ctxWithTenant(60001), g.ID)
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		if got.SOPStage != stage {
			t.Fatalf("advance %d: want stage %q, got %q", i, stage, got.SOPStage)
		}
	}

	// refer is terminal: one more advance errors.
	if _, err := s.AdvanceSOP(ctxWithTenant(60001), g.ID); !errors.Is(err, ErrSOPTerminal) {
		t.Fatalf("advance past refer should yield ErrSOPTerminal, got %v", err)
	}
}

func TestAdvanceSOP_EmptyStageSeedsRecruit(t *testing.T) {
	// A group created directly via repo (bypassing the service default) with an
	// empty stage should seed to recruit on first advance.
	repo := NewMemoryRepo()
	g, err := repo.CreateGroup(ctxWithTenant(60001), &CommunityGroup{StoreID: 101, SOPStage: ""})
	if err != nil {
		t.Fatalf("repo.CreateGroup: %v", err)
	}
	if g.SOPStage != SOPRecruit {
		t.Fatalf("repo should also default empty stage to recruit, got %q", g.SOPStage)
	}
}

// --- Living-code resolution ---------------------------------------------

func TestResolveCode_UnknownCode(t *testing.T) {
	s := newSvc(t)
	if _, _, err := s.ResolveCode(ctxWithTenant(60001), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown code should yield ErrNotFound, got %v", err)
	}
}

func TestResolveCode_RoundRobin(t *testing.T) {
	s := newSvc(t)
	// Seed the tenant with two stores (via groups) so the candidate set is
	// {301, 302}. The round-robin code itself also lives in store 301.
	mustCreateGroup(t, s, 60001, 301, "c-301", "群A", SOPRecruit)
	mustCreateGroup(t, s, 60001, 302, "c-302", "群B", SOPRecruit)
	lc := mustCreateCode(t, s, 60001, 301, "rr-1", SceneJoinGroup, RuleRoundRobin, "欢迎")

	// The rotation should cycle 301 → 302 → 301 → 302.
	want := []int64{301, 302, 301, 302}
	for i, w := range want {
		sid, cfg, err := s.ResolveCode(ctxWithTenant(60001), lc.Code)
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if sid != w {
			t.Fatalf("resolve %d: want store %d, got %d", i, w, sid)
		}
		if cfg.WelcomeMsg != "欢迎" || cfg.AutoAssignRule != RuleRoundRobin {
			t.Fatalf("resolve %d: cfg not echoed: %+v", i, cfg)
		}
	}
}

func TestResolveCode_RoundRobin_SingleStore(t *testing.T) {
	// Only one store exists → round-robin trivially keeps returning it.
	s := newSvc(t)
	lc := mustCreateCode(t, s, 60001, 401, "rr-only", SceneAddFriend, RuleRoundRobin, "hi")
	for i := 0; i < 3; i++ {
		sid, _, err := s.ResolveCode(ctxWithTenant(60001), lc.Code)
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if sid != 401 {
			t.Fatalf("resolve %d: want 401, got %d", i, sid)
		}
	}
}

func TestResolveCode_NearestStore_NoPicker(t *testing.T) {
	// No StorePicker wired → fall back to the code's own store.
	s := newSvc(t)
	lc := mustCreateCode(t, s, 60001, 501, "near-1", SceneAddFriend, RuleNearestStore, "wb")
	sid, _, err := s.ResolveCode(ctxWithTenant(60001), lc.Code)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sid != 501 {
		t.Fatalf("nearest without picker should fall back to code.StoreID, got %d", sid)
	}
}

func TestResolveCode_NearestStore_WithPicker(t *testing.T) {
	p := &fakePicker{store: 999}
	s := newSvc(t, WithStorePicker(p))
	lc := mustCreateCode(t, s, 60001, 501, "near-2", SceneAddFriend, RuleNearestStore, "wb")
	sid, _, err := s.ResolveCode(ctxWithTenant(60001), lc.Code)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sid != 999 {
		t.Fatalf("nearest with picker should use picker result, got %d", sid)
	}
	if p.calls != 1 {
		t.Fatalf("picker should be called once, got %d", p.calls)
	}
}

func TestResolveCode_NearestStore_PickerError(t *testing.T) {
	// Picker errors → fall back to the code's own store.
	p := &fakePicker{err: errors.New("lbs unavailable")}
	s := newSvc(t, WithStorePicker(p))
	lc := mustCreateCode(t, s, 60001, 501, "near-3", SceneAddFriend, RuleNearestStore, "wb")
	sid, _, err := s.ResolveCode(ctxWithTenant(60001), lc.Code)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sid != 501 {
		t.Fatalf("picker error should fall back to code.StoreID, got %d", sid)
	}
}

func TestResolveCode_UnknownRuleFallback(t *testing.T) {
	// Build the code via repo so we can plant an unknown rule the service won't
	// reject at creation.
	repo := NewMemoryRepo()
	s := New(repo)
	lc, err := repo.CreateCode(ctxWithTenant(60001), &LivingCode{
		TenantID: 60001, StoreID: 601, Code: "weird", Scene: SceneAddFriend,
		Config: LivingCodeConfig{AutoAssignRule: "galactic_nearest"},
	})
	if err != nil {
		t.Fatalf("repo.CreateCode: %v", err)
	}
	sid, _, err := s.ResolveCode(ctxWithTenant(60001), lc.Code)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sid != 601 {
		t.Fatalf("unknown rule should fall back to code.StoreID, got %d", sid)
	}
}

// --- Cross-tenant isolation ---------------------------------------------

func TestGetGroup_CrossTenant(t *testing.T) {
	s := newSvc(t)
	g := mustCreateGroup(t, s, 60001, 101, "chat-1", "群", "")
	if _, err := s.GetGroup(ctxWithTenant(60002), g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant GetGroup should yield ErrNotFound, got %v", err)
	}
}

func TestAdvanceSOP_CrossTenant(t *testing.T) {
	s := newSvc(t)
	g := mustCreateGroup(t, s, 60001, 101, "chat-1", "群", "")
	if _, err := s.AdvanceSOP(ctxWithTenant(60002), g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant AdvanceSOP should yield ErrNotFound, got %v", err)
	}
}

func TestResolveCode_CrossTenant(t *testing.T) {
	s := newSvc(t)
	lc := mustCreateCode(t, s, 60001, 301, "shared-code", SceneJoinGroup, RuleRoundRobin, "hi")
	// Tenant B resolving tenant A's code slug → ErrNotFound (never leaks).
	if _, _, err := s.ResolveCode(ctxWithTenant(60002), lc.Code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant ResolveCode should yield ErrNotFound, got %v", err)
	}
}

func TestAddGroupMembers_CrossTenant(t *testing.T) {
	s := newSvc(t)
	g := mustCreateGroup(t, s, 60001, 101, "chat-1", "群", "")
	if _, err := s.AddGroupMembers(ctxWithTenant(60002), g.ID, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant AddGroupMembers should yield ErrNotFound, got %v", err)
	}
}

// --- Member-count mutation ----------------------------------------------

func TestAddGroupMembers_DeltaAndClamp(t *testing.T) {
	s := newSvc(t)
	g := mustCreateGroup(t, s, 60001, 101, "chat-1", "群", "")

	// +5 members.
	got, err := s.AddGroupMembers(ctxWithTenant(60001), g.ID, 5)
	if err != nil {
		t.Fatalf("add +5: %v", err)
	}
	if got.MemberCount != 5 {
		t.Fatalf("add +5: want 5, got %d", got.MemberCount)
	}

	// -3 members.
	if got, err := s.AddGroupMembers(ctxWithTenant(60001), g.ID, -3); err != nil {
		t.Fatalf("add -3: %v", err)
	} else if got.MemberCount != 2 {
		t.Fatalf("add -3: want 2, got %d", got.MemberCount)
	}

	// -100 overshoot clamps at 0 (headcount never negative).
	if got, err := s.AddGroupMembers(ctxWithTenant(60001), g.ID, -100); err != nil {
		t.Fatalf("add -100: %v", err)
	} else if got.MemberCount != 0 {
		t.Fatalf("add -100: want clamped 0, got %d", got.MemberCount)
	}
}

// --- Tenant enforcement --------------------------------------------------

func TestNoTenant_Refuses(t *testing.T) {
	s := newSvc(t)
	// A bare context (no tenant) must short-circuit every entry point.
	bare := context.Background()
	if _, err := s.AdvanceSOP(bare, 1); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("AdvanceSOP without tenant should yield ErrNoTenant, got %v", err)
	}
	if _, _, err := s.ResolveCode(bare, "x"); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("ResolveCode without tenant should yield ErrNoTenant, got %v", err)
	}
	if _, err := s.CreateGroup(bare, &CommunityGroup{}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("CreateGroup without tenant should yield ErrNoTenant, got %v", err)
	}
}

// --- Listing / store filter ---------------------------------------------

func TestListGroups_StoreFilter(t *testing.T) {
	s := newSvc(t)
	mustCreateGroup(t, s, 60001, 301, "c1", "A", "")
	mustCreateGroup(t, s, 60001, 302, "c2", "B", "")
	mustCreateGroup(t, s, 60002, 301, "c3", "C", "") // other tenant

	all, err := s.ListGroups(ctxWithTenant(60001), 0)
	if err != nil {
		t.Fatalf("ListGroups all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListGroups all: want 2 for tenant, got %d", len(all))
	}
	only302, err := s.ListGroups(ctxWithTenant(60001), 302)
	if err != nil {
		t.Fatalf("ListGroups store: %v", err)
	}
	if len(only302) != 1 || only302[0].StoreID != 302 {
		t.Fatalf("ListGroups store=302: want one 302, got %+v", only302)
	}
}
