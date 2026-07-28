// Package strategy persists the daily AI marketing advice ("今日3件事") and its
// per-item execution / feedback state (DB-04, CONTRACT §12.3.1 / tech plan
// ch05 §5.3.8). A Strategy is one tenant+store+day's advice: a snapshot of the
// signals the engine saw, the data-quality score, who generated it (cache /
// offline / live LLM / rule-only), and 1-3 ranked StrategyItems. Each item
// carries an ActionType (the execution channel, 6 values) plus a ReasonType
// (the rule semantic, 7 values), the target / params / copy the boss sees, the
// margin-rail verdict, and an execution_status state machine
// pending → executed | blocked | skipped | failed driven by BE-B-11
// ActionExecutor.
//
// The package is the data substrate of the product's core selling point and is
// deliberately anti-corruption-clean: it imports NO other internal domain (not
// signal, not margin, not marketing). Signals is carried as an opaque
// json.RawMessage snapshot; ActionType / ReasonType are bare strings (the rule
// package owns the constants; the migration 000007 CHECK is the enum authority).
// Every repository and service method resolves the tenant from ctx via
// multitenancy.EnforceTenant — tenant is NEVER trusted from a request field.
package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Status / generator constants (CONTRACT §12.3.1). These mirror the CHECK
// constraints on strategies.status / strategy_items.execution_status /
// strategies.generated_by in migration 000007.
const (
	// strategies.status — advice lifecycle.
	StatusGenerated         = "generated"
	StatusPartiallyExecuted = "partially_executed"
	StatusExecuted          = "executed"
	StatusDismissed         = "dismissed"

	// strategy_items.execution_status — one-click execution state machine.
	ItemPending  = "pending"
	ItemExecuted = "executed"
	ItemSkipped  = "skipped"
	ItemFailed   = "failed"
	ItemBlocked  = "blocked" // one-click blocked by compliance (CONTRACT §12 增补)

	// strategies.generated_by — which stage produced this advice.
	GenCacheHit      = "cache_hit"
	GenOfflinePregen = "offline_pregen" // M3-S2
	GenLiveLLM       = "live_llm"
	GenRuleOnly      = "rule_only"
)

// validActionTypes / validReasonTypes mirror the CHECK constraints on
// strategy_items in migration 000007 (and the constants in rule/actions.go,
// CONTRACT §12.3.0). strategy must not import rule, so the enum is replicated
// here purely for early input validation; the DB CHECK remains authoritative.
var (
	validActionTypes = map[string]bool{
		"group_seckill":  true,
		"coupon_wake":    true,
		"moments_post":   true,
		"price_adj":      true,
		"groupbuy":       true,
		"recharge_promo": true,
	}
	validReasonTypes = map[string]bool{
		"clearance_coupon":  true,
		"rain_delivery":     true,
		"bundle_slow":       true,
		"wake_dormant":      true,
		"festival_discount": true,
		"birthday_care":     true,
		"high_value_push":   true,
	}
	validGeneratedBy = map[string]bool{
		GenCacheHit:      true,
		GenOfflinePregen: true,
		GenLiveLLM:       true,
		GenRuleOnly:      true,
	}
	validStatuses = map[string]bool{
		StatusGenerated:         true,
		StatusPartiallyExecuted: true,
		StatusExecuted:          true,
		StatusDismissed:         true,
	}
	validItemStatuses = map[string]bool{
		ItemPending:  true,
		ItemExecuted: true,
		ItemSkipped:  true,
		ItemFailed:   true,
		ItemBlocked:  true,
	}
)

// Strategy is one tenant+store+day's advice. Same-day regeneration overwrites
// the row in place (upsert by tenantID+storeID+ForDate); the row keeps its ID
// and original CreatedAt while its items are rebuilt with fresh IDs.
type Strategy struct {
	ID                  int64           `json:"id"`
	TenantID            int64           `json:"tenant_id"`
	StoreID             int64           `json:"store_id"`
	ForDate             string          `json:"for_date"`
	Signals             json.RawMessage `json:"signals"`
	DataQualityScore    int             `json:"data_quality_score"`
	GeneratedBy         string          `json:"generated_by"`
	LLMTier             string          `json:"llm_tier,omitempty"`
	TotalCostMicroCents int64           `json:"total_cost_microcents"`
	Status              string          `json:"status"`
	Items               []StrategyItem  `json:"items"`
	CreatedAt           time.Time       `json:"created_at"`
}

// StrategyItem is one of the "今日3件事". ActionType is the execution channel
// (dispatched on by exec.ActionExecutor); ReasonType is the rule semantic that
// produced it. ExecutionStatus is the one-click state machine.
type StrategyItem struct {
	ID              int64          `json:"id"`
	TenantID        int64          `json:"tenant_id"`
	StrategyID      int64          `json:"strategy_id"`
	Rank            int            `json:"rank"`
	ActionType      string         `json:"action_type"`
	ReasonType      string         `json:"reason_type"`
	Target          map[string]any `json:"target,omitempty"`
	ProductID       int64          `json:"product_id,omitempty"`
	Params          map[string]any `json:"params"`
	Copy            string         `json:"copy"`
	Reason          string         `json:"reason,omitempty"`
	ExpectedEffect  map[string]any `json:"expected_effect,omitempty"`
	MarginOK        bool           `json:"margin_ok"`
	ExecutionStatus string         `json:"execution_status"`
	ExecutedAt      *time.Time     `json:"executed_at,omitempty"`
}

// StrategyFeedback is the boss's 👍/👎 on one item. UNIQUE(tenantID, item):
// repeated feedback on the same item overwrites (RecordFeedback).
type StrategyFeedback struct {
	ID             int64     `json:"id"`
	TenantID       int64     `json:"tenant_id"`
	StrategyItemID int64     `json:"strategy_item_id"`
	Useful         bool      `json:"useful"`
	CreatedAt      time.Time `json:"created_at"`
}

// StrategyFilter narrows a strategy listing. Zero-value fields are not applied.
type StrategyFilter struct {
	StoreID int64
	Date    string // 空不过滤
	Status  string // 空不过滤
	Limit   int    // <=0 默认 50
	Offset  int
}

// Sentinel errors. Cross-tenant access is reported as ErrNotFound (absence and
// foreign ownership are indistinguishable by design — ARCH-01). ErrParam covers
// malformed input and illegal state-machine transitions.
var (
	ErrNotFound = errors.New("strategy: not found")
	ErrParam    = errors.New("strategy: invalid param")
)

// StrategyRepo persists daily advice, items and feedback (CONTRACT §12.3.1).
// The real implementation will be pgx-backed (strategies / strategy_items /
// strategy_attribution / strategy_feedback tables with tenant_id row
// isolation); an in-memory implementation covers dev and tests.
//
// SaveAdvice is the atomic upsert: it rewrites the strategies row for
// (tenant, store, forDate) and rebuilds its items in one critical section (a
// real transaction under pgx). Every method treats a record owned by another
// tenant as absent (ErrNotFound).
type StrategyRepo interface {
	// SaveAdvice atomically upserts the advice for (tenantID, storeID, forDate):
	// same-day regeneration overwrites the existing row and rebuilds items (old
	// items and their feedback are discarded), preserving the strategy ID and
	// original CreatedAt. Strategy.ID and Items[].ID are backfilled on the input
	// pointer and the returned clone.
	SaveAdvice(ctx context.Context, s *Strategy) (*Strategy, error)

	Get(ctx context.Context, tenantID, id int64) (*Strategy, error) // ErrNotFound/跨租户
	List(ctx context.Context, tenantID int64, f StrategyFilter) ([]*Strategy, error)

	// GetToday loads the advice for (tenant, store, date). ErrNotFound/跨租户.
	GetToday(ctx context.Context, tenantID, storeID int64, date string) (*Strategy, error)

	GetItem(ctx context.Context, tenantID, itemID int64) (*StrategyItem, error) // ErrNotFound/跨租户

	// UpdateItemStatus applies one execution_status transition and stamps
	// executedAt. Illegal transitions yield ErrParam; absent/cross-tenant items
	// yield ErrNotFound. Applied under the repo write lock (race-safe).
	UpdateItemStatus(ctx context.Context, tenantID, itemID int64, status string, executedAt *time.Time, now time.Time) (*StrategyItem, error)

	// RecordFeedback upserts the boss's feedback for one item. UNIQUE(tenant,
	// item) → repeat overwrites Useful/CreatedAt, keeping the feedback ID.
	RecordFeedback(ctx context.Context, tenantID, strategyItemID int64, useful bool, now time.Time) (*StrategyFeedback, error)
}

// isAllowedItemTransition encodes the execution_status state machine
// (CONTRACT §12 constraint 6): pending may move to any terminal; the four
// terminal states are self-idempotent; any other migration is illegal.
// Universal self-transition (to==from) is a no-op success.
func isAllowedItemTransition(from, to string) bool {
	if to == from {
		return true
	}
	if from == ItemPending {
		switch to {
		case ItemExecuted, ItemBlocked, ItemSkipped, ItemFailed:
			return true
		}
	}
	return false
}

// cloneStringAnyMap returns a shallow copy of m (new map, same values). The
// values originate from JSON unmarshal (scalars / slices / maps the caller does
// not mutate in place), so detaching the map container is sufficient to stop a
// caller from adding/removing keys on the stored record.
func cloneStringAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneRawMessage returns a copy of the bytes of b (nil preserved).
func cloneRawMessage(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	out := make(json.RawMessage, len(b))
	copy(out, b)
	return out
}

// cloneItem returns a deep copy of it: maps detached, ExecutedAt pointer
// detached.
func cloneItem(it StrategyItem) StrategyItem {
	cp := it
	cp.Target = cloneStringAnyMap(it.Target)
	cp.Params = cloneStringAnyMap(it.Params)
	cp.ExpectedEffect = cloneStringAnyMap(it.ExpectedEffect)
	if it.ExecutedAt != nil {
		v := *it.ExecutedAt
		cp.ExecutedAt = &v
	}
	return cp
}

// CloneItem returns a deep copy of it (pointer variant, exported for the cache
// package which stores *Strategy snapshots and must hand back defensive copies).
func CloneItem(it *StrategyItem) *StrategyItem {
	if it == nil {
		return nil
	}
	cp := cloneItem(*it)
	return &cp
}

// CloneStrategy returns a deep copy of s: Signals bytes detached, Items slice
// rebuilt with each item deep-copied. Exported so the in-memory repo and the
// cache package share one canonical deep-copy (no duplicate clone logic).
func CloneStrategy(s *Strategy) *Strategy {
	if s == nil {
		return nil
	}
	cp := *s
	cp.Signals = cloneRawMessage(s.Signals)
	if s.Items != nil {
		cp.Items = make([]StrategyItem, len(s.Items))
		for i := range s.Items {
			cp.Items[i] = cloneItem(s.Items[i])
		}
	}
	return &cp
}

// cloneFeedback returns a shallow copy of f (no slices/maps inside).
func cloneFeedback(f *StrategyFeedback) *StrategyFeedback {
	if f == nil {
		return nil
	}
	cp := *f
	return &cp
}
