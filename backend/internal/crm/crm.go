// Package crm implements the community-group + WeCom-living-code domain
// (BE-B-06, CONTRACT §11.3.5 / tech plan §4.2.4 + §5.3.6). A CommunityGroup is
// one tenant-scoped WeCom customer chat that is shepherded through the SOP
// funnel recruit → activate → convert → refer (refer is terminal). A LivingCode
// is one tenant-scoped QR "活码" that fans inbound customers out to a store,
// either by round-robin across the tenant's stores (built-in) or by nearest-store
// (delegated to an injected StorePicker, with a deterministic fallback).
//
// Every repository and service method resolves the tenant from ctx via
// multitenancy.EnforceTenant — tenant is NEVER trusted from a request field.
// The package is anti-corruption-clean: it imports neither catalog, membership,
// nor marketing. Store selection for nearest_store goes through the injected
// StorePicker port (the main thread adapts an LBS / store-directory service to
// it at the composition root).
package crm

import (
	"context"
	"errors"
	"time"
)

// CommunityGroup is one tenant-scoped WeCom customer group (community_groups
// table, ch05 §5.3.6). SOPStage walks the four-stage funnel recruit → activate
// → convert → refer; an empty stage means the SOP has not started (CreateGroup
// defaults it to recruit so the funnel is always seeded). MemberCount is the
// live headcount (clamped at 0 by AddGroupMembers).
type CommunityGroup struct {
	ID, TenantID, StoreID int64
	WeworkChatID, Name    string
	MemberCount           int
	SOPStage              string // recruit|activate|convert|refer (empty = SOP not started)
	CreatedAt, UpdatedAt  time.Time
}

// LivingCode is one tenant-scoped QR "活码" (wework_living_codes table, ch05
// §5.3.6). One code fans inbound customers out to exactly one store per resolve,
// picked by Config.AutoAssignRule. Scene classifies the code's intent
// (add_friend | join_group).
type LivingCode struct {
	ID, TenantID, StoreID int64
	Code, Scene           string // code = the live QR slug; Scene = add_friend|join_group
	Config                LivingCodeConfig
	CreatedAt             time.Time
}

// LivingCodeConfig holds the auto-assign policy + welcome copy for a living code.
// AutoAssignRule is nearest_store (LBS via StorePicker) or round_robin (built-in
// rotation across the tenant's stores).
type LivingCodeConfig struct {
	AutoAssignRule string // nearest_store|round_robin
	WelcomeMsg     string
}

// SOP stage constants — the community funnel. refer is terminal.
const (
	SOPRecruit  = "recruit"
	SOPActivate = "activate"
	SOPConvert  = "convert"
	SOPRefer    = "refer"
)

// Living-code scene constants.
const (
	SceneAddFriend = "add_friend"
	SceneJoinGroup = "join_group"
)

// Auto-assign rule constants.
const (
	RuleNearestStore = "nearest_store"
	RuleRoundRobin   = "round_robin"
)

// sopOrder is the canonical funnel order, used by AdvanceSOP.
var sopOrder = []string{SOPRecruit, SOPActivate, SOPConvert, SOPRefer}

// Sentinel errors. Cross-tenant access is reported as ErrNotFound (absence and
// foreign ownership are indistinguishable by design — ARCH-01). ErrSOPTerminal
// signals that a group is already at the terminal refer stage and cannot be
// advanced further.
var (
	ErrNotFound    = errors.New("crm: not found")
	ErrSOPTerminal = errors.New("crm: sop terminal stage")
)

// Repo persists community groups and living codes. The real implementation will
// be pgx-backed (community_groups / wework_living_codes tables with tenant_id
// row isolation); an in-memory implementation covers dev and tests. Every method
// treats a record owned by another tenant as absent (ErrNotFound).
//
// storeID <= 0 in ListGroups / ListCodes means "no store filter" (all stores for
// the tenant) — the service uses this to derive the round-robin candidate set.
type Repo interface {
	CreateGroup(ctx context.Context, g *CommunityGroup) (*CommunityGroup, error)
	GetGroup(ctx context.Context, tenantID, id int64) (*CommunityGroup, error)
	ListGroups(ctx context.Context, tenantID, storeID int64) ([]*CommunityGroup, error)
	UpdateGroupSOP(ctx context.Context, tenantID, id int64, stage string, now time.Time) (*CommunityGroup, error)
	AddGroupMembers(ctx context.Context, tenantID, id int64, delta int, now time.Time) (*CommunityGroup, error)
	CreateCode(ctx context.Context, lc *LivingCode) (*LivingCode, error)
	GetCode(ctx context.Context, tenantID int64, code string) (*LivingCode, error)
	ListCodes(ctx context.Context, tenantID, storeID int64) ([]*LivingCode, error)
}

// StorePicker is the anti-corruption port for nearest-store selection (crm must
// not import a store/LBS service). The main thread adapts the store-directory or
// LBS service to it. Pick returns the chosen storeID for the given rule (only
// RuleNearestStore is routed here in M2-S2); an error makes ResolveCode fall back
// to LivingCode.StoreID.
type StorePicker interface {
	Pick(ctx context.Context, tenantID int64, rule string) (storeID int64, err error)
}

// Option customises a Service.
type Option func(*Service)

// WithNowFunc overrides the service clock (deterministic time-stamp / window
// tests). A nil f is ignored.
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// WithStorePicker wires the nearest-store selection port. Without it,
// nearest_store resolve falls back to LivingCode.StoreID.
func WithStorePicker(p StorePicker) Option {
	return func(s *Service) {
		if p != nil {
			s.picker = p
		}
	}
}
