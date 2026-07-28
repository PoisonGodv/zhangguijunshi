package handler

// SubscriptionHandler exposes the boss-surface plan + paywall endpoints (M4-S1,
// CONTRACT §15.7). The current plan / subscription is read from the ctx tenant;
// Subscribe switches plan; CheckFeature lets the front-end probe a feature gate
// before showing a paywall.

import (
	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/subscription"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// SubscriptionHandler serves subscription endpoints.
type SubscriptionHandler struct {
	svc *subscription.Service
}

// NewSubscriptionHandler constructs a SubscriptionHandler.
func NewSubscriptionHandler(svc *subscription.Service) *SubscriptionHandler {
	return &SubscriptionHandler{svc: svc}
}

// GetSubscription returns the caller tenant's current subscription + plan.
// GET /v1/boss/subscription
func (h *SubscriptionHandler) GetSubscription(c *gin.Context) {
	plan, err := h.svc.CurrentPlan(c.Request.Context())
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	sub, err := h.svc.GetSubscription(c.Request.Context())
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"plan": plan, "subscription": sub})
}

type subscribeRequest struct {
	Plan         string `json:"plan" binding:"required"`
	PeriodMonths int    `json:"period_months"`
	Source       string `json:"source"`
}

// Subscribe switches / renews the caller tenant's plan (manual / admin).
// POST /v1/boss/subscription
func (h *SubscriptionHandler) Subscribe(c *gin.Context) {
	var req subscribeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	source := req.Source
	if source == "" {
		source = "manual"
	}
	sub, err := h.svc.Subscribe(c.Request.Context(), subscription.SubscribeInput{
		Plan:         req.Plan,
		PeriodMonths: req.PeriodMonths,
		Source:       source,
	})
	if err != nil {
		if err == subscription.ErrPlanUpgradeRequired {
			httpx.Fail(c, errcode.ErrPlanUpgradeRequired)
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, sub)
}

type checkFeatureRequest struct {
	Feature string `json:"feature" binding:"required"`
}

// CheckFeature reports whether the caller tenant's current plan allows a feature.
// POST /v1/boss/subscription/check
func (h *SubscriptionHandler) CheckFeature(c *gin.Context) {
	var req checkFeatureRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	ok, err := h.svc.Gate().Can(c.Request.Context(), req.Feature)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"feature": req.Feature, "allowed": ok})
}
