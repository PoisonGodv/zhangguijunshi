package aiengine

// enrich.go implements the audience-resolution + coupon-template enrichment
// stage (aiengine 修复②). It runs AFTER the agent Enhance stage and BEFORE
// converge/output.Encode, and closes the core-selling-point defect where every
// pipeline-generated coupon_wake / group_seckill item landed permanently
// blocked(invalid_item): the pipeline never resolved a concrete member_id /
// coupon_id, so the executor's paramInt64(...,"member_id"/"coupon_id") guards
// always saw 0 and refused to execute.
//
// The stage is driven by two narrow anti-corruption ports:
//
//   - MemberResolver: (target segment, tenant, store) → a concrete member_id.
//   - CouponTemplateSelector: (action, reason, tenant, store) → a coupon_id.
//
// The main thread adapts the real membership / marketing domains to these ports
// in bootstrap/aiengine_build.go. When a port is nil (dev / unwired) OR a
// resolution yields 0 (no matching member / no active coupon template), the
// stage leaves the corresponding Params key UNSET rather than writing a
// fabricated id. The executor then returns an HONEST blocked(audience_unresolved)
// instead of the misleading invalid_item — a deliberate degradation that is
// surfaced to the boss as actionable feedback, never a fake green.
//
// group_seckill semantics (documented choice): exec drives group_seckill as a
// single coupon issuance + one wework_group reach to a member. This stage
// therefore resolves ONE representative member for the segment (the port's
// deterministic pick) and writes it as member_id. True per-member fan-out is a
// reach-domain concern deferred to a later milestone; resolving a representative
// keeps the one-click path executable end-to-end today.

import (
	"context"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"go.uber.org/zap"
)

// MemberResolver resolves a concrete member_id for one routed item's audience.
// target is the RoutedItem.Target segment string (e.g. "30天未到店高价值会员",
// "临期SKU群秒杀"); tenantID/storeID scope the resolution (NEVER trusted from the
// item payload — the pipeline passes the ctx-resolved identity). Returns 0 when
// no member can be resolved for the segment (the enrichment stage then leaves
// member_id unset → exec blocks audience_unresolved). Errors are surfaced for
// logging; the stage treats an error like a 0 result (best-effort, never aborts
// the pipeline).
type MemberResolver interface {
	ResolveMember(ctx context.Context, tenantID, storeID int64, target string) (memberID int64, err error)
}

// CouponTemplateSelector selects an existing coupon template id for an action.
// actionType/reasonType let the adapter prefer a template whose shape matches
// the action (full_reduction for wake, discount for seckill); tenantID/storeID
// scope the lookup. Returns 0 when no usable (active) template exists (the
// enrichment stage then leaves coupon_id unset → exec blocks
// audience_unresolved). Errors are surfaced for logging only.
type CouponTemplateSelector interface {
	SelectCoupon(ctx context.Context, tenantID, storeID int64, actionType, reasonType string) (couponID int64, err error)
}

// Enricher is the enrichment stage port. Enrich mutates each coupon-bearing
// EnhancedItem's Params in place (member_id / coupon_id) and returns the same
// slice. It never returns an error: every failure mode degrades to "leave the
// key unset" so converge/encode always proceed (CONTRACT §12.5.6 spirit — the
// pipeline always has output).
type Enricher interface {
	Enrich(ctx context.Context, tenantID, storeID int64, items []*agent.EnhancedItem) []*agent.EnhancedItem
}

// enrichService is the default Enricher: it drives the two ports for every
// coupon-bearing action (coupon_wake / group_seckill) and writes the resolved
// ids into Params. Both ports are nil-safe (a nil port skips its field).
type enrichService struct {
	members MemberResolver
	coupons CouponTemplateSelector
	log     *zap.Logger
}

// NewEnricher builds the enrichment stage. Either port may be nil — the
// corresponding field is then left unset and the executor degrades to
// blocked(audience_unresolved) for that item (documented degradation).
func NewEnricher(members MemberResolver, coupons CouponTemplateSelector, log *zap.Logger) Enricher {
	if log == nil {
		log = zap.NewNop()
	}
	return &enrichService{members: members, coupons: coupons, log: log}
}

// Enrich resolves member_id / coupon_id for each coupon-bearing item and writes
// them into Params. Only coupon_wake and group_seckill carry a coupon issuance
// in exec; every other action is left untouched. A resolved 0 (or port error)
// leaves the key UNSET — never a fabricated id — so exec reports an honest
// blocked(audience_unresolved) instead of issuing to member 0 / coupon 0.
func (s *enrichService) Enrich(ctx context.Context, tenantID, storeID int64, items []*agent.EnhancedItem) []*agent.EnhancedItem {
	if s == nil || len(items) == 0 {
		return items
	}
	for _, ei := range items {
		if ei == nil || !needsAudience(ei.Routed.ActionType) {
			continue
		}
		if ei.Params == nil {
			ei.Params = make(map[string]any)
		}
		// member_id (representative member for the segment — see package note on
		// group_seckill fan-out).
		if s.members != nil {
			if mid, err := s.members.ResolveMember(ctx, tenantID, storeID, ei.Routed.Target); err != nil {
				s.log.Debug("enrich member resolve failed (degrade to audience_unresolved)",
					zap.Int64("tenant_id", tenantID), zap.Int64("store_id", storeID),
					zap.String("action", ei.Routed.ActionType), zap.Error(err))
			} else if mid > 0 {
				ei.Params["member_id"] = mid
			}
		}
		// coupon_id (an active template chosen by action/reason).
		if s.coupons != nil {
			if cid, err := s.coupons.SelectCoupon(ctx, tenantID, storeID, ei.Routed.ActionType, ei.Routed.ReasonType); err != nil {
				s.log.Debug("enrich coupon select failed (degrade to audience_unresolved)",
					zap.Int64("tenant_id", tenantID), zap.Int64("store_id", storeID),
					zap.String("action", ei.Routed.ActionType), zap.Error(err))
			} else if cid > 0 {
				ei.Params["coupon_id"] = cid
			}
		}
	}
	return items
}

// needsAudience reports whether an ActionType drives a coupon issuance in exec
// and therefore needs member_id + coupon_id enriched. Mirrors exec's dispatch:
// only group_seckill and coupon_wake call CouponIssuer.Issue.
func needsAudience(actionType string) bool {
	return actionType == rule.ActCouponWake || actionType == rule.ActGroupSeckill
}
