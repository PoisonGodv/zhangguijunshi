// Package groupbuy implements the group-buy (拼团) fission domain (BE-B-03,
// CONTRACT §11.3.1 / tech plan §4.2.3). A Groupbuy is a tenant-scoped,
// time-boxed buying squad: RequiredCount distinct members must Join before
// EndAt to reach success; if the window closes short of the count the squad
// fails; the merchant can Cancel an active squad. Each Participant carries a
// per-member status (joined → paid), with the payment closed-loop deferred to
// the payment domain in M4 — M2-S2 only persists the state flag.
//
// Every repository and service method resolves the tenant from ctx via
// multitenancy.EnforceTenant — tenant is NEVER trusted from a request field.
// The package is anti-corruption-clean: it imports neither membership, catalog,
// nor marketing. Member existence is checked through the injected MemberLookuper
// interface (adapted at the composition root, cmd/api/main.go).
package groupbuy

import (
	"context"
	"errors"
	"time"
)

// Groupbuy is one tenant-scoped buying squad. Status follows the state machine
// active → success | failed | cancelled (CONTRACT §11.3.1 / tech plan §4.2.3);
// success/failed/cancelled are all terminal. Joined is the live participant
// count; when it reaches RequiredCount the squad flips to success atomically
// inside JoinAtomic.
type Groupbuy struct {
	ID, TenantID, StoreID, ProductID int64
	StrategyID                       int64 `json:"strategy_id"` // 归因键: 自哪条 strategy_item (0=非AI来源); M3-S3 groupbuy→strategy linkage
	Title                            string
	Params                           GroupbuyParams
	RequiredCount                    int // 3-4 成团
	Joined                           int
	Status                           string // active|success|failed|cancelled
	StartAt, EndAt                   time.Time
	CreatedAt, UpdatedAt             time.Time
}

// GroupbuyParams holds the elastic parameters (stored as JSONB in PostgreSQL).
// GroupSize / ValidHours are fallback sources for CreateInput when the caller
// omits the top-level RequiredCount / ValidHours.
type GroupbuyParams struct {
	GroupSize  int   `json:"group_size,omitempty"`
	PriceCents int64 `json:"price_cents,omitempty"`
	ValidHours int   `json:"valid_hours,omitempty"`
}

// Participant is one member's seat in a squad. Status: joined → paid (→
// refunded once the payment domain lands in M4).
type Participant struct {
	ID, TenantID, GroupbuyID, MemberID int64
	Status                             string // joined|paid|refunded
	JoinedAt                           time.Time
}

// Groupbuy / Participant status constants.
const (
	StatusActive    = "active"
	StatusSuccess   = "success"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"

	PStatusJoined   = "joined"
	PStatusPaid     = "paid"
	PStatusRefunded = "refunded"
)

// Sentinel errors. Cross-tenant access is reported as ErrNotFound (absence and
// foreign ownership are indistinguishable by design — ARCH-01).
var (
	ErrNotFound       = errors.New("groupbuy: not found")
	ErrGroupbuyFull   = errors.New("groupbuy: already full")
	ErrGroupbuyClosed = errors.New("groupbuy: not active")
	ErrAlreadyJoined  = errors.New("groupbuy: member already joined")
)

// GroupbuyRepo persists groupbuys and participants. The real implementation
// will be pgx-backed (groupbuys / groupbuy_participants tables with tenant_id
// row isolation); an in-memory implementation covers dev and tests.
//
// JoinAtomic is the atomic critical section: the in-memory implementation
// serialises it under a single mutex; the pgx implementation will wrap it in a
// real transaction. Every method treats a record owned by another tenant as
// absent (ErrNotFound).
type GroupbuyRepo interface {
	Create(ctx context.Context, g *Groupbuy) (*Groupbuy, error)
	GetByID(ctx context.Context, tenantID, id int64) (*Groupbuy, error)
	ListActive(ctx context.Context, tenantID, storeID int64) ([]*Groupbuy, error)
	// ListByStrategy returns every groupbuy (all statuses) attributed to
	// strategyID within tenantID. strategyID<=0 yields an empty slice. A record
	// owned by another tenant is never returned (filtered by tenantID). Used by
	// Service.StatsByStrategy to compute the groupbuy_success 归因 metric.
	ListByStrategy(ctx context.Context, tenantID, strategyID int64) ([]*Groupbuy, error)
	UpdateStatus(ctx context.Context, tenantID, id int64, status string, now time.Time) (*Groupbuy, error)

	// JoinAtomic atomically validates (not already joined, not full, active) and
	// inserts one participant(joined) + increments Groupbuy.Joined. If Joined
	// reaches RequiredCount the squad flips to success. Any precondition failure
	// returns the matching sentinel with no state change.
	JoinAtomic(ctx context.Context, tenantID, groupbuyID, memberID int64, now time.Time) (*Participant, *Groupbuy, error)

	GetParticipant(ctx context.Context, tenantID, groupbuyID, memberID int64) (*Participant, error)
	ListParticipants(ctx context.Context, tenantID, groupbuyID int64) ([]*Participant, error)

	// MarkPaid transitions a participant joined→paid (payment closed-loop deferred
	// to M4; M2-S2 only persists the state flag).
	MarkPaid(ctx context.Context, tenantID, groupbuyID, memberID int64, now time.Time) (*Participant, error)
}

// MemberLookuper is the anti-corruption port for member existence (groupbuy
// must not import membership). The main thread adapts *membership.Service to it.
type MemberLookuper interface {
	Exists(ctx context.Context, tenantID, memberID int64) (bool, error)
}

// CreateInput carries the caller-supplied squad fields. TenantID is NOT trusted
// from the caller (it comes from ctx); StoreID falls back to the ctx store when
// zero. RequiredCount<=0 falls back to Params.GroupSize then 3; ValidHours<=0
// falls back to Params.ValidHours then 24.
type CreateInput struct {
	StoreID, ProductID int64
	StrategyID         int64 // 归因键: 写入 Groupbuy.StrategyID; 0=非AI来源
	Title              string
	Params             GroupbuyParams
	RequiredCount      int // <=0 时取 Params.GroupSize 或默认 3
	ValidHours         int // 计算 EndAt = StartAt + ValidHours；<=0 默认 24
}
